#!/usr/bin/env python3
"""Cross-check: every PromQL metric name in the Grafana dashboard JSON must
appear in backend/internal/api/prometheus.go. Run from the repo root:

    python3 docker/grafana/check_metrics.py

Exits non-zero if any dashboard expression references a metric the controller
does not actually emit.
"""
import json
import re
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
DASHBOARD = REPO / "docker" / "grafana" / "encode-system-dashboard.json"
PROM_SOURCE = REPO / "backend" / "internal" / "api" / "prometheus.go"

# ---- metric names actually emitted by prometheus.go ----
# Pasted/curated from prometheus.go — the authoritative ground truth. This
# list is intentionally literal so a drift in the Go source is caught here.
KNOWN_METRICS = {
    "encode_jobs_pending",
    "encode_jobs_assigned",
    "encode_jobs_running",
    "encode_jobs_done_total",
    "encode_jobs_failed_total",
    "encode_jobs_cancelled_total",
    "encode_node_online",
    "encode_node_enabled",
    "encode_node_active_jobs",
    "encode_node_max_concurrent_jobs",
}


def exprs_from_dashboard(dash: dict) -> list[str]:
    """Recursively collect every target 'expr' string from all panels."""
    out = []
    for panel in dash.get("panels", []):
        for tgt in panel.get("targets", []):
            e = tgt.get("expr")
            if e:
                out.append(e)
        # nested rows (row panels can contain panels)
        for sub in panel.get("panels", []):
            for tgt in sub.get("targets", []):
                e = tgt.get("expr")
                if e:
                    out.append(e)
    return out


def metric_names_from_expr(expr: str) -> set[str]:
    """Extract bare metric identifiers from a PromQL expression.

    PromQL identifiers match [a-zA-Z_:][a-zA-Z0-9_:]*. We strip out function
    names, aggregators, and PromQL operators by relying on the fact that
    encode_* metric names all start with 'encode_'.
    """
    return {m for m in re.findall(r"[a-zA-Z_:][a-zA-Z0-9_:]*", expr) if m.startswith("encode_")}


def verify_against_source() -> set[str]:
    """Independently parse prometheus.go and return the metric names it emits,
    so the KNOWN_METRICS literal is double-checked against the live source."""
    src = PROM_SOURCE.read_text()
    # match writeGauge(b, "name", ...) / writeCounter(b, "name", ...) and
    # bare fmt.Fprintf(&b, "name{...} ...) lines
    names: set[str] = set()
    for m in re.finditer(r'write(?:Gauge|Counter)\(&b,\s*"([a-zA-Z_:][a-zA-Z0-9_:]*)"', src):
        names.add(m.group(1))
    for m in re.finditer(r'fmt\.Fprintf\(&b,\s*"([a-zA-Z_:][a-zA-Z0-9_:]*)\{', src):
        names.add(m.group(1))
    return names


def main() -> int:
    errors = []

    # 1) JSON parses
    try:
        dash = json.loads(DASHBOARD.read_text())
    except json.JSONDecodeError as exc:
        print(f"FAIL: dashboard JSON does not parse: {exc}", file=sys.stderr)
        return 1

    # 2) schemaVersion check
    sv = dash.get("schemaVersion", 0)
    if sv < 27:
        errors.append(f"schemaVersion {sv} < 27 (require >= 27)")
    else:
        print(f"OK: schemaVersion = {sv} (>= 27)")

    # 3) uid check
    uid = dash.get("uid")
    if uid != "encode-system":
        errors.append(f"uid is {uid!r}, expected 'encode-system'")
    else:
        print("OK: uid = 'encode-system'")

    # 4) datasource templating
    inputs = dash.get("__inputs", [])
    if not any(i.get("name") == "DS_PROMETHEUS" for i in inputs):
        errors.append("__inputs missing DS_PROMETHEUS entry")
    else:
        print("OK: __inputs contains DS_PROMETHEUS")
    requires = dash.get("__requires", [])
    if not requires:
        errors.append("__requires is empty")
    else:
        print(f"OK: __requires has {len(requires)} entries")

    # 5) metric-name cross-check
    source_metrics = verify_against_source()
    if source_metrics != KNOWN_METRICS:
        missing_in_literal = source_metrics - KNOWN_METRICS
        extra_in_literal = KNOWN_METRICS - source_metrics
        if missing_in_literal:
            errors.append(
                f"KNOWN_METRICS literal is stale — prometheus.go emits "
                f"{sorted(missing_in_literal)} not in the literal"
            )
        if extra_in_literal:
            errors.append(
                f"KNOWN_METRICS literal is stale — literal lists "
                f"{sorted(extra_in_literal)} not emitted by prometheus.go"
            )
    else:
        print(f"OK: KNOWN_METRICS literal matches prometheus.go ({len(source_metrics)} metrics)")

    exprs = exprs_from_dashboard(dash)
    print(f"OK: extracted {len(exprs)} target expressions from {len(dash.get('panels', []))} panels")

    used: set[str] = set()
    for e in exprs:
        used |= metric_names_from_expr(e)

    unknown = used - KNOWN_METRICS
    if unknown:
        errors.append(
            f"dashboard references metrics not in prometheus.go: {sorted(unknown)}"
        )
    else:
        print(f"OK: all {len(used)} dashboard metrics exist in prometheus.go: {sorted(used)}")

    # 6) panel grid overlap check
    panels = dash.get("panels", [])
    rects = []
    for p in panels:
        gp = p.get("gridPos", {})
        x = gp.get("x", 0)
        y = gp.get("y", 0)
        w = gp.get("w", 0)
        h = gp.get("h", 0)
        rects.append((p.get("title", f"panel-{p.get('id')}"), x, y, w, h))
    overlap = False
    for i in range(len(rects)):
        for j in range(i + 1, len(rects)):
            t1, x1, y1, w1, h1 = rects[i]
            t2, x2, y2, w2, h2 = rects[j]
            if w1 == 0 or h1 == 0 or w2 == 0 or h2 == 0:
                continue
            ix = max(0, min(x1 + w1, x2 + w2) - max(x1, x2))
            iy = max(0, min(y1 + h1, y2 + h2) - max(y1, y2))
            if ix > 0 and iy > 0:
                errors.append(f"panel grid overlap: '{t1}' and '{t2}'")
                overlap = True
    if not overlap:
        print(f"OK: no panel grid overlaps ({len(rects)} panels)")

    if errors:
        print("\n" + "=" * 60)
        print("VALIDATION FAILED:")
        for e in errors:
            print(f"  - {e}")
        return 1

    print("\nALL CHECKS PASSED")
    return 0


if __name__ == "__main__":
    sys.exit(main())
