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

- **Scanner** polls the mounted `scripts/` share; an episode folder is "ready" when it contains a source video (`*.m2ts|*.ts|*.mkv`) plus at least one filter script. Script priority: `2160.vpy > 2160.avs > 1080.vpy > 1080.avs > any other .avs/.vpy` (VapourSynth wins at the same resolution). Ready-and-unseen folders become jobs.
- **Queue** persists jobs in SQLite. Job lifecycle: `pending → assigned → running → muxing → done` (or `failed`). Exactly one active job per node enforced in the store.
- **Flow renderer** turns a flow definition (ordered steps + params) plus job variables (series, episode, paths, output name) into a self-contained PowerShell script. The generated script calls functions from `EncodeLib.ps1`.
- **Agent API** under `/api/agent/*`: heartbeat + claim, step progress, log tail upload, job completion, update manifest + binary/script download. Token auth per node.
- **UI API** under `/api/*` for the SPA: nodes, jobs, flows, scanner config, settings. Management-plane auth is a normal username/password login (`POST /api/auth/login` issues session tokens; bcrypt-hashed passwords; sliding 24h expiry; logout revokes; 5-failure throttle).

### Agent (`backend/cmd/agent` → `encode-agent.exe`)

- Runs as a Windows service (or foreground for debugging). Config file + flags: controller URL, node token, data dir.
- Loop: heartbeat (status JSON: current job, step, tasks_since_boot, agent version) → controller response carries instructions: `job` (rendered script + vars), `reboot`, `update`.
- Executes jobs by writing the generated `.ps1`, invoking `powershell.exe -NoProfile -ExecutionPolicy Bypass -File`, streaming stdout/stderr to the controller, tracking exit code.
- Reboot: on `reboot` instruction with no running job, schedules `shutdown /r /t 30` and reports; counter resets naturally on next boot.
- Auto-update: on `update` instruction, downloads new `encode-agent.exe` + `EncodeLib.ps1` to staging, verifies checksum, swaps the script immediately and the binary on next service restart (self-replace via sidecar restart command).

### Step templates — every flow section owns its PowerShell

Phase-2 architecture: a flow is an ordered list of references to **step
templates**, and each template carries its own PowerShell function. At render
time the controller **links** every referenced function into the final job
script and calls it with a shared `$Job` context object plus the step's
`$Params` — so a saved flow is always self-contained and custom steps need no
node-side install.

- **Built-in templates** (seeded at boot, editable, not deletable):
  `source_rename`, `media_probe`, `dgindex`, `hdr_probe`, `audio`,
  `audio_branch`, `audio_lang`, `flac_audio`, `encode`, `encode_4k`, `mux`,
  `crc32_rename`, `release_copy`, `keyframes`.
- **HDR/4K chain**: `hdr_probe` (a separate flow step) writes `hdr.json`
  (HDR10/HLG/DoVi detection from MediaInfo); `encode_4k` (2160p x265, CTU 64
  defaults) consumes the sidecar — it never probes the source itself.
  HDR10/HLG switch the color signaling to bt2020 + PQ/HLG (fork-exact
  spellings: `smpte2084`, `bt2020nc`). Dolby Vision: dovi_tool extracts the
  source RPU (reuse-cached in `rpu.bin`) and x265 encodes profile 8.1 with
  the RPU embedded per frame (`--dolby-vision-rpu`, verified closed-loop on a
  real node) plus the mandatory VBV + mastering-display flags; extraction
  failures fall back to HDR10 signaling. Guarded factory upgrade
  (Encode4kFactoryV1 → current) keeps user-edited scripts in effect.
- **Language-aware audio**: `audio_lang` selects the audio track by a
  MediaInfo language priority list (default `jpn,eng`, falls back to the
  first track with a loud warning), then runs eac3to → WAV → opusenc and
  records the pick in `audio.json`; the mux template (V3 factory) reads
  `audio.json` and sets the mkvmerge track language instead of hardcoding
  `jpn`. The mux upgrade is byte-for-byte guarded (V1 → V3 and V2 → V3), so
  user-edited mux scripts survive.
- **Custom templates**: created in the UI (key + params + PowerShell),
  syntax-checked by the controller when pwsh is available, validated to
  define a function; they appear in the flow builder palette automatically.
- **Function contract**: `param([Parameter(Mandatory=$true)] [pscustomobject] $Job,
  [pscustomobject] $Params)`; helpers come from EncodeLib.ps1 (Resolve-Tool,
  Invoke-Tool, Find-SourceFile, Assert-SafeName); progress via
  `ENCODE_STEP <key> <pct>` lines.

`powershell/EncodeLib.ps1` is the shared helper library only:

Functions:

