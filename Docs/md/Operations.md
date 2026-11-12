# Operations

## Health checks

- Controller: `GET /api/health` → `{"status":"ok"}`. Include in any container orchestrator liveness config. With native TLS enabled the same endpoint is served over HTTPS (`curl --cacert ca.crt https://<controller>:8080/api/health`); the compose healthcheck switches via `ENCODE_HEALTHCHECK_URL`/`ENCODE_HEALTHCHECK_FLAGS` — see the Native TLS runbook below.
- Node considered stale when no heartbeat for 3× the heartbeat interval (default 15s → stale at 45s). UI shows `offline`.

## Logs

- Controller: JSON logs to stdout (`slog`), fields `component`, `job_id`, `node`. Container log driver captures them.
- Agent: `<data_dir>\agent.log` (rolling, 5×10MB) + per-job logs under `<data_dir>\jobs\<job_id>\run.log` — `C:\encode-agent` for service installs, `C:\encode-agent-dist` for the scheduled-task dist installs used on the current farm. On Linux the agent logs to stdout, so under systemd the agent log is the journal (`journalctl -u encode-agent`) and per-job logs live under `<data_dir>/jobs/<job_id>/run.log` — `<data_dir>` is the `data_dir` field from `agent.json` (`/opt/encode-agent` for controller-provisioned nodes, `/opt/encode-system` for standalone `install-agent.sh` installs). The agent also uploads the log tail (and the last 1 MiB as `log_full`) on completion/failure.
- Job stdout/stderr from x265/mkvmerge/etc. is captured in full in the per-job log; progress lines follow `ENCODE_STEP <name> <pct>`; quality/output stats follow `ENCODE_METRIC <key>=<value>`.
- Audit log: UI → Audit (or `GET /api/audit`) records every mutating admin action with actor, action, object, and a small detail snippet.

## Alerts and signals

- Job `failed` with non-zero exit code — UI job detail shows the tail; investigate `run.log`.
- Node stale — check the agent scheduled task (`Get-ScheduledTask EncodeAgentDist`, or `Get-Service encode-agent` on service installs), event log, or whether the node is mid-reboot (expected after the 10-task cycle). On Linux: `systemctl status encode-agent` (or `journalctl -u encode-agent -f` for the live log) — the unit has `Restart=always` so a crash loop is the usual failure mode. If `settings.notify_node_status` is on, a stale node also fires an alert on the online→offline transition (30s loop, leader-gated on HA) to every configured notification channel (Discord, generic webhook, ntfy, email).
- Disk-space alert (all notification channels, per-node cooldown) — the node soft-drained: no new jobs until free space recovers above `settings.disk_alert_gb`. Free the node's work disk or raise the threshold.
- Scanner backlog growing while nodes idle — usually all nodes disabled in the UI, token mismatch, every series paused, a node_group with no matching nodes, or drain mode left on.
- Prometheus scrape endpoint: `GET /metrics` on the controller port (`encode_jobs_*`, `encode_node_*`).

## Operator tasks

