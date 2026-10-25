# encode-system

Distributed encode farm: a Linux control plane queues and monitors encoding jobs on a fleet of Windows and Linux nodes. Windows nodes run the legacy GPU path (DGIndexNV + AviSynth/VapourSynth + x265 fork); Linux nodes run a VapourSynth-first path (`.vpy` source filter → `vspipe | x265`, ffmpeg decode-only → `opusenc`/`flac`) with no GPU or DGIndexNV dependency. The controller selects each node's script dialect at dispatch time from the platform its heartbeat reports.

## What it does

- Watches storage shares (`scripts/`, `ReleaseFolders/`) for new episode material (source video + authored `.avs`/`.vpy` filter scripts). Storage is pluggable: **NFS**, **SMB**, or **S3-compatible object storage** (MinIO, Ceph RGW, AWS) — S3 needs no mounts at all: the controller lists the bucket and agents stage each episode locally per job.
- Renders an episode's job from a selectable **flow** (ordered pipeline steps: DGIndexNV index → eac3to+opusenc audio → x265 encode → mkvmerge mux → output verification → release-folder copy → SCXvid keyframes). Windows renders PowerShell (`.ps1`, dot-sources `EncodeLib.ps1`); Linux renders bash (`.sh`, `set -euo pipefail`, self-contained — `dgindex` and `keyframes` are refused on Linux with a clear render error: DGIndexNV is Windows/Nvidia-GPU only and no faithful SCXvid Linux port exists yet).
- Dispatches jobs across the fleet with per-node concurrency slots, node-group routing, priorities, auto-retry with backoff (steered away from the node that failed), drain mode, and disk-space soft-drain.
- Monitors everything from the web UI: live SSE log tails, per-step timings, node telemetry (CPU/RAM/disk/GPU/fps), fleet stats, job ETAs, audit log, Discord alerts (per-series channels, digest mode) — plus a Prometheus `GET /metrics` endpoint.
- Enforces **reboot after 10 tasks**: the controller watches each node's `tasks_since_boot` and issues a reboot instruction when the limit is reached.
- **Auto-updates** agents: controller pushes new `encode-agent.exe` and `EncodeLib.ps1` versions (with one-click rollback to the previous release); agents swap on next idle heartbeat. Linux agents self-update via a `swap-update.sh` sidecar (setsid-detached, systemd-aware — exits to let `Restart=always` relaunch, or relaunches directly in bare mode).
- Visual flow builder in the web UI: reorder steps, set parameters (CRF, preset, Opus bitrate, x265 extra args), save named flows, pick a flow per job.
- Operator conveniences: scheduled DB backups, scoped API tokens, bulk job retry/cancel, job-history retention, one-command installers for controller and nodes.

## Components

| Path | What |
| --- | --- |
| `backend/` | Go: controller service, queue, scanner, flow renderer (PowerShell + bash dialects), REST API; also builds the Windows agent (`encode-agent.exe`) and the Linux agent (`encode-agent-linux-amd64`) |
| `frontend/` | React + TypeScript SPA: dashboard, jobs, nodes, flow builder — dark ops-console design system (violet accent, self-hosted Inter/JetBrains Mono, no CDN deps) |
| `powershell/` | `EncodeLib.ps1` — the encode step implementations run on Windows nodes (dot-sourced by rendered `.ps1` scripts; not needed on Linux) |
| `infra/ansible/` | Playbooks: Windows nodes (`site.yml` + WinRM roles), Linux nodes (`linux-node.yml` + `roles/linux-node` over SSH with CIFS/NFS mounts) |
| `infra/systemd/` | `encode-agent.service` — the canonical systemd unit for Linux agents (single source of truth; the role and `install-agent.sh` both deploy it) |
| `docker/` | Controller images, compose files, `install.sh` / `install-agent.ps1` / `install-agent.sh` one-command installers |
| `Docs/` | Architecture, Deployment, Operations |

## Quick start

**Controller (any Docker host)** — from a [release](https://github.com/badskater/encode-system/releases):

```bash
curl -fsSL -o install.sh \
  https://github.com/badskater/encode-system/releases/latest/download/install.sh
chmod +x install.sh
sudo ./install.sh          # installs /opt/encode-system, pulls the GHCR image, starts compose
```

Private repo? Export `GITHUB_TOKEN` (PAT with `repo` + `read:packages`) before running, and `docker login ghcr.io` once. The image is `ghcr.io/badskater/encode-system:<version>`; without a registry the installer builds `docker/Dockerfile.runtime` from the bundle instead. Then open `http://<host>:8080`, sign in (`admin` + the generated password from `docker logs encode-controller`), and point it at your storage: **Settings → Storage shares** (NFS/SMB/S3).

**Windows node** — elevated PowerShell, with a pairing code from UI → Nodes:

```powershell
irm <release-url>/install-agent.ps1 -OutFile install-agent.ps1
.\install-agent.ps1 -ControllerUrl http://<controller>:8080 -PairingCode <CODE>
```

The node appears as `idle` within ~5 s. Toolchain (`C:\bin`: x265, mkvmerge, ffmpeg, eac3to, opusenc, DGIndexNV, SCXvid) is provisioned separately — UI → Provision (runs Ansible from the controller) or `infra/ansible/site.yml`.

**Linux node** — as root, with a pairing code from UI → Nodes:

```bash
curl -fsSL -o install-agent.sh \
  https://github.com/badskater/encode-system/releases/latest/download/install-agent.sh
chmod +x install-agent.sh
sudo ./install-agent.sh -c http://<controller>:8080 -p <pairing-code>
# or: sudo ./install-agent.sh -c http://<controller>:8080 -t <permanent-token>
```

The installer downloads `encode-agent-linux-amd64`, verifies its SHA-256 against `SHA256SUMS`, installs `/opt/encode-system/encode-agent`, writes `/etc/encode-agent/agent.json` (mode 0600), and registers `encode-agent.service` (systemd, `Restart=always`). Env overrides: `CONTROLLER_URL`, `PAIRING_CODE`, `TOKEN`, `NODE_NAME`, `VERSION`, `REPO`, `INSTALL_DIR`, `CONFIG_DIR`, `GITHUB_TOKEN` (private repos). The node appears as `idle` within ~5 s.

Linux toolchain (on `PATH`, provisioned separately — UI → Provision or `infra/ansible/linux-node.yml`): `bash`, VapourSynth (`vspipe`), `x265`, `ffmpeg` (decode-only), `opusenc`, `flac`, `mkvmerge` (mkvtoolnix), `mediainfo`, `jq`, `bc`, coreutils (`cksum`/`sha256sum`), `curl`; `cifs-utils`/`nfs-common` (Debian) or `nfs-utils` (RHEL) for mounts. systemd is recommended (or run the agent as a bare foreground loop).

**From source:**

```bash
cd backend && go build -o bin/controller ./cmd/controller
ENCODE_DATA=/data ./bin/controller
cd backend && GOOS=windows GOARCH=amd64 go build -o bin/encode-agent.exe ./cmd/agent
cd backend && GOOS=linux GOARCH=amd64 go build -o bin/encode-agent-linux-amd64 ./cmd/agent
```

See `Docs/md/Deployment.md` for the full deployment (Docker compose + Ansible) and `Docs/md/FirstNodeDeploy.md` for the node runbook.

## Releases

Push a `v*` tag → the Release workflow builds binaries + SPA, publishes a complete offline deploy bundle (`encode-system-<ver>-deploy.tar.gz`) and pushes the container image to GHCR. See `.github/workflows/release.yml`.

## Status

Greenfield build in progress — see `KANBAN.md`.
