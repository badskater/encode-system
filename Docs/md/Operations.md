# Operations

## Health checks

- Controller: `GET /api/health` → `{"status":"ok"}`. Include in any container orchestrator liveness config.
- Node considered stale when no heartbeat for 3× the heartbeat interval (default 15s → stale at 45s). UI shows `offline`.

## Logs

- Controller: JSON logs to stdout (`slog`), fields `component`, `job_id`, `node`. Container log driver captures them.
- Agent: `<data_dir>\agent.log` (rolling, 5×10MB) + per-job logs under `<data_dir>\jobs\<job_id>\run.log` — `C:\encode-agent` for service installs, `C:\encode-agent-dist` for the scheduled-task dist installs used on the current farm. On Linux the agent logs to stdout, so under systemd the agent log is the journal (`journalctl -u encode-agent`) and per-job logs live under `<data_dir>/jobs/<job_id>/run.log` — `<data_dir>` is the `data_dir` field from `agent.json` (`/opt/encode-agent` for controller-provisioned nodes, `/opt/encode-system` for standalone `install-agent.sh` installs). The agent also uploads the log tail (and the last 1 MiB as `log_full`) on completion/failure.
- Job stdout/stderr from x265/mkvmerge/etc. is captured in full in the per-job log; progress lines follow `ENCODE_STEP <name> <pct>`; quality/output stats follow `ENCODE_METRIC <key>=<value>`.
- Audit log: UI → Audit (or `GET /api/audit`) records every mutating admin action with actor, action, object, and a small detail snippet.

## Alerts and signals

- Job `failed` with non-zero exit code — UI job detail shows the tail; investigate `run.log`.
- Node stale — check the agent scheduled task (`Get-ScheduledTask EncodeAgentDist`, or `Get-Service encode-agent` on service installs), event log, or whether the node is mid-reboot (expected after the 10-task cycle). On Linux: `systemctl status encode-agent` (or `journalctl -u encode-agent -f` for the live log) — the unit has `Restart=always` so a crash loop is the usual failure mode.
- Disk-space alert (Discord, per-node cooldown) — the node soft-drained: no new jobs until free space recovers above `settings.disk_alert_gb`. Free the node's work disk or raise the threshold.
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
| Send one series' alerts to its own channel | UI → Series → alert-channel cell (per-series webhook URL; empty = global webhook). Digest mode always posts to the global channel. |
|| Push agent update | Settings → Push to nodes: upload new `encode-agent.exe`/`encode-agent`/`EncodeLib.ps1`/bin zip (SHA-256 hashed, version-gated), or **Fetch from URL** for a `badskater/encode-bin` release asset. Idle nodes self-swap on the next heartbeat — Windows via `swap-update.bat`, Linux via `swap-update.sh` (systemd-aware). |
| Roll back a bad agent release | Settings → Push to nodes → **Rollback** (`POST /api/updates/agent/rollback`) — the previous published agent becomes current again; nodes self-downgrade through the same version-diff sync. One `.prev` slot only (409 when there is none). |
| Inspect queue | UI → Jobs (filter by status), or `GET /api/jobs?status=pending`. |
| Read a job's full log | UI → Jobs → **Log** on a finished job (monospace viewer + download); step timings render in the same dialog. Note `GET /api/jobs/{id}/log` returns raw text, not JSON. |
| See where jobs fail / how long they take | UI → Stats (24h/7d/30d): totals, per-node/per-flow durations, failures-by-step, episodes per day. The **Avg speedup** card and per-node/per-flow Speedup columns show duration-weighted media-seconds per encode-second ("—" = no jobs reported a duration metric). |
| Spot stuck episodes | UI → Stats → **Stuck episodes (repeated failures)**: failed jobs that burned ≥1 auto-retry, worst-first (cap 25) with attempts, node, step, and error. |
| Read a node's agent log without WinRM | UI → Nodes → **Log**: the tail the agent ships in its heartbeat (last ~40 lines, pretty-printed). Empty until the node runs agent ≥1.18. |
| Check per-job quality/output stats | Jobs → job detail: `ENCODE_METRIC` values (vmaf, bitrate, durations, sizes) reported by the flow script. |
| Watch node health | UI → Nodes: live CPU/RAM/disk/GPU/fps chips; click **Metrics** for 1h/6h/24h sparklines. |
| Scrape fleet state for monitoring | `GET /metrics` (Prometheus text format, controller port): `encode_jobs_pending/assigned/running`, `encode_jobs_done_total/failed_total/cancelled_total`, per-node `encode_node_online/enabled/active_jobs/max_concurrent_jobs`. |
| Back up / restore the controller DB | Settings → **DB backups**: enable scheduled snapshots + interval, download any snapshot. Restore = stop container, replace `encode.db` in the data volume, start. On Postgres the snapshot is a logical JSON-lines `.dump` (not a `.db`); restore is via `pgimport -force` into a fresh PG, or `pg_restore`-equivalent from the JSON. See "Postgres backup & rollback" below. |
| Automate against the API | Settings → API tokens: create a scoped token (`read` or `admin`, shown once) and send it as `Authorization: Bearer <token>`. Delete tokens when done; usage is audited as `api-token:<name>`. |
| Review who changed what | UI → **Audit** page (`GET /api/audit`): every mutating admin action with actor, action, object, detail. |
| Pause assignment fleet-wide (drain) | UI → Settings → **Drain mode** toggle. Running jobs finish, nothing new dispatches; toggle off to resume. Use before pushing a bin package or host maintenance. |
| Auto-retry failed jobs | Flow builder → Max retries (0 = off, up to 10) + Retry backoff minutes. Failed jobs re-queue silently until retries are exhausted; Discord fires once on final failure with "(after N retries)". |
| Jump a job ahead in the queue | UI → Jobs → priority select on a pending job (Normal/High). High dispatches first; oldest wins within a tier. |
| Silence a noisy series' alerts | UI → Series → 🔔/🔕 per row (mute applies to direct alerts and the digest). |
| Batch job alerts into one hourly summary | UI → Settings → Discord notifications → **Hourly digest**. Per-job posts stop; one summary posts per hour with done/failed counts and per-job lines. |
| Follow a Discord alert straight to the job | Alert links open `<controller_url>/jobs?job=<id>` — the Jobs page auto-opens that job's log viewer. |
| Verify muxed output integrity | Default flows include `verify_output` after mux (tracks + duration); add it to custom flows from the step list. |
| Track a series' completion | UI → Series → Progress column (done/total bar, failed/active counts). |

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
on its next retry (~30s interval), so background loops (scanner, prune,
orphan recovery, backup scheduler) re-home automatically. Job dispatch is
unaffected: the `FOR UPDATE` row lock means only one replica can assign a
given job, so killing a replica mid-heartbeat never double-assigns. Agents
heartbeating the dead replica's Traefik backend get a connection error and
retry on the next heartbeat interval (default 15s); Traefik v3's
load-balancer stops routing to the dead container once its healthcheck
fails. Expect ~30s for the lock retry + up to one heartbeat interval for
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

