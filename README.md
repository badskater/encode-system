# encode-system

Distributed encode farm: a Linux control plane queues and monitors encoding jobs on a fleet of Windows Server 2025 nodes with Nvidia GPUs.

## What it does

- Watches storage shares (`scripts/`, `ReleaseFolders/`) for new episode material (source video + authored `.avs`/`.vpy` filter scripts). Storage is pluggable: **NFS**, **SMB**, or **S3-compatible object storage** (MinIO, Ceph RGW, AWS) — S3 needs no mounts at all: the controller lists the bucket and agents stage each episode locally per job.
- Renders an episode's job from a selectable **flow** (ordered pipeline steps: DGIndexNV index → eac3to+opusenc audio → x265 encode → mkvmerge mux → release-folder copy → SCXvid keyframes).
- Assigns exactly **one job per enabled node**; agents report status every heartbeat.
- Enforces **reboot after 10 tasks**: the controller watches each node's `tasks_since_boot` and issues a reboot instruction when the limit is reached.
- **Auto-updates** agents: controller pushes new `encode-agent.exe` and `EncodeLib.ps1` versions; agents swap on next idle heartbeat.
- Visual flow builder in the web UI: reorder steps, set parameters (CRF, preset, Opus bitrate, x265 extra args), save named flows, pick a flow per job.

## Components

| Path | What |
| --- | --- |
| `backend/` | Go: controller service, queue, scanner, flow renderer, REST API (also builds the Windows agent) |
| `frontend/` | React + TypeScript SPA: dashboard, jobs, nodes, flow builder |
| `powershell/` | `EncodeLib.ps1` — the encode step implementations run on Windows nodes |
| `infra/ansible/` | Playbooks: NFS/SMB client setup, folder patterns, `C:\bin`, agent deployment |
| `docker/` | Controller images, compose files, `install.sh` / `install-agent.ps1` one-command installers |
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

**From source:**

```bash
cd backend && go build -o bin/controller ./cmd/controller
ENCODE_DATA=/data ./bin/controller
cd backend && GOOS=windows GOARCH=amd64 go build -o bin/encode-agent.exe ./cmd/agent
```

See `Docs/md/Deployment.md` for the full deployment (Docker compose + Ansible) and `Docs/md/FirstNodeDeploy.md` for the node runbook.

## Releases

Push a `v*` tag → the Release workflow builds binaries + SPA, publishes a complete offline deploy bundle (`encode-system-<ver>-deploy.tar.gz`) and pushes the container image to GHCR. See `.github/workflows/release.yml`.

## Status

Greenfield build in progress — see `KANBAN.md`.
