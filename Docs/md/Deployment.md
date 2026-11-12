# Deployment

## Prerequisites

- Storage for `scripts/` and `ReleaseFolders/`: an NFS or SMB server (the unRAID boxes already serve these), or an S3-compatible bucket (MinIO, Ceph RGW, AWS) — configured later from UI → Settings → Storage shares. S3 needs no mounts on the controller or the nodes.
- Linux host/container runtime for the controller; NFS/SMB mounts are only needed when using those transports (compose volumes for the legacy NFS compose; runtime compose uses plain volumes + UI-configured shares).
- Windows Server 2025 nodes with:
  - NFS client feature enabled for NFS transports (Ansible: `Install-WindowsFeature NFS-Clnt`); SMB uses `New-SmbMapping`; S3 needs nothing installed.
  - `C:\bin` populated with encode tools (DGIndexNV, x265_x64, mkvmerge, ffmpeg, eac3to, opusenc, SCXvid) — Ansible copies from a staging dir, or publish a bin zip from the UI.
  - The shares mounted at `C:\Encodes\scripts` and `C:\Encodes\ReleaseFolders` (nfs/smb only).
- Linux nodes (CPU, no GPU required) with:
  - Toolchain on `PATH`: `bash`, VapourSynth (`vspipe`), `x265`, `ffmpeg` (decode-only), `opusenc`, `flac`, `mkvmerge` (mkvtoolnix), `mediainfo`, `jq`, `bc`, coreutils (`cksum`/`sha256sum`), `curl`. `dgindex` is refused on Linux (no GPU/DGIndexNV) — use a flow that omits it (e.g. a VapourSynth-first `default-4k-cpu` flow). `keyframes` works on Linux: it needs the `vapoursynth-scxvid` pip package (Python ≥ 3.12, the Python VapourSynth uses) plus a source filter (`lsmas` or `ffms2`) and the `resize` (zimg) plugin; the linux-node ansible role installs the pip package automatically.
  - `cifs-utils` + `nfs-common` (Debian) or `nfs-utils` (RHEL) for share mounts; S3 needs nothing.
  - systemd (recommended — the agent ships a `encode-agent.service` unit with `Restart=always`) or run the agent as a bare foreground loop.
- Ansible control node with `pywinrm` (or SSH) reachability to the Windows hosts — or use the controller's built-in Web UI provisioning (no workstation needed).

## Controller

### Install from a GitHub release (recommended)

Every `v*` tag publishes a complete deploy bundle and a GHCR image:

```bash
# Controller host (Docker + compose required)
curl -fsSL -o install.sh \
  https://github.com/badskater/encode-system/releases/latest/download/install.sh
sudo GITHUB_TOKEN=<pat> ./install.sh        # private repo; omit token if public
```

`install.sh` installs to `/opt/encode-system` (`ENCODE_HOME` overrides),
preserves `.env` and state across upgrades, snapshots the DB, pulls
`ghcr.io/badskater/encode-system:<ver>` (or builds `Dockerfile.runtime` from
the bundle when the registry is unreachable), starts compose, and
health-checks. Release assets are publicly downloadable even though the repo
is private; the `GITHUB_TOKEN` is only needed for API resolution of the
latest release. Storage shares (NFS/SMB/S3) are then configured from the UI —
S3 needs no host mounts.

**Isolated test install beside production:** set `ENCODE_HOME=/opt/encode-install-test`,
`ENCODE_COMPOSE_PROJECT=encode-test`, and in that dir's `.env`
`ENCODE_PORT=8081` + `ENCODE_CONTAINER_NAME=encode-controller-test`.
install.sh refuses to touch a container owned by a different compose
project (guard reads `ENCODE_CONTAINER_NAME` from `.env`).

> **Gotcha:** `docker compose` commands are scoped by project name. Two
> deploys both defaulting to project `encode-system` will recreate each
> other's containers — always set `COMPOSE_PROJECT_NAME` (or
> `ENCODE_COMPOSE_PROJECT` for install.sh) and `ENCODE_CONTAINER_NAME` when
> running anything but the single production deploy.

Windows nodes install with one elevated PowerShell command using a pairing
code from UI → Nodes:

```powershell
.\install-agent.ps1 -ControllerUrl http://<controller>:8080 -PairingCode <CODE>
```

Linux nodes install with one root command using a pairing code from
UI → Nodes:

```bash
sudo ./install-agent.sh -c http://<controller>:8080 -p <pairing-code>
# or: sudo ./install-agent.sh -c http://<controller>:8080 -t <permanent-token>
# env overrides: CONTROLLER_URL, PAIRING_CODE, TOKEN, NODE_NAME, VERSION,
#   REPO, INSTALL_DIR (default /opt/encode-system), CONFIG_DIR (default
#   /etc/encode-agent), GITHUB_TOKEN (private repos)
```

`install-agent.sh` downloads `encode-agent-linux-amd64` from the GitHub
release, verifies its SHA-256 against `SHA256SUMS`, installs the binary
to `/opt/encode-system`, writes `/etc/encode-agent/agent.json` (mode 0600),
and registers `encode-agent.service` (prefers the bundled
`infra/systemd/encode-agent.service` unit if present, else writes an
inline unit). The toolchain is expected on `PATH` — provision it
separately (UI → Provision or `infra/ansible/linux-node.yml`).

### Manual compose (build from source)

```bash
cd docker
cp .env.example .env   # set ENCODE_ADMIN_PASSWORD (or leave empty to auto-generate), ports, storage vars as needed
docker compose up -d
```

Compose mounts the NFS shares into `/data/scripts` and `/data/release` (see
`docker/docker-compose.yml`). Controller state lives in the persistent
`encode-state` volume mounted over the whole `/data` dir (SQLite DB at
`/data/encode.db` + update payloads); the share mounts are layered on top as
sub-mounts. The SPA is baked into the image at `/app/ui` (served via
`ENCODE_UI_DIR`), so it stays out of the data volume.

All observability and admin state — full job logs, step timings, node metrics
history, retry/priority bookkeeping, stats, audit log, API tokens, share
credentials (encrypted key file beside the DB), and DB backup snapshots —
lives in the same SQLite data dir; the 2026-08/09 feature sets add no new
volumes, services, or infra.

**First deploy?** Follow `Docs/md/FirstNodeDeploy.md` — a copy-paste runbook
covering controller setup, toolchain staging, inventory, credentials
(manual token or pairing code), and smoke verification.

First-boot env:

