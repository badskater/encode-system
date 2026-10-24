#!/usr/bin/env bash
# install-agent.sh — one-command encode-system node installer for Linux.
#
# Mirrors install-agent.ps1 (Windows): downloads encode-agent-linux-amd64 from a
# GitHub release, verifies its sha256 against SHA256SUMS, installs it to
# /opt/encode-system, writes /etc/encode-agent/agent.json with a one-shot pairing
# code (preferred) or a permanent node token, and registers a systemd service
# (encode-agent) that runs the agent with -config pointing at that JSON. The
# controller pushes agent updates automatically after the node registers.
#
# Usage:
#   sudo ./install-agent.sh -c http://192.168.1.10:8080 -p ABC123
#   sudo ./install-agent.sh -c http://192.168.1.10:8080 -t <permanent-token>
#   sudo CONTROLLER_URL=http://192.168.1.10:8080 PAIRING_CODE=ABC123 ./install-agent.sh
#   curl -fsSL <raw-url>/install-agent.sh | sudo bash -s -- -c http://... -p ABC123
#
# Toolchain (ffmpeg, x265, mkvmerge, opusenc, ...) is expected on PATH — provision
# it separately via the UI Provision page or the Ansible bin-tools play. This
# script only installs the agent itself.
#
# Env overrides:
#   CONTROLLER_URL   base URL of the controller (e.g. http://192.168.1.10:8080)
#   PAIRING_CODE     one-shot pairing code from UI -> Nodes -> Issue pairing code
#   TOKEN            permanent node token from UI -> Nodes -> register node
#   NODE_NAME        node name to register as (default: hostname)
#   VERSION          release tag to install (default: latest)
#   REPO             GitHub repo (default: badskater/encode-system)
#   INSTALL_DIR      binary install dir (default: /opt/encode-system)
#   CONFIG_DIR       agent.json dir (default: /etc/encode-agent)
#   GITHUB_TOKEN     PAT with repo read access for private repos
set -euo pipefail

REPO="${REPO:-${ENCODE_REPO:-badskater/encode-system}}"
INSTALL_DIR="${INSTALL_DIR:-/opt/encode-system}"
CONFIG_DIR="${CONFIG_DIR:-/etc/encode-agent}"
VERSION="${VERSION:-latest}"
NODE_NAME="${NODE_NAME:-$(hostname 2>/dev/null || echo linux-node)}"

log() { printf '[install] %s\n' "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

# --- parse args (flags mirror install-agent.ps1 short names) -----------------
CONTROLLER_URL="${CONTROLLER_URL:-}"
PAIRING_CODE="${PAIRING_CODE:-}"
TOKEN="${TOKEN:-}"
while [ $# -gt 0 ]; do
    case "$1" in
        -c|--controller-url) CONTROLLER_URL="${2:-}"; shift 2 ;;
        -p|--pairing-code)   PAIRING_CODE="${2:-}"; shift 2 ;;
        -t|--token)          TOKEN="${2:-}"; shift 2 ;;
        -n|--node-name)     NODE_NAME="${2:-}"; shift 2 ;;
        -V|--version)       VERSION="${2:-}"; shift 2 ;;
        -r|--repo)          REPO="${2:-}"; shift 2 ;;
        -h|--help)
            sed -n '2,30p' "$0" 2>/dev/null || true
            exit 0 ;;
        *) die "unknown argument: $1 (use -c URL -p CODE|-t TOKEN)" ;;
    esac
done

# --- preflight ---------------------------------------------------------------
[ "$(id -u)" -eq 0 ] || die "run as root (systemd install requires root)"
command -v curl >/dev/null || die "curl not found"
command -v sha256sum >/dev/null || die "sha256sum not found"
command -v systemctl >/dev/null || die "systemctl not found — systemd is required"

