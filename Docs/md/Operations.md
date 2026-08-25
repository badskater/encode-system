# Operations

## Health checks

- Controller: `GET /api/health` → `{"status":"ok"}`. Include in any container orchestrator liveness config.
- Node considered stale when no heartbeat for 3× the heartbeat interval (default 15s → stale at 45s). UI shows `offline`.

## Logs

- Controller: JSON logs to stdout (`slog`), fields `component`, `job_id`, `node`. Container log driver captures them.
- Agent: `C:\encode-agent\agent.log` (rolling, 5×10MB) + per-job logs under `C:\encode-agent\jobs\<job_id>\run.log`. The agent also uploads the log tail on completion/failure.
- Job stdout/stderr from x265/mkvmerge/etc. is captured in full in the per-job log; progress lines follow `ENCODE_STEP <name> <pct>`.

## Alerts and signals

- Job `failed` with non-zero exit code — UI job detail shows the tail; investigate `run.log`.
- Node stale — check Windows service (`Get-Service encode-agent`), event log, or whether the node is mid-reboot (expected after the 10-task cycle).
- Scanner backlog growing while nodes idle — usually all nodes disabled in the UI or token mismatch.

## Operator tasks

| Task | How |
| --- | --- |
| Pause a node | UI → Nodes → toggle enabled. No new jobs assigned; running job finishes. |
| Force reboot a node | UI → Nodes → "Reboot now" (issues reboot instruction on next heartbeat). |
| Retry a failed job | UI → Jobs → Retry (re-queues as pending). |
| Change a job's flow before it starts | `PATCH /api/jobs/{id}` while the job is `pending` (UI support tracked in KANBAN). |
| Pick the flow a series encodes with | UI → Series → per-series flow selector (0 = default flow). |
| Pause one series | UI → Series → toggle (scanner stops queueing it; other series keep running). |
| Scaffold a new series' folders | UI → Series → **Create series** (name, episode count, optional tag + flow): builds `Ep 01…NN` on the scripts share and the `[Group] … - Raws [Tag]` release folder. Re-run with a higher count to extend. |
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
| Push agent update | Upload new `encode-agent.exe`/`EncodeLib.ps1` to the controller's update store (they are SHA-256 hashed); manifest bump triggers staged rollout on idle nodes. Agents verify the checksum before installing. |
| Inspect queue | UI → Jobs (filter by status), or `GET /api/jobs?status=pending`. |

## Known failure modes

- **NFS mount dropped on Windows node**: jobs fail at source read; remount play `ansible-playbook site.yml --tags nfs-client`.
- **x265 fork crash on odd dimensions**: step fails with non-zero exit; inspect `run.log` for the x265 banner error; usually a filter-script issue (`.avs` crop values).
- **opusenc missing**: audio step fails fast with "required tool not found" — Ansible `bin-tools` play fixes.
- **Reboot during a job**: controller defers reboot instructions until the node reports idle; a crash-reboot mid-job leaves the job `running`-stale → operator retries.
- **Node stuck in reboot_pending**: attempts expire after a 10-minute grace period and the node rejoins automatically; check the agent log (`C:\encode-agent\agent.log`) if the node never actually reboots (permissions, pending reboot).
- **Two jobs on one node**: impossible by store constraint (unique active job per node), but if the DB is restored manually, verify with `GET /api/jobs?status=running`.
