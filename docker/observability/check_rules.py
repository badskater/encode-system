#!/usr/bin/env python3
"""Cross-check: every metric name referenced by the alert rules in
alert-rules.yml must be emitted by the controller
(backend/internal/api/prometheus.go) or come from a documented external
source (Prometheus' own `up`, node-exporter's node_filesystem_*).

Companion to docker/grafana/check_metrics.py (which does the same for the
dashboard). Run from the repo root:

    python3 docker/observability/check_rules.py

Exits non-zero if a rule references an unknown metric or the YAML fails to
parse. For full PromQL/rule validation also run:

    promtool check rules docker/observability/alert-rules.yml
"""
import re
import sys
from pathlib import Path

import yaml

REPO = Path(__file__).resolve().parents[2]
RULES = REPO / "docker" / "observability" / "alert-rules.yml"
PROM_SOURCE = REPO / "backend" / "internal" / "api" / "prometheus.go"

# Metrics NOT emitted by the controller, with their documented provenance.
# Anything outside these sets and prometheus.go is a hard error.
EXTERNAL_METRICS = {
    # Synthesized by Prometheus itself for every scrape target.
    "up": "Prometheus-internal (scrape health)",
    # node-exporter, scraped under job="node" by the operator's stack.
    "node_filesystem_avail_bytes": "node-exporter (job=node)",
    "node_filesystem_size_bytes": "node-exporter (job=node)",
}


def metrics_from_source() -> set[str]:
    """Parse prometheus.go for the metric names it actually emits:
    writeGauge/writeCounter calls and the per-node fmt.Fprintf lines."""
    src = PROM_SOURCE.read_text()
    names: set[str] = set()
    for m in re.finditer(r'write(?:Gauge|Counter)\(&b,\s*"([a-zA-Z_:][a-zA-Z0-9_:]*)"', src):
        names.add(m.group(1))
    for m in re.finditer(r'fmt\.Fprintf\(&b,\s*"([a-zA-Z_:][a-zA-Z0-9_:]*)\{', src):
        names.add(m.group(1))
    return names


def identifiers_from_expr(expr: str) -> set[str]:
    """Bare PromQL identifiers ([a-zA-Z_:][a-zA-Z0-9_:]*), after stripping
    label selectors ({...}), range vectors ([1h]) and quoted strings so
    label names/values and duration units are not mistaken for metrics.
    Function names, aggregators and keywords are filtered by PROMQL_WORDS."""
    expr = re.sub(r"\{[^}]*\}", "", expr)
    expr = re.sub(r"\[[^\]]*\]", "", expr)
    expr = re.sub(r'"[^"]*"', "", expr)
    expr = re.sub(r"'[^']*'", "", expr)
    return set(re.findall(r"[a-zA-Z_:][a-zA-Z0-9_:]*", expr))


# PromQL functions/aggregators/keywords that can appear where a metric name
# could — not metrics, never flagged.
PROMQL_WORDS = {
    "sum", "min", "max", "avg", "count", "stddev", "stdvar", "topk", "bottomk",
    "quantile", "count_values", "group", "abs", "ceil", "floor", "round",
    "sqrt", "exp", "ln", "log2", "log10", "clamp", "clamp_min", "clamp_max",
    "rate", "irate", "increase", "delta", "idelta", "deriv", "predict_linear",
    "resets", "changes", "vector", "scalar", "time", "timestamp", "absent",
    "absent_over_time", "present_over_time", "last_over_time", "avg_over_time",
    "min_over_time", "max_over_time", "sum_over_time", "count_over_time",
    "quantile_over_time", "stddev_over_time", "stdvar_over_time", "holt_winters",
    "label_replace", "label_join", "sort", "sort_desc", "sort_by_label",
    "sort_by_label_desc", "histogram_quantile", "histogram_avg", "histogram_sum",
    "histogram_count", "histogram_fraction", "histogram_stddev", "histogram_stdvar",
    "and", "or", "unless", "by", "without", "on", "ignoring", "group_left",
    "group_right", "offset", "bool", "start", "end", "atan2", "deg", "rad",
    "acos", "asin", "atan", "cos", "sin", "tan", "cosh", "sinh", "tanh",
    "acosh", "asinh", "atanh", "sgn", "mad_over_time", "limitk", "limit_ratio",
    "info", "day_of_month", "day_of_week", "day_of_year", "days_in_month",
    "hour", "minute", "month", "year",
}


def main() -> int:
    errors = []

    # 1) YAML parses
    try:
        doc = yaml.safe_load(RULES.read_text())
    except yaml.YAMLError as exc:
        print(f"FAIL: alert-rules.yml does not parse: {exc}", file=sys.stderr)
        return 1
    print("OK: alert-rules.yml parses as YAML")

    groups = doc.get("groups", [])
    rules = [r for g in groups for r in g.get("rules", [])]
    if not rules:
        print("FAIL: no rules found", file=sys.stderr)
        return 1
    print(f"OK: {len(groups)} group(s), {len(rules)} rules")

    # 2) structural sanity: every alert rule has the required fields
    for r in rules:
        name = r.get("alert", "<unnamed>")
        for field in ("alert", "expr", "labels", "annotations"):
            if field not in r:
                errors.append(f"rule {name}: missing '{field}'")
        for field in ("summary", "description"):
            if field not in r.get("annotations", {}):
                errors.append(f"rule {name}: missing annotation '{field}'")

    # 3) metric cross-check against the live Go source
    source_metrics = metrics_from_source()
    print(f"OK: prometheus.go emits {len(source_metrics)} metrics")

    used: set[str] = set()
    for r in rules:
        used |= identifiers_from_expr(r.get("expr", ""))

    unknown = used - source_metrics - set(EXTERNAL_METRICS) - PROMQL_WORDS
    if unknown:
        errors.append(f"rules reference unknown metrics: {sorted(unknown)}")
    else:
        controller_used = sorted(used & source_metrics)
        external_used = sorted(used & set(EXTERNAL_METRICS))
        print(f"OK: {len(controller_used)} controller metrics verified: {controller_used}")
        print(f"OK: external metrics (documented provenance): {external_used}")
        unused = source_metrics - used
        if unused:
            print(f"NOTE: controller metrics not used by any rule: {sorted(unused)}")

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