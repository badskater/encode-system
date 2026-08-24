# Deployment

## Prerequisites

- NFS server exporting `scripts/` and `ReleaseFolders/` (the unRAID boxes already serve these).
- Linux host/container runtime for the controller with access to mount both shares (NFSv3/v4 client).
- Windows Server 2025 nodes with:
  - NFS client feature enabled (Ansible does this: `Install-WindowsFeature NFS-Clnt`).
  - `C:\bin` populated with encode tools (DGIndexNV, x265_x64, mkvmerge, ffmpeg, eac3to, opusenc, SCXvid) — Ansible copies from a staging dir.
  - The two NFS shares mounted at `C:\Encodes\scripts` and `C:\Encodes\ReleaseFolders`.
- Ansible control node with `pywinrm` (or SSH) reachability to the Windows hosts.

## Controller

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

**First deploy?** Follow `Docs/md/FirstNodeDeploy.md` — a copy-paste runbook
covering controller setup, toolchain staging, inventory, credentials
(manual token or pairing code), and smoke verification.

First-boot env:

| Var | Meaning |
| --- | --- |
| `ENCODE_ADMIN_USER` | Management-plane login username (default `admin`). |
| `ENCODE_ADMIN_PASSWORD` | Initial password — used only on the boot that creates the account. Leave empty to auto-generate (logged once). |
| `ENCODE_DATA` | Data dir (default `/data`). |
| `ENCODE_SCAN_INTERVAL` | Seconds between share scans (default `30`). |
| `ENCODE_TASKS_BEFORE_REBOOT` | Default `10`. |
| `ENCODE_LISTEN` | Listen address (default `:8080`). |

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

## Rollback

- Controller: pin image tag in compose; `docker compose pull && up -d` forward, restore previous tag to roll back. SQLite DB is a single file — snapshot before upgrades.
- Agents: controller keeps the previous `encode-agent.exe` + `EncodeLib.ps1` version in the update store; set the manifest back to the old version to trigger downgrade on next heartbeat.
- Ansible plays are idempotent; revert folder/service changes by re-running with the previous playbook revision.

## Troubleshooting

- Node never appears: check agent log `C:\encode-agent\agent.log`, verify token + TLS trust, confirm outbound HTTPS to controller.
- Jobs stuck `assigned`: agent heartbeats but doesn't claim — usually PowerShell execution policy or missing binary in `C:\bin`; the heartbeat's last error field shows the step that failed.
- Scanner misses folders: episode folder needs both a source media file and a `.avs`/`.vpy`; check scanner logs for the rejection reason.
