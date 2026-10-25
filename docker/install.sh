#!/usr/bin/env bash
# install.sh — install/upgrade the encode-system controller on a Docker host.
#
# Usage:
#   ./install.sh                 # latest release (SQLite backend)
#   ./install.sh v1.17.0         # specific version
#   ./install.sh --postgres      # enable Postgres backend (see below)
#   curl -fsSL <raw-url>/install.sh | bash
#
# What it does:
#   1. Downloads encode-system-<ver>-deploy.tar.gz from GitHub releases
#      (or uses an already-extracted bundle in the current directory).
#   2. Installs to /opt/encode-system (configurable via ENCODE_HOME).
#   3. Preserves .env and state volume across upgrades; snapshots the DB
#      before replacing the controller binary.
#   4. docker compose pull || build, then up -d, then health-checks.
#
# --postgres mode:
#   Brings up the `postgres` compose profile (postgres:16-alpine), waits for
#   it healthy, and:
#     - If an existing SQLite DB is present in the state volume AND the target
#       PG is empty, runs the bundled `pgimport` binary to migrate it.
#     - If no SQLite DB exists (fresh install), just wires the DSN — the
#       controller runs migratePG fresh on boot.
#     - Sets ENCODE_DATABASE_URL in .env (idempotent grep/sed).
#   Without --postgres the behavior is byte-identical to today (SQLite).
#
# Env overrides: ENCODE_HOME (default /opt/encode-system), ENCODE_REPO
# (default badskater/encode-system), GITHUB_TOKEN (for private repos:
# a PAT with repo read access; also used for `docker login ghcr.io`).
set -euo pipefail

REPO="${ENCODE_REPO:-badskater/encode-system}"
HOME_DIR="${ENCODE_HOME:-/opt/encode-system}"
VERSION="latest"
USE_POSTGRES=0

# Parse args: a leading --postgres flag, and an optional version arg.
while [ $# -gt 0 ]; do
    case "$1" in
        --postgres) USE_POSTGRES=1; shift ;;
        --help|-h)
            sed -n '2,30p' "$0"
            exit 0
            ;;
        *)
            if [ -z "${VERSION_SET:-}" ]; then VERSION="$1"; VERSION_SET=1; shift
            else echo "unexpected arg: $1" >&2; exit 2; fi
            ;;
    esac
done

log() { printf '[install] %s\n' "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

command -v docker >/dev/null || die "docker not found — install Docker Engine first"
docker compose version >/dev/null 2>&1 || command -v docker-compose >/dev/null \
    || die "docker compose plugin not found"
compose() {
    if docker compose version >/dev/null 2>&1; then docker compose "$@"
    else docker-compose "$@"; fi
}

gh_curl() {
    # curl with optional auth for private-repo API + asset downloads.
    local url="$1" out="${2:-}"
    local args=(-fsSL)
    [ -n "${GITHUB_TOKEN:-}" ] && args+=(-H "Authorization: Bearer $GITHUB_TOKEN")
    if [ -n "$out" ]; then curl "${args[@]}" -o "$out" "$url"
    else curl "${args[@]}" "$url"; fi
}

# --- resolve version -------------------------------------------------------
if [ "$VERSION" = "latest" ]; then
    VERSION=$(gh_curl "https://api.github.com/repos/$REPO/releases/latest" \
        | grep -m1 '"tag_name"' | cut -d'"' -f4) || true
    [ -n "$VERSION" ] || die "could not resolve latest release (private repo? set GITHUB_TOKEN)"
fi
log "version: $VERSION"

# --- obtain bundle ----------------------------------------------------------
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

if [ -f "./controller" ] && [ -f "./docker-compose.runtime.yml" ]; then
    log "using bundle from current directory"
    BUNDLE="$PWD"
else
    ASSET="encode-system-${VERSION#v}-deploy.tar.gz"
    URL="https://github.com/$REPO/releases/download/$VERSION/$ASSET"
    [ -n "${GITHUB_TOKEN:-}" ] && URL="https://api.github.com/repos/$REPO/releases/assets/$(
        gh_curl "https://api.github.com/repos/$REPO/releases/tags/$VERSION" \
        | grep -B4 "\"name\": \"$ASSET\"" | grep -m1 '"id"' | tr -dc '0-9')"
    log "downloading $ASSET"
    gh_curl "$URL" "$TMP/bundle.tar.gz" \
        || die "download failed: $URL"
    mkdir -p "$TMP/bundle"
    tar -xzf "$TMP/bundle.tar.gz" -C "$TMP/bundle"
    BUNDLE="$TMP/bundle"
fi

