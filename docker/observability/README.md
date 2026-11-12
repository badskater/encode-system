# Encode-System Observability Wiring (opt-in)

Assets for wiring the encode-system controller into an **existing**
Prometheus / Grafana / Loki / Alloy / Alertmanager stack — for example a
network-wide observability system that already runs on your LAN.

**Nothing here is applied automatically.** This repo ships no Prometheus,
Alertmanager, or Grafana of its own; these files are snippets and examples
you merge into *your* stack by hand. That stack is a separate system:
changes to it are a deliberate, separate operation (edit its config,
reload/restart its services, verify), not a side effect of deploying
encode-system. If you only want dashboards, start with
[`../grafana/README.md`](../grafana/README.md) — the scrape job below is the
only hard prerequisite.

## Files

| File | Purpose |
| --- | --- |
| `prometheus-scrape.yml` | Scrape-job snippet to merge into your `prometheus.yml` (`job_name: encode`). |
| `alert-rules.yml` | 6 Prometheus alert rules (controller down, queue stalled, node offline, failure spike, job stalled, host disk low). |
| `alertmanager.example.yml` | Example Alertmanager config routing alerts to Discord via a secret file. |
| `discord_webhook.example` | Placeholder for the Discord webhook secret file (never commit the real one). |
| `alloy-json-enrichment.alloy` | Optional `loki.process` snippet: promote JSON log `level` to a Loki label. |
| `check_rules.py` | Cross-checks rule metric names against `backend/internal/api/prometheus.go`. |

All `encode_*` metric names in the rules are verified against the
controller's exposition source (`backend/internal/api/prometheus.go`); the
only non-controller metrics used are Prometheus' own `up` and
node-exporter's `node_filesystem_*` (both documented in the rule comments).

## 0. Prerequisite: reach the metrics endpoint

The controller serves Prometheus text exposition at `GET /metrics` on its
HTTP port (default `8080`), unauthenticated by design — keep it on the
management network (see the security note in `Docs/md/Operations.md` →
"Monitoring with Grafana"). From the host that will run Prometheus:

```bash
curl -s http://172.24.92.232:8080/metrics | head
```

You should see `encode_jobs_pending`, `encode_node_online{node=...}`, etc.

## 1. Merge the scrape job

Copy the job from `prometheus-scrape.yml` into your stack's `scrape_configs`:

```yaml
scrape_configs:
  # ... your existing jobs ...
  - job_name: encode
    metrics_path: /metrics
    static_configs:
      - targets: ["172.24.92.232:8080"]
```

- **Host-published target (default):** the example IP is the current encode
  farm controller host with `:8080` published on the host; Prometheus
  scrapes it over the LAN. Replace with your controller's address.
- **Shared-network alternative:** if the encode-system and observability
  stacks ever share a Docker network, use the container name instead —
  `targets: ["encode-controller:8080"]` — no host publish required.
- **Keep `job_name: encode`.** The alert rules match `up{job="encode"}`;
  renaming the job silently disables `EncodeControllerDown`.
- `scrape_interval` inherits your global (15s in the reference stack).

Reload Prometheus (`POST /-/reload` with `--web.enable-lifecycle`, `kill -HUP`,
or restart the container), then check **Status → Targets**: the `encode`
target must be **UP**.

## 2. Install the alert rules

1. Copy `alert-rules.yml` into your stack's rules directory (e.g.
   `config/prometheus/rules/encode.yml`).
2. Reference it from `prometheus.yml` (create the `rule_files` key if absent):

   ```yaml
   rule_files:
     - /etc/prometheus/rules/*.yml   # or the exact path you copied to
   ```

3. Validate before reloading:

   ```bash
   promtool check rules config/prometheus/rules/encode.yml
   ```

4. Hot-reload Prometheus (same mechanisms as step 1) and check
   **Status → Rules**: the `encode-system` group with 5 rules must be listed.

Rules at a glance (thresholds are tunable defaults — see the comments in the
file):

| Alert | Fires when | Severity |
| --- | --- | --- |
| `EncodeControllerDown` | `up{job="encode"} == 0` for 5m | critical |
| `EncodeQueueStalled` | pending > 0, nothing running/assigned, no node online, for 15m | warning |
| `EncodeNodeOffline` | a registered node's `encode_node_online == 0` for 10m | warning |
| `EncodeFailureSpike` | `increase(encode_jobs_failed_total[1h]) > 5` for 5m | warning |
| `EncodeHostDiskLow` | node-exporter filesystem < 10% free for 10m (`job="node"`) | warning |