[ -n "$CONTROLLER_URL" ] || read -r -p "Controller URL (e.g. http://192.168.1.10:8080): " CONTROLLER_URL
if [ -z "$PAIRING_CODE" ] && [ -z "$TOKEN" ]; then
    read -r -p "Pairing code (UI -> Nodes -> Issue pairing code): " PAIRING_CODE
fi
[ -n "$CONTROLLER_URL" ] || die "controller URL (-c or CONTROLLER_URL) is required"
[ -n "$PAIRING_CODE" ] || [ -n "$TOKEN" ] || die "either -p (pairing code) or -t (token) is required"
CONTROLLER_URL="${CONTROLLER_URL%/}"

# curl with optional auth for private-repo API + asset downloads. The token is
# passed as a header value, never interpolated into the URL or echoed to logs.
gh_curl() {
    local url="$1" out="${2:-}"
    local args=(-fsSL)
    [ -n "${GITHUB_TOKEN:-}" ] && args+=(-H "Authorization: Bearer ***")
    if [ -n "$out" ]; then curl "${args[@]}" -o "$out" "$url"
    else curl "${args[@]}" "$url"; fi
}

# --- resolve version ---------------------------------------------------------
if [ "$VERSION" = "latest" ]; then
    VERSION=$(gh_curl "https://api.github.com/repos/$REPO/releases/latest" \
        | grep -m1 '"tag_name"' | cut -d'"' -f4) || true
    [ -n "$VERSION" ] || die "could not resolve latest release (private repo? set GITHUB_TOKEN)"
fi
log "version: $VERSION"

# --- download agent ----------------------------------------------------------
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

ASSET="encode-agent-linux-amd64"
URL="https://github.com/$REPO/releases/download/$VERSION/$ASSET"
log "downloading $ASSET"
gh_curl "$URL" "$TMP/agent.new" || die "download failed: $URL"

# Fetch SHA256SUMS from the same release (same root of trust as install-agent.ps1).
SUMS_URL="https://github.com/$REPO/releases/download/$VERSION/SHA256SUMS"
if ! gh_curl "$SUMS_URL" "$TMP/SHA256SUMS" 2>/dev/null; then
    # Private repo: resolve the asset ID through the API (mirror install.sh).
    REL_INFO="$(gh_curl "https://api.github.com/repos/$REPO/releases/tags/$VERSION")"
    SUMS_ID=$(printf '%s' "$REL_INFO" | grep -B4 '"name": "SHA256SUMS"' | grep -m1 '"id"' | tr -dc '0-9')
    [ -n "$SUMS_ID" ] || die "SHA256SUMS not found in release $VERSION"
    gh_curl "https://api.github.com/repos/$REPO/releases/assets/$SUMS_ID" "$TMP/SHA256SUMS" \
        || die "download failed: SHA256SUMS"
fi

# --- verify checksum ---------------------------------------------------------
# Refuse to install an unverified binary (security audit 2026-10, parity with
# install-agent.ps1). A mismatch means a tampered asset or bad download.
GOT=$(sha256sum "$TMP/agent.new" | awk '{print $1}')
# `|| true`: grep exits 1 when the entry is absent — guard that under
# set -euo pipefail so a missing row aborts via WANT check, not silently.
WANT=$(grep -E "\\s$ASSET\$" "$TMP/SHA256SUMS" | awk '{print $1}' || true)
[ -n "$WANT" ] || die "SHA256SUMS has no entry for $ASSET"
[ "$GOT" = "$WANT" ] || die "checksum mismatch for $ASSET: want $WANT got $GOT - refusing to install"
log "checksum verified: $GOT"

# --- install binary ----------------------------------------------------------
# Stop the service before replacing the binary (upgrade case).
if systemctl is-active --quiet encode-agent 2>/dev/null; then
    systemctl stop encode-agent || true
    sleep 2
fi

mkdir -p "$INSTALL_DIR" "$CONFIG_DIR"
install -m 0755 "$TMP/agent.new" "$INSTALL_DIR/encode-agent"
log "agent binary installed: $INSTALL_DIR/encode-agent"