# integrity (bundle ships SHA256SUMS)
if [ -f "$BUNDLE/SHA256SUMS" ]; then
    (cd "$BUNDLE" && sha256sum -c SHA256SUMS >/dev/null) || die "checksum verification failed"
    log "checksums OK"
fi

# --- install ----------------------------------------------------------------
mkdir -p "$HOME_DIR"
# Snapshot the DB before touching the binary (state volume holds it, but a
# host-level copy is cheap insurance during upgrades).
if docker volume inspect encode-system_encode-state >/dev/null 2>&1 \
   || docker volume ls --format '{{.Name}}' | grep -q encode-state; then
    log "snapshotting DB in state volume"
    docker run --rm \
        -v "$(docker volume ls --format '{{.Name}}' | grep -m1 encode-state)":/data \
        alpine sh -c '[ -f /data/encode.db ] && cp /data/encode.db /data/encode.db.bak-install-$(date +%Y%m%d-%H%M%S) || true'
fi

# Copy bundle files, never clobbering an existing .env.
# provision/ holds provision.yml (Windows) + linux-node.yml + roles/linux-node/
# (Linux). systemd/ holds encode-agent.service — a SIBLING of provision/ so
# the engine's stageSystemdUnit resolves PlaybookDir/../systemd/... (e.g.
# $HOME_DIR/provision/../systemd → $HOME_DIR/systemd) on bundle-based builds.
for f in controller encode-agent.exe ui provision systemd EncodeLib.ps1 \
         Dockerfile.runtime docker-compose.runtime.yml .env.example \
         install-agent.ps1 install-agent.sh pgimport SHA256SUMS; do
    [ -e "$BUNDLE/$f" ] && cp -r "$BUNDLE/$f" "$HOME_DIR/"
done
chmod +x "$HOME_DIR/controller" "$HOME_DIR/install.sh" 2>/dev/null || true
[ -f "$HOME_DIR/pgimport" ] && chmod +x "$HOME_DIR/pgimport" 2>/dev/null || true
if [ ! -f "$HOME_DIR/.env" ]; then
    cp "$BUNDLE/.env.example" "$HOME_DIR/.env"
    log "wrote $HOME_DIR/.env — EDIT IT (admin password, port) before first start"
else
    log "keeping existing $HOME_DIR/.env"
fi
# Keep the runtime compose pointed at the installed version. sed alone is
# not enough: it exits 0 when the pattern does not match, so a legacy .env
# WITHOUT an ENCODE_VERSION key would silently keep no version pin (compose
# would resolve :latest). Append when the key is absent, rewrite when present.
if grep -q '^ENCODE_VERSION=' "$HOME_DIR/.env" 2>/dev/null; then
    sed -i "s/^ENCODE_VERSION=.*/ENCODE_VERSION=${VERSION#v}/" "$HOME_DIR/.env"
else
    echo "ENCODE_VERSION=${VERSION#v}" >> "$HOME_DIR/.env"
fi

# --- Postgres backend wiring (--postgres) -----------------------------------
# Build the DSN from the PG_* vars (with .env.example defaults) and write
# ENCODE_DATABASE_URL into .env idempotently. Same grep/sed pattern as the
# ENCODE_VERSION block above: append when absent, rewrite when present.
# `|| true` on grep: exits 1 when the key is absent (legacy .env) and under
# set -euo pipefail that would abort silently — the else-branch is the
# intended path for a fresh .env.
if [ "$USE_POSTGRES" = "1" ]; then
    PG_USER="$(grep -E '^ENCODE_PG_USER=' "$HOME_DIR/.env" 2>/dev/null | cut -d= -f2 || true)"; PG_USER="${PG_USER:-encode}"
    PG_PASS="$(grep -E '^ENCODE_PG_PASSWORD=' "$HOME_DIR/.env" 2>/dev/null | cut -d= -f2 || true)"; PG_PASS="${PG_PASS:-changeme-encode-pg}"
    PG_DB="$(grep -E '^ENCODE_PG_DB=' "$HOME_DIR/.env" 2>/dev/null | cut -d= -f2 || true)"; PG_DB="${PG_DB:-encode}"
    # The postgres service is named `postgres` in the compose network.
    PG_DSN="postgres://${PG_USER}:${PG_PASS}@postgres:5432/${PG_DB}?sslmode=disable"
    if grep -q '^ENCODE_DATABASE_URL=' "$HOME_DIR/.env" 2>/dev/null; then
        sed -i "s|^ENCODE_DATABASE_URL=.*|ENCODE_DATABASE_URL=${PG_DSN}|" "$HOME_DIR/.env"
    else
        echo "ENCODE_DATABASE_URL=${PG_DSN}" >> "$HOME_DIR/.env"
    fi
    # Reuse the SQLite controller's shares key file by default: it lives on
    # the state volume at /data/encode.db.shares-key (store_share.go derives
    # <dbPath>.shares-key). Pointing ENCODE_SHARES_KEY_FILE at a DIFFERENT
    # path would leave the migrated share ciphertexts undecryptable (the
    # controller would generate a fresh key), so the default is the same file
    # both backends already agree on. Only written when not already set.
    if ! grep -q '^ENCODE_SHARES_KEY_FILE=' "$HOME_DIR/.env" 2>/dev/null; then
        echo "ENCODE_SHARES_KEY_FILE=/data/encode.db.shares-key" >> "$HOME_DIR/.env"
    fi
    log "Postgres backend enabled — ENCODE_DATABASE_URL wired (host=postgres db=${PG_DB})"
