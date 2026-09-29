# Architecture

## Objectives and NFRs

- Queue-driven encode farm: detect new episode material on storage shares, run jobs on a fleet of nodes, monitor everything from one web UI.
- Storage is pluggable: **NFS**, **SMB**, or **S3-compatible object storage** (MinIO, Ceph RGW, AWS). S3 needs no mounts anywhere — the controller lists the bucket and agents stage episodes per job.
- Windows Server 2025 nodes with Nvidia GPUs; encode tooling unchanged (DGIndexNV, x265 fork, mkvmerge), audio migrated from FLAC/eac3to-only to Opus (eac3to → WAV → opusenc).
- NFRs: single source of truth (controller DB), bounded concurrency per node, node reboot after 10 tasks, agent auto-update without touching each box, structured logs with job IDs, audit trail for admin mutations.

## System overview

```text
  NFS / SMB / S3                 +-----------------------------+
  scripts/  <------------------> | Controller (Linux container)|
  ReleaseFolders/                |  Go HTTP + SQLite           |
  (S3: no mount — API/listing)   |  scanner, queue, renderer   |
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

Storage transport per role is chosen from the **shares table** (`nfs|smb|s3`);
the controller mounts nfs/smb (or lists s3), and nodes reach the same source.

## Components

### Controller (`backend/cmd/controller`)

- **Scanner** discovers episode folders on the configured storage sources. On a mounted share (nfs/smb) it polls `scripts/`; when an **enabled s3 share** serves the scripts role it switches to bucket listing (`scanner.ScanS3`: depth-2 `<series>/<episode>/` keys — no mount anywhere). An episode folder is "ready" when it contains a source video (`*.m2ts|*.ts|*.mkv`) plus at least one filter script. Script priority: `2160.vpy > 2160.avs > 1080.vpy > 1080.avs > any other .avs/.vpy` (VapourSynth wins at the same resolution). Ready-and-unseen folders become jobs. Sources younger than the stability window (2 min) are deferred — both for mid-copy share uploads and for S3 (`LastModified` gate).
- **Queue** persists jobs in SQLite. Job lifecycle: `pending → assigned → running → muxing → done` (or `failed`/`cancelled`). Dispatch respects: per-node concurrency slots (`max_concurrent_jobs`, default 1, clamped 1–8), series pause (queued jobs of a paused series hold), node-group routing (series `node_group` → nodes with a matching `group`; empty group = wildcard), priority (`priority DESC, id ASC` FIFO within a tier), retry backoff (`next_retry_at`), and **retry steering** — a retried job prefers a node other than its `last_failed_node_id` within the same priority tier. Drain mode (live setting) halts assignment fleet-wide; per-node disk alerts soft-drain a single node.
- **Flow renderer** turns a flow definition (ordered steps + params) plus job variables (series, episode, paths, output name) into a self-contained PowerShell script. The generated script calls functions from `EncodeLib.ps1`. For s3-backed roles, `$ScriptsDir`/`$ReleaseDir` point at a `{{JOBDIR}}` staging placeholder and the job payload carries an `S3Transfer` spec (bucket/keys) — the agent downloads `scripts/<episode_dir>` before the run and uploads outputs after success only.
- **Agent API** under `/api/agent/*`: heartbeat + claim, step progress, log tail upload, job completion, update manifest + binary/script download. Token auth per node.
- **UI API** under `/api/*` for the SPA: nodes, jobs (incl. bulk retry/cancel, prune, SSE log stream), flows, step templates, series, shares, scanner config, settings, stats, metrics, audit log, API tokens, DB backups, updates (agent/lib/bin + rollback). Management-plane auth is a normal username/password login (`POST /api/auth/login` issues session tokens; bcrypt-hashed passwords; sliding 24h expiry; logout revokes; 5-failure throttle). **Scoped API tokens** (`/api/tokens`, scopes `admin`/`read`, hashed at rest) give external automation the same API without a session. A Prometheus endpoint is served at `GET /metrics` (mounted outside the SPA).

### Agent (`backend/cmd/agent` → `encode-agent.exe`)

- Runs as a Windows scheduled task (`EncodeAgentDist`, restart-safe) or service/foreground for debugging. Config file + flags: controller URL, node token, data dir.
- Loop: heartbeat (status JSON: current job(s), step, tasks_since_boot, agent version, metrics) → controller response carries instructions: `job` (rendered script + vars), `reboot`, `update`.
- Executes jobs by writing the generated `.ps1`, invoking `powershell.exe -NoProfile -ExecutionPolicy Bypass -File`, streaming stdout/stderr to the controller, tracking exit code. Reports per-step timings and `ENCODE_METRIC key=value` quality/output stats (vmaf, bitrate, durations, sizes) in the completion payload.
- S3-backed jobs: downloads `scripts/<episode_dir>` into the job staging dir before the run (mirroring the subdir layout the rendered script joins) and uploads outputs after success only.
- Reboot: on `reboot` instruction with no running job, schedules `shutdown /r /t 30` and reports; counter resets naturally on next boot.
- Auto-update: on `update` instruction, downloads new `encode-agent.exe` + `EncodeLib.ps1` to staging, verifies checksum, swaps the script immediately and the binary via the swap sidecar (task relaunch). The controller keeps one `.prev` agent slot; `POST /api/updates/agent/rollback` re-promotes it and nodes self-downgrade through the same version-diff sync.

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
  `verify_output`, `crc32_rename`, `release_copy`, `keyframes`,
  `discord_notify`.
- **Discord notification step**: `discord_notify` posts an episode-progress
  message to a Discord webhook when the flow reaches it (e.g. after mux or
  release copy). Webhook resolution: flow param first, then the controller's
  Discord webhook (live Settings-page field `discord_webhook`, env
  `ENCODE_DISCORD_WEBHOOK` seeds the default) injected into the `$Job`
  context at render time. Best-effort: unreachable webhooks warn, never fail
  the encode; a URL guard limits targets to discord.com/discordapp.com
  webhooks plus loopback (mock testing). Job-outcome alerts (done/failed,
  `notify` package) use the same live webhook and resolve it per alert, so
  Settings-page edits apply to both without a restart — a saved blank value
  turns notifications off even when the env var is set.
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
- **enabled flag** — a disabled series is skipped by the scanner (no new jobs),
  without affecting other series;
- **paused flag** — a stronger hold than `enabled=false`: the scanner skips the
  series AND already-queued pending jobs do not dispatch until it is lifted
  (queue position preserved; UI toggle on the Series page);
- **node_group** — free-form routing label; when non-empty, the series' jobs
  only dispatch to nodes whose `group` matches (nodes with an empty group are
  wildcards and still accept them);
- **notify / webhook_url** — per-series Discord mute toggle, and an optional
  per-series webhook override so alerts for one series go to its own channel
  (direct alerts only; digest mode batches into the global channel by design).

Jobs carry the flow fixed at creation time; operators can change a *pending*
job's flow via `PATCH /api/jobs/{id}` before it starts. Episodes distribute
across enabled idle nodes per the dispatch rules (concurrency slots, groups,
priority, steering).

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

## Storage shares

`GET|POST /api/shares`, `PUT|DELETE /api/shares/{id}` — one row per
source, `{kind: nfs|smb|s3, role: scripts|release}` (UI: Settings → Storage
shares card).

- **Credentials are write-only**: the API returns `has_password`, never the
  value; omit/blank the password on PUT to keep the stored credential.
  Secrets are AES-GCM encrypted with a key file beside the DB, so backup
  downloads never carry them. Mutations audit as `share.create/update/delete`.
- **Provisioning** resolves one enabled share per role (preference
  smb > nfs > s3). SMB mounts on nodes go through Ansible `encode_smb_*`
  vars (`New-SmbMapping` + a startup remount task), gated by the
  `mount_shares` provision option.
- **S3** works against any plain-S3 endpoint (MinIO, Ceph RGW, AWS) via
  minio-go; the shared client lives in `internal/s3`. S3-backed roles never
  mount: the controller lists the bucket for the scanner, and the agent
  stages episodes per job (download before the run, upload outputs after
  success). An enabled s3 share on the scripts role switches the scanner to
  bucket listing automatically — full autonomy with no mounts anywhere.

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
  "log_tail": "...last lines...",
  "metrics": {
    "cpu_pct": 71.5, "mem_used_mb": 24576, "mem_total_mb": 32768,
    "disk_free_gb": 812, "gpu_util": 96, "gpu_temp": 64,
    "gpu_mem_used_mb": 9216, "encode_fps": 12.34
  }
}
```

`metrics` is optional (old agents omit it). GPU fields are `-1` when the node
has no GPU / `nvidia-smi` is absent; `encode_fps` is parsed live from the
current job's `run.log` tail (x265/ffmpeg fps lines), `0` when idle.
Collection is best-effort with a bounded timeout budget — a metrics failure
can never block or fail a heartbeat.

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

`POST /api/agent/job/<id>/complete` with `{ "status": "done|failed", "exit_code": n, "outputs": [paths], "log_tail": "...", "log_full": "...", "step_timings": [...], "metrics": {...} }`.

`log_full` carries the last 1 MiB of the job's `run.log` (cut on a line
boundary aligned to a UTF-8 rune boundary, `[…truncated…]` marker when cut;
the completion route accepts a 4 MiB body because JSON escaping inflates a
1 MiB log past the general cap); `step_timings` carries per-step wall-clock
durations derived live from the `ENCODE_STEP` markers as output arrives
(first-seen timestamp per step; a step's duration ends when the next step
starts, the last step ends at job finish); `metrics` carries `ENCODE_METRIC
key=value` pairs emitted by the flow script (vmaf, bitrate, durations,
sizes…). All three fields are optional — old agents omit them, and the
controller re-caps `log_full` at 1 MiB defensively.

## Observability and queue control (2026-08-30 feature set)

- **Full job logs + step timings** — persisted from the completion report
  (`jobs.full_log`, `jobs.step_timings_json`); `GET /api/jobs/{id}/log`
  serves the raw log (admin-auth); the Jobs page shows a log viewer dialog
  and a per-step duration breakdown with proportional bars.
- **Node telemetry** — heartbeat `metrics` are stored in a `node_metrics`
  ring table (24h retention, pruned per insert on the `(node_id, ts)`
  index); `GET /api/nodes/{id}/metrics?range=1h|6h|24h` downsamples to
  ≤500 points; `GET /api/nodes` embeds each node's latest sample as
  `last_metrics`. UI renders chips, sparklines, and a fleet strip.
- **Fleet stats** — `GET /api/stats?range=24h|7d|30d|all` aggregates job
  history in SQL (totals, avg duration, per-node/per-flow breakdowns,
  failures-by-step, done-per-day). No dedicated storage — derived columns.
- **Retry policy** — per-flow `options_json` (`max_retries`,
  `retry_backoff_minutes`; zero retries = off). Failed jobs under a policy
  re-queue silently (`retry_count+1`, `next_retry_at = now + backoff`,
  floor 1 minute, cap 24h); Discord fires only on final failure with
  "(after N retries)". Assignment gates on `next_retry_at` and orders
  `priority DESC, id ASC` (FIFO within a priority tier). Orphan-job
  recovery honors the same policy. Manual retry clears the gate.
- **Job priority** — `jobs.priority` settable via `PATCH /api/jobs/{id}`
  while pending; highest dispatches first.
- **Drain mode** — live `settings.drain_mode`: heartbeats reply
  `instruction: none` before any assignment (running jobs finish; reboot
  and update instructions unaffected). For host maintenance / bin pushes.
- **Series progress** — `GET /api/series` enriches each row with
  `episodes_done/failed/active/total` from job history ("eventually done
  wins": a retried success is not also failed); total falls back from
  scaffolded `Ep *` folders on the scripts root to distinct job dirs.
- **Notifications** — per-series mute (`series.notify`, UI toggle); Discord
  alerts deep-link to `<controller_url>/jobs?job=<id>` (Jobs page
  auto-opens the log dialog); optional hourly digest mode
  (`settings.notify_digest`) buffers outcomes in memory and posts one
  summary per hour (buffer lost on restart by design — the jobs table is
  the source of truth).
- **`verify_output` step** — post-mux integrity check seeded into the
  default flows after `mux`: the MKV must exist non-empty, carry ≥1 video
  and ≥1 audio track (`mkvmerge -J`), and pass a duration check — compared
  against the source media via MediaInfo (±2s) when discoverable, else a
  sanity floor (>60s). Factory text is byte-guarded like the other built-ins.

## Queue control, storage & admin features (2026-09 feature set)

- **Live job log streaming (SSE)** — `GET /api/jobs/{id}/log/stream`
  (admin-auth): snapshot → progress events fed by heartbeats → `final` event
  closes the stream; `: ping` keepalives every 15s. Live step/progress/log
  tail require an agent new enough to populate heartbeat progress; older
  agents get snapshot-only streams. UI: JobLogDialog tails live.
- **Bulk job actions** — `POST /api/jobs/bulk {action:"retry"|"cancel",
  ids:[…]}`, per-id guarded (max 500); wrong-state/missing ids come back in
  `skipped`, never fail the batch. UI: checkbox selection + Retry/Cancel.
- **Job history retention** — `POST /api/jobs/prune {days:N}` (1–3650) and
  live `settings.job_retention_days` (0 = keep forever) with an hourly prune
  loop; terminal jobs only — pending/running never pruned.
- **Dispatch steering** — jobs carry `last_failed_node_id`; retries prefer a
  different node within the same priority tier (priority always outranks
  steering; single-node farms still dispatch the steered job).
- **Agent release rollback** — `POST /api/updates/agent/rollback` swaps the
  previous published agent back to current (409 when no prev; publishing
  keeps exactly one `.prev` slot). Nodes self-downgrade through the normal
  version-diff sync. UI: rollback button on the Settings publish card.
- **Per-series pause** — `series.paused` holds BOTH scanning and dispatch of
  already-queued jobs (vs `enabled=false`, which only stops new job
  creation). UI pause/resume toggle per row.
- **Node-group routing** — free-form `nodes.group` + `series.node_group`;
  matching jobs only dispatch to matching nodes, empty group = wildcard.
  PATCH `/api/nodes/{id} {group}` / `/api/series/{id} {node_group}`; UI on
  Nodes and Series pages.
- **Per-node job concurrency** — `nodes.max_concurrent_jobs` (default 1,
  clamped 1–8); the store enforces active-jobs ≤ slots and `/api/nodes`
  reports `active_jobs` ("1/2 slots" in the UI). Light steps can overlap;
  heavy x265 encodes usually want 1.
- **Disk-space alert + soft drain** — `settings.disk_alert_gb`: a heartbeat
  reporting less free space fires a Discord alert (per-node cooldown) and
  soft-drains that node (no new jobs until it recovers). 0 = disabled.
- **Job metrics (`ENCODE_METRIC`)** — flow scripts emit
  `ENCODE_METRIC key=value` lines (vmaf, output_bitrate_kbps, duration_sec,
  sizes…); the agent collects them into the completion report →
  `jobs.metrics_json` → UI job detail.
- **Per-series webhooks + alert stats** — `series.webhook_url` overrides the
  global Discord webhook per series (direct alerts; digest stays global);
  alerts include fleet stats context.
- **Job ETA** — `GET /api/jobs/{id}/eta`: average done-duration for the same
  flow (≥2 samples) minus elapsed, never negative, `-1` = no estimate; the
  UI blends it with reported progress.
- **Prometheus `/metrics`** — mounted outside the SPA at `GET /metrics`
  (unauthenticated, LAN scrape): `encode_jobs_pending/assigned/running`,
  `encode_jobs_done_total/failed_total/cancelled_total`,
  `encode_node_online/enabled/active_jobs/max_concurrent_jobs` per node.
- **Audit log** — mutating admin actions write an audit row (actor = session
  username or `api-token:<name>`, action like `settings.update`, object like
  `node:3`, small JSON detail — never secrets). `GET /api/audit?limit=N`;
  UI Audit page.
- **Scoped API tokens** — `GET|POST /api/tokens`, `DELETE /api/tokens/{id}`;
  scopes `admin` | `read`, hashed at rest, `last_used_at` tracked; bearer
  auth alongside session cookies for external automation.
- **Controller DB backups** — scheduled SQLite snapshots (VACUUM INTO) with
  retention, `GET /api/backup` (status/list), `POST /api/backup` (backup
  now), `GET|DELETE /api/backup/{name}` (download/delete),
  `PUT /api/backup/settings` (enabled + interval, live). Secrets encrypted
  beside the DB stay out of downloads. UI: Settings → DB backups card.
- **Storage shares CRUD** — see "Storage shares" above (nfs/smb/s3,
  encrypted write-only credentials, S3 scanner autonomy).

## Runtime flows

1. User drops `Ep 05/` with `src.m2ts` + `1080.vpy` into `scripts/<Series>/` (or uploads the same keys to an S3 scripts bucket).
2. Scanner detects it next cycle (sources younger than 2 minutes are deferred, so mid-copy uploads don't trigger jobs) → job `pending` with default flow (or UI-assigned).
3. Node `enc-02` heartbeats with a free slot (and matching group, series not paused, retry gate open) → controller assigns the job, response carries the rendered script (plus the `S3Transfer` spec for s3-backed roles).
4. Agent executes step by step; heartbeats carry progress; UI live-updates.
5. Job completes → controller verifies output path exists on the share → `done`.
6. After the node's 10th completed task, controller responds with `reboot`; the instruction is re-issued on every idle heartbeat until the node's counter drops (proof of reboot), so a missed packet self-heals. Agent defers until idle, reboots; node comes back with counter 0 and rejoins the pool. A reboot attempt expires after a 10-minute grace period so no node can be locked out by a stuck flag.

## Safety invariants enforced by the store/API

- Active (assigned/running) jobs per node are capped by `max_concurrent_jobs` (default 1 = the historical one-job rule); `AssignJob` verifies rows-affected and node enabled-state before marking a slot busy.
- A node may only report progress/completion for jobs assigned to it.
- Terminal jobs cannot regress via heartbeats; completion is idempotent and guarded against concurrent cancel (`ErrJobNotFinishable`).
- The configured default flow is protected from deletion; flows with job history refuse deletion (FK).
- Bulk job actions are per-id guarded: wrong-state or missing ids are returned as `skipped`, never failing the batch.
- Retention/prune only ever touches terminal jobs.

## Security and observability

- Agent tokens: random per-node tokens issued at node registration, stored hashed (SHA-256, constant-time verify). Management plane: session tokens issued at login, stored SHA-256-hashed at rest, sliding 24h expiry; scoped API tokens (`admin`/`read`) hashed at rest for automation. TLS optional behind reverse proxy.
- Auto-update payloads (agent binary + EncodeLib.ps1) are SHA-256 verified by the agent against the manifest before install; downloads are size-capped. Request bodies are capped at 1 MiB (job completion reports at 4 MiB — JSON escaping inflates a 1 MiB log past the general cap).
- Share credentials (SMB password, S3 secret key) are AES-GCM encrypted with a key file beside the DB and never serialized in API responses (`has_password` only).
- Mutating admin actions write to the audit log (actor, action, object, small JSON detail — never secrets or full bodies).
- Structured JSON logs (`slog`) with `job_id` / `node` fields on both sides; agent log tails retained per job for post-mortem; Prometheus `GET /metrics` for external scraping.

## Open decisions

- Episode numbering source: derived from folder name (`Ep NN`) today; metadata file later if needed.
- Multi-audio-track support: today audio track index is a flow param (default `2:` like the legacy script).
