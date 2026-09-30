#!/usr/bin/env bash
# install.sh — install/upgrade the encode-system controller on a Docker host.
#
# Usage:
#   ./install.sh                 # latest release
#   ./install.sh v1.17.0         # specific version
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
# Env overrides: ENCODE_HOME (default /opt/encode-system), ENCODE_REPO
# (default badskater/encode-system), GITHUB_TOKEN (for private repos:
# a PAT with repo read access; also used for `docker login ghcr.io`).
set -euo pipefail

REPO="${ENCODE_REPO:-badskater/encode-system}"
HOME_DIR="${ENCODE_HOME:-/opt/encode-system}"
VERSION="${1:-latest}"

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
for f in controller encode-agent.exe ui provision EncodeLib.ps1 \
         Dockerfile.runtime docker-compose.runtime.yml .env.example \
         install-agent.ps1 SHA256SUMS; do
    [ -e "$BUNDLE/$f" ] && cp -r "$BUNDLE/$f" "$HOME_DIR/"
done
chmod +x "$HOME_DIR/controller" "$HOME_DIR/install.sh" 2>/dev/null || true
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