## Known failure modes

- **Share mount dropped on a Windows node** (NFS/SMB): jobs fail at source read; remount via the Ansible play (`ansible-playbook site.yml --tags nfs-client`) or the provisioning "mount shares" option. On Linux: `mount -a` or `systemctl restart mnt-*.automount` (the CIFS/NFS mounts use `x-systemd.automount` so they auto-remount on first access; check the 0600 credential files at `/etc/encode-agent/cifs-*.cred` if a mount refuses).
- **S3-backed job fails at download/upload**: controller logs `s3 scan failed …` (scanner side) or the job errors in staging; check the share's endpoint/credentials (`has_password` in the API), bucket reachability from BOTH the controller container and the node, and that keys mirror `scripts/<episode_dir>`.
- **x265 fork crash on odd dimensions**: step fails with non-zero exit; inspect `run.log` for the x265 banner error; usually a filter-script issue (`.avs` crop values).
- **opusenc missing**: audio step fails fast with "required tool not found" — Ansible `bin-tools` play or a bin-package push fixes.
- **Reboot during a job**: controller defers reboot instructions until the node reports idle; a crash-reboot mid-job leaves the job orphaned — recovery fails it with a clear retry message (retry policy honored) instead of staying `running` forever.
- **Node stuck in reboot_pending**: attempts expire after a 10-minute grace period and the node rejoins automatically; check the agent log (`<data_dir>\agent.log`) if the node never actually reboots (permissions, pending reboot).
- **Node offline after an agent self-update**: the swap sidecar relaunches the task, but if it didn't fire, `Start-ScheduledTask -TaskName EncodeAgentDist` over WinRM; the agent stages the pending `.exe.new`, exits once more, and comes back on the new version (~1 min). Also check for a **stale pre-swap process**: two `encode-agent.exe` PIDs double-heartbeat — kill the one with the earlier StartTime. On Linux the `swap-update.sh` sidecar retries the `mv` (15 attempts, logged to `swap-update.log` in the data dir) then either exits to let systemd's `Restart=always` relaunch (detected via `INVOCATION_ID` + `systemctl is-active encode-agent`) or relaunches the new binary directly via `setsid`; check `swap-update.log` and `journalctl -u encode-agent` if the node never returns.
- **Two jobs on one node beyond its slots**: impossible by store constraint (active jobs ≤ `max_concurrent_jobs`), but if the DB is restored manually, verify with `GET /api/jobs?status=running`.
- **SQLite "out of memory (14)" on a fresh volume**: the data dir is owned by the wrong user — `/data` must be chowned to the runtime user (fixed in the v1.17.4 image; reappears after base-image changes).