| Task | How |
| --- | --- |
| Pause a node | UI → Nodes → toggle enabled. No new jobs assigned; running job finishes. |
| Force reboot a node | UI → Nodes → "Reboot now" (issues reboot instruction on next heartbeat). |
| Retry a failed job | UI → Jobs → Retry (re-queues as pending; the retry budget refreshes). Retries are steered to a different node than the one that last failed it when one is idle in the same priority tier. |
| Retry/cancel many jobs at once | UI → Jobs → select rows (checkboxes) → Retry / Cancel (`POST /api/jobs/bulk`; wrong-state ids come back `skipped`, never fail the batch). |
| Limit job history growth | Settings → Job retention days (0 = keep forever); an hourly loop prunes terminal jobs older than N days. One-off: `POST /api/jobs/prune {days:N}`. |
| Watch a job live | UI → Jobs → Log on a running job — the dialog tails the SSE stream (`GET /api/jobs/{id}/log/stream`). Or `curl -N -H "Authorization: Bearer …" …/api/jobs/<id>/log/stream`. |
| Estimate a running job's finish | Job detail shows an ETA (avg done-duration of the same flow minus elapsed; needs ≥2 finished samples). |
| Change a job's flow before it starts | UI → Jobs → flow dropdown on a pending job (`PATCH /api/jobs/{id}`; locked once assigned/running). |
| Pick the flow a series encodes with | UI → Series → per-series flow selector (0 = default flow). |
| Pause one series | UI → Series → **Pause** toggle: the scanner stops queueing it AND already-queued jobs hold (queue position kept). The enable toggle only stops new job creation. |
| Route a series to specific nodes | Set a group on the nodes (UI → Nodes) and the same group on the series (UI → Series → node_group). Empty node group = accepts anything. |
| Run several jobs per node | UI → Nodes → concurrency slots (1–8; default 1). Slots show as "1/2" busy in the node list. |
| Alert + hold jobs on low disk | Settings → Disk alert threshold GB: below it the node posts a Discord alert and soft-drains (no new jobs) until it recovers. 0 = off. |
| Manage storage sources | UI → Settings → **Storage shares**: add/edit NFS/SMB/S3 sources per role (scripts/release). Passwords are write-only (never shown again). An enabled S3 scripts share switches the scanner to bucket listing — no mounts needed anywhere. |
| Scaffold a new series' folders | UI → Series → **Create series** (name, episode count, optional tag + flow): builds `Ep 01…NN` on the scripts share and the `[Group] … - Raws [Tag]` release folder. Re-run with a higher count to extend. |
| Delete a series (keep history) | UI → Series → **Delete** (admin; the confirm dialog shows the series' done/total progress and job count) or `DELETE /api/series/{id}`. Removes the registry row ONLY — jobs, history, stats and files are untouched. See [Delete a series](#delete-a-series-registry-cleanup) for the scanner re-registration caveat. |
| Override a series' quality tag | UI → Series → Tag column (click to edit; blank = global tag from Settings). |
| Encode a 4K/HDR series | Flow builder: use `hdr_probe` → `encode_4k` (reads hdr.json, bt2020/PQ for HDR10); author `2160.avs/.vpy` in the episode folder (outranks 1080 scripts). |
| Auto-select the audio track by language | Flow builder: use the `audio_lang` step (language priority, default `jpn,eng`); the mux step tags the track from audio.json. |
| Discord message mid-flow | Flow builder: add the `discord_notify` step anywhere (e.g. after release copy); webhook from the step param or the live Settings webhook. |
| Change the Discord webhook | UI → Settings → **Discord notifications** → webhook URL → Save. Applies immediately to job-outcome alerts AND the `discord_notify` step fallback; blank turns notifications off (no restart). |
| Add a custom pipeline section | UI → Steps → New step template (PowerShell is syntax-checked), then add it to any flow. |
| Share a flow | UI → Flows → Export JSON (embeds custom templates) / Import JSON. |
| Update the node tools (bin) folder | Zip a node's `C:\bin`, publish via Settings → Push to nodes (upload, or **Fetch from URL** for a `badskater/encode-bin` GitHub release asset + optional SHA-256 pin); nodes converge on next idle heartbeat. |
| Register a node without copy-pasting tokens | UI → Nodes → Issue pairing code → `pairing_code` in the node's `agent.json`. |
| Get job alerts in Discord | Settings → Discord notifications → webhook URL → Save (env `ENCODE_DISCORD_WEBHOOK` seeds the default); done/failed alerts post automatically with series/episode/node/error. Blank = off. |
| Get job alerts in ntfy | Settings → **Notification channels** → ntfy topic URL (e.g. `https://ntfy.sh/encode-alerts`) + optional access token → Save. Failed jobs/alerts push at high priority, done jobs at default. Blank = off. |
| Get job alerts on a generic webhook | Settings → **Notification channels** → generic webhook URL → Save. The controller POSTs structured JSON events (`job_finished` / `alert`) — payload shape in the Notification channels section below. Blank = off. |
| Get job alerts by email | Settings → **Notification channels** → SMTP host + from + to (port 0 = 587, optional user/password) → Save. Plain-text mail with the same facts as the other channels. Blank host = off. |
| Send one series' alerts to its own channel | UI → Series → alert-channel cell (per-series webhook URL; empty = global webhook). Digest mode always posts to the global channel. |
| Push agent update | Settings → Push to nodes: upload new `encode-agent.exe`/`encode-agent`/`EncodeLib.ps1`/bin zip (SHA-256 hashed, version-gated), or **Fetch from URL** for a `badskater/encode-bin` release asset. Idle nodes self-swap on the next heartbeat — Windows via `swap-update.bat`, Linux via `swap-update.sh` (systemd-aware). |
| Roll back a bad agent release | Settings → Push to nodes → **Rollback** (`POST /api/updates/agent/rollback`) — the previous published agent becomes current again; nodes self-downgrade through the same version-diff sync. One `.prev` slot only (409 when there is none). |
| Inspect queue | UI → Jobs (filter by status), or `GET /api/jobs?status=pending`. |
| Read a job's full log | UI → Jobs → **Log** on a finished job (monospace viewer + download); step timings render in the same dialog. Note `GET /api/jobs/{id}/log` returns raw text, not JSON. |
| See where jobs fail / how long they take | UI → Stats (24h/7d/30d): totals, per-node/per-flow durations, failures-by-step, episodes per day. The **Avg speedup** card and per-node/per-flow Speedup columns show duration-weighted media-seconds per encode-second ("—" = no jobs reported a duration metric). |
| Spot stuck episodes | UI → Stats → **Stuck episodes (repeated failures)**: failed jobs that burned ≥1 auto-retry, worst-first (cap 25) with attempts, node, step, and error. |
| Read a node's agent log without WinRM | UI → Nodes → **Log**: the tail the agent ships in its heartbeat (last ~40 lines, pretty-printed). Empty until the node runs agent ≥1.18. |
| Check per-job quality/output stats | Jobs → job detail: `ENCODE_METRIC` values (vmaf, quality_score, bitrate, durations, sizes) reported by the flow script. |
| Watch node health | UI → Nodes: live CPU/RAM/disk/GPU/fps chips; click **Metrics** for 1h/6h/24h sparklines. |
| Scrape fleet state for monitoring | `GET /metrics` (Prometheus text format, controller port): `encode_jobs_pending/assigned/running`, `encode_jobs_done_total/failed_total/cancelled_total`, per-node `encode_node_online/enabled/active_jobs/max_concurrent_jobs`. |
| Back up / restore the controller DB | Settings → **DB backups**: enable scheduled snapshots + interval, download any snapshot. Restore: click **Restore** on a snapshot (admin-only, `POST /api/backup/{name}/restore`) — on SQLite the controller swaps the backup over the live DB and exits for a restart (see restore runbook below); on Postgres the restore is an in-place single transaction (no restart). |
| Automate against the API | Settings → API tokens: create a scoped token (`read` or `admin`, shown once) and send it as `Authorization: Bearer <token>`. Delete tokens when done; usage is audited as `api-token:<name>`. |
| Review who changed what | UI → **Audit** page (`GET /api/audit`): every mutating admin action with actor, action, object, detail. |
| Pause assignment fleet-wide (drain) | UI → Settings → **Drain mode** toggle. Running jobs finish, nothing new dispatches; toggle off to resume. Use before pushing a bin package or host maintenance. |
| Auto-retry failed jobs | Flow builder → Max retries (0 = off, up to 10) + Retry backoff minutes. Failed jobs re-queue silently until retries are exhausted; Discord fires once on final failure with "(after N retries)". |
| Jump a job ahead in the queue | UI → Jobs → priority select on a pending job (Normal/High). High dispatches first; oldest wins within a tier. |
| Silence a noisy series' alerts | UI → Series → 🔔/🔕 per row (mute applies to direct alerts and the digest). |
| Batch job alerts into one hourly summary | UI → Settings → Discord notifications → **Hourly digest**. Per-job posts stop; one summary posts per hour with done/failed counts and per-job lines. |
| Follow a Discord alert straight to the job | Alert links open `<controller_url>/jobs?job=<id>` — the Jobs page auto-opens that job's log viewer. |
| Trigger a scan immediately | UI → Dashboard → **Scan now** (admin, `POST /api/scan` → `202 {"triggered":true}`), or `encodectl scan`. Non-blocking — a second request while one is queued coalesces. On HA only the leader's scanner consumes the trigger. |
| Delete a finished job | UI → Jobs → **Delete** on a terminal job (done/failed/cancelled), or `encodectl job-delete <id>`. Non-terminal jobs (pending/assigned/running) are refused with 409; missing job → 404. |
| Bulk set priority or flow on pending jobs | UI → Jobs → select rows → **Set priority** / **Set flow** (`POST /api/jobs/bulk` action `set_priority`/`set_flow`; max 500; wrong-state ids → `skipped`). |
| Duplicate a flow | UI → Flows → **Duplicate** on a flow (admin, `POST /api/flows/{id}/duplicate`). Default name `<original> (copy)`; the copy is never the default flow. |
| Manage user accounts | UI → **Users** card (admin-only): list, create (role admin/viewer), delete (409 on self-delete / last-admin), reset password. Viewers see no mutation controls. |
| Check for a new release | UI → Dashboard / Settings → **Update check** card (`GET /api/updates/check`, any user): compares the running version to the latest GitHub release. Needs `ENCODE_GITHUB_TOKEN` for private repos. |
| Alert on node online/offline | UI → Settings → **Node status alerts** toggle (`settings.notify_node_status`, default off). Fires alerts on real online↔offline transitions (30s loop, leader-gated on HA) to every configured channel. Requires at least one notification channel (Discord webhook, generic webhook, ntfy topic or SMTP email). |
| Verify muxed output integrity | Default flows include `verify_output` after mux (tracks + duration); add it to custom flows from the step list. |
| Gate encode quality | Default flows include `quality_gate` after `verify_output` (sampled VMAF vs source, SSIM fallback); see the quality gate section below for tuning. |
| Add subs/fonts/chapters to a finished encode | Queue the `default-subs` flow as a separate job on the same episode dir after the encode flow; see the subtitles/fonts/chapters section below. |
| Track a series' completion | UI → Series → Progress column (done/total bar, failed/active counts). |

### Delete a series (registry cleanup)

`DELETE /api/series/{id}` (admin-only; UI: Series page → **Delete**, with a
confirm dialog showing the series' done/total progress and job count)
removes the series **registry row only**:

- **Jobs and history survive.** Jobs store the series name as plain text
  (no foreign key), so every job keeps its series/episode data, stays
  listable and filterable on the Jobs page, and keeps counting in
  `/api/stats` and the per-series progress queries.
- **Per-series overrides are lost for NEW jobs.** The flow override, quality
  tag override and mute (`notify=false`) live on the registry row: jobs
  queued after the delete fall back to the default flow, the global Settings
  tag and normal notifications. Already-queued jobs keep their denormalized
  `flow_id` and finish cleanly on that flow. If the scanner re-registers the
  series (below), the fresh row starts with DEFAULT settings — re-apply any
  overrides after deleting.
- **No file operations.** Episode folders, sources, filter scripts and
  finished outputs on the scripts/release shares are never touched.
- **Scanner re-registration.** If the series folder still exists on the
  scripts share (or S3 scripts bucket) and contains *ready* episodes
  (source + filter script past the 2-minute stability gate), the next scan
  re-registers the series automatically — a fresh row, enabled, default
  flow. Existing jobs still dedupe, so no duplicate encodes are queued.
  Scaffold-only/empty folders produce no candidates and do **not**
  re-register. **Remove or rename the folder on the share to make the
  deletion stick.**
- Responses: `204` on success, `404` for an unknown id, `403` for viewer
  sessions and read-scope tokens. Audited as `series.delete` (actor +
  series name/id).

Use it to clean up stale registry rows (renamed/removed shows, accidental
registrations). To stop a series from queueing without deleting anything,
use **Pause** or the enable toggle instead.

## Quality gate (VMAF/SSIM)

`quality_gate` is a builtin step seeded into NEW default flows immediately
after `verify_output` (existing flows are never modified — add it manually
from the step palette if wanted). It compares the muxed encode against the
episode's source media (`src.*` in the episode dir) with ffmpeg and fails
the job when the perceptual score is below the threshold. `verify_output`
proves the file is structurally sound; `quality_gate` proves the encode
looks like the source.

- **Params** (Flow builder): `metric` = `vmaf` (default) or `ssim`;
  `threshold` = minimum score, blank uses the metric default — **90 for
  VMAF** (0–100 scale), **0.95 for SSIM** (0–1 scale); `sampling` = compare
  every Nth frame, default **12** (~2 samples/sec at 24fps; a 24-minute
  episode compares ~2,900 frames instead of ~34,500). Set `sampling=1` for
  a full-frame comparison, raise it for speed.
- **Sampling mechanics**: ffmpeg `select='not(mod(n\,N))'` keeps every Nth
  decoded frame on BOTH inputs (identical indices → exact pairing),
  re-timestamps, converts both streams to 10-bit 4:2:0 (the normal case is
  an 8-bit source vs a 10-bit encode), and scales the source to the encode's
  dimensions before running libvmaf/ssim.
- **libvmaf fallback**: the step probes `ffmpeg -filters` for libvmaf. When
  absent it logs a loud WARNING and falls back to SSIM; a blank threshold
  then defaults to 0.95 (SSIM scale). An explicit VMAF-scale threshold
  (>1) fails loudly instead of comparing SSIM against 90 — set an SSIM
  threshold explicitly if you want the fallback to pass. `fallback_used`
  is recorded in the artifact.
- **Artifacts**: `quality.json` in the episode dir (`metric`, `score`,
  `threshold`, `sampled_frames`, `fallback_used`) plus `ENCODE_METRIC
  quality_score=<score>`. `metrics_json` is float-only, so
  `ENCODE_METRIC quality_metric` is numeric: **1 = VMAF, 0 = SSIM** (the
  readable name lives in `quality.json` and the job log).
- **Failure classes**: missing ffmpeg / missing source or MKV / ffmpeg
  crash / unparseable score are tool errors; only a parsed score below the
  threshold is a quality rejection ("score X is below threshold Y"). Both
  fail the step, but the log message tells you which happened. A rejected
  encode still reports its score and writes `quality.json` before failing.
- **Fail-closed scores**: a parsed score must be a finite number inside the
  metric's range (VMAF 0–100, SSIM 0–1). Non-finite values (`NaN`,
  `Infinity` — which some JSON parsers accept), negative or out-of-range
  scores fail the step as a tool error ("failing closed"); they never pass
  the gate and never reach `quality.json`/`ENCODE_METRIC`. Metric lines use
  fixed decimals (4 for VMAF, 6 for SSIM) in the invariant culture.
- **Self-compare guard**: with no source present, the source finder can
  return the output MKV itself. The step compares FILE IDENTITY (resolved
  symlink targets plus hardlink identity — volume serial + file index on
  Windows, device + inode on Linux), not path strings, and treats a match
  as "no source". A `src.mkv` symlink/hardlink to the encode cannot fake a
  perfect score.
- **Frame-count pre-check**: before comparing, the step ffprobes both
  inputs (`nb_frames`, falling back to `count_packets` — MKV often reports
  `N/A`). A mismatch (e.g. 24-frame source vs 48-frame encode after
  IVTC/trim drift) fails with both counts named, because the comparison
  would silently mispair frames. Missing ffprobe logs a WARNING and skips
  the check; it never breaks the gate.
- **Filtergraph contract**: the source is scaled to the encode's dimensions
  with `[ref][dist]scale2ref` (reference FIRST) and the metric filter
  receives `[dists][refs]` (distorted first). Swapped operands silently
  flip scores (measured: SSIM 0.979 → 0.930 across the 0.95 threshold), so
  both the render-contract tests and the e2e stub pin the exact operand
  order. `scale2ref` is deprecated in ffmpeg 7.1+; the planned drop-in is
  `scale=rw:rh` — keep the operand order when migrating.
- **Tuning**: VMAF 90 suits anime at CRF ~15; lower it (e.g. 85) for
  grain-heavy sources, raise it for archive-grade encodes. SSIM 0.95 is a
  structural-loss floor, not a perceptual target — prefer VMAF when the
  node's ffmpeg has libvmaf. The gate runs before `release_copy`, so a
  rejected encode never reaches the release folder.

## Subtitles, fonts & chapters (default-subs)

`default-subs` is a SEPARATE post-encode flow (P6). It extracts subtitle
tracks, font attachments and chapters from the episode's source and muxes
them into the already-encoded output MKV. It is seeded on fresh installs
only (name-guarded, like every factory flow — existing installs get the
four builtins in the step palette but no flow changes) and it never
becomes the default flow.

- **How to run it**: after the encode flow finishes for an episode, create
  a NEW job on the SAME series/episode dir with flow `default-subs`
  (UI → Jobs → new job, or `POST /api/jobs` with the flow id). Nothing
  chains it into the encode defaults automatically — the encode flow's
  output is left untouched until you queue this. The source media
  (`src.*`) must still be present in the episode dir.
- **Steps** (in order): `subtitle_extract` → `font_extract` →
  `chapter_extract` → `mux_extras`. Each is also usable standalone in
  custom flows.
- **Tool requirements**: MKVToolNix on the node — Windows:
  `mkvmerge.exe` + `mkvextract.exe` in the bin dir (`C:\bin`); Linux:
  `mkvmerge`/`mkvextract` on PATH (package `mkvtoolnix`) plus `jq` for the
  bash ports. Missing tools fail the step with an error naming the tool
  and the package.
- **What gets extracted**: ALL subtitle tracks (every language/signs
  track — fansub releases keep them all), written as
  `subs_<trackid>_<lang>.<ext>` (extension from the codec: SRT/ASS/SSA/
  WebVTT/PGS `.sup`/VobSub `.sub`+`.idx`; unknown codecs extract raw as
  `.sub`). Font attachments are detected by MIME type
  (`application/x-truetype-font`, `application/vnd.ms-opentype`, `font/*`,
  …) or by `.ttf`/`.otf`/`.woff`/`.ttc` extension, written under `fonts/`.
  Chapters are extracted as Matroska XML (`chapters.xml`). Track
  language, name and default/forced flags are preserved in `subs.json`
  and re-applied when muxing.
- **Graceful skips**: a source with no subtitle tracks / no font
  attachments / no chapters is NOT a failure — the step logs the skip and
  succeeds. Manifests (`subs.json`, `fonts.json`) are always rewritten
  (empty when nothing was found) so re-runs reflect the current source.
  When NOTHING was extracted, `mux_extras` succeeds without touching the
  output (no mkvmerge invocation — the log says "output unchanged").
- **Output discovery**: `mux_extras` finds the encoded MKV in the episode
  dir: the optional `output` param (explicit filename) first, else the
  standard release-named MKV (`<Series> - <Ep> [<Tag>].mkv` — exactly what
  `mux` writes and `release_copy` reads), else the newest non-source MKV
  (covers outputs renamed by `crc32_rename`). Every candidate is checked
  against the source by FILE IDENTITY (symlink/hardlink aware) — the
  source is never modified; an `output` param pointing at the source is a
  loud error. No encoded output found → loud tool error (run the encode
  flow first).
- **Atomic replace + idempotency**: mkvmerge writes a UNIQUE temp
  `.mux_extras.<pid>.<rand>.tmp.mkv` in the episode dir, the result is
  verified non-empty, then renamed over the encoded output — a failed mux
  never corrupts it, and the temp is cleaned up on every exit path. A
  per-episode lock (`.mux_extras.lock`; bash `flock` with a 600s timeout,
  PowerShell `FileShare.None` open loop on the same file) serializes
  concurrent `mux_extras` runs on one episode dir — two jobs (or a retry
  racing the original) can no longer destroy each other's output. Output
  discovery excludes both the unique temp pattern and the legacy fixed
  `.mux_extras.tmp.mkv` name. The encoded input is read with
  `--no-subtitles --no-chapters --no-attachments`, so re-running the flow
  REPLACES the extras instead of duplicating them.
- **Untrusted-metadata hardening**: track names/languages come from the
  SOURCE file. Both dialects strip C0 control characters + DEL and cap
  names at 200 chars at extraction (and re-sanitize manifest values in
  `mux_extras`), so metadata can never forge `ENCODE_STEP`/`ENCODE_METRIC`
  protocol lines (markers are line-anchored in the agent) or split bash
  records. Manifests must be JSON ARRAYS — scalars, objects and empty
  files fail closed; an empty array is a graceful no-extras success.
  Manifest file entries are validated (relative names only, no `..`,
  separators, drive letters or control chars) and their RESOLVED paths
  must stay inside the episode dir (symlink-escape guard). Chapter
  `num_entries` must be 1–9 digits per edition in both dialects. Flag
  truthiness (`true`/`1`/`"true"`/`"yes"`) and MIME case-folding are
  identical across dialects.
- **Windows PowerShell 5.1 argument escaping**: the agent's fallback
  interpreter on Windows nodes is PS 5.1, whose LEGACY native-argument
  serialization wraps quotes without escaping them — an untrusted
  track_name could inject mkvmerge flags (e.g. `--attach-file` exfil).
  `EncodeLib.ps1` `Invoke-Tool` pre-escapes every argument with MSVCRT
  (`CommandLineToArgvW`) rules on PS < 7; pwsh 7+ Standard passing is
  unchanged. Validated against a CommandLineToArgvW oracle and live
  Legacy-mode full-flow runs.
- **Windows command-line ceiling**: `CreateProcess` caps the command line
  at 32767 characters. A mux with very many subtitle tracks (each carries
  language/name/flag options) can approach that ceiling on Windows nodes;
  the 200-char name cap bounds per-track growth. If mkvmerge fails to
  launch with a command-line-too-long error, reduce the track count (the
  bash dialect on Linux nodes has no such limit — `execve` argv is bounded
  only by memory).
- **Release copy**: `default-subs` modifies the episode-dir MKV only. The
  release folder copy made by the encode flow is NOT refreshed
  automatically — append a `release_copy` step to the flow (or copy
  manually) if the release should carry the extras too.
- **Failure classes**: missing tools / missing source / mkvmerge or
  mkvextract crash / missing extracted files / missing encoded output are
  tool errors with distinct messages; "nothing to extract" is always a
  success.

## Keyframes (scene-change detection)

The `keyframes` step writes `<Series> - <Episode> Keyframes.txt` into the
release folder — an **xvid first-pass stats log** (header comments + one
`type quant kblks mblks ublks length hlength` line per frame; `i` lines are
the scene-change keyframes). Aegisub opens it directly via
Video → Open Keyframes. Both dialects produce the same artifact format:

- **Windows**: `ffmpeg` downscales the release MKV to a temp y4m
  (`scale=640:360`, `yuv420p`), `SCXvid.exe` (SCXvid-standalone) reads it on
  stdin and writes the log.
- **Linux**: a generated `.vpy` loads the release MKV (lsmas
  `LWLibavSource`, ffms2 fallback), resizes to 640x360 YUV420P8 and runs
  `scxvid.Scxvid(clip, log=…, use_slices=True)`; `vspipe -c y4m` processes
  the whole clip to a null sink so the log is complete. The plugin comes
  from the `vapoursynth-scxvid` pip package (xvidcore bundled in the
  wheel); the linux-node ansible role installs it. `use_slices=True`
  mirrors SCXvid-standalone's `num_slices = num_threads`; the encoder
  settings are identical upstream, so detection matches given the same
  pixels (resampler differences between ffmpeg and zimg can cause minor
  deltas at threshold boundaries).

Operational notes:

- **Requirements (Linux)**: VapourSynth (`vspipe`, Python ≥ 3.12),
  `vapoursynth-scxvid` (`pip install vapoursynth-scxvid`), a source filter
  (`lsmas` or `ffms2`) and the `resize` (zimg) plugin. Missing
  vspipe/plugin fails the step with an actionable message naming the pip
  package — it never falls back to fake detection.
- **Skip/regenerate**: an existing Keyframes.txt newer than the release
  MKV skips the step; an older one is regenerated. Linux writes the log to
  a unique temp file next to the artifact and renames it on success, so a
  failed run never leaves a partial file that the skip check would treat
  as fresh on retry. The skip check requires a REGULAR file — a directory
  named `Keyframes.txt` never satisfies it, and every publish-critical
  command (stale removal, temp creation, rename) fails the step explicitly
  on error; a post-publish check confirms a regular file landed. Windows
  drops partial/invalid direct-write logs on failure for the same reason.
- **Log validity**: the published log must contain at least one real
  frame-stats line (`i`/`p`/`b`/`s` + six numbers). xvidcore writes the
  two header comment lines at plugin CREATE, before any frame is
  processed — a header-only log means ZERO frames were processed (a
  crashed run), not a static clip; a genuinely static clip still yields
  one `b`/`p` line per frame and passes. Junk/whitespace-only/header-only
  logs fail the step in both dialects.
- **Input is the RELEASE MKV**, not the raw source — run the step after
  `release_copy` (as the default flows do). Without the release MKV the
  step fails with `release MKV not found`.
- **Runtime**: detection encodes every frame with xvid at 640x360 — expect
  minutes per episode, not seconds. Seeking is not used (the plugin
  processes from frame 0); the whole clip runs in one vspipe pass.
- **Progress + stall watchdog (Linux)**: `vspipe -p` streams
  `Frame: N/M` progress; the step parses it and emits throttled
  `ENCODE_STEP keyframes <pct>` markers (≥2% change or ≥5s, capped at 99
  until publish). The controller's stall watchdog refreshes
  `last_progress_at` only on step/progress CHANGE, so these markers keep
  long keyframes runs alive under `settings.stall_timeout_minutes`.
- **Progress + stall watchdog (Windows) — known limitation**:
  SCXvid-standalone emits NO parseable progress (verified against
  soyokaze/SCXvid-standalone `scxvid.c`: the frame loop prints nothing;
  only error lines go to stderr). The step echoes those lines live, but
  there is no honest percentage to report, so none is fabricated. A
  Windows keyframes step therefore reports progress only at start (0) and
  finish (100): set `settings.stall_timeout_minutes` ABOVE the longest
  expected Windows keyframes runtime (minutes per episode on slow nodes),
  or 0 to disable the watchdog. A too-low threshold fails healthy Windows
  keyframes runs and burns retries. Linux nodes are unaffected.

## Notification channels

Discord stays the default transport (unchanged). Three additive channels ship
off-by-default, configured in Settings → **Notification channels**
(`generic_webhook_url`, `ntfy_topic_url`/`ntfy_token`, `smtp_host`,
`smtp_port`, `smtp_user`, `smtp_password`, `smtp_from`, `smtp_to`). All
channels receive the SAME events with the same semantics: job outcomes
(done/failed), node online/offline transitions, low-disk alerts and the
hourly digest. The skip rules apply to every channel alike — a muted series,
digest buffering and the silent auto-retry re-queue notify NO channel. One
channel failing (timeout, HTTP error, SMTP refusal) is logged structured and
never blocks the others.

- **Generic webhook** — JSON POST per event:

  ```json
  {
    "event": "job_finished",
    "timestamp": "2026-11-11T12:00:00Z",
    "job_id": 7, "series": "SomeShow", "episode": "03",
    "status": "failed", "step": "encode", "error": "x265 crashed",
    "node": "enc-01", "retry_count": 2, "duration_seconds": 90,
    "link": "http://ctrl:8080/jobs?job=7"
  }
  ```

  Operational alerts (node status, disk, digest summaries) post
  `{"event":"alert","timestamp":…,"message":"<text>"}`. Job fields are
  omitted on alert events; `link` requires `controller_url`.
- **ntfy** — POST to the topic URL with the message as the body, `Title`
  ("Encode done/failed — Series Ep NN", "Encode alert") and `Priority`
  headers (failed jobs and alerts = 4/high, done jobs and digests =
  3/default). A configured token is sent as `Authorization: Bearer`.
- **Email** — plain-text mail via `net/smtp` (STARTTLS when the relay
  offers it; authenticated relays must offer TLS). Subject matches the ntfy
  title; the body carries series/episode/status/node/step/error/retries/
  duration/timestamp. There is NO Link line in email or ntfy bodies — the
  deep link is a generic-webhook-only field (it requires `controller_url`,
  which only the webhook payload carries). Port 0 defaults to 587; `smtp_to`
  is a comma-separated recipient list. If `smtp_user`/`smtp_password` are
  set but the relay does not advertise AUTH, the send still proceeds
  (unauthenticated) and a structured warning is logged.

Secrets (`ntfy_token`, `smtp_password`) are WRITE-ONLY, following the storage
shares pattern: never returned by `GET /api/settings` (the response carries
`has_ntfy_token` / `has_smtp_password` booleans instead), AES-GCM encrypted
at rest with the shares key file beside the DB (backup downloads never carry
usable secrets). On `PUT /api/settings` an empty secret KEEPS the stored
value; a non-empty value replaces it. Clear the topic URL / SMTP host to
disable a channel. Channel URLs must be http(s); SMTP port 0 or 1-65535;
from/to must parse as email addresses (host+from+to together enable email);
`smtp_host` is a hostname or bracketed IPv6 literal (no `host:port` — the
port goes in `smtp_port`); `smtp_user` requires a password (stored or new).

Channel URLs can embed credentials in the query string (`?auth=<token>`).
For NON-admin readers (viewer sessions, read-scope API tokens) `GET
/api/settings` masks those query VALUES in `ntfy_topic_url` and
`generic_webhook_url` (scheme/host/path stay visible), and empties
`backup_offsite.s3.access_key` (`has_s3_access_key` reports presence);
admins see the full values. Channel error logs never carry raw query
credentials either — URLs are redacted before logging, including URLs
EMBEDDED in larger transport-error strings.

**Key-loss failure mode (fail-closed save).** If the shares key file is lost
or replaced, stored secrets become undecryptable. `GET /api/settings` still
serves env defaults but logs an ERROR naming the shares key. `PUT
/api/settings` REFUSES to save (500 `stored settings unreadable — restore
the shares key before saving`) rather than silently overwriting the stored
secrets with blanks — restore the original key file (e.g. from the offsite
key bundle) and retry. Notification fan-out delivers to all channels in
parallel, so one slow transport bounds the whole fan-out (~10s) instead of
stacking per-channel timeouts.

## Offsite backups (S3 + SFTP)

After **every successful local snapshot — scheduled AND manual** — the
controller uploads the snapshot to each enabled offsite destination and
prunes old copies to `keep_n` per destination. Upload failures **never fail
the local backup**: they are logged (structured, credential-redacted),
audited as `backup.offsite_failed`, and fanned out as an operational alert
through the configured notification channels (Discord/webhook/ntfy/email).
Successful copies audit as `backup.offsite`. The upload runs detached from
the request/scheduler with a 10-minute bound, so a slow or hung destination
never blocks the backup API or the scheduler tick. Destination
CONSTRUCTION is inside that bound too: the SFTP dial, SSH handshake and
subsystem exchange each carry a 15s deadline (and abort immediately on
cancellation), so a stalling server fails the run instead of hanging it.
A panicked hook is contained — it is logged and audited as
`backup.offsite_failed` (`reason=panic`), never crashing the controller.

**Configuration** lives in the settings blob (`backup_offsite`), edited via
Settings → **Offsite backups (S3 · SFTP)** or `PUT /api/settings`:

- `s3`: `enabled`, `endpoint` (host:port or http(s) URL — normalized to
  host:port on save), `bucket`, `prefix` (optional key namespace),
  `access_key`, `secret_key` (WRITE-ONLY), `use_tls`. Reuses the shared
  `internal/s3` minio client — any plain-S3 endpoint works (MinIO, Ceph RGW,
  AWS).
- `sftp`: `enabled`, `host` (hostname or bracketed IPv6 — no `host:port`),
  `port` (0 = default 22), `user`, `password` (WRITE-ONLY), `private_key`
  (WRITE-ONLY, unencrypted PEM — passphrase-protected keys are rejected at
  save time), `path` (remote dir, empty = the SSH home dir; created if
  missing), `host_key` (**REQUIRED** when enabled — the server's public SSH
  host key, pinned on every connection; see the security note below).
- `keep_n`: per-destination retention, 0 (default 30) or 1-365.
- `include_shares_key`: bundle the shares key with each snapshot
  (default **true**).

The three destination secrets follow the shares pattern exactly: `json:"-"`
on the model, `has_s3_secret_key` / `has_sftp_password` /
`has_sftp_private_key` booleans on GET (block-level), AES-GCM encrypted at
rest with the shares key. On PUT, a blank secret KEEPS the stored value; an
OMITTED `backup_offsite` block keeps the stored block wholesale (pre-P4
automation cannot wipe it). An enabled destination without a credential
(stored or supplied) is refused at save time. Validation mirrors the
notification-channel rules: port bounds, no bare-colon SFTP hosts, no
traversal (`..`) in prefixes/paths, S3 endpoint ports 1-65535. The SFTP
`host_key` is parsed at save time (`ssh.ParsePublicKey`) and stored
normalized to `<type> <base64>`; accepted shapes are known-hosts style
(`ssh-ed25519 AAAA… [comment]`, including raw `ssh-keyscan` output with a
leading hostname) or a bare base64 blob. Garbage is refused with an
`ssh-keyscan` hint.

**Remote layout and retention.** Objects are named exactly like the local
snapshot — `encode-<YYYYMMDD>-<HHMMSS>-<sched|manual>[-N].db` (SQLite) or
`.dump` (Postgres), UTC timestamp embedded so name order IS time order —
under the S3 prefix / SFTP path. With key bundling on, each snapshot has a
companion `<snapshot-name>.shares-key` (the raw 32-byte key file). Uploads
are crash-safe: S3 PUTs are atomic; SFTP writes to a unique
`<name>.partial-<tag>` temp object, chmods it **0600** (both snapshot and
key companion — server-default 0644 would expose the key), and renames it
onto the final name only after a fully successful copy. A failed upload
removes the partial, so a corrupt file can never sit at a generation name.
After a successful upload the destination is listed and generations beyond
the newest `keep_n` are deleted **key companion first, then the snapshot**
(a failed pair delete leaves snapshot-only, never a key-only orphan).
Retention only counts names matching the EXACT snapshot format above
(real timestamp, `sched|manual` kind, optional `-N` counter) — nested or
foreign `encode-*` objects are ignored, and the generation just uploaded is
**never** a delete candidate, even if a newer generation from another
replica (clock skew) sorts ahead of it. Foreign objects under the same root
are never touched. Note the difference from local retention: offsite
`keep_n` applies to ALL uploaded snapshots, manual ones included (local
retention never auto-prunes manual snapshots).

**Key bundling failure modes.** A MISSING shares-key file is normal on a
fresh install (the cipher creates it lazily) — the run uploads
snapshot-only and audits success with `shares_key_bundled:false`. Any OTHER
read error (permissions, I/O, corruption) FAILS the offsite run
(`backup.offsite_failed` + alert): a bundle that cannot decrypt on restore
must never be reported as a successful backup.

**SECURITY — read before enabling.** The shares-key bundle contains the key
that decrypts EVERY secret in the database (share passwords, ntfy tokens,
SMTP passwords, the offsite credentials themselves). Anyone who can read
the offsite destination can decrypt the backed-up DB's secrets. The
destination MUST be trusted and access-controlled (private bucket policy /
restricted SFTP account, ideally encrypted at rest by the provider). If
that is not acceptable, untick `include_shares_key`: copies become
snapshot-only, and a restore then needs the key file delivered out of band.
SFTP host keys ARE pinned: `host_key` is required configuration and every
dial verifies the server against it (`ssh.FixedHostKey`) — a mismatch
aborts the backup with an audited failure. Get the key once from a trusted
network: `ssh-keyscan <host>` (paste the `ssh-ed25519` line). If the server
is reinstalled and its host key changes, update `host_key` deliberately —
that is exactly the MITM signal pinning exists to surface. The key
companion is uploaded as a separate object, so destination ACLs must cover
both objects, not just the snapshot.

**Offsite restore runbook.** Download the snapshot AND its `.shares-key`
companion from the destination. Then either:

1. *In-place*: copy the snapshot into the controller's backups dir and use
   the normal restore endpoint/UI (see below). Place the key file where the
   controller resolves it — SQLite: `<db path>.shares-key` beside the DB;
   Postgres: `ENCODE_SHARES_KEY_FILE` (or the DSN-derived temp path) —
   BEFORE restoring, or stored secrets become undecryptable. The PG
   in-place restore re-reads the key file automatically after the swap
   (the cached cipher is invalidated); the SQLite path exits and reboots,
   which also picks the key up. If you replace the key file WITHOUT a
   restore, restart the controller — see the Postgres restore note below.
2. *Cold*: stop the controller, replace the DB file with the snapshot
   (SQLite) or re-import the dump (`pgimport -force`, Postgres), place the
   key file as above, start. Without the key the DB restores but every
   encrypted secret is lost — the fail-closed settings save will refuse
   writes until the original key is back.

**HA note.** The scheduled loop runs on the leader replica only
(advisory lock 1005); a manual backup on any replica also uploads.
Same-second local name collisions are handled by the counter suffix
(`-1`, `-2`, …): SQLite's VACUUM INTO and the PG dump's exclusive create
both refuse to clobber an existing snapshot. Overlapping uploads of the
same snapshot name are safe: S3 PUT is atomic, SFTP uses temp+rename with
per-upload unique temp names, so the last complete upload wins and no
partial is ever visible at a generation name.

## Restore runbook (in-place DB restore)

`POST /api/backup/{name}/restore` (admin-only; UI: Settings → DB backups →
**Restore**) swaps a snapshot back into the live database. The endpoint is
backend-aware — the guards and the restore mechanism differ between SQLite
and Postgres.

### Guards (both backends)

- **Name traversal-safe**: the backup name must be a plain filename — no path
  separators (`/` or `\`), no `..`. A bad name returns 400.
- **Extension matches the active backend**: `.db` for SQLite, `.dump` for
  Postgres. A mismatch returns **409** (`backup extension does not match
  active database backend`) so you can't restore a SQLite snapshot into PG or
  vice versa.
- **No active jobs**: any pending or running job returns **409** (`cancel or
  drain active jobs first`) — a restore mid-flight would lose in-flight work.
  Drain or cancel active jobs first (Settings → Drain mode, or Jobs → cancel).
- The **audit row is written before the swap** (`backup.restore`) so it
  survives even when the process exits immediately after (SQLite path).

### SQLite restore

1. The controller snapshots the current DB to
   `encode.db.pre-restore-<unix-ts>` in the data dir (beside `encode.db`).
   This is your rollback hatch if the restore is wrong.
2. It copies the backup to a temp file, then atomically renames it over the
   live `encode.db` (same filesystem — `os.Rename` is atomic).
3. It responds `200 {"restored":true,"restart":true,"snapshot":"encode.db.pre-restore-<ts>"}`.
4. The controller **exits the process** (after a 200ms flush so the response
   hits the wire). The container's restart policy (`restart: unless-stopped`
   in compose, `Restart=always` for systemd) reboots the controller on the
   restored DB.

**Restart-policy dependency:** the SQLite restore relies on the container
orchestrator restarting the process. If the restart policy is `no` or
`on-failure` (not `unless-stopped`/`always`), the controller stays down after
the exit — restart it manually (`docker compose up -d` / `systemctl start
encode-controller`). The response includes `"restart":true` so a caller can
detect this path.

The **shares-key file** (`encode.db.shares-key`) is never touched — encrypted
share credentials remain decryptable after the restore.

### Postgres restore

1. The controller reads the JSON-lines dump file (header record +
   `{"_table":"…"}` markers + `{"_table":"…","_row":{…}}` row records).
2. In a **single transaction**: TRUNCATEs every table (reverse FK order,
   `RESTART IDENTITY`), INSERTs each row with parameterized columns (rebound
   via `q()` to `$N` placeholders), and advances each serial sequence via
   `setval`/`pg_get_serial_sequence` (skipping non-serial tables like
   `settings`).
3. Commits. Responds `200 {"restored":true,"restart":false}` — **no restart
   needed**. The controller continues serving on the restored data.

If the transaction fails (e.g. a schema mismatch between the dump and the
live DB), it rolls back and the handler returns 500 with the error — the live
DB is untouched.

**Shares-key swap on PG.** The restore never touches the key file, but the
restored rows may be sealed with a DIFFERENT key (offsite bundle). After a
successful commit the controller invalidates its cached shares cipher, so
the next secret read re-reads the key file — placing the bundle's key at
`ENCODE_SHARES_KEY_FILE` BEFORE restoring is picked up automatically, no
restart needed. If you replace the key file WITHOUT running a restore
(manual swap), **restart the controller**: the cached cipher otherwise
keeps using the old key and restored/rotated secrets stay undecryptable.
(The SQLite in-place restore exits the process, so it always re-reads.)

### After any restore

- Verify with `GET /api/health` (returns `version` + `instance`) and
  `GET /api/nodes` / `GET /api/jobs` that the restored state looks right.
- The pre-restore SQLite snapshot (`encode.db.pre-restore-<ts>`) can be
  deleted once you've confirmed the restore is correct.

## encodectl quickstart

`encodectl` is a stdlib-only operator CLI shipped as
`encodectl-linux-amd64` in the release assets and the deploy tarball. It
wraps the controller REST API for shell/CI scripting. Server/token come from
`-server`/`-token` flags or the `ENCODECTL_SERVER` / `ENCODECTL_TOKEN`
environment variables (flags win). Use a scoped API token (Settings → API
tokens → create `admin`-scope) as the token.

```bash
# Connection (use env vars for repeated calls):
export ENCODECTL_SERVER=https://ctrl.example:8443
export ENCODECTL_TOKEN=<api-token>      # admin-scope token from Settings

# Health + version:
encodectl health

# Trigger a scan:
encodectl scan

# List nodes / jobs / series / stats:
encodectl nodes
encodectl jobs -status failed -limit 20
encodectl series
encodectl stats

# Job lifecycle:
encodectl job-retry 42
encodectl job-cancel 17
encodectl job-delete 99          # terminal jobs only

# Prune old jobs:
encodectl prune -days 30

# Agent publish / rollback:
encodectl publish-agent -platform linux -version 1.4.0 -file ./encode-agent-linux-amd64
encodectl rollback-agent -platform linux

# Raw JSON output for piping:
encodectl -json jobs -status running | jq .

# Version:
encodectl version
```

Exit codes: **0** success, **1** API/transport error (the server's error
message is printed to stderr), **2** usage error (missing flags, unknown
command). `-json` dumps the raw response body verbatim instead of the
tabular view.

## Postgres backup & rollback

When the controller runs against Postgres (`ENCODE_DATABASE_URL` set), the
backup endpoint produces a logical **table-walk JSON-lines dump**
(`encode-<ts>-<kind>.dump`), not a `VACUUM INTO` `.db` file:

- Each line is a JSON object: a header record (`{"_format":"encode-pg-dump","_version":1}`),
  a table marker (`{"_table":"<name>"}`), or a row record
  (`{"_table":"<name>","_row":{...}}`). Tables are walked in FK order so the
  dump can be re-imported with FKs satisfied.
- **No-secrets contract (parity with SQLite):** the `shares.password` column
  is copied verbatim — it holds the AES-GCM ciphertext (`v1:<hex>`), never
  plaintext. The encryption key file lives beside the SQLite DB / at
  `ENCODE_SHARES_KEY_FILE` on PG and is never inside the database, so neither
  backup format leaks the key. Back the key file up out of band; without it,
  stored share passwords can't be decrypted after a restore.
- The dump is human-inspectable and streamable (the writer never holds the
  whole DB in memory). Download via `GET /api/backup/<name>` (same endpoint,
  same `Content-Disposition: attachment` as SQLite snapshots).

**Restore (PG → PG):** point the controller at a fresh PG database
(`migratePG` builds the schema on boot), then re-import the dump. The dump's
JSON-lines format restores via a small INSERT loop (or `pgimport -force` from
a SQLite source if you're crossing backends).

**Rollback (PG → SQLite):** to roll back to the SQLite backend, unset
`ENCODE_DATABASE_URL` in `.env` and redeploy the old image — the controller
reopens the SQLite file in the `encode-state` volume. **Caveat:** once new
jobs have run on PG after the cutover, rolling back to SQLite loses every
write that landed on PG (jobs, heartbeats, settings edits, audit rows).
The safe rollback once PG has taken writes is to **re-import forward** (run
`pgimport` from the SQLite file into a fresh PG, or restore a PG `.dump`
snapshot into a fresh PG and repoint the controller) rather than revert to
the stale SQLite file. Roll back to SQLite only when no writes have occurred
on PG (immediate post-cutover rollback window).

## HA runbook (active/active, Postgres required)

The HA deploy (`docker-compose.ha.yml` / `install.sh --ha`) runs two
controller replicas behind one Traefik v3 load-balanced service, both
sharing a single Postgres backend. SQLite is single-writer and cannot serve
two replicas — `install.sh --ha` refuses when `ENCODE_DATABASE_URL` is
empty/SQLite.

### Verify both replicas healthy + distinct instance ids

```bash
# Both containers reporting healthy:
docker inspect -f '{{.Name}} {{.State.Health.Status}}' encode-controller-1 encode-controller-2
# Distinct instance ids (the /api/health `instance` field):
docker exec encode-controller-1 wget -qO- http://localhost:8080/api/health
docker exec encode-controller-2 wget -qO- http://localhost:8080/api/health
```

The two `instance` values must differ (`controller-1` / `controller-2`) —
that is how the advisory-lock singleton loops and failover logs distinguish
the replicas. If both report the same id, the `ENCODE_INSTANCE_ID` env was
overridden (it is hardcoded in the HA compose — do not override it).

### Failover (`kill -9` expectations)

A replica killed mid-loop releases its Postgres advisory lock on
connection close. The surviving replica's singleton loop acquires the lock
on its next retry (15s poll interval; total failover ≈ 10s keepalive
detection + 15s retry ≈ ≤25s), so background loops (scanner, prune,
digest flush, backup scheduler) re-home automatically. Job dispatch is
unaffected: the conditional `UPDATE … WHERE status='pending'` guard means
only one replica can flip a given job, and the `FOR UPDATE` node-row lock
serializes capacity checks — killing a replica mid-heartbeat never
double-assigns. Agents
heartbeating the dead replica's Traefik backend get a connection error and
retry on the next heartbeat interval (default 15s); Traefik v3's
load-balancer stops routing to the dead container once its healthcheck
fails. Expect ≤25s for the lock failover + up to one heartbeat interval for
agent reconnect.

### Rollback

- **Scale to one replica** (degrade to single-controller-with-PG):
  `docker compose -f docker-compose.ha.yml up -d --scale controller-2=0`
  (or stop `controller-2` outright). The remaining replica is a
  single-instance PG-backed controller — fully functional, no longer HA.
- **Roll back to SQLite single-instance:** stop the HA stack, unset
  `ENCODE_DATABASE_URL` in `.env`, bring up `docker-compose.runtime.yml`.
  Caveat: writes on PG after the HA cutover are lost on this path — roll
  back to SQLite only in the immediate post-cutover window; otherwise
  re-import forward (see Postgres backup & rollback above).

### Postgres is the remaining SPOF

Patroni / PG-side HA is deferred by user decision. A single bundled
`postgres:16-alpine` container is itself a SPOF — it is for trials only.
Production HA should point `ENCODE_DATABASE_URL` at a managed/external PG
(RDS, Cloud SQL, a Patroni cluster). If the PG dies, both replicas
fail-fast-exit on boot (same path as a corrupt SQLite file); bring PG back
and the replicas reconnect + re-acquire their advisory locks on restart.

## Native TLS runbook

Native controller HTTPS (`ENCODE_TLS_CERT` + `ENCODE_TLS_KEY`) and agent CA
trust (`ENCODE_AGENT_CA_CERT`). Setup recipe, compose wiring and the farm
cutover order live in `Docs/md/Deployment.md` §Native HTTPS — this section
covers verification, rotation and failure diagnosis. With TLS enabled,
`ENCODE_AGENT_ALLOW_HTTP_UPDATES` is unnecessary (the opt-in only gates
plain-HTTP downloads); remove it after cutover.

Cipher suites: the controller leaves `CipherSuites` unset, so Go's defaults
apply — PFS-preferred (ECDHE AES-GCM / ChaCha20-Poly1305; TLS 1.3 suites
always provide PFS). The ECDHE-RSA CBC suite is still accepted on TLS 1.2 for
older-client compatibility, but static RSA key exchange (plain CBC-RSA /
GCM-RSA) is disabled by default since Go 1.22. There is no operator knob for
this.

### Verify

```bash
# Health + metrics over TLS with the farm CA (from any machine holding ca.crt):
curl --cacert ca.crt https://<controller>:8080/api/health
curl --cacert ca.crt https://<controller>:8080/metrics | head

# Handshake + served chain + expiry (no CA file needed for inspection):
openssl s_client -connect <controller>:8080 -servername <controller> </dev/null 2>/dev/null \
  | openssl x509 -noout -subject -issuer -dates
# On the controller host, straight from the file:
openssl x509 -in /opt/encode-system/tls/controller.crt -noout -enddate -ext subjectAltName
```

Agent side: the node shows `online` in UI → Nodes within one heartbeat;
`agent.log` (Windows) / `journalctl -u encode-agent` (Linux) has no
`x509`/`certificate` errors. A successful agent self-update after cutover
proves the download path trusts the cert too.

### Certificate rotation

- **Server cert only (CA unchanged)** — the common case: sign a new
  `controller.crt` with the existing `ca.key` (recipe step 3 in
  Deployment.md), replace the mounted file, then
  `docker compose -f docker-compose.runtime.yml restart controller`
  (or `up -d` if the mount changed). **Agents need nothing** — they trust
  the CA, not the leaf. Check the new cert's SANs still cover every
  name/IP agents dial.
- **CA rotation**: distribute the NEW `ca.crt` to every node FIRST (append
  it to the existing PEM file — the agent pool accepts multiple certs —
  and restart agents), then switch the controller to a cert signed by the
  new CA. Doing it controller-first breaks every node until they get the
  new CA.
- Track expiry: server certs are signed for 825 days in the recipe; set a
  calendar reminder or scrape `openssl x509 -enddate` in monitoring. An
  expired cert fails every agent handshake (`x509: certificate has expired
  or is not yet valid`).

### Failure diagnosis

| Symptom | Cause | Fix |
| --- | --- | --- |
| Controller exits at boot: `ENCODE_TLS_KEY is set but ENCODE_TLS_CERT is missing` (or vice versa) | Partial TLS config — fail-fast by design | Set BOTH vars in `.env` or remove both (plain HTTP). |
| Controller exits: `load TLS keypair …` | Missing/unreadable/mismatched cert or key files | Check the compose mounts exist and container paths match the env values; cert and key must be a matching PEM pair. |
| Controller exits: `load TLS keypair … permission denied` | Host-side ownership: bind mounts keep host ownership, and the container runs as `encode` (uid 100, gid 101) — a root-owned 0600 key is unreadable inside | On the host: `ls -n /opt/encode-system/tls/` — `controller.key` must show owner `100` and mode `-rw-------`. Fix: `chown 100:101 /opt/encode-system/tls/controller.crt /opt/encode-system/tls/controller.key && chmod 600 /opt/encode-system/tls/controller.key`, then `docker compose -f docker-compose.runtime.yml restart controller`. |
| Container `unhealthy` after enabling TLS | Healthcheck still probes plain HTTP | Set `ENCODE_HEALTHCHECK_URL=https://localhost:8080/api/health` (+ `ENCODE_HEALTHCHECK_FLAGS=--no-check-certificate` for self-signed) in `.env`, recreate. |
| Controller boot log: `TLS certificate is EXPIRED …` / `NOT YET VALID …` (warning — it still starts) | Loaded cert is outside its validity window; every agent handshake will fail | Replace the cert (rotation above) and restart. Warning-only by design so a mid-rotation boot still comes up. |
| Agent log: `trust anchor is a leaf certificate …` | `ENCODE_AGENT_CA_CERT` points at the server (leaf) cert instead of the CA — works today, breaks at the next rotation | Point it at `ca.crt` and restart the agent. |
| Agent log: `x509: certificate signed by unknown authority` | `ENCODE_AGENT_CA_CERT` unset, wrong file, or not the CA that signed the controller cert | Point it at the correct `ca.crt` (PEM); restart the agent. Machine env on Windows needs an agent restart to take effect. |
| Agent log: `x509: certificate is valid for X, not Y` | SAN mismatch — cert lacks the IP/hostname in `controller_url` | Re-sign with the right `subjectAltName` (IP SAN for IP URLs); replace controller cert, restart controller. |
| Agent refuses to start: `ENCODE_AGENT_CA_CERT: cannot read …` / `contains no valid PEM certificates` | Broken path or non-PEM file — fail-fast by design | Fix the path/file; on Linux check the `ca-cert.conf` drop-in, on Windows the machine env var. |
| Agent log: `refusing download … over plain HTTP` | `controller_url` still `http://` after cutover | Switch `agent.json` to `https://` (or, temporarily, set `ENCODE_AGENT_ALLOW_HTTP_UPDATES=1`). |
| Node offline right after controller TLS cutover | Node not switched yet (expected mid-cutover) | Apply the node steps (CA env + https URL + restart); old agents retry every heartbeat and recover on their own once switched. |

### Cutover / rollback order (summary)

Controller first (drain or idle window → TLS env + mounts + healthcheck →
recreate → verify with `curl --cacert`), then node-by-node (CA file + env +
`https://` URL + restart). Rollback = remove both `ENCODE_TLS_*` vars AND the
healthcheck override (`ENCODE_HEALTHCHECK_URL`/`ENCODE_HEALTHCHECK_FLAGS` — a
leftover https probe against plain HTTP exits 4 and stays permanently
unhealthy), recreate, point nodes back to `http://` (the CA env is inert on
plain HTTP). Full step-by-step: `Docs/md/Deployment.md` §Switching an existing
farm.

## OIDC login (optional SSO)

Off by default; enabled only when all four core `ENCODE_OIDC_*` vars are set
(see `Docs/md/Deployment.md` §Optional OIDC login for setup, role mapping and
security notes). Local login is unaffected either way.

### Verify / operate

```bash
# Is SSO enabled on this controller? (unauthenticated bootstrap flag)
curl -s http://<controller>:8080/api/health | jq .oidc_enabled
# true  → login page shows "Sign in with SSO"; routes below exist.
# false → no OIDC routes (404), local-only install (the default).

# Start a flow by hand (browser does this via the SSO button):
curl -sI http://<controller>:8080/api/auth/oidc/login
# 302 → Location: <issuer>/authorize?...state=...&nonce=...&code_challenge=...
#       + Set-Cookie: encode_oidc_flow=… (HttpOnly, SameSite=Lax, 10-min
#         MaxAge) — binds the flow to THIS browser; the callback verifies it.
# 429 → login-start lockout (5 attempts → 30 s; see diagnosis below).
# 503 → provider discovery failed (see diagnosis below).

# SSO logins are audited like other auth events:
# GET /api/audit → action "auth.oidc_login", actor = username,
# detail {"role":..., "provisioned":true|false}.
```

- **Enable**: set the four core vars (+ optional `ENCODE_OIDC_ADMIN_GROUP`,
  `ENCODE_OIDC_GROUPS_CLAIM`) in `.env`, recreate the controller. Startup
  logs `OIDC login enabled` with the issuer (never the secret).
- **Disable**: unset/blank the four core vars, recreate. Routes and button
  disappear; provisioned accounts and live sessions remain until they expire
  or are deleted (user management).
- **Role changes**: with `ENCODE_OIDC_ADMIN_GROUP` set, roles sync on each
  login (both directions). A demoted user's EXISTING sessions pick up the
  new role on their next request (sessions join the user row); revoke them
  explicitly via user delete/password reset if immediate cutoff is needed.
- **HA**: pending flows are per-replica memory — the IdP callback must reach
  the replica that started the login (sticky sessions on the LB). Without
  stickiness users see intermittent "unknown or expired login state" and
  must retry; this is the documented limitation, not a bug. The flow cookie
  travels with the browser and does not remove the stickiness requirement.
- **Login CSRF protection is cookie-bound**: the callback requires the
  `encode_oidc_flow` cookie set at `/login` (hash verified in constant time).
  A flow started in one browser cannot be completed in another — the generic
  client error is `OIDC login failed`; the controller log carries only a
  sanitized error class/status, never raw IdP error bodies.

### Diagnosis

| Symptom | Cause / fix |
| --- | --- |
| Controller refuses to start, `oidc config … missing` | Partial env: some but not all four core vars set. Set all four or none (fail-fast by design). |
| SSO button missing | `oidc_enabled` false — vars not set in the controller's actual environment (check `.env` → compose passthrough), or an old UI bundle. |
| `GET /api/auth/oidc/login` → 503 | Discovery failed: issuer URL wrong/unreachable from the controller, or the IdP is down. Check `curl <issuer>/.well-known/openid-configuration` FROM the controller host/container. Retried on every attempt — no restart needed once the IdP recovers. |
| `GET /api/auth/oidc/login` → 429 | Login-start lockout: 5 attempts within the window → 30 s cooldown (same mechanism as local login, separate counter). A completed SSO callback resets it. |
| Callback → 401 `unknown or expired login state` | Login took >10 min, the callback was replayed, the controller restarted mid-flow, or (HA) the callback hit the other replica. Start the login again; on HA enable sticky sessions. |
| Callback → 401, log `login flow cookie missing or mismatched` | The callback reached a different browser/profile than the one that started the login (cookies blocked, cross-browser login-CSRF attempt, or the 10-min flow cookie expired). Start the login again in the same browser. |
| Callback → 401 `OIDC login failed`, log `id token verification failed` / `id token nonce mismatch` | Provider misconfiguration (wrong client, token for another audience) or clock skew: expiry is checked with ZERO leeway — verify NTP on the controller host. Check the provider's client ID/secret. |
| Callback → 401 `OIDC login failed`, log `token exchange rejected (HTTP …)` | The IdP rejected the code exchange (status in the log; the raw IdP body is deliberately not logged). Check client secret, redirect-URI registration and code lifetime at the provider. |
| Provider rejects the redirect | `ENCODE_OIDC_REDIRECT_URL` does not exactly match a registered redirect URI (scheme/host/port/path). |
| User gets viewer instead of admin | Groups claim not emitted by the provider, wrong claim name (`ENCODE_OIDC_GROUPS_CLAIM`), group name mismatch (exact match), or `ENCODE_OIDC_ADMIN_GROUP` unset (no mapping → existing roles kept, new users viewer). Inspect the ID token's claims at the provider side. |
| SSO user cannot log in locally | By design: provisioned accounts have no local password. Set one via user management if hybrid access is wanted. |

## Known failure modes

- **Share mount dropped on a Windows node** (NFS/SMB): jobs fail at source read; remount via the Ansible play (`ansible-playbook site.yml --tags nfs-client`) or the provisioning "mount shares" option. On Linux: `mount -a` or `systemctl restart mnt-*.automount` (the CIFS/NFS mounts use `x-systemd.automount` so they auto-remount on first access; check the 0600 credential files at `/etc/encode-agent/cifs-*.cred` if a mount refuses).
- **S3-backed job fails at download/upload**: controller logs `s3 scan failed …` (scanner side) or the job errors in staging; check the share's endpoint/credentials (`has_password` in the API), bucket reachability from BOTH the controller container and the node, and that keys mirror `scripts/<episode_dir>`.
- **x265 fork crash on odd dimensions**: step fails with non-zero exit; inspect `run.log` for the x265 banner error; usually a filter-script issue (`.avs` crop values).
- **opusenc missing**: audio step fails fast with "required tool not found" — Ansible `bin-tools` play or a bin-package push fixes.
- **Reboot during a job**: controller defers reboot instructions until the node reports idle; a crash-reboot mid-job leaves the job orphaned — recovery fails it with a clear retry message (retry policy honored) instead of staying `running` forever.
- **Wedged encode (job stalled)**: with `settings.stall_timeout_minutes` > 0 the watchdog fails running jobs that report no step/progress change for that long (unchanged heartbeats do not count), applies the flow retry policy, and — when the node is online — sends a `cancel` instruction on the next heartbeat so the agent kills the stuck process tree and frees the slot. Set the threshold above the longest gap between progress updates of any step (a long encode that only marks start/finish needs a threshold above its whole runtime — notably Windows keyframes, see § Keyframes → Progress + stall watchdog (Windows)); a too-low threshold fails healthy jobs. The `encode_jobs_stalled_total` metric counts currently failed-stalled jobs, so stalls that auto-retry to success are not reflected.
- **Node stuck in reboot_pending**: attempts expire after a 10-minute grace period and the node rejoins automatically; check the agent log (`<data_dir>\agent.log`) if the node never actually reboots (permissions, pending reboot).
- **Node offline after an agent self-update**: the swap sidecar relaunches the task, but if it didn't fire, `Start-ScheduledTask -TaskName EncodeAgentDist` over WinRM; the agent stages the pending `.exe.new`, exits once more, and comes back on the new version (~1 min). Also check for a **stale pre-swap process**: two `encode-agent.exe` PIDs double-heartbeat — kill the one with the earlier StartTime. On Linux the `swap-update.sh` sidecar retries the `mv` (15 attempts, logged to `swap-update.log` in the data dir) then either exits to let systemd's `Restart=always` relaunch (detected via `INVOCATION_ID` + `systemctl is-active encode-agent`) or relaunches the new binary directly via `setsid`; check `swap-update.log` and `journalctl -u encode-agent` if the node never returns.
- **Two jobs on one node beyond its slots**: impossible by store constraint (active jobs ≤ `max_concurrent_jobs`), but if the DB is restored manually, verify with `GET /api/jobs?status=running`.
- **SQLite "out of memory (14)" on a fresh volume**: the data dir is owned by the wrong user — `/data` must be chowned to the runtime user (fixed in the v1.17.4 image; reappears after base-image changes).

## Monitoring with Grafana

A pre-built Grafana dashboard ships under `docker/grafana/` and visualizes the
controller's `/metrics` (Prometheus text exposition) endpoint — the same
aggregate fleet state the Stats page and unauthenticated health endpoint
surface. See `docker/grafana/README.md` for import instructions and a minimal
`prometheus.yml` scrape snippet (target placeholder `CONTROLLER_HOST:8080`;
the current farm host is `172.24.92.232:8080`).

### What the dashboard covers

- **Node Online State** — per-node `encode_node_online` (1=online, 0=stale) as
  color-coded stat tiles.
- **Queue Depth (by state)** — `encode_jobs_pending` / `assigned` / `running`
  as current-value stat tiles; the **Queue Depth Over Time** time series
  tracks backlog growth and dispatch stalls.
- **Job Status Totals** — terminal counters `encode_jobs_done_total` /
  `failed_total` / `cancelled_total` as stat tiles; the **Throughput
  Counters** time series shows the completion/failure rate slope.
- **Per-Node Slot Utilization** — `encode_node_active_jobs` vs
  `encode_node_max_concurrent_jobs` overlaid so saturation is visible.
- **Node Fleet Status** — a table joining online / enabled / active / max
  per node (enabled=0 = paused).
- **Per-Node Slot Saturation** — a gauge of `active_jobs /
  max_concurrent_jobs` per node.

The dashboard JSON lives at `docker/grafana/encode-system-dashboard.json`
(uid `encode-system`, `schemaVersion` 39, 30s refresh, templated
`${DS_PROMETHEUS}` datasource). It uses only `stat`, `timeseries`, `table`,
and `gauge` panels — all built **only** from the 10 metrics the controller
actually emits (verified against `backend/internal/api/prometheus.go`; no
synthetic series).

### Opt-in Prometheus wiring and alerting

`docker/observability/` ships opt-in assets for wiring the controller into an
**existing** Prometheus/Grafana/Loki/Alloy stack (e.g. the network-wide
observability system): a scrape-job snippet (`prometheus-scrape.yml`,
`job_name: encode`), 5 alert rules (`alert-rules.yml`: controller down, queue
stalled, node offline, failure spike, host disk low), an Alertmanager →
Discord example (`alertmanager.example.yml`, webhook read from a `chmod 600`
secret file — never committed), and an optional Alloy JSON-log enrichment
snippet. Nothing is applied automatically — merging into the monitoring
stack is a deliberate, separate operation. See
`docker/observability/README.md` for the wiring guide and post-wiring
verification checklist. Metric names are cross-checked against
`backend/internal/api/prometheus.go` by `docker/observability/check_rules.py`
and `docker/grafana/check_metrics.py`; rules/config also validate with
`promtool check rules` / `amtool check-config`.

### Security note

The `/metrics` endpoint is **unauthenticated** by design: the route
`GET /metrics` is registered in `backend/internal/api/server.go` **without**
the `withAdmin` wrapper (the comment on the preceding line states scrapers
authenticate at the network layer). The endpoint exposes only aggregate
operational numbers — queue depth, node liveness, terminal counters — the
same data the unauthenticated health endpoint and the Stats page already
surface to any admin. Restrict network access to the controller port
(firewall, VPN, or management network) so only trusted Prometheus scrapers
can reach it.
