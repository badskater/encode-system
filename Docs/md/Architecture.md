# Architecture

## Objectives and NFRs

- Queue-driven encode farm: detect new episode material on NFS shares, run one job per node, monitor everything from one web UI.
- Windows Server 2025 nodes with Nvidia GPUs; encode tooling unchanged (DGIndexNV, x265 fork, mkvmerge), audio migrated from FLAC/eac3to-only to Opus (eac3to → WAV → opusenc).
- NFRs: single source of truth (controller DB), at-most-one job per node, node reboot after 10 tasks, agent auto-update without touching each box, structured logs with job IDs.

## System overview

```text
                     +-----------------------------+
 NFS shares <------> | Controller (Linux container)|
 scripts/            |  Go HTTP + SQLite           |
 ReleaseFolders/     |  scanner, queue, renderer   |
                     |  React SPA (dashboard)      |
                     +--------------+--------------+
                                    | HTTPS (agent poll / heartbeat)
                     +--------------+--------------+
                     |                             |
              +------+------+               +------+------+
              | Win node A  |   ...         | Win node N  |
              | encode-agent|               | encode-agent|
              | + PS encod  |               | + PS encod  |
              +-------------+               +-------------+
```

## Components

### Controller (`backend/cmd/controller`)

- **Scanner** polls the mounted `scripts/` share; an episode folder is "ready" when it contains a source video (`*.m2ts|*.ts|*.mkv`) plus at least one filter script (`1080.avs` or `1080.vpy`). Ready-and-unseen folders become jobs.
- **Queue** persists jobs in SQLite. Job lifecycle: `pending → assigned → running → muxing → done` (or `failed`). Exactly one active job per node enforced in the store.
- **Flow renderer** turns a flow definition (ordered steps + params) plus job variables (series, episode, paths, output name) into a self-contained PowerShell script. The generated script calls functions from `EncodeLib.ps1`.
- **Agent API** under `/api/agent/*`: heartbeat + claim, step progress, log tail upload, job completion, update manifest + binary/script download. Token auth per node.
- **UI API** under `/api/*` for the SPA: nodes, jobs, flows, scanner config, settings. Single-admin token auth (env-supplied).

### Agent (`backend/cmd/agent` → `encode-agent.exe`)

- Runs as a Windows service (or foreground for debugging). Config file + flags: controller URL, node token, data dir.
- Loop: heartbeat (status JSON: current job, step, tasks_since_boot, agent version) → controller response carries instructions: `job` (rendered script + vars), `reboot`, `update`.
- Executes jobs by writing the generated `.ps1`, invoking `powershell.exe -NoProfile -ExecutionPolicy Bypass -File`, streaming stdout/stderr to the controller, tracking exit code.
- Reboot: on `reboot` instruction with no running job, schedules `shutdown /r /t 30` and reports; counter resets naturally on next boot.
- Auto-update: on `update` instruction, downloads new `encode-agent.exe` + `EncodeLib.ps1` to staging, verifies checksum, swaps the script immediately and the binary on next service restart (self-replace via sidecar restart command).

### PowerShell layer (`powershell/EncodeLib.ps1`)

Functions (each = one flow step):

- `Invoke-SourceRename` — rename the raw source to `src.<ext>` (legacy `rename *.m2ts src.m2ts`).
- `Invoke-DgIndex` — `DGIndexNV.exe -i src -o src.dgi -h`.
- `Invoke-AudioExtract` — `eac3to.exe src 2: audio.wav -down16` then `opusenc --bitrate <n> audio.wav audio.opus`; deletes the WAV unless `-KeepWav`.
- `Invoke-VideoEncode` — `x265_x64.exe` with the flow's parameter set and the episode's `.avs`/`.vpy` as input.
- `Invoke-Mux` — `mkvmerge` with the repo's standard track flags (jpn, default video, no chapters/global tags).
- `Invoke-ReleaseCopy` — copy the finished MKV into the `ReleaseFolders/[group] Series - Raws [tag]/` pattern.
- `Invoke-Keyframes` — `ffmpeg` writes a downscaled temp y4m, `SCXvid` reads it into the keyframes file; skipped if it already exists.

All steps write structured progress lines (`ENCODE_STEP <name> <pct>`) that the agent parses into heartbeat progress.

## Contracts

### Heartbeat (agent → controller)

`POST /api/agent/heartbeat`

```json
{
  "node": "enc-01",
  "agent_version": "0.3.0",
  "tasks_since_boot": 4,
  "job_id": "j_018",
  "job_status": "running",
  "step": "encode",
  "step_progress": 42.5,
  "log_tail": "...last lines..."
}
```

Response:

```json
{
  "instruction": "job|reboot|update|none",
  "job": { "id": "j_019", "script": "<rendered ps1>", "vars": { }, "flow": "default-1080" },
  "reboot": { "delay_seconds": 30 },
  "update": { "agent_version": "0.4.0", "agent_sha256": "...", "lib_version": 3, "lib_sha256": "..." }
}
```

### Job completion

`POST /api/agent/job/<id>/complete` with `{ "status": "done|failed", "exit_code": n, "outputs": [paths], "log_tail": "..." }`.

## Runtime flows

1. User drops `Ep 05/` with `src.m2ts` + `1080.vpy` into `scripts/<Series>/`.
2. Scanner detects it next cycle → job `pending` with default flow (or UI-assigned).
3. Node `enc-02` heartbeats idle → controller assigns job, response carries rendered script.
4. Agent executes step by step; heartbeats carry progress; UI live-updates.
5. Job completes → controller verifies output path exists on the share → `done`.
6. After the node's 10th completed task, controller responds with `reboot`; agent defers until idle, reboots; node comes back with counter 0.

## Security and observability

- Agent tokens: random per-node tokens issued at node registration, stored hashed. UI admin token from env. TLS optional behind reverse proxy.
- Structured JSON logs (`slog`) with `job_id` / `node` fields on both sides; agent log tails retained per job for post-mortem.

## Open decisions

- Episode numbering source: derived from folder name (`Ep NN`) today; metadata file later if needed.
- Multi-audio-track support: today audio track index is a flow param (default `2:` like the legacy script).