| Var | Meaning |
| --- | --- |
| `ENCODE_ADMIN_USER` | Management-plane login username (default `admin`). |
| `ENCODE_ADMIN_PASSWORD` | Initial password — used only on the boot that creates the account. Leave empty to auto-generate (logged once). Rotate later via UI → *Change password*, then delete this line from `.env`. |
| `ENCODE_ADMIN_FORCE_PASSWORD` | Lost-password recovery hatch. With `=1` set, `ENCODE_ADMIN_PASSWORD` overwrites the stored admin hash once (logged as a warning). Rotate via UI immediately after and remove both vars. |
| `ENCODE_DATA` | Data dir (default `/data`). |
| `ENCODE_SCAN_INTERVAL` | Seconds between share scans (default `30`). |
| `ENCODE_TASKS_BEFORE_REBOOT` | Default `10`. |
| `ENCODE_LISTEN` | Listen address (default `:8080`). |
| `ENCODE_TLS_CERT` / `ENCODE_TLS_KEY` | Native HTTPS: PEM cert + key file paths. Both set → TLS 1.2+; one set → fail-fast; neither → plain HTTP (default). See [Native HTTPS](#native-https-controller-tls). |
| `ENCODE_BACKUP_ENABLED` / `ENCODE_BACKUP_EVERY_SECONDS` | Seed the scheduled DB-backup defaults (live-editable afterwards via `PUT /api/backup/settings`). |
| `ENCODE_GROUP` / `ENCODE_TAG` / `ENCODE_DEFAULT_FLOW` | Seed release naming + the default flow name (live Settings afterwards). |
| `ENCODE_DISCORD_WEBHOOK` | Seeds the live Settings webhook (saved blank turns notifications off). |
| `ENCODE_NODE_SCRIPTS/RELEASE/BIN` | Seed the node path mapping used by the job renderer when there are no shares yet. |
| `ENCODE_GITHUB_TOKEN` | PAT for authenticated GitHub API calls (update check). Needed for private repos; empty = unauthenticated (lower rate limit). |
| `ENCODE_GITHUB_REPO` | GitHub `owner/repo` to check for latest releases (default `badskater/encode-system`). |
| `ENCODE_GITHUB_API` | GitHub API base URL (default `https://api.github.com`; override for tests or GitHub Enterprise `https://ghe.example/api/v3`). |

Compose/installer-only vars: `ENCODE_PORT` (host port), `ENCODE_VERSION`
(GHCR image tag, set by install.sh), `ENCODE_CONTAINER_NAME` (container name
override — required for isolated test deploys), `ENCODE_HOME` /
`ENCODE_COMPOSE_PROJECT` / `ENCODE_REPO` (install.sh overrides),
`NFS_SERVER` (legacy NFS compose only).

## Postgres backend (optional)

The default backend is SQLite (state lives in the `encode-state` volume at
`/data/encode.db`). For deployments that want a managed/HA database or a
shared backend across controller instances, the controller can run against
Postgres instead — selected by a single env var, with no code change.

### `ENCODE_DATABASE_URL`

Empty (the default) keeps the SQLite behavior byte-for-byte. Set it to a
`postgres://` DSN to switch the controller to the Postgres backend:

```bash
# .env
ENCODE_DATABASE_URL=postgres://encode:secret@postgres:5432/encode?sslmode=disable
```

On boot the controller opens the PG backend (`store.Open` → `migratePG`),
which creates the full schema fresh on a new database and is a no-op on an
existing one — so the first boot of a fresh PG is a self-migrating install.
The controller logs one line stating which backend is active, with the DSN
**redacted** (only scheme+host+db; the password is never logged). If PG is
unreachable the controller exits non-zero with the connection error (same
fail-fast path as a corrupt SQLite file).

### Compose profile

The runtime compose ships an **optional** `postgres` service
(`postgres:16-alpine`), profile-gated (`profiles: [postgres]`) so the default
`docker compose up -d` is byte-identical to the SQLite-only deploy. To run
the controller against the bundled Postgres:

```bash
# 1. Set the DSN + PG credentials in .env (see .env.example):
#    ENCODE_DATABASE_URL, ENCODE_PG_USER/PASSWORD/DB
# 2. Bring up the postgres profile + controller together:
docker compose --profile postgres up -d
```

The controller does **not** hard-`depends_on` postgres: a non-profiled
service may not `depends_on` a profiled one (compose errors in default
mode), so the ordering is handled by bringing the profile up explicitly.
The postgres healthcheck (`pg_isready`) makes it ready before the controller
connects; the controller fail-fast-exits if PG is unreachable, so a
misordered `up` self-corrects on the next restart.

### `install.sh --postgres`

`install.sh --postgres` automates the whole path: brings up the postgres
profile, waits for it healthy, and:

- If an existing SQLite DB is present in the state volume **and** the target
  PG is empty, runs the bundled `pgimport` binary to migrate it (dry-run
  validates counts first, then the real import).
- If no SQLite DB exists (fresh install), just wires the DSN — the
  controller runs `migratePG` fresh on boot.
- Sets `ENCODE_DATABASE_URL` in `.env` idempotently (and
  `ENCODE_SHARES_KEY_FILE` to a path on the state volume).

Without `--postgres` the behavior is byte-identical to today (SQLite).

### `pgimport` (standalone SQLite → Postgres migration)

The release ships a `pgimport` binary (linux/amd64) for one-shot SQLite →
Postgres migration outside `install.sh`:

```bash
# Migrate an existing SQLite DB into a fresh PG (refuses a non-empty target):
./pgimport -sqlite /data/encode.db -pg 'postgres://encode@host:5432/encode?sslmode=disable'

# Dry-run (validate counts, write nothing):
./pgimport -sqlite /data/encode.db -pg '<dsn>' -dry-run

# Re-import into a non-empty target (TRUNCATEs every table CASCADE first):
./pgimport -sqlite /data/encode.db -pg '<dsn>' -force
```

It copies tables in FK order, preserves explicit ids, advances the PG
sequences via `setval`, and verifies per-table counts + key aggregates via
the store package afterwards. See `cmd/pgimport` for the full design notes.

### Shares key file on Postgres

Share credentials (SMB password, S3 secret key) are AES-GCM encrypted with a
key file that lives **beside** the SQLite DB (`<dataDir>/encode.db.shares-key`),
never inside it — so the DB backup never leaks the key. On Postgres the DB is
remote, so the key file resolves to `ENCODE_SHARES_KEY_FILE` (or a temp-dir
path derived from the DSN's db name as a fallback). **Set
`ENCODE_SHARES_KEY_FILE` to a path on the `encode-state` volume** so the key
survives container recreation, and back it up out of band — losing it means
stored share passwords can't be decrypted.

## Windows nodes (Ansible)

```bash
cd infra/ansible
cp inventory.example inventory.yml          # fill real hosts
cp group_vars/secrets.yml.example group_vars/secrets.yml   # local only, gitignored
ansible-playbook -i inventory.yml site.yml
```

Playbook order in `site.yml`:

1. `nfs-client.yml` — enable `NFS-Clnt` feature, mount both shares to `C:\Encodes\*` (persist via `New-SmbMapping`/registry per Ansible's `win_mount` equivalent).
2. `bin-tools.yml` — create `C:\bin`, copy encode binaries from `files/bin/` staging.
3. `agent.yml` — create `C:\encode-agent` dir, install `encode-agent.exe` + `EncodeLib.ps1`, register the Windows service, write `agent.json` config (controller URL + node token).

Node credentials — two options:

- **Manual token**: register the node in the UI (Nodes page), copy the
  one-time token into `encode_node_tokens` in `group_vars/secrets.yml`
  (never commit).
- **Pairing code** (zero-touch): issue a pairing code in the UI (valid 1
  hour), and put `pairing_code` + `node_name` into the node's `agent.json`
  instead of a token (Ansible template supports both). The agent registers
  itself on first start and stores its own credential.

## Linux nodes (Ansible)

The controller's provision engine invokes `infra/ansible/linux-node.yml`
over SSH with a generated inventory + a 0600 JSON vars file (SSH
credentials, CIFS share passwords, and a one-shot pairing code — never
on a command line; all tasks are `no_log: true`). From the Web UI
Provision page this is automatic; from a workstation:

```bash
cd infra/ansible
# The controller engine generates inventory + vars.json; a manual run needs:
#   ansible-playbook -i inventory.yml linux-node.yml \
#     -e @vars.json   # 0600, carries SSH creds + CIFS passwords + pairing code
```

The `roles/linux-node` role (`roles/linux-node/tasks/main.yml`):

1. Installs packages: `cifs-utils` + `nfs-common` (Debian/apt) or
   `cifs-utils` + `nfs-utils` (RHEL/dnf) — auto-detected by
   `ansible_os_family`.
2. Installs the `vapoursynth-scxvid` pip plugin for the `keyframes` step:
   first a preflight asserts the node's `python3` is ≥ 3.12 (the wheel's
   `Requires-Python`; fails with an actionable message otherwise), then
   `pip3 install vapoursynth-scxvid` with
   `environment: PIP_BREAK_SYSTEM_PACKAGES: "1"` for PEP 668 distros
   (Debian 12+). The env var is used instead of the pip module's
   `break_system_packages` parameter because that parameter requires
   ansible-core ≥ 2.17 — the env var works on older cores too (pip itself
   honors it; same effect). The plugin must land in the SAME Python
   `vspipe` uses; a venv's site-packages is not auto-loaded.
3. Creates the `encode` service user (`system: true`, `nologin` shell)
   and agent + bin + mount directories.
4. Copies the agent binary to `/opt/encode-agent/encode-agent` (0755),
   writes `/etc/encode-agent/agent.json` (0640, built as a dict and
   serialized with `to_nice_json` so path backslashes are escaped
   correctly; `no_log: true` because it briefly carries a pairing
   code), and deploys the systemd unit from
   `infra/systemd/encode-agent.service` (single source of truth — the
   role never inlines a unit that could drift; `After=` automount units
   + `ReadWritePaths=`/`NoNewPrivileges=`/`PrivateTmp=` hardening).
5. Mounts CIFS shares (`ansible.posix.mount`, `state: mounted`) with
   per-share 0600 credential files
   (`/etc/encode-agent/cifs-scripts.cred`, `cifs-release.cred`, written
   via `copy content`) and `x-systemd.automount` + `x-systemd.idle-timeout=60`
   + `noauto` opts (so systemd generates a `.automount` unit from fstab
   — a dead NAS never holds the boot path). NFS shares mount the same
   way (no credentials needed).
6. Enables + starts `encode-agent.service`, waits for the agent to pair
   (polls for `<data_dir>/node.token`), then strips the one-shot pairing
   code from `agent.json` (idempotent, atomic-ish copy).

Two install channels diverge — both are documented:

- **Standalone `install-agent.sh`** → `/opt/encode-system`, runs as
  root, prefers the bundled systemd unit else writes an inline unit.
- **Controller-provisioned (ansible)** → `/opt/encode-agent`, `User=`
  encode hardened unit from `infra/systemd/encode-agent.service`.

Unit name is `encode-agent.service` in both. The agent's `swap-update.sh`
self-update works in both channels (`os.Executable()` resolves the
running binary).

### Per-platform node paths (mixed Windows/Linux farms)

The Settings page exposes three optional overrides — `linux_bin_dir`,
`linux_scripts_dir`, `linux_release_dir` — used only when a job renders
for a node whose `platform` is `linux`. When a field is non-empty, the
rendered bash script carries the POSIX path instead of the Windows value
(`node_bin_dir`/`node_scripts_dir`/`node_release_dir`). When empty, the
Windows value is used unchanged (back-compat: nothing changes until an
operator sets them). S3-staged jobs ignore these overrides: the scripts
and release roles point at the per-job `{{JOBDIR}}` staging placeholder
regardless of platform. Set these when Linux nodes mount shares or tools
at different locations than the Windows nodes (e.g. `/mnt/encode/scripts`
on Linux vs `C:\Encodes\scripts` on Windows).

### WinRM over HTTPS (recommended)

Plain WinRM (`http`/5985) transmits the WinRM password unencrypted on the
LAN. Migrate each node to `https`/5986 once:

```bash
cd infra/ansible
# 1. bootstrap an HTTPS listener (self-signed by default) over the current connection
ansible-playbook -i inventory.yml site.yml --tags winrm-https
# 2. switch inventory.yml group vars:
#      ansible_winrm_scheme: https
#      ansible_port: 5986
#      ansible_winrm_server_cert_validation: ignore   # self-signed (homelab)
#                                                       # or 'validate' with a CA cert
# 3. re-run to confirm management works over 5986
ansible-playbook -i inventory.yml site.yml
```

CA-issued certs: set `encode_winrm_cert_thumbprint` in `group_vars/all.yml`
(and `cert_validation: validate` in inventory) to pin identity instead of
trusting any self-signed cert. The controller-driven Provision page exposes
the same choice: **Enable WinRM HTTPS listener** (bootstrap) and **Accept
self-signed listener certificate** (opt-in `ignore`; off = `validate`).

### WinRM password rotation

`group_vars/all/secrets.yml` holds the account password in plaintext
(local-only, gitignored, mode 600). Rotate it fleet-wide with:

```bash
cd infra/ansible
ansible-playbook -i inventory.yml rotate-winrm-password.yml
# interactive prompt for the new password (avoid -e on the CLI: shell history)
```

The playbook sets the new password on every node, **verifies it actually
authenticates over WinRM**, and only then rewrites `ansible_password` in
`secrets.yml` (keeping mode 600 and all other keys). If any node rejects
the new password the run aborts *before* touching the secrets file, so
local management can never be locked out by a failed rotation — re-run is
idempotent.

### Security hardening knobs (2026-10 audit)

| Setting | Where | Default | Effect |
| --- | --- | --- | --- |
| `ENCODE_BIND_ADDR` | controller `.env` | `0.0.0.0` | bind API to one interface; set `127.0.0.1` behind a reverse proxy |
| `ENCODE_TLS_CERT` / `ENCODE_TLS_KEY` | controller `.env` | unset (plain HTTP) | native HTTPS on the controller (both or neither; partial config fail-fasts) |
| `ENCODE_AGENT_CA_CERT` | agent node env | unset (system roots) | PEM CA file appended to the agent's trust pool for all controller requests (native controller TLS) |
| `ENCODE_AGENT_ALLOW_HTTP_UPDATES` | agent node env | unset (refuse) | allow lib/bin/agent downloads over plain HTTP (trusted LAN only; unnecessary with an https:// controller) |
| `ENCODE_BIN_FETCH_PRIVATE` | controller env | unset (refuse) | allow `publishBinFromURL` to fetch from LAN/localhost hosts (SSRF guard opt-in) |
| `-ServiceUser` / `-ServicePassword` | `install-agent.ps1` | LocalSystem | run the agent service as a least-privilege account |

**Runtime image upgrade note:** `docker/Dockerfile.runtime` now runs as the
unprivileged `encode` user. `install.sh` performs a one-time `chown` of the
existing `/data` volume via a throwaway root container; for manual compose
deploys run once after upgrading:

```bash
docker compose -f docker-compose.runtime.yml run --rm --user 0 --entrypoint chown \
    controller -R encode:encode /data
```

**Controller image provisioning collections:** both `docker/Dockerfile` and
`docker/Dockerfile.runtime` pin `ansible.windows:3.7.0` (Windows
`provision.yml` over WinRM) and `ansible.posix:2.2.2` (Linux
`roles/linux-node`'s `ansible.posix.mount` tasks for CIFS/NFS mounts). An
unpinned rebuild could pull a breaking release and silently break
provisioning; the pins are the guard. Control-side ansible-core is
deliberately NOT pinned to a minimum: the linux-node role avoids
core-version-gated module parameters (the scxvid pip task sets
`PIP_BREAK_SYSTEM_PACKAGES` via `environment:` rather than
`break_system_packages`, which needs core ≥ 2.17), so provisioning works
on older cores as well. The release bundle ships the
`provision/` and `systemd/` trees, and both Dockerfiles COPY them into
the GHCR image at `/app/provision` and `/app/systemd`
(`ENCODE_PROVISION_DIR=/app/provision` sets the engine's `PlaybookDir`,
default in `backend/cmd/controller/main.go`).

## Native HTTPS (controller TLS)

The controller can terminate TLS itself — no reverse proxy required. The
**whole server** (API, UI, `/metrics`, health) moves to HTTPS on the same
port, and agents verify the certificate against a CA you distribute.

| Var | Where | Meaning |
| --- | --- | --- |
| `ENCODE_TLS_CERT` | controller env / `.env` | PEM certificate file path (inside the container when compose-mounted). |
| `ENCODE_TLS_KEY` | controller env / `.env` | Matching PEM private key file path. |
| `ENCODE_AGENT_CA_CERT` | agent node env | PEM CA certificate file. The agent **appends** it to the system trust pool and uses it for EVERY controller request: pairing, heartbeats, completion reports, and lib/bin/agent update downloads. |

Behavior rules (all fail-fast, tested):

- **Both** controller vars set → HTTPS with TLS 1.2 minimum.
- **Exactly one** set → the controller refuses to start, naming the missing
  var. It never silently falls back to HTTP.
- **Neither** set → plain HTTP, byte-identical to previous releases. This is
  the default; existing deploys need no change.
- Unreadable/invalid/mismatched cert or key files → startup error with the
  offending paths.
- `ENCODE_AGENT_CA_CERT` missing/unreadable/not-PEM → the agent refuses to
  start. Public CAs keep working (append, never replace). There is no
  `InsecureSkipVerify` anywhere — a wrong or untrusted cert fails the
  handshake.
- With an `https://` controller URL, **`ENCODE_AGENT_ALLOW_HTTP_UPDATES` is
  no longer needed** — that opt-in only gates plain-HTTP downloads. Remove
  it (and the `http-updates.conf` systemd drop-in) after the cutover.

### Compose wiring

All three composes (`docker-compose.runtime.yml`, `docker-compose.yml`,
`docker-compose.ha.yml`) pass `ENCODE_TLS_CERT`/`ENCODE_TLS_KEY` through from
`.env` (empty default = plain HTTP) and carry commented read-only cert/key
mounts. To enable:

1. Place the cert + key under the compose dir (e.g. `tls/controller.crt`,
   `tls/controller.key`; recipe below).
2. Uncomment the two `./tls/...` volume lines in the compose file.
3. In `.env` set `ENCODE_TLS_CERT=/tls/controller.crt`,
   `ENCODE_TLS_KEY=/tls/controller.key`, plus the healthcheck probe override:
   `ENCODE_HEALTHCHECK_URL=https://localhost:8080/api/health` and (self-signed
   CA only) `ENCODE_HEALTHCHECK_FLAGS=--no-check-certificate`. The flag
   relaxes ONLY the container's local probe — agents still verify via
   `ENCODE_AGENT_CA_CERT`. If you verify the probe WITH the CA instead
   (no `--no-check-certificate`), the URL must use a SAN-covered name/IP:
   the recipe's SANs are `DNS:encode.lan` + `IP:<CTRL_IP>`, NOT `localhost`,
   so `--ca-cert` + `https://localhost:8080` fails hostname verification
   (and a public CA can never cover `localhost`) — use e.g.
   `ENCODE_HEALTHCHECK_URL=https://<CTRL_IP>:8080/api/health`, or keep
   `--no-check-certificate` for the loopback probe.
4. `docker compose -f docker-compose.runtime.yml up -d` (recreates the
   container). `install.sh` detects the TLS vars and health-probes over
   https automatically.

HA note: with Traefik terminating TLS at the edge, native TLS is usually
left unset (Traefik→replica stays HTTP on the internal network). Set it only
when the replicas themselves must serve HTTPS — and then ALSO set
`ENCODE_TRAEFIK_SCHEME=https` in `.env`: Traefik's backend scheme defaults to
`http`, and plain HTTP against a TLS listener 502s every request. The recipe
below applies unchanged — both replicas mount the same `./tls/` files, so the
single ownership step covers both.

### Self-signed CA recipe (LAN farms)

Run as root on the controller host (or any trusted machine — the ownership
step needs root), then copy the files as noted. Adjust the two variables to
YOUR farm — the SANs must cover every name/IP agents will dial (an
`https://<ip>:8080` URL needs the **IP SAN**; no DNS lookup happens):

```bash
CTRL_IP=172.24.92.232          # the address agents use in controller_url
CTRL_DNS=encode.lan            # optional hostname agents may use instead
mkdir -p /opt/encode-system/tls && cd /opt/encode-system/tls

# 1. Farm CA. Keep ca.key 0600 and backed up offline — it signs everything
#    the agents will trust. 10-year lifetime is typical for a private CA.
openssl genrsa -out ca.key 4096
openssl req -x509 -new -key ca.key -sha256 -days 3650 \
  -subj "/CN=encode-farm CA" -out ca.crt

# 2. Server key + CSR.
openssl genrsa -out controller.key 2048
openssl req -new -key controller.key -subj "/CN=encode-controller" \
  -out controller.csr

# 3. Sign with SANs for every controller name/IP + serverAuth usage.
cat > san.cnf <<EOF
subjectAltName = DNS:${CTRL_DNS}, IP:${CTRL_IP}
extendedKeyUsage = serverAuth
keyUsage = digitalSignature, keyEncipherment
EOF
openssl x509 -req -in controller.csr -CA ca.crt -CAkey ca.key \
  -CAcreateserial -days 825 -sha256 -extfile san.cnf -out controller.crt

# 4. Permissions + ownership for the container. Both controller images run
#    as `encode` = uid 100, gid 101 (docker/Dockerfile on Alpine,
#    docker/Dockerfile.runtime on Debian; verify with
#    `docker compose -f docker-compose.runtime.yml exec controller id`).
#    Bind mounts keep HOST ownership: a root-owned 0600 key is unreadable
#    inside the container and the controller exits at boot with
#    "load TLS keypair … permission denied". Own the two mounted files as
#    the container user. ca.key never enters the container — keep it
#    root-owned and backed up offline.
chmod 600 ca.key controller.key
chown 100:101 controller.crt controller.key
# Sanity: the SANs must list the IP/hostname agents will use.
openssl x509 -in controller.crt -noout -text | grep -A1 "Subject Alternative Name"
```

Controller placement (compose deploy): `controller.crt`/`controller.key` go
to `/opt/encode-system/tls/` (mounted per the wiring above); set the `.env`
vars; recreate. Bare-binary deploy: point `ENCODE_TLS_CERT`/`ENCODE_TLS_KEY`
at the files directly and restart the process — the key must be readable by
the user the process runs as (the recipe's `chown` covers the uid-100
container case).

Agent placement — copy **`ca.crt` only** (never `ca.key`) to each node:

```powershell
# Windows node (elevated PowerShell). Example path for the farm's
# scheduled-task installs (C:\encode-agent-dist); service installs can use
# C:\encode-agent\encode-ca.crt the same way.
# 1. Copy ca.crt to the node, e.g. C:\encode-agent-dist\encode-ca.crt
# 2. Machine-level env (picked up by services AND scheduled tasks on start):
[Environment]::SetEnvironmentVariable("ENCODE_AGENT_CA_CERT", "C:\encode-agent-dist\encode-ca.crt", "Machine")
# 3. Point agent.json at https:
#    "controller_url": "https://172.24.92.232:8080"
# 4. Restart the agent:
Restart-Service encode-agent                       # service installs
Stop-ScheduledTask EncodeAgentDist; Start-ScheduledTask EncodeAgentDist  # task installs
```

```bash
# Linux node: install-agent.sh passes the env through as a systemd drop-in.
sudo cp ca.crt /etc/encode-agent/encode-ca.crt
sudo ENCODE_AGENT_CA_CERT=/etc/encode-agent/encode-ca.crt \
  ./install-agent.sh -c https://<controller>:8080 -p <pairing-code>
# Existing install instead: write /etc/systemd/system/encode-agent.service.d/ca-cert.conf
#   [Service]
#   Environment="ENCODE_AGENT_CA_CERT=/etc/encode-agent/encode-ca.crt"
# edit controller_url in agent.json to https://, then:
sudo systemctl daemon-reload && sudo systemctl restart encode-agent
```

### Switching an existing farm (restart order)

The cutover is **controller first, then nodes**. Old agents keep retrying
heartbeats every tick against the TLS-only controller (warnings in the log,
no crash) until they are switched; jobs are held by the drain.

1. Generate the CA + server cert (recipe above); place the files on the
   controller host. Verify the SANs cover the address agents use.
2. **Drain the farm** (UI → Settings → Drain mode) or wait for an idle
   window: a job running during the cutover would lose its completion
   report (the old-URL agent cannot reach the TLS-only controller), leaving
   the job to stall-watchdog/orphan recovery. Running jobs finish first.
3. **Controller**: set the `.env` TLS vars + healthcheck override, uncomment
   the mounts, `docker compose -f docker-compose.runtime.yml up -d`.
   Verify: `curl --cacert /opt/encode-system/tls/ca.crt https://<CTRL_IP>:8080/api/health`
   → `{"status":"ok",...}`. The container must report healthy.
4. **Each node**: copy `ca.crt`, set `ENCODE_AGENT_CA_CERT` (machine env /
   systemd drop-in), change `controller_url` to `https://…`, restart the
   agent (service or scheduled task). Nodes reconnect on the next heartbeat
   (~5s); confirm `online`/`idle` in UI → Nodes. Repeat node-by-node.
5. Release the drain. Remove `ENCODE_AGENT_ALLOW_HTTP_UPDATES` (and the
   `http-updates.conf` drop-in on Linux nodes) — TLS makes it unnecessary.
   Publish an agent update once to confirm the download path works over TLS.

**Rollback**: remove BOTH `ENCODE_TLS_*` vars AND the healthcheck override
(`ENCODE_HEALTHCHECK_URL`/`ENCODE_HEALTHCHECK_FLAGS`) from `.env`, recreate
the controller (plain HTTP returns), and point nodes' `controller_url` back to
`http://`. A leftover `https://` healthcheck probe against a plain-HTTP
controller fails (wget exit 4) and the container stays permanently unhealthy.
The CA env can stay on nodes — it is inert for plain HTTP.

**Publicly trusted certs**: skip the CA recipe — use a cert from your normal
issuer (or Let's Encrypt via the Traefik HA path). Agents need no
`ENCODE_AGENT_CA_CERT` then (system roots already trust it), and the
healthcheck needs no `--no-check-certificate`.

## Optional OIDC login

The controller can offer **Sign in with SSO** (OpenID Connect) next to the
local username/password form. It is **OFF by default and entirely optional**
— a LAN farm with local accounts needs none of this, and with the vars unset
the controller registers no OIDC routes and behaves exactly as before.
Local login keeps working unchanged when OIDC is enabled; both coexist.

### Environment variables

| Var | Required | Meaning |
| --- | --- | --- |
| `ENCODE_OIDC_ISSUER` | yes* | Provider identifier URL. Must EXACTLY match the `iss` in the provider's discovery document (`<issuer>/.well-known/openid-configuration`). |
| `ENCODE_OIDC_CLIENT_ID` | yes* | Client ID registered on the provider. |
| `ENCODE_OIDC_CLIENT_SECRET` | yes* | Client secret. Never logged; treat like any credential (`.env` perms, secrets manager). |
| `ENCODE_OIDC_REDIRECT_URL` | yes* | The controller's callback URL, e.g. `https://encode.example.com/api/auth/oidc/callback`. Must EXACTLY match a redirect URI registered on the provider. |
| `ENCODE_OIDC_ADMIN_GROUP` | no | Group name that grants the **admin** role. Unset = no group mapping (see below). |
| `ENCODE_OIDC_GROUPS_CLAIM` | no | ID-token claim carrying the group list (default `groups`). |

\* All four core vars together enable OIDC. **All unset = disabled** (the
default). **A partial set fails fast at startup** with an error naming the
missing vars — the controller never silently falls back to local-only when
you intended SSO (same contract as `ENCODE_TLS_CERT`/`ENCODE_TLS_KEY`).

Docker deploys: all six vars are passed through by every compose file
(`docker-compose.yml`, `docker-compose.runtime.yml`, `docker-compose.ha.yml`);
set them in `.env` (commented template in `docker/.env.example`).

### Provider setup (generic)

Works with any standards-compliant OIDC provider — pocket-id, Authelia,
Keycloak, Authentik, etc.:

1. Create a **confidential client** (authorization-code flow) for the
   controller. Note the client ID and secret.
2. Register the redirect URI **exactly** as
   `https://<controller-host>/api/auth/oidc/callback` (scheme, host, port and
   path must match `ENCODE_OIDC_REDIRECT_URL` character-for-character).
3. Ensure the provider issues the `openid profile email` scopes and includes
   `preferred_username` (or `email`) in the ID token.
4. For role mapping, make the provider emit the user's groups in the ID
   token (Keycloak: a "group membership" mapper on the client; Authelia:
   `groups` claim via access-control configuration; pocket-id: user groups).
   Set `ENCODE_OIDC_ADMIN_GROUP` to the group that should be admin.
5. **NTP-sync the controller host.** ID-token expiry is verified with ZERO
   clock-skew leeway (`go-oidc`; only the optional `nbf` claim gets a 5-minute
   grace). A controller clock drifting ahead of the IdP rejects fresh tokens
   as expired — SSO logins fail while local login keeps working.
6. Set the four core vars (+ optional two) and restart the controller. The
   login page shows **Sign in with SSO** only when enabled — the UI learns
   the flag from the unauthenticated `/api/health` bootstrap (`oidc_enabled`).

Example `.env` (Keycloak-style realm):

```bash
ENCODE_OIDC_ISSUER=https://keycloak.example.com/realms/farm
ENCODE_OIDC_CLIENT_ID=encode-system
ENCODE_OIDC_CLIENT_SECRET=<from the client's Credentials tab>
ENCODE_OIDC_REDIRECT_URL=https://encode.example.com/api/auth/oidc/callback
ENCODE_OIDC_ADMIN_GROUP=encode-admins
```

### Accounts and role mapping

- **First SSO login auto-provisions** a local account. Username derivation:
  `preferred_username` → email local part → `sub`, sanitized to `[a-z0-9._-]`
  (separator runs collapse to `-`). Lowercasing is **ASCII-only** and every
  non-ASCII rune becomes a separator, so Unicode confusables (`İ`, the Kelvin
  sign `K`, fullwidth letters) can never fold into ASCII lookalikes such as
  `admin`. Names longer than 64 chars become the first 56 chars + `-` + a
  7-char lowercase hex SHA-256 digest of the **raw `sub`** — two distinct IdP
  subjects whose names share a long prefix can never merge into one account.
  The derivation is deterministic, so repeat logins reuse the same account —
  no duplicates.
- **New accounts get the viewer role**, or **admin** when
  `ENCODE_OIDC_ADMIN_GROUP` is set and the groups claim contains it (exact
  match, not substring).
- **Existing accounts**: with `ENCODE_OIDC_ADMIN_GROUP` set, the role is
  **synced on every login in both directions** (in the group → admin, out →
  viewer). With no admin group configured, existing accounts **keep their
  stored role** — nothing is mapped, and an existing admin is never silently
  demoted. Plan IdP usernames so they do not collide with local accounts
  unless you intend them to be the same account (matching is exact on the
  derived, lowercased username).
- SSO-provisioned accounts have **no local password** (local login for them
  is always 401). An admin can set one via user management to make the
  account hybrid; deleting the account removes SSO access too (it will be
  re-provisioned on the next SSO login).
- Sessions are ordinary controller sessions: same 24h sliding expiry, same
  logout/revocation, same role enforcement as local login. Every SSO login
  writes an `auth.oidc_login` audit event (actor = username, detail carries
  the role and whether the account was just provisioned).

> **Admin-collision warning.** An IdP identity whose derived username is
> `admin` (e.g. `preferred_username=admin`) lands on the **local admin
> account** — derivation matches local accounts exactly. Without
> `ENCODE_OIDC_ADMIN_GROUP`, anyone the IdP authenticates under that name
> gets a **passwordless admin session**. With the admin group configured,
> group sync can **demote the local admin** (and their live sessions) when
> the colliding IdP identity is outside the group — a lockout risk. Plan IdP
> usernames so privileged local account names cannot be derived; for
> privileged accounts prefer `sub`-derived names (leave `preferred_username`
> and `email` unset or unique per user) and treat the local `admin` name as
> reserved.

### Security notes

- The flow is authorization-code with **state** (single-use, 10-minute TTL),
  **nonce** (bound into the ID token and checked) and **PKCE** (S256).
  **Login CSRF is blocked by a browser-binding flow cookie**, not by the
  state alone: `/api/auth/oidc/login` sets a crypto-random `encode_oidc_flow`
  cookie (HttpOnly, SameSite=Lax, Secure when the request is TLS, scoped to
  `/api/auth/oidc`, 10-minute MaxAge). Only its SHA-256 hash is stored
  server-side; the callback verifies it in constant time and clears the
  cookie on every outcome. An attacker-initiated flow cannot be completed by
  the victim's browser — the victim carries no matching cookie, so the
  callback is rejected and no session is issued.
- The ID token is verified via `go-oidc`: signature against the provider's
  JWKS, issuer, audience and expiry. Any failure rejects the login. Expiry
  has **zero clock-skew leeway** — keep the controller host NTP-synced
  (setup step 5).
- **Throttling**: `/api/auth/oidc/login` uses the same lockout mechanism as
  local login (5 attempts → 30 s cooldown) with a **separate counter**, so
  OIDC bursts cannot lock out local login and vice versa. A completed
  callback resets the lockout.
- **Error hygiene**: failed callbacks return a generic `OIDC login failed`
  to the browser; controller logs keep only a sanitized error class/HTTP
  status. Raw IdP error bodies — which may echo authorization codes, secrets
  or PII — never reach the client or the logs.
- The session token reaches the SPA in the URL **fragment** (`/#oidc=…`),
  which browsers never send to servers or logs; the SPA consumes and strips
  it immediately. **HTTPS is strongly recommended** for the controller (see
  §Native HTTPS or the Traefik HA path) — over plain HTTP the fragment is
  still exposed to anything observing the browser side.
- The redirect URL must match the provider's registered URI exactly; a
  mismatch is rejected by the provider before any token is issued.
- Discovery is lazy: the controller boots even if the IdP is down; the first
  SSO attempt then returns 503 and retries discovery on the next attempt.
- **HA (two replicas)**: pending login flows live in the memory of the
  replica that started them. The callback must reach the SAME replica —
  enable sticky sessions on the load balancer, or run SSO on a single
  replica. Without stickiness, callbacks that land on the other replica fail
  with "unknown or expired login state" and the user simply retries.

### Disabling / rollback

Unset the four core vars (or blank them in `.env`) and restart: the routes
and the SSO button disappear, local login is untouched. Already-provisioned
accounts and their sessions remain (sessions expire on the normal TTL);
delete the accounts via user management if you want them gone.

## Download ingestion (experimental, off by default)

P11 can poll a BitTorrent client for completed downloads and stage their
source video files into the scripts share, where the scanner picks them
up once an operator authors the `.avs`/`.vpy` filter script. The feature
is **OFF by default** and **experimental**; nothing changes on upgrade
until it is explicitly enabled on the Settings page. See Operations.md
§Download ingestion for the workflow, observability and diagnosis.

### Mount requirement (read first)

Ingestion copies files — it does not download them. The controller must
see the client's completed downloads on a local path:

- `ingest.download_root` is the **controller-visible** directory that
  corresponds to the client's download root — the directory that
  **directly contains the completed torrent folders**.
- The client must save completed torrents **directly under that root**
  (flat layout). Per-category subfolders are not resolved: the controller
  maps a torrent to `download_root/<basename of the client save path>`
  and verifies it exists before staging (missing = skip + log).
- Symlinked torrent folders are resolved and must stay **inside**
  `download_root`; a symlink pointing outside the root is refused
  (symlink-escape containment).
- Example: qBittorrent saves to `/downloads/complete` on the download
  host; the controller container mounts that same directory at
  `/mnt/downloads` → set `download_root: /mnt/downloads`. A torrent saved
  as `/downloads/complete/Show.S01E01` is read from
  `/mnt/downloads/Show.S01E01`.

Compose wiring (controller service):

```yaml
volumes:
  - /srv/downloads/complete:/mnt/downloads:ro
```

A read-only mount is sufficient (ingestion only reads the download side;
writes go to the scripts share). The scripts share must already be
mounted at `scripts_root` as for normal scanning.

### qBittorrent setup (first-class)

1. Enable the Web UI (Tools → Options → Web UI) and note its port.
2. Either create a user for the controller, or whitelist the controller's
   IP ("Bypass authentication for clients on localhost" / IP whitelist)
   and leave the ingest username empty.
3. (Optional) Create a category (e.g. `anime`) and set the ingest
   `category` filter or the category→series map. Completed torrents in
   that category are polled via `torrents/info?filter=completed`.
4. Settings page → Download ingestion: provider `qbittorrent`, URL
   `http://<host>:<port>`, credentials, download root. Save.

Sessions use the cookie login (`/api/v2/auth/login`). Both protocol
generations are supported: 4.x–5.1 answer `200 "Ok."` / `"Fails."`,
≥5.2 answers `204` on success and `401` on bad credentials (verified
against upstream qBittorrent source; both generations are covered by
mock-server regression tests). An expired/rejected session (403, or
401 on the ≥5.2 auth paths) is re-logged-in once automatically.
Repeated failed logins can get the controller IP banned by qBittorrent
— fix credentials, then unban.

### ruTorrent setup (best-effort, experimental)

1. Enable the **httprpc** plugin (it ships with ruTorrent; the client
   calls `plugins/httprpc/action.php`).
2. Put HTTP **Basic** auth in front of ruTorrent (or leave it open on a
   trusted LAN); digest-only auth is not supported.
3. Use ruTorrent **labels** as categories (the client matches
   `d.custom1`); the category filter/map works on labels.
4. Settings page: provider `rutorrent`, URL = the ruTorrent web root
   (e.g. `http://host/rutorrent`), credentials, download root.

Limitations (documented, not bugs to file): completion is read from the
appended `d.get_complete` command (fallback `left_bytes=0` on builds that
drop it); single-file torrents resolve their save dir as the parent of
`d.get_base_path`; only Basic auth; no per-torrent retry tuning. Prefer
qBittorrent for production use.

### Enable / disable / rollback

- Enable: Settings card (admin) or `PUT /api/settings` with the `ingest`
  block. The password is write-only (blank keeps the stored one) and
  AES-GCM encrypted with the shares key — back up the key file alongside
  the DB as usual.
- Disable: uncheck "Enable download ingestion" and save. Every poll tick
  becomes a no-op; staged files and the `ingest_ledger` table are left
  untouched. No restart needed.
- Rollback: the feature is additive (settings JSON blob + one new table).
  Older binaries ignore the `ingest` settings key; the `ingest_ledger`
  table is harmless to leave in place. On Postgres the table is created
  by `CREATE TABLE IF NOT EXISTS` on every open (fresh and upgrade).
- HA: the poll loop holds advisory lock **1008**; only the leader replica
  polls. No additional configuration is required.

## Post-deploy validation

See `Docs/md/FirstNodeDeploy.md` §6 for the full smoke procedure. Short form:

1. UI at `http://<controller>:8080` shows the sign-in form; log in with the admin account (password from `ENCODE_ADMIN_PASSWORD` or the generated one in the startup logs).
2. `GET /api/health` returns `ok`.
3. Register a node (or run the agent in foreground: `encode-agent.exe -foreground`), see it appear as `idle` within one heartbeat interval.
4. Create a test episode folder with a tiny source + `1080.avs`, watch job go `pending → assigned → running → done`.
5. Check `ReleaseFolders` receives the output per the naming pattern.

## CI/CD (GitHub Actions)

Two workflows in `.github/workflows/`:

- **CI** (`ci.yml`) — on every push/PR to `main`: runs the AGENTS.md command
  map. Backend: `gofmt -l` (must be clean), `go vet`, `go test ./...`
  (includes the pwsh-rendered-script E2E tests — PowerShell 7 ships on the
  runner), and cross-builds `controller` (linux) + `encode-agent.exe`
  (windows). Frontend: `eslint`, `tsc --noEmit`, `vitest`, `vite build`.
- **Release** (`release.yml`) — on a version tag (`git tag v1.4.0 && git push
  origin v1.4.0`): builds controller + `encode-agent.exe` (windows/amd64) +
  `encode-agent-linux-amd64` (`GOOS=linux GOARCH=amd64`, `-trimpath`,
  version ldflags) + `pgimport` + `encodectl-linux-amd64` (the operator CLI,
  same ldflags) + the built SPA, packs the complete offline bundle
  `encode-system-<ver>-deploy.tar.gz` (binaries, ui/, `provision/`
  (provision.yml + linux-node.yml + roles/linux-node), `systemd/`
  (encode-agent.service), Dockerfile.runtime, compose, .env.example,
  installers, SHA256SUMS), pushes `ghcr.io/badskater/encode-system:<ver>`
  + `:latest`, and attaches everything plus loose `install.sh` /
  `install-agent.ps1` / `install-agent.sh` to the GitHub release. The
  `encodectl-linux-amd64` binary is in the tarball, in `SHA256SUMS`, and as a
  standalone release asset.

Release-workflow operational notes:

- **Actions runs the workflow as of the TAGGED COMMIT** — a fix pushed to
  main after tagging needs a NEW tag; re-running the old tag reuses the
  broken workflow. If a release tag fails, fix on main and cut a new patch
  tag; never force-move a published tag.
- `gh` inside the workflow needs `env: GH_TOKEN: ${{ github.token }}`
  explicitly; release notes are generated via the API + `--notes-file`
  (`--generate-notes` and `--notes` are mutually exclusive).
- Validate workflow edits locally before tagging with `actionlint`
  (`actionlint -no-color -oneline` in the repo root).
- The Docker host sits on a private LAN that GitHub runners cannot reach, so
  deployment stays an explicit operator step — but it is one command now:
  `curl -fsSL <release>/install.sh | sudo ENCODE_HOME=/opt/encode-system bash -s v<ver>`.
- **The farm controller (172.24.92.232) runs the release pipeline as of
  v1.18.1** (migrated 2026-09-30 from the hand-rolled bind-mount compose):
  `docker-compose.runtime.yml`, GHCR image, named volumes
  (`encode-system_encode-{state,scripts,release}`). **It currently runs
  v1.19.3 on SQLite and STAYS there until the P13 rollout** — the upgrade
  to v1.23.0, the SQLite→Postgres cutover and the native-TLS cutover are
  one operator-scheduled operation (validated runbooks: the fleet-ops
  skill + §Native HTTPS below), not part of tagging the release.
  Upgrades are
  `install.sh vX.Y.Z` with data preserved. Migration carry-over recipe
  (state volume → `encode-state` incl. `encode.db` + `encode.db.shares-key`,
  bind dirs → the scripts/release volumes) plus one manual step: the image
  runs as non-root `encode` (uid 100), so a root-owned carried volume needs
  `docker run --rm -u root --entrypoint sh -v <vol>:/data <image> -c 'chown -R encode:encode /data'`
  before the controller can open SQLite (fails as "readonly database (8)"
  otherwise). install.sh v1.18.1+ tolerates legacy `.env` files missing
  `ENCODE_VERSION`/`ENCODE_CONTAINER_NAME`/`ENCODE_PORT`.

The SQLite driver is pure Go (`modernc.org/sqlite`), so CI needs no system
libraries. Frontend lockfile hygiene matters: after bumping a major build
dep (vite/vitest/plugin-react), regenerate `package-lock.json` and prove a
clean `npm ci` — a stale lockfile passes locally (existing node_modules) and
only detonates as ERESOLVE inside the CI/Docker image build
(`@vitejs/plugin-react` must be v5+ for vite 8).

## Rollback

- Controller: pin image tag in compose; `docker compose pull && up -d` forward, restore previous tag to roll back. SQLite DB is a single file — snapshot before upgrades (manual copy, or use the built-in scheduled DB backups: Settings → DB backups → download). **In-place restore** (`POST /api/backup/{name}/restore`, admin-only, Settings → DB backups → Restore): on SQLite the controller snapshots the current DB to `encode.db.pre-restore-<ts>`, atomically swaps the backup over the live DB, then **exits the process** — the container's restart policy (`restart: unless-stopped` / `Restart=always`) must be in effect so the controller reboots on the restored DB. On Postgres the restore replays the JSON-lines dump in a single transaction (no restart). See `Operations.md` → restore runbook for guards and the pre-restore snapshot location.
- Agents: `POST /api/updates/agent/rollback` (UI: Settings → Push to nodes → Rollback) re-promotes the one kept `.prev` agent; nodes self-downgrade through the normal version-diff sync on their next heartbeat.
- Ansible plays are idempotent; revert folder/service changes by re-running with the previous playbook revision.

## Troubleshooting

- Node never appears: check agent log `<data_dir>\agent.log` (`C:\encode-agent` service / `C:\encode-agent-dist` task install), verify token + TLS trust, confirm outbound connectivity to the controller. A node stuck offline after a failed self-update: `Start-ScheduledTask -TaskName EncodeAgentDist` over WinRM (it stages the pending `.exe.new`, exits once more, returns on the new version ~1 min).
- Jobs stuck `assigned`: agent heartbeats but doesn't claim — usually PowerShell execution policy or missing binary in `C:\bin`; the heartbeat's last error field shows the step that failed.
- Scanner misses folders: episode folder needs both a source media file and a `.avs`/`.vpy`; check scanner logs for the rejection reason. For S3, keys must be `<series>/<episode>/…` exactly two levels deep, and objects newer than 2 minutes are deferred by the stability gate.
- Controller exits at boot with SQLite "out of memory (14)": the fresh `/data` volume is owned by root instead of the runtime user — `chown` the volume (fixed in the image since v1.17.4; only reappears after base-image changes).
- `docker compose up` recreated the wrong controller: project-name collision — see the isolated-test-deploy gotcha above.

## High-availability (active/active, Postgres required)

The default deploy is a single controller instance with a SQLite backend.
For deployments that need controller-side redundancy, the controller can
run as **two replicas behind one Traefik v3 load-balanced service**, both
sharing a single Postgres backend. HA is **opt-in and Postgres-only** —
SQLite is single-writer and cannot serve two replicas, so the SQLite
single-instance deploy remains the fully supported default.

### Prerequisites

- A Postgres backend. Point `ENCODE_DATABASE_URL` at a `postgres://` DSN
  (the bundled `postgres:16-alpine` in `docker-compose.ha.yml` is for
  trials only — production HA should use a managed/external PG such as
  RDS, Cloud SQL, or a Patroni cluster, since a single bundled postgres
  container is itself the remaining SPOF).
- An existing Traefik v3 stack on the host (the HA compose attaches to the
  operator's external Traefik network; it does **not** bring up Traefik).
- The HA vars set in `.env` (see `docker/.env.example`): `ENCODE_DOMAIN`
  (required — the Traefik Host rule has no default), `ENCODE_TRAEFIK_NETWORK`,
  `ENCODE_TRAEFIK_ENTRYPOINT`, `ENCODE_TRAEFIK_RESOLVER`, optional
  `ENCODE_AUTH_MIDDLEWARE` (Authelia forwardAuth).

### `docker-compose.ha.yml`

The HA compose (`docker/docker-compose.ha.yml`) defines:

- `controller-1` / `controller-2` — two replicas of the same GHCR image
  (`ghcr.io/badskater/encode-system:<ver>`), each with a **distinct
  hardcoded `ENCODE_INSTANCE_ID`** (`controller-1` / `controller-2`) so the
  advisory-lock singleton loops and the `/api/health` `instance` field
  distinguish them. Both carry the **required** `ENCODE_DATABASE_URL` (no
  default — compose fails fast if unset), `ENCODE_SHARES_KEY_FILE`, and the
  shared `encode-state` / `encode-scripts` / `encode-release` volumes (same
  named volumes as `docker-compose.runtime.yml` so publish, backup, and
  shares-key behavior match the single-instance deploy).
- A `postgres:16-alpine` service (no profile gate — HA requires PG) with
  the `encode-pgdata` volume, for trials. Point `ENCODE_DATABASE_URL`
  elsewhere to use an external PG; the bundled service is then unused.
- **Traefik v3 load balancing**: both replicas use the **identical** Traefik
  router+service name (`encode-controller` / `encode-controller-svc`) via
  Docker labels. Traefik v3 coalesces services with the same name across
  providers into one load-balanced backend accumulating the server entries
  from both containers — this is the idiomatic v3 multi-container LB pattern
  (a single compose service's labels can only describe one Traefik service,
  so the shared-name coalescing is what spans the two replicas). The Host
  rule, entrypoint, resolver, and domain come from `.env` `${VARS}`;
  an optional Authelia forwardAuth middleware is chained when
  `ENCODE_AUTH_MIDDLEWARE` is set.

```bash
cd docker
cp .env.example .env   # set ENCODE_DATABASE_URL + HA/Traefik vars
docker compose -f docker-compose.ha.yml up -d
```

### `install.sh --ha`

`install.sh --ha` automates the HA deploy: it **refuses** to run when
`ENCODE_DATABASE_URL` is empty/SQLite with the message
`HA requires Postgres: set ENCODE_DATABASE_URL (see --postgres)`, brings up
the HA compose (postgres first, waits healthy, then both replicas), applies
the same `COMPOSE_PROJECT_NAME` safety guard to both replica container names,
and **health-checks BOTH replicas** (polling each container's
`/api/health` via `docker exec` so the check works regardless of Traefik
routing state). Without `--ha`, behavior is byte-identical to the
single-instance deploy.

```bash
sudo ./install.sh --ha        # refuses if ENCODE_DATABASE_URL is empty/SQLite
```

### Rolling upgrades

Upgrade one replica at a time to keep the controller available through the
swap:

```bash
docker compose -f docker-compose.ha.yml up -d --no-deps controller-1
# wait for encode-controller-1 healthy, then:
docker compose -f docker-compose.ha.yml up -d --no-deps controller-2
```

`migratePG` is idempotent (no-op on an existing schema), so a replica booting
against an already-migrated PG is safe. The advisory-lock singleton loops
re-acquire on each boot; a replica killed mid-loop releases its advisory lock
on connection close and the other replica's loop takes over within its retry
interval.

### Rollback from HA

To roll back to the single-instance SQLite deploy: stop the HA stack, unset
`ENCODE_DATABASE_URL` in `.env`, and bring up `docker-compose.runtime.yml`.
**Caveat:** once new writes have landed on PG after the HA cutover, rolling
back to SQLite loses those writes (the safe rollback is to re-import
forward — see `Operations.md` §Postgres backup & rollback). Roll back to
SQLite only in the immediate post-cutover window when no writes have occurred.

### HA limitations

- **SSE live log streaming is per-replica.** The log hub is in-memory
  per-process; an SSE client attached to replica A only sees progress
  events for heartbeats that landed on A. If a heartbeat lands on replica B,
  the progress update is written to the shared PG DB but A's SSE stream only
  re-syncs on its next 15s keepalive DB re-check. See `Architecture.md`
  §Active/active.
- **Postgres is the remaining SPOF.** Patroni/PG HA is deferred by user
  decision; the bundled `postgres:16-alpine` is for trials only.
