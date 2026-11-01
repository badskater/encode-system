# Encode-System Grafana Dashboard

A pre-built Grafana dashboard that visualizes the encode-system controller's
`/metrics` (Prometheus text exposition) endpoint — job queue depth, terminal
counters, per-node liveness, and slot utilization.

## Files

| File | Purpose |
| --- | --- |
| `encode-system-dashboard.json` | Importable Grafana dashboard (uid `encode-system`). |

## Import

1. Open Grafana → **Dashboards** → **New** → **Import**.
2. **Upload JSON file** and select `encode-system-dashboard.json`
   (or paste the JSON into the text box).
3. Grafana resolves `${DS_PROMETHEUS}` from `__inputs` — select your
   Prometheus datasource when prompted (the dashboard ships a
   `DS_PROMETHEUS` datasource input so Import binds it without manual edits).
4. Click **Load** → **Import**. The dashboard opens immediately.

The dashboard uses `schemaVersion` 39 and declares `__requires` for the
`stat`, `timeseries`, `table`, and `gauge` panel plugins, so a stock Grafana
11+ install needs no extra plugins.

## Prometheus scrape config

Add a scrape job to your `prometheus.yml`. The controller serves
`GET /metrics` on its HTTP port (default `8080`) in Prometheus text format:

```yaml
scrape_configs:
  - job_name: encode-system
    metrics_path: /metrics
    scrape_interval: 30s
    static_configs:
      # Replace CONTROLLER_HOST:8080 with your controller's address.
      # Example — the current encode farm controller:
      #   172.24.92.232:8080
      - targets:
          - CONTROLLER_HOST:8080
```

After editing, reload Prometheus (`POST /-/reload` on the Prometheus port, or
restart the container) and verify the `encode-system` job is **UP** in
Prometheus → Status → Targets.

## Metrics covered

All panels are built **only** from metrics the controller actually emits
(see `backend/internal/api/prometheus.go`). No synthetic series.

| Metric | Type | Labels | Panels |
| --- | --- | --- | --- |
| `encode_jobs_pending` | gauge | — | Queue Depth (by state), Queue Depth Over Time |
| `encode_jobs_assigned` | gauge | — | Queue Depth (by state), Queue Depth Over Time |
| `encode_jobs_running` | gauge | — | Queue Depth (by state), Queue Depth Over Time |
| `encode_jobs_done_total` | counter | — | Job Status Totals, Throughput Counters |
| `encode_jobs_failed_total` | counter | — | Job Status Totals, Throughput Counters |
| `encode_jobs_cancelled_total` | counter | — | Job Status Totals, Throughput Counters |
| `encode_node_online` | gauge | `node` | Node Online State, Node Fleet Status |
| `encode_node_enabled` | gauge | `node` | Node Fleet Status |
| `encode_node_active_jobs` | gauge | `node` | Per-Node Slot Utilization, Node Fleet Status, Per-Node Slot Saturation |
| `encode_node_max_concurrent_jobs` | gauge | `node` | Per-Node Slot Utilization, Node Fleet Status, Per-Node Slot Saturation |

## Notes

- The `/metrics` endpoint is **unauthenticated** by design (verified in
  `backend/internal/api/server.go`: `GET /metrics` is registered without the
  `withAdmin` wrapper). Scrapers are expected to live on the management
  network. See `Docs/md/Operations.md` → "Monitoring with Grafana" for the
  security note.
- `cpu`, `mem`, `gpu`, `disk free`, `fps`, and `speedup` are **not** exposed
  via `/metrics` — those live in the controller's `/api/nodes/{id}/metrics`
  time-series endpoint (admin-gated) and the UI's Nodes page, not in the
  Prometheus exposition. This dashboard intentionally covers only what
  `/metrics` emits.
