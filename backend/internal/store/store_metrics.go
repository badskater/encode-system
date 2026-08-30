package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// metricsRetention is the ring-buffer retention window. Rows older than this
// are pruned on every insert so the table holds at most ~24h of samples per
// node (bounded by the agent's heartbeat interval, e.g. ~2880 rows/node/day
// at a 30s cadence). The prune is scoped per node and seeks the composite
// index idx_metrics_node_ts(node_id, ts), so it is an index seek (SEARCH),
// not a full-table scan.
const metricsRetention = 24 * time.Hour

// metricsMaxPoints is the ceiling on the number of samples served by
// ListNodeMetrics regardless of raw row count. When the raw count for the
// requested range exceeds this, rows are evenly sampled (every Nth row) in
// Go so the dashboard receives a bounded payload. 500 is well below the
// worst-case 24h row count and high enough that a 1h/6h range is never
// downsampled.
const metricsMaxPoints = 500

// InsertNodeMetric appends one resource sample to the node_metrics ring table
// and prunes rows older than the retention window. The caller (the heartbeat
// handler) treats the returned error as best-effort: it logs a WARN and
// continues so a metrics write failure never kills the heartbeat (which would
// strand the agent without instructions). ts uses datetime('now') so the value
// matches the "2006-01-02 15:04:05" UTC format the schema and every other
// timestamp column use — the (node_id, ts) index sorts lexically, so an
// RFC3339 offset format would break ordering.
func (s *Store) InsertNodeMetric(ctx context.Context, nodeID int64, m *model.NodeMetrics) error {
	if m == nil {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO node_metrics (node_id, ts, cpu_pct, mem_used_mb, mem_total_mb, disk_free_gb, gpu_util, gpu_temp, gpu_mem_used_mb, encode_fps)
		 VALUES (?, datetime('now'), ?, ?, ?, ?, ?, ?, ?, ?)`,
		nodeID, m.CPUPct, m.MemUsedMB, m.MemTotalMB, m.DiskFreeGB,
		m.GPUUtil, m.GPUTemp, m.GPUMemUsedMB, m.EncodeFPS); err != nil {
		return fmt.Errorf("insert node metric: %w", err)
	}
	// Prune after every insert, scoped to THIS node. The per-node DELETE
	// seeks the composite index idx_metrics_node_ts(node_id, ts) (SEARCH,
	// not SCAN — a ts-only predicate can't use the index, so the prune
	// must carry node_id). At most one stale row ages out per heartbeat,
	// so the cost is a single index-backed seek per insert — negligible
	// against the insert itself.
	if err := s.PruneNodeMetrics(ctx, nodeID, metricsRetention); err != nil {
		// Prune failure is non-fatal: the ring still grows correctly,
		// it just isn't trimmed this cycle. Log-only at the caller.
		return fmt.Errorf("prune node metrics: %w", err)
	}
	return nil
}

// LatestNodeMetric returns the most recent metric sample for a node as a
// NodeMetrics pointer, or nil if the node has never sent metrics. Used by the
// node list view to surface the last-known sample per node without holding
// nodes in memory between heartbeats.
func (s *Store) LatestNodeMetric(ctx context.Context, nodeID int64) (*model.NodeMetrics, error) {
	var m model.NodeMetrics
	var ts string
	err := s.db.QueryRowContext(ctx,
		`SELECT ts, cpu_pct, mem_used_mb, mem_total_mb, disk_free_gb, gpu_util, gpu_temp, gpu_mem_used_mb, encode_fps
		 FROM node_metrics WHERE node_id = ? ORDER BY ts DESC LIMIT 1`, nodeID).
		Scan(&ts, &m.CPUPct, &m.MemUsedMB, &m.MemTotalMB, &m.DiskFreeGB,
			&m.GPUUtil, &m.GPUTemp, &m.GPUMemUsedMB, &m.EncodeFPS)
	if err != nil {
		// sql.ErrNoRows means no metrics yet (old agent or fresh node).
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("latest node metric: %w", err)
	}
	return &m, nil
}

// PruneNodeMetrics deletes rows older than maxAge for a single node from
// node_metrics. The WHERE node_id = ? AND ts < … predicate seeks the composite
// index idx_metrics_node_ts(node_id, ts) (EXPLAIN QUERY PLAN reports SEARCH,
// not SCAN) — a ts-only predicate cannot use that index, so the prune is
// scoped per node rather than a full-table sweep. Uses SQLite's datetime
// modifier so the comparison stays in the same text format the rows were
// written in (no cross-format parsing). Idempotent: a no-op when nothing is
// older than the cutoff.
func (s *Store) PruneNodeMetrics(ctx context.Context, nodeID int64, maxAge time.Duration) error {
	// Format the negative duration as a SQLite datetime modifier ("-24 hours").
	// SQLite accepts "-N hours" / "-N days"; we normalize to hours so the
	// modifier is always valid regardless of how the caller expressed maxAge.
	// int() truncates fractional hours, which is fine here: metricsRetention is
	// a whole-hours constant (24h), so there is no sub-hour precision to lose.
	hours := int(maxAge.Hours())
	if hours <= 0 {
		return nil // nothing to prune (or caller passed a non-positive window)
	}
	mod := fmt.Sprintf("-%d hours", hours)
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM node_metrics WHERE node_id = ? AND ts < datetime('now', ?)`, nodeID, mod)
	return err
}