`EncodeHostDiskLow` requires node-exporter scraped under job name `node`
(the reference stack's convention) — adjust the selector or drop the rule if
your stack differs. `EncodeFailureSpike` caveat: the failed-jobs "counter" is
a cumulative DB count, so job pruning can look like a counter reset to
`increase()` — details in the rule comment.

## 3. Alertmanager → Discord (optional)

If your stack has no Alertmanager yet, run one pinned to
`prom/alertmanager:v0.28.1` (the version these assets were validated
against). If it already runs one, merge the routing bits instead of
replacing its config wholesale.

1. **Create the webhook secret on the Alertmanager host** (the real URL
   comes from Discord → channel settings → Integrations → Webhooks; never
   commit it — `discord_webhook.example` is a placeholder for reference):

   ```bash
   mkdir -p secrets
   # replace EXAMPLE/EXAMPLE with your real webhook id/token:
   echo 'https://discord.com/api/webhooks/EXAMPLE/EXAMPLE' > secrets/discord_webhook
   chmod 600 secrets/discord_webhook
   ```

2. **Bind-mount it into the container** at the path the config reads:
   `./secrets/discord_webhook:/etc/alertmanager/secrets/discord_webhook:ro`
   (adapt to your stack's compose/run layout).

3. Copy `alertmanager.example.yml` to your stack's Alertmanager config path
   and validate:

   ```bash
   amtool check-config alertmanager.yml
   ```

   `amtool` checks structure even when the secret file is absent; a missing
   file only fails at notification time (Alertmanager keeps running and
   alerts stay visible in its UI/API).

4. **Point Prometheus at it** in `prometheus.yml`:

   ```yaml
   alerting:
     alertmanagers:
       - static_configs:
           - targets: ["alertmanager:9093"]   # or host:port per your layout
   ```

Config behavior worth knowing: alerts group by `alertname`/`job`/`severity`
(30s batch wait, 5m group interval, 4h repeat); `Watchdog`/`InfoInhibitor`
route to a no-op `"null"` receiver instead of Discord; `send_resolved: true`
posts recovery messages.

## 4. Import the dashboard

Follow [`../grafana/README.md`](../grafana/README.md) — the importable JSON
lives at `../grafana/encode-system-dashboard.json` (uid `encode-system`,
templated `${DS_PROMETHEUS}` datasource). It is not duplicated here.

## 5. Optional: Alloy log enrichment

`alloy-json-enrichment.alloy` is a `loki.process` stage for an existing
Alloy config: it parses JSON container log lines and promotes `level` to a
Loki label (non-JSON lines pass through untouched; the syslog pipeline is
not affected). Insert it between your `loki.source.docker` and `loki.write`
and repoint the source's `forward_to` at `loki.process.docker_json.receiver`
— the file header has the exact wiring. Validate with `alloy validate`
before reloading Alloy. Skip this entirely if you don't run Alloy or don't
want the label.

## Post-wiring verification checklist

- [ ] **Target UP:** Prometheus → Status → Targets shows `encode` UP;
      `encode_jobs_pending` returns data in the expression browser.
- [ ] **Rules listed:** Status → Rules shows the `encode-system` group
      (5 rules), all green/healthy.
- [ ] **Alertmanager discovered:** Status → Runtime & Build Info (or
      `GET /api/v1/alertmanagers`) lists your Alertmanager; its own
      `/-/healthy` responds.
- [ ] **Dashboard renders:** the `encode-system` dashboard shows live queue
      depth and node tiles (no "No data" with the target UP).
- [ ] **Test alert:** fire a synthetic alert through the full path, e.g.
      `amtool alert add EncodeWiringTest severity=warning \
      --alertmanager.url=http://<alertmanager>:9093` and confirm it lands in
      Discord; or stop the controller briefly and watch
      `EncodeControllerDown` fire after its 5m `for` window (then restart
      and confirm the resolved message).

## Validation (re-run after edits)

```bash
python3 docker/grafana/check_metrics.py          # dashboard vs prometheus.go
python3 docker/observability/check_rules.py      # rules vs prometheus.go
promtool check rules docker/observability/alert-rules.yml
amtool check-config docker/observability/alertmanager.example.yml
# scrape snippet: wrap in a minimal prometheus.yml, then
promtool check config <merged.yml>
# alloy snippet: wrap with a loki.write block, then
alloy validate <wrapper.alloy>
```

All of the above pass as shipped (Prometheus v3.15.0 `promtool`,
Alertmanager v0.28.1 `amtool`, Alloy v1.21.3).

## Notes

- **Opt-in and reversible:** every step is a config merge in *your* stack;
  removing the job/rules/receiver cleanly unwires encode-system. Nothing in
  this directory is mounted or read by encode-system's own containers.
- **Secrets:** only `discord_webhook.example` (a placeholder) belongs in
  git. Real webhook files stay on the deploy host, `chmod 600`, outside the
  repo.
- **Unauthenticated `/metrics`:** aggregate operational numbers only, but
  still restrict reachability to trusted scrapers (management network /
  firewall) — see `Docs/md/Operations.md`.