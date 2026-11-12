# encode-system

Distributed encode farm: a Linux control plane queues and monitors encoding jobs on a fleet of Windows and Linux nodes. Windows nodes run the legacy GPU path (DGIndexNV + AviSynth/VapourSynth + x265 fork); Linux nodes run a VapourSynth-first path (`.vpy` source filter → `vspipe | x265`, ffmpeg decode-only → `opusenc`/`flac`) with no GPU or DGIndexNV dependency. The controller selects each node's script dialect at dispatch time from the platform its heartbeat reports.

## What it does

- Watches storage shares (`scripts/`, `ReleaseFolders/`) for new episode material (source video + authored `.avs`/`.vpy` filter scripts). Storage is pluggable: **NFS**, **SMB**, or **S3-compatible object storage** (MinIO, Ceph RGW, AWS) — S3 needs no mounts at all: the controller lists the bucket and agents stage each episode locally per job.
- Renders an episode's job from a selectable **flow** — an ordered pipeline of the **21 built-in step templates** (DGIndexNV index → eac3to+opusenc audio → x265 encode → mkvmerge mux → output verification → VMAF/SSIM `quality_gate` → release-folder copy → SCXvid keyframes, plus `subtitle_extract`/`font_extract`/`chapter_extract`/`mux_extras`, probes, CRC32 rename, Discord notify…) and any custom steps. Four factory flows seed on fresh installs (name-guarded, existing flows never touched): `default-1080`, `default-4k`, `default-4k-cpu` (GPU-free) and `default-subs` — a SEPARATE post-encode extras pipeline the operator queues as its own job after the encode. Windows renders PowerShell (`.ps1`, dot-sources `EncodeLib.ps1`); Linux renders bash (`.sh`, `set -euo pipefail`, self-contained — `dgindex` is refused on Linux with a clear render error since DGIndexNV is Windows/Nvidia-GPU only; `keyframes` runs on Linux via the `vapoursynth-scxvid` plugin, producing the same xvid first-pass keyframes log as Windows).
- Dispatches jobs across the fleet with per-node concurrency slots, node-group routing, priorities, auto-retry with backoff (steered away from the node that failed), drain mode, and disk-space soft-drain.
- Monitors everything from the web UI: live SSE log tails, per-step timings, node telemetry (CPU/RAM/disk/GPU/fps), fleet stats, job ETAs, audit log, and job-outcome alerts fanned out to **Discord, generic webhook, ntfy and email** (per-series Discord channels, digest mode, node online/offline + disk alerts) — plus a Prometheus `GET /metrics` endpoint (11 series, incl. `encode_jobs_stalled_total`).
- Enforces **reboot after 10 tasks**: the controller watches each node's `tasks_since_boot` and issues a reboot instruction when the limit is reached.
- **Auto-updates** agents: controller pushes new `encode-agent.exe` and `EncodeLib.ps1` versions (with one-click rollback to the previous release); agents swap on next idle heartbeat. Linux agents self-update via a `swap-update.sh` sidecar (setsid-detached, systemd-aware — exits to let `Restart=always` relaunch, or relaunches directly in bare mode).
- Visual flow builder in the web UI: reorder steps, set parameters (CRF, preset, Opus bitrate, x265 extra args), save named flows, pick a flow per job.
- Operator conveniences: scheduled DB backups with one-click restore (SQLite exit-and-restart, Postgres in-place single transaction) and **offsite copies** (S3 + SFTP with pinned host keys, shares-key bundle, per-destination retention), scoped API tokens, bulk job retry/cancel/delete/set-priority/set-flow, single-job delete (terminal only), **series delete** (registry row only — job history and files survive), manual scan trigger (`POST /api/scan`), flow duplication, GitHub release update check (`GET /api/updates/check`), node online/offline alerts, a **viewer role** (read-only management accounts — viewers see all monitoring surfaces, admins keep all mutations and user/token/audit/backup management), and `encodectl` — a CLI for scripting fleet ops from a shell or CI.
- Resilience & security: per-flow **job/step timeouts** enforced by the agent (process-tree kill, per-step deadlines) plus a controller **stall watchdog** (`stall_timeout_minutes`, HA advisory-lock gated) that fails wedged jobs, applies the retry policy and tells the agent to kill the stuck run; **native HTTPS** on the controller (`ENCODE_TLS_CERT`/`ENCODE_TLS_KEY`, TLS 1.2 floor, fail-fast on partial config) with agent CA trust (`ENCODE_AGENT_CA_CERT`); **optional OIDC SSO** (env-gated, off by default — auto-provisioning, group→role mapping, PKCE + flow-cookie CSRF binding); **experimental download ingestion** (qBittorrent/ruTorrent completed-download staging into the scripts share, off by default).

