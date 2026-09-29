# Deployment

## Prerequisites

- Storage for `scripts/` and `ReleaseFolders/`: an NFS or SMB server (the unRAID boxes already serve these), or an S3-compatible bucket (MinIO, Ceph RGW, AWS) — configured later from UI → Settings → Storage shares. S3 needs no mounts on the controller or the nodes.
- Linux host/container runtime for the controller; NFS/SMB mounts are only needed when using those transports (compose volumes for the legacy NFS compose; runtime compose uses plain volumes + UI-configured shares).
- Windows Server 2025 nodes with:
  - NFS client feature enabled for NFS transports (Ansible: `Install-WindowsFeature NFS-Clnt`); SMB uses `New-SmbMapping`; S3 needs nothing installed.
  - `C:\bin` populated with encode tools (DGIndexNV, x265_x64, mkvmerge, ffmpeg, eac3to, opusenc, SCXvid) — Ansible copies from a staging dir, or publish a bin zip from the UI.
  - The shares mounted at `C:\Encodes\scripts` and `C:\Encodes\ReleaseFolders` (nfs/smb only).
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

### Manual compose (build from source)

```bash
cd docker
cp .env.example .env   # set ADMIN_TOKEN, NODE CIDR/allowed names if needed
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
| `ENCODE_BACKUP_ENABLED` / `ENCODE_BACKUP_EVERY_SECONDS` | Seed the scheduled DB-backup defaults (live-editable afterwards via `PUT /api/backup/settings`). |
| `ENCODE_GROUP` / `ENCODE_TAG` / `ENCODE_DEFAULT_FLOW` | Seed release naming + the default flow name (live Settings afterwards). |
| `ENCODE_DISCORD_WEBHOOK` | Seeds the live Settings webhook (saved blank turns notifications off). |
| `ENCODE_NODE_SCRIPTS/RELEASE/BIN` | Seed the node path mapping used by the job renderer when there are no shares yet. |

Compose/installer-only vars: `ENCODE_PORT` (host port), `ENCODE_VERSION`
(GHCR image tag, set by install.sh), `ENCODE_CONTAINER_NAME` (container name
override — required for isolated test deploys), `ENCODE_HOME` /
`ENCODE_COMPOSE_PROJECT` / `ENCODE_REPO` (install.sh overrides),
`NFS_SERVER` (legacy NFS compose only).

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
  origin v1.4.0`): builds controller + agent binaries with `-X main.Version`,
  the built SPA, packs the complete offline bundle
  `encode-system-<ver>-deploy.tar.gz` (binaries, ui/, provision/,
  Dockerfile.runtime, compose, .env.example, installers, SHA256SUMS), pushes
  `ghcr.io/badskater/encode-system:<ver>` + `:latest`, and attaches
  everything plus loose `install.sh` / `install-agent.ps1` to the GitHub
  release.

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

The SQLite driver is pure Go (`modernc.org/sqlite`), so CI needs no system
libraries. Frontend lockfile hygiene matters: after bumping a major build
dep (vite/vitest/plugin-react), regenerate `package-lock.json` and prove a
clean `npm ci` — a stale lockfile passes locally (existing node_modules) and
only detonates as ERESOLVE inside the CI/Docker image build
(`@vitejs/plugin-react` must be v5+ for vite 8).

## Rollback

- Controller: pin image tag in compose; `docker compose pull && up -d` forward, restore previous tag to roll back. SQLite DB is a single file — snapshot before upgrades (manual copy, or use the built-in scheduled DB backups: Settings → DB backups → download).
- Agents: `POST /api/updates/agent/rollback` (UI: Settings → Push to nodes → Rollback) re-promotes the one kept `.prev` agent; nodes self-downgrade through the normal version-diff sync on their next heartbeat.
- Ansible plays are idempotent; revert folder/service changes by re-running with the previous playbook revision.

## Troubleshooting

- Node never appears: check agent log `<data_dir>\agent.log` (`C:\encode-agent` service / `C:\encode-agent-dist` task install), verify token + TLS trust, confirm outbound connectivity to the controller. A node stuck offline after a failed self-update: `Start-ScheduledTask -TaskName EncodeAgentDist` over WinRM (it stages the pending `.exe.new`, exits once more, returns on the new version ~1 min).
- Jobs stuck `assigned`: agent heartbeats but doesn't claim — usually PowerShell execution policy or missing binary in `C:\bin`; the heartbeat's last error field shows the step that failed.
- Scanner misses folders: episode folder needs both a source media file and a `.avs`/`.vpy`; check scanner logs for the rejection reason. For S3, keys must be `<series>/<episode>/…` exactly two levels deep, and objects newer than 2 minutes are deferred by the stability gate.
- Controller exits at boot with SQLite "out of memory (14)": the fresh `/data` volume is owned by root instead of the runtime user — `chown` the volume (fixed in the image since v1.17.4; only reappears after base-image changes).
- `docker compose up` recreated the wrong controller: project-name collision — see the isolated-test-deploy gotcha above.