// ListNodeMetrics returns metric samples for a node newer than since, ordered
// oldest-first, capped at metricsMaxPoints. When the raw row count for the
// range exceeds the cap, rows are evenly sampled (every Nth row) so the
// dashboard receives a bounded, representative payload instead of thousands
// of points.
func (s *Store) ListNodeMetrics(ctx context.Context, nodeID int64, since time.Time, limit int) ([]model.NodeMetricSample, error) {
	// Fetch all rows in range — the worst case is ~2880 rows/node/day at a
	// 30s heartbeat cadence, which is trivially small to hold in memory for
	// the sampling pass. Going through Go avoids a complex SQL window
	// function that SQLite's older parser shapes handle inconsistently.
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, cpu_pct, mem_used_mb, mem_total_mb, disk_free_gb, gpu_util, gpu_temp, gpu_mem_used_mb, encode_fps
		 FROM node_metrics
		 WHERE node_id = ? AND ts >= ?
		 ORDER BY ts ASC`, nodeID, since.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return nil, fmt.Errorf("list node metrics: %w", err)
	}
	defer rows.Close()

	var raw []model.NodeMetricSample
	for rows.Next() {
		var s model.NodeMetricSample
		var ts string
		if err := rows.Scan(&ts, &s.CPUPct, &s.MemUsedMB, &s.MemTotalMB,
			&s.DiskFreeGB, &s.GPUUtil, &s.GPUTemp, &s.GPUMemUsedMB, &s.EncodeFPS); err != nil {
			return nil, err
		}
		s.Ts = parseTime(ts)
		raw = append(raw, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return []model.NodeMetricSample{}, nil
	}

	// Honor the caller's limit if it is smaller than the internal cap.
	cap := metricsMaxPoints
	if limit > 0 && limit < cap {
		cap = limit
	}
	if len(raw) <= cap {
		return raw, nil
	}

	// Even stride sampling: pick every Nth row to spread the samples
	// uniformly across the time range, always including the first and
	// last points so the endpoints of the range are represented. Use a
	// ceiling division so the stride always reduces the count below the
	// cap (integer division of 600/500 = 1, which would keep every row).
	stride := (len(raw) + cap - 1) / cap
	out := make([]model.NodeMetricSample, 0, cap)
	for i, s := range raw {
		if i%stride == 0 {
			out = append(out, s)
		}
	}
	// Guarantee the last sample is included so the most-recent point is
	// always present (the dashboard's "current" reading) — but only when
	// there is room under the cap. With a stride that divides evenly into
	// the row count minus one (e.g. n=1000, stride=2: indices 0,2,…,998 =
	// exactly cap rows), the last raw index (999) is NOT already in out and
	// the unguarded append would push len(out) to cap+1 (501). Guarding with
	// len(out) < cap holds the invariant len(out) <= cap; when out is full,
	// the cap — not the missing last point — is the limiting factor.
	if len(out) < cap {
		if last := out[len(out)-1]; last.Ts != raw[len(raw)-1].Ts {
			out = append(out, raw[len(raw)-1])
		}
	}
	return out, nil
}