## Components

| Path | What |
| --- | --- |
| `backend/` | Go: controller service, queue, scanner, flow renderer (PowerShell + bash dialects), REST API; also builds the Windows agent (`encode-agent.exe`) and the Linux agent (`encode-agent-linux-amd64`) |
| `frontend/` | React + TypeScript SPA: dashboard, jobs, nodes, flow builder — dark ops-console design system (violet accent, self-hosted Inter/JetBrains Mono, no CDN deps) |
| `powershell/` | `EncodeLib.ps1` — the encode step implementations run on Windows nodes (dot-sourced by rendered `.ps1` scripts; not needed on Linux) |
| `infra/ansible/` | Playbooks: Windows nodes (`site.yml` + WinRM roles), Linux nodes (`linux-node.yml` + `roles/linux-node` over SSH with CIFS/NFS mounts) |
| `infra/systemd/` | `encode-agent.service` — the canonical systemd unit for Linux agents (single source of truth; the role and `install-agent.sh` both deploy it) |
| `docker/` | Controller images, compose files, `install.sh` / `install-agent.ps1` / `install-agent.sh` one-command installers |
| `docker/grafana/` | Pre-built Grafana dashboard JSON + README — imports into a stock Grafana 11+ to visualize 10 of the 11 `/metrics` series (the `encode_jobs_stalled_total` counter is scrape-ready but not yet panelled) |
| `docker/observability/` | Opt-in Prometheus scrape snippet, alert rules, Alertmanager→Discord example and Alloy log-enrichment snippet for wiring into an EXISTING monitoring stack (nothing applied automatically) |
| `backend/cmd/encodectl` | `encodectl` — stdlib-only operator CLI wrapping the controller REST API (health, scan, nodes, jobs, prune, series, stats, agent publish/rollback); shipped as `encodectl-linux-amd64` in releases and the deploy tarball |
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

Push a `v*` tag → the Release workflow builds the controller, `encode-agent.exe` (Windows), `encode-agent-linux-amd64`, `pgimport`, and `encodectl-linux-amd64` + SPA, publishes a complete offline deploy bundle (`encode-system-<ver>-deploy.tar.gz`) and pushes the container image to GHCR. See `.github/workflows/release.yml`.

## Development

Gates (run before every push):

- Backend (from `backend/`): `gofmt -l .` must print nothing, `go vet ./...`,
  `go test ./... -count=1` — **1,095 Go tests across 16 packages**, including
  dual-backend store/API contract suites that additionally run against
  Postgres when `ENCODE_TEST_PG_DSN` is set (CI runs them; local PG runs must
  include `./cmd/pgimport`). Cross-builds: linux controller,
  `GOOS=windows GOARCH=amd64 go build ./cmd/agent` (release gate) and
  `GOOS=windows go build ./...` — the Windows agent executes rendered
  scripts and never imports the flow renderer, whose bash dialect is
  Linux-only by build tag.
- Frontend (from `frontend/`): `npm run lint` (0 errors), `npx tsc --noEmit`,
  `npm run test` — **311 Vitest tests in 25 files** — and `npm run build`.

## Status

Greenfield build in progress — see `KANBAN.md`.
