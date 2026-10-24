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
#   ENCODE_AGENT_ALLOW_HTTP_UPDATES  set to 1 to allow agent self-updates over
#                    plain HTTP (auto-enabled when CONTROLLER_URL is http://; the
#                    agent refuses these downloads otherwise — see agent.go getAuth)
set -euo pipefail

REPO="${REPO:-${ENCODE_REPO:-badskater/encode-system}}"
INSTALL_DIR="${INSTALL_DIR:-/opt/encode-system}"
CONFIG_DIR="${CONFIG_DIR:-/etc/encode-agent}"
VERSION="${VERSION:-latest}"
NODE_NAME="${NODE_NAME:-$(hostname 2>/dev/null || echo linux-node)}"

log() { printf '[install] %s\n' "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

# I1: escape backslash and double-quote for safe interpolation into a JSON
# string value. Operator-supplied values (pairing code, token, node name,
# controller URL) could contain these characters; without escaping a single
# `"` or `\` would break the generated agent.json and the agent would fail to
# start. Backslash+quote escaping is sufficient for the JSON string grammar;
# control characters are not expected in these fields.
json_escape() {
    local s="$1"
    s="${s//\\/\\\\}"
    s="${s//\"/\\\"}"
    printf '%s' "$s"
}

# --- parse args (flags mirror install-agent.ps1 short names) -----------------
CONTROLLER_URL="${CONTROLLER_URL:-}"
PAIRING_CODE="${PAIRING_CODE:-}"
TOKEN="${TOKEN:-}"
while [ $# -gt 0 ]; do
    case "$1" in
        # I2: every value-taking flag checks a value follows before consuming,
        # so e.g. `install-agent.sh -c` (no URL) fails loudly instead of
        # silently proceeding with an empty string.
        -c|--controller-url) [ $# -ge 2 ] || die "-c requires a value"; CONTROLLER_URL="$2"; shift 2 ;;
        -p|--pairing-code)   [ $# -ge 2 ] || die "-p requires a value"; PAIRING_CODE="$2"; shift 2 ;;
        -t|--token)          [ $# -ge 2 ] || die "-t requires a value"; TOKEN="$2"; shift 2 ;;
        -n|--node-name)      [ $# -ge 2 ] || die "-n requires a value"; NODE_NAME="$2"; shift 2 ;;
        -V|--version)        [ $# -ge 2 ] || die "-V requires a value"; VERSION="$2"; shift 2 ;;
        -r|--repo)           [ $# -ge 2 ] || die "-r requires a value"; REPO="$2"; shift 2 ;;
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
    [ -n "${GITHUB_TOKEN:-}" ] && args+=(-H "Authorization: Bearer $GITHUB_TOKEN")
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
# I4: install to a temp path then mv -f into place (atomic rename), so a crash
# mid-install never leaves a truncated binary on disk. Matches the agent.json
# temp+mv pattern below. The service is already stopped above (verified order).
install -m 0755 "$TMP/agent.new" "$INSTALL_DIR/encode-agent.new"
mv -f "$INSTALL_DIR/encode-agent.new" "$INSTALL_DIR/encode-agent"
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
    CRED_LINE="\"pairing_code\": \"$(json_escape "$PAIRING_CODE")\","
elif [ -n "$TOKEN" ]; then
    CRED_LINE="\"token\": \"$(json_escape "$TOKEN")\","
fi
# Build the JSON with the credential line inline. printf to the temp file so
# the secret never crosses argv or an external interpreter's stdin.
{
    printf '{\n'
    printf '  "controller_url": "%s",\n' "$(json_escape "$CONTROLLER_URL")"
    printf '  "node_name": "%s",\n' "$(json_escape "$NODE_NAME")"
    [ -n "$CRED_LINE" ] && printf '  %s\n' "$CRED_LINE"
    printf '  "data_dir": "%s",\n' "$(json_escape "$INSTALL_DIR")"
    printf '  "heartbeat_seconds": 5\n'
    printf '}\n'
} > "$CFG.tmp"
chmod 0600 "$CFG.tmp"
mv -f "$CFG.tmp" "$CFG"
log "wrote $CFG (controller=$CONTROLLER_URL node=$NODE_NAME)"

# --- systemd unit ------------------------------------------------------------
# There are two install channels for the Linux agent, each with a different
# on-disk layout:
#
#   standalone (this script): binary at /opt/encode-system (INSTALL_DIR),
#     runs as root, inline unit written below (no -foreground flag: the agent
#     runs a foreground loop under systemd per service_other.go).
#   provisioned (ansible roles/linux-node): binary at /opt/encode-agent
#     (encode_agent_dir), runs as User=encode, uses the canonical repo unit
#     infra/systemd/encode-agent.service (with -foreground flag + hardening).
#
# This script prefers a bundled unit if one ships with the release tarball;
# otherwise it writes an inline unit. The canonical repo unit lives at
# infra/systemd/encode-agent.service — do NOT change it from here.
#
# Bundled-unit lookup: the release tarball (install.sh) ships the unit at
# systemd/encode-agent.service as a SIBLING of provision/ — matching the
# engine's stageSystemdUnit resolution (PlaybookDir/../systemd/...). So the
# primary lookup is $INSTALL_DIR/systemd/encode-agent.service. The older
# infra/systemd/ path and a bare-neighbor check cover prior bundle layouts and
# a git-checkout run where the script sits next to infra/.
UNIT_FILE="/etc/systemd/system/encode-agent.service"
BUNDLED_UNIT=""
for candidate in \
    "$INSTALL_DIR/systemd/encode-agent.service" \
    "$INSTALL_DIR/infra/systemd/encode-agent.service" \
    "$INSTALL_DIR/encode-agent.service" \
    "./systemd/encode-agent.service" \
    "./infra/systemd/encode-agent.service"; do
    if [ -f "$candidate" ]; then BUNDLED_UNIT="$candidate"; break; fi
done

if [ -n "$BUNDLED_UNIT" ]; then
    log "using bundled systemd unit: $BUNDLED_UNIT"
    install -m 0644 "$BUNDLED_UNIT" "$UNIT_FILE"
else
    log "no bundled unit found — writing inline systemd unit"
    # I3: unquoted heredoc delimiter so $INSTALL_DIR/$CONFIG_DIR expand into
    # the unit. The unit has no shell expansions of its own (verified: no $ in
    # the body below), so this is safe.
    cat > "$UNIT_FILE" <<UNIT
[Unit]
Description=encode-system agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$INSTALL_DIR/encode-agent -config $CONFIG_DIR/agent.json
Restart=always
RestartSec=5
# Hardening: the agent needs network + its data dir, little else.
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=true
PrivateTmp=true
WorkingDirectory=$INSTALL_DIR
ReadWritePaths=$INSTALL_DIR

[Install]
WantedBy=multi-user.target
UNIT
fi

# --- HTTP-update opt-in (I2) -------------------------------------------------
# The agent refuses self-update downloads (lib/bin/agent binaries) over plain
# HTTP unless ENCODE_AGENT_ALLOW_HTTP_UPDATES=1 — a MITM on plaintext HTTP can
# swap payload and checksum together (getAuth, agent.go:403-408). This farm's
# controller is commonly http:// on a trusted LAN. When that is the case, emit
# a systemd drop-in so the env reaches the agent regardless of whether the unit
# was bundled or inline. Drop-ins compose with any unit source and are cleaner
# than editing the shipped file.
HTTP_DROPIN_DIR="/etc/systemd/system/encode-agent.service.d"
if [ "${CONTROLLER_URL#http://}" != "$CONTROLLER_URL" ]; then
    log "controller URL is http:// — enabling ENCODE_AGENT_ALLOW_HTTP_UPDATES (agent updates over plain HTTP); prefer HTTPS in production"
    mkdir -p "$HTTP_DROPIN_DIR"
    cat > "$HTTP_DROPIN_DIR/http-updates.conf" <<'DROPIN'
[Service]
Environment=ENCODE_AGENT_ALLOW_HTTP_UPDATES=1
DROPIN
else
    # An HTTPS controller does not need the opt-in; remove a stale drop-in from
    # a prior install that pointed at HTTP so it does not linger.
    rm -f "$HTTP_DROPIN_DIR/http-updates.conf"
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