# --- agent.json --------------------------------------------------------------
# Preserve an existing persisted token (data_dir/token) across reinstalls: the
# agent prefers the persisted credential, so re-pairing is not needed.
# Credentials are never written into the script body — they pass through from
# the env/args the operator supplied directly into the JSON file (mode 0600),
# mirroring install-agent.ps1's Set-Content. The file is written via a temp
# path then renamed, so a crash mid-write never leaves a partial config.
CFG="$CONFIG_DIR/agent.json"
CRED_LINE=""
if [ -n "$PAIRING_CODE" ]; then
    CRED_LINE="\"pairing_code\": \"$PAIRING_CODE\","
elif [ -n "$TOKEN" ]; then
    CRED_LINE="\"token\": \"$TOKEN\","
fi
# Build the JSON with the credential line inline. printf to the temp file so
# the secret never crosses argv or an external interpreter's stdin.
{
    printf '{\n'
    printf '  "controller_url": "%s",\n' "$CONTROLLER_URL"
    printf '  "node_name": "%s",\n' "$NODE_NAME"
    [ -n "$CRED_LINE" ] && printf '  %s\n' "$CRED_LINE"
    printf '  "data_dir": "%s",\n' "$INSTALL_DIR"
    printf '  "heartbeat_seconds": 5\n'
    printf '}\n'
} > "$CFG.tmp"
chmod 0600 "$CFG.tmp"
mv -f "$CFG.tmp" "$CFG"
log "wrote $CFG (controller=$CONTROLLER_URL node=$NODE_NAME)"

# --- systemd unit ------------------------------------------------------------
# Task 1.5 owns infra/systemd/encode-agent.service; prefer it from the bundle if
# present (the deploy tarball ships it alongside the installer), else write an
# inline unit. The agent on Linux runs a foreground loop under systemd
# (service_other.go) — no -foreground flag — so ExecStart points the binary at
# the config file directly.
UNIT_FILE="/etc/systemd/system/encode-agent.service"
BUNDLED_UNIT=""
for candidate in \
    "$INSTALL_DIR/infra/systemd/encode-agent.service" \
    "$INSTALL_DIR/encode-agent.service" \
    "./infra/systemd/encode-agent.service"; do
    if [ -f "$candidate" ]; then BUNDLED_UNIT="$candidate"; break; fi
done

if [ -n "$BUNDLED_UNIT" ]; then
    log "using bundled systemd unit: $BUNDLED_UNIT"
    install -m 0644 "$BUNDLED_UNIT" "$UNIT_FILE"
else
    log "no bundled unit found — writing inline systemd unit"
    cat > "$UNIT_FILE" <<'UNIT'
[Unit]
Description=encode-system agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/encode-system/encode-agent -config /etc/encode-agent/agent.json
Restart=always
RestartSec=5
# Hardening: the agent needs network + its data dir, little else.
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=true
PrivateTmp=true
WorkingDirectory=/opt/encode-system
ReadWritePaths=/opt/encode-system

[Install]
WantedBy=multi-user.target
UNIT
fi

# --- enable + start ----------------------------------------------------------
systemctl daemon-reload
systemctl enable encode-agent
systemctl restart encode-agent
log "service encode-agent enabled and started"

# --- verify ------------------------------------------------------------------
sleep 6
if ! systemctl is-active --quiet encode-agent; then
    log "agent not active — recent logs:"
    journalctl -u encode-agent --no-pager -n 20 || true
    die "agent service failed to stay running — check: journalctl -u encode-agent"
fi
if curl -fsS "$CONTROLLER_URL/api/health" >/dev/null 2>&1; then
    log "controller reachable at $CONTROLLER_URL"
else
    log "WARNING: controller not reachable from this node ($CONTROLLER_URL) — agent will retry"
fi
log "done. The node appears in the UI within one heartbeat (~5s)."
log "Next: provision encode tools on PATH (UI Provision page or Ansible bin-tools)."