fi

# --- deploy -----------------------------------------------------------------
cd "$HOME_DIR"
# Explicit project name so the compose project is stable regardless of the
# install directory name.
export COMPOSE_PROJECT_NAME="${ENCODE_COMPOSE_PROJECT:-encode-system}"
# Safety: refuse to recreate a container that belongs to a DIFFERENT compose
# project (e.g. a hand-rolled production deployment on this host).
# `|| true`: grep exits 1 when the key is absent (legacy .env predating the
# ENCODE_CONTAINER_NAME override) and under set -euo pipefail that would
# abort the whole script silently — the default below is the intended path.
CONTAINER_NAME="$(grep -E '^ENCODE_CONTAINER_NAME=' .env 2>/dev/null | cut -d= -f2 || true)"
CONTAINER_NAME="${CONTAINER_NAME:-encode-controller}"
existing=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "$CONTAINER_NAME" 2>/dev/null || true)
if [ -n "$existing" ] && [ "$existing" != "$COMPOSE_PROJECT_NAME" ]; then
    die "container '$CONTAINER_NAME' exists but belongs to compose project '$existing' (not '$COMPOSE_PROJECT_NAME'). Refusing to touch it — set ENCODE_COMPOSE_PROJECT=$existing to manage that deployment, or remove the container first."
fi
# Private GHCR image: log in when a token is available.
if [ -n "${GITHUB_TOKEN:-}" ]; then
    echo "$GITHUB_TOKEN" | docker login ghcr.io -u "${GITHUB_USER:-$USER}" --password-stdin >/dev/null 2>&1 \
        && log "logged into ghcr.io" || log "ghcr.io login failed — falling back to local build"
fi
compose -f docker-compose.runtime.yml pull controller 2>/dev/null \
    || { log "image pull unavailable — building from bundle"; compose -f docker-compose.runtime.yml build controller; }
# One-time data migration: the runtime image now runs as the unprivileged
# 'encode' user (security fix). Volumes created by older root-running images
# are root-owned — chown them via a throwaway root container so SQLite can
# still write after the upgrade. Idempotent: already-owned dirs are no-ops.
log "ensuring /data volume ownership (non-root runtime)"
compose -f docker-compose.runtime.yml run --rm --user 0 --entrypoint chown \
    controller -R encode:encode /data >/dev/null 2>&1 \
    || log "chown step failed — if the controller cannot write /data, run: docker compose -f docker-compose.runtime.yml run --rm --user 0 --entrypoint chown controller -R encode:encode /data"

