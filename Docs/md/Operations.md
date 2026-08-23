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
| Change a job's flow before it starts | UI → Jobs → edit flow while job is `pending`. |
| Push agent update | UI → Updates → upload new `encode-agent.exe`/`EncodeLib.ps1`; manifest bump triggers staged rollout on idle nodes. |
| Inspect queue | UI → Jobs (filter by status), or `GET /api/jobs?status=pending`. |

## Known failure modes

- **NFS mount dropped on Windows node**: jobs fail at source read; remount play `ansible-playbook site.yml --tags nfs-client`.
- **x265 fork crash on odd dimensions**: step fails with non-zero exit; inspect `run.log` for the x265 banner error; usually a filter-script issue (`.avs` crop values).
- **opusenc missing**: audio step fails fast with "command not found" — Ansible `bin-tools` play fixes.
- **Reboot during a job**: controller defers reboot instructions until the node reports idle; a crash-reboot mid-job leaves the job `running`-stale → operator retries.
- **Two jobs on one node**: impossible by store constraint (unique active job per node), but if the DB is restored manually, verify with `GET /api/jobs?status=running`.
