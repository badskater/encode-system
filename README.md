# encode-system

Distributed encode farm: a Linux control plane queues and monitors anime encoding jobs on a fleet of Windows Server 2025 nodes with Nvidia GPUs.

## What it does

- Watches NFS-mounted shares (`scripts/`, `ReleaseFolders/`) for new episode material (source video + authored `.avs`/`.vpy` filter scripts).
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
| `infra/ansible/` | Playbooks: NFS client feature, folder patterns, `C:\bin`, agent service deployment |
| `docker/` | Controller image + compose with NFS volume mounts |
| `Docs/` | Architecture, Deployment, Operations |

## Quick start

```bash
# Controller (Linux/container)
cd backend && go build -o bin/controller ./cmd/controller
ENCODE_DATA=/data ./bin/controller

# Agent (Windows, built cross-platform)
cd backend && GOOS=windows GOARCH=amd64 go build -o bin/encode-agent.exe ./cmd/agent
```

See `Docs/md/Deployment.md` for the full deployment (Docker compose + Ansible).

## Status

Greenfield build in progress — see `KANBAN.md`.