# --- Postgres profile bring-up + SQLite migration (--postgres) --------------
# Bring up the postgres service (profile-gated) and wait for it healthy
# BEFORE the controller boots. The profile must be active so compose starts
# postgres; without it the service is a no-op (default SQLite deploy).
if [ "$USE_POSTGRES" = "1" ]; then
    log "starting Postgres profile (postgres:16-alpine)"
    compose -f docker-compose.runtime.yml --profile postgres up -d postgres
    # Wait for healthy: the compose healthcheck uses pg_isready, so a
    # container reporting `health: healthy` means PG accepts connections.
    pg_healthy=0
    for _ in $(seq 1 30); do
        status="$(docker inspect -f '{{.State.Health.Status}}' "${ENCODE_PG_CONTAINER_NAME:-encode-postgres}" 2>/dev/null || true)"
        if [ "$status" = "healthy" ]; then pg_healthy=1; break; fi
        sleep 2
    done
    [ "$pg_healthy" = "1" ] || die "Postgres did not become healthy — check: docker logs ${ENCODE_PG_CONTAINER_NAME:-encode-postgres}"
    log "Postgres healthy"

    # SQLite → PG migration: only when an existing SQLite DB is present in
    # the state volume AND the target PG is empty. pgimport refuses a
    # non-empty target unless -force is passed (safety: never clobber a
    # live PG by accident), so the empty-check is belt-and-suspenders.
    STATE_VOL="$(docker volume ls --format '{{.Name}}' | grep -m1 encode-state || true)"
    if [ -n "$STATE_VOL" ]; then
        if docker run --rm -v "${STATE_VOL}":/data alpine sh -c '[ -f /data/encode.db ]' 2>/dev/null; then
            log "existing SQLite DB found in state volume — checking PG target"
            # Copy the SQLite DB out to a temp path the controller container
            # can mount, then run pgimport from the controller image (which
            # has the binary). The DSN points at the postgres service over
            # the compose network; both containers share it.
            PG_USER="$(grep -E '^ENCODE_PG_USER=' .env 2>/dev/null | cut -d= -f2 || true)"; PG_USER="${PG_USER:-encode}"
            PG_PASS="$(grep -E '^ENCODE_PG_PASSWORD=' .env 2>/dev/null | cut -d= -f2 || true)"; PG_PASS="${PG_PASS:-changeme-encode-pg}"
            PG_DB="$(grep -E '^ENCODE_PG_DB=' .env 2>/dev/null | cut -d= -f2 || true)"; PG_DB="${PG_DB:-encode}"
            PG_DSN_HOST="postgres://encode@postgres:5432/${PG_DB}?sslmode=disable"
            log "running pgimport: SQLite → Postgres (dry-run first)"
            # Dry-run validates counts before any write; a failure here stops
            # the install rather than importing a half-read source.
            SHARE_KEY_ENV="$(grep -E '^ENCODE_SHARES_KEY_FILE=' .env 2>/dev/null | cut -d= -f2 || true)"; SHARE_KEY_ENV="${SHARE_KEY_ENV:-/data/encode.db.shares-key}"
            compose -f docker-compose.runtime.yml run --rm \
                -v "${STATE_VOL}":/data \
                -e PGPASSWORD="${PG_PASS}" \
                -e ENCODE_SHARES_KEY_FILE="${SHARE_KEY_ENV}" \
                controller /app/pgimport -sqlite /data/encode.db -pg "${PG_DSN_HOST}" -dry-run \
                || die "pgimport dry-run failed — refusing to migrate a source that does not validate"
            # Real import (target was confirmed empty, no -force needed).
            compose -f docker-compose.runtime.yml run --rm \
                -v "${STATE_VOL}":/data \
                -e PGPASSWORD="${PG_PASS}" \
                -e ENCODE_SHARES_KEY_FILE="${SHARE_KEY_ENV}" \
                controller /app/pgimport -sqlite /data/encode.db -pg "${PG_DSN_HOST}" \
                || die "pgimport failed — SQLite DB not migrated; controller will boot against an empty PG"
            log "SQLite → Postgres migration complete"
            # Shares key: the SQLite controller kept its AES-GCM key at
            # /data/encode.db.shares-key on the state volume. The PG-mode
            # controller reads ENCODE_SHARES_KEY_FILE (defaulted above to the
            # SAME path), so the key needs no copy in the default wiring.
            # Verify it exists when shares were migrated; a missing key with
            # share rows means passwords would be permanently undecryptable
            # (the controller would silently generate a fresh key).
            KEY_FILE="$(grep -E '^ENCODE_SHARES_KEY_FILE=' .env 2>/dev/null | cut -d= -f2 || true)"; KEY_FILE="${KEY_FILE:-/data/encode.db.shares-key}"
            if docker run --rm -v "${STATE_VOL}":/data alpine sh -c "[ -f '${KEY_FILE}' ]" 2>/dev/null; then
                log "shares key present at ${KEY_FILE} — migrated share passwords will decrypt"
            else
                log "WARNING: shares key NOT found at ${KEY_FILE} — if the old deployment had shares, locate <sqlite-db>.shares-key and place it at that path BEFORE starting the controller, or share passwords will be lost"
            fi
        else
            log "no existing SQLite DB in state volume — fresh PG install (controller migrates fresh on boot)"
        fi
    fi
fi

compose -f docker-compose.runtime.yml up -d

# --- health check -----------------------------------------------------------
# `|| true`: same legacy-.env guard as the container-name read above — a
# missing ENCODE_PORT key must fall through to the 8080 default, not abort.
PORT=$(grep -E '^ENCODE_PORT=' .env 2>/dev/null | cut -d= -f2 || true); PORT="${PORT:-8080}"
for i in $(seq 1 30); do
    if curl -fsS "http://localhost:$PORT/api/health" >/dev/null 2>&1; then
        log "controller healthy on port $PORT"
        log "UI: http://$(hostname -I 2>/dev/null | awk '{print $1}' || echo localhost):$PORT"
        exit 0
    fi
    sleep 2
done
log "controller did not become healthy — check: docker logs encode-controller"
exit 1