- `Invoke-SourceRename` — rename the raw source to `src.<ext>` (legacy `rename *.m2ts src.m2ts`).
- `Invoke-DgIndex` — `DGIndexNV.exe -i src -o src.dgi -h`.
- `Invoke-AudioExtract` — `eac3to.exe src 2: audio.wav -down16` then `opusenc --bitrate <n> audio.wav audio.opus`; deletes the WAV unless `-KeepWav`.
- `Invoke-VideoEncode` — `x265_x64.exe` with the flow's parameter set and the episode's `.avs`/`.vpy` as input.
- `Invoke-Mux` — `mkvmerge` with the repo's standard track flags (jpn, default video, no chapters/global tags).
- `Invoke-ReleaseCopy` — copy the finished MKV into the `ReleaseFolders/[group] Series - Raws [tag]/` pattern.
- `Invoke-Keyframes` — `ffmpeg` writes a downscaled temp y4m, `SCXvid` reads it into the keyframes file; skipped if it already exists.

All steps write structured progress lines (`ENCODE_STEP <name> <pct>`) that the agent parses into heartbeat progress.

## Node registration

Two paths:

1. **Manual**: admin creates the node in the UI, copies the one-time agent
   token into `agent.json` (`token` field).
2. **Pairing (one-shot code)**: admin issues a pairing code in the UI
   (1-hour TTL); the node ships only `node_name` + `pairing_code` in
   `agent.json`. On first start the agent calls the unauthenticated
   `POST /api/agent/pair`, which consumes the code, creates the node, and
   returns a permanent token the agent persists at `data_dir/node.token`
   (0600). Later starts reuse the persisted credential; the code itself is
   single-use and hash-only on the server.

## Series registry

The scanner auto-registers every series folder it sees. Each series has:

- **flow selection** — an explicit flow, or 0 to inherit the flagged default flow;
- **tag override** — a per-series quality tag (e.g. `2160p`); blank inherits
  the global settings tag. The renderer uses it for output names
  (`<Series> - <Ep> [<Tag>].mkv`) and the release folder
  (`[<Group>] <Series> - Raws [<Tag>]`);
- **enabled flag** — a paused series is skipped by the scanner (no new jobs),
  without affecting other series.

Jobs carry the flow fixed at creation time; operators can change a *pending*
job's flow via `PATCH /api/jobs/{id}` before it starts. Episodes distribute
across all enabled idle nodes naturally (one job per node).

### Create series (scaffolding)

`POST /api/series` (UI: Series page → **Create series**) builds the full
folder structure up front:

- scripts share: `<ScriptsRoot>/<Name>/Ep 01 … Ep NN` (empty — sources +
  filter scripts are dropped in by hand; the scanner only queues episodes
  that have both);
- release share: `<ReleaseRoot>/[<Group>] <Name> - Raws [<Tag>]` — the
  release_copy destination.

Idempotent: re-running adds missing episode folders (extend a series by
raising the count), never duplicates. Names are validated against the
Windows-reserved characters before any filesystem touch; episode folders pad
to three digits for 100+ episode shows (`Ep 001`).

## Flows: multiple sequences, one default

Any number of flows can be saved; exactly one carries `is_default`
(atomic swap). Resolution order for new jobs: series flow → default flow →
configured default name. Flows export/import as JSON; the export embeds any
custom step templates the flow uses (built-ins resolve at the destination),
and import refuses flows referencing unknown templates.

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
2. Scanner detects it next cycle (sources younger than 2 minutes are deferred, so mid-copy NFS uploads don't trigger jobs) → job `pending` with default flow (or UI-assigned).
3. Node `enc-02` heartbeats idle → controller assigns job, response carries rendered script.
4. Agent executes step by step; heartbeats carry progress; UI live-updates.
5. Job completes → controller verifies output path exists on the share → `done`.
6. After the node's 10th completed task, controller responds with `reboot`; the instruction is re-issued on every idle heartbeat until the node's counter drops (proof of reboot), so a missed packet self-heals. Agent defers until idle, reboots; node comes back with counter 0 and rejoins the pool. A reboot attempt expires after a 10-minute grace period so no node can be locked out by a stuck flag.

## Safety invariants enforced by the store/API

- At most one active (assigned/running) job per node; `AssignJob` verifies rows-affected and node enabled-state before marking a node busy.
- A node may only report progress/completion for jobs assigned to it.
- Terminal jobs cannot regress via heartbeats; completion is idempotent.
- The configured default flow is protected from deletion; flows with job history refuse deletion (FK).

## Security and observability

- Agent tokens: random per-node tokens issued at node registration, stored hashed (SHA-256, constant-time verify). Management plane: session tokens issued at login, stored SHA-256-hashed at rest, sliding 24h expiry. TLS optional behind reverse proxy.
- Auto-update payloads (agent binary + EncodeLib.ps1) are SHA-256 verified by the agent against the manifest before install; downloads are size-capped. Request bodies are capped at 1 MiB.
- Structured JSON logs (`slog`) with `job_id` / `node` fields on both sides; agent log tails retained per job for post-mortem.

## Open decisions

- Episode numbering source: derived from folder name (`Ep NN`) today; metadata file later if needed.
- Multi-audio-track support: today audio track index is a flow param (default `2:` like the legacy script).
