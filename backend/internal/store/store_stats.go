package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// JobStats computes the fleet-wide aggregate snapshot over the job history
// for jobs that finished within the given range window (finished_at >=
// now-range). rng<=0 means "all time" (no finished_at lower bound).
//
// Every aggregate is a SQL query over the existing jobs table + LEFT JOINs
// to nodes/flows for display names — no new storage, no schema change. The
// queries are shaped to use the existing indexes:
//   - idx_jobs_status(status) backs the status-filtered terminal counts
//     (done/failed/cancelled) and the failures-by-step GROUP BY.
//   - idx_jobs_node_status(node_id, status) backs the per-node GROUP BY
//     (node_id, status) — a composite seek, not a full scan.
//
// The finished_at range predicate is applied as a secondary filter after the
// index seek; at the fleet's volume (thousands of finished jobs, not
// millions) this is a cheap bounded scan over the index-matched rows. An
// empty/never-used fleet returns a zeroed Stats shape (non-nil slices
// marshaled as []) — never an error — so the Stats page renders cleanly on a
// fresh install.
func (s *Store) JobStats(ctx context.Context, rng time.Duration) (*model.Stats, error) {
	// Build the finished_at range lower bound. "all" (rng<=0) drops the
	// predicate entirely. The cutoff is formatted in the same
	// "2006-01-02 15:04:05" UTC text shape the column stores, so the
	// comparison is a lexically-correct text compare.
	cutoff := ""
	days := 0
	if rng > 0 {
		cutoff = time.Now().UTC().Add(-rng).Format("2006-01-02 15:04:05")
		days = int(rng / (24 * time.Hour))
	}

	out := &model.Stats{
		RangeDays:      days,
		PerNode:        []model.StatsNodeRow{},
		PerFlow:        []model.StatsFlowRow{},
		FailuresByStep: []model.StatsStepRow{},
		PerDay:         []model.StatsDayRow{},
		RepeatFailures: []model.StatsRepeatRow{},
	}

	if err := s.statsTotals(ctx, out, cutoff); err != nil {
		return nil, err
	}
	if err := s.statsPerNode(ctx, out, cutoff); err != nil {
		return nil, err
	}
	if err := s.statsPerFlow(ctx, out, cutoff); err != nil {
		return nil, err
	}
	if err := s.statsFailuresByStep(ctx, out, cutoff); err != nil {
		return nil, err
	}
	if err := s.statsPerDay(ctx, out, cutoff); err != nil {
		return nil, err
	}
	if err := s.statsRepeatFailures(ctx, out, cutoff); err != nil {
		return nil, err
	}
	return out, nil
}

// statsTotals fills the done/failed/cancelled counts and the fleet-wide
// average finished-job duration. The three counts are status-equality
// predicates that seek idx_jobs_status; the AVG uses julianday subtraction
// (the SQLite idiom for seconds between two text timestamps) and only counts
// rows where BOTH started_at and finished_at are non-null (a job that never
// started has no duration to average). COALESCE turns the NULL AVG (zero
// matching rows) into 0 so an empty fleet is a clean zero, not a JSON null.
func (s *Store) statsTotals(ctx context.Context, out *model.Stats, cutoff string) error {
	q := `SELECT
    COUNT(CASE WHEN status='done' THEN 1 END),
    COUNT(CASE WHEN status='failed' THEN 1 END),
    COUNT(CASE WHEN status='cancelled' THEN 1 END),
    COALESCE(AVG((julianday(finished_at) - julianday(started_at)) * 86400), 0),
    ` + speedupAgg("") + `
  FROM jobs` +
		whereFinishedAt("finished_at", cutoff)
	var avg sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, q, rangeArgs(cutoff)...).Scan(
		&out.Totals.Done, &out.Totals.Failed, &out.Totals.Cancelled, &avg, &out.Totals.AvgSpeedup); err != nil {
		return fmt.Errorf("stats totals: %w", err)
	}
	out.Totals.AvgDurationSec = avg.Float64
	return nil
}

// statsPerNode fills the per-node aggregate rows. GROUP BY node_id seeks the
// composite index idx_jobs_node_status(node_id, status); the conditional
// COUNTs ride that same seek. LEFT JOIN nodes gives the display name (a node
// row always exists — AssignJob refuses unknown nodes — but LEFT JOIN keeps
// the query robust against a deleted node). Only nodes with ≥1 finished job
// in range appear; zero-row nodes are skipped (the Nodes page lists the full
// fleet).
func (s *Store) statsPerNode(ctx context.Context, out *model.Stats, cutoff string) error {
	q := `SELECT j.node_id, COALESCE(n.name, ''),
    COUNT(CASE WHEN j.status='done' THEN 1 END),
    COUNT(CASE WHEN j.status='failed' THEN 1 END),
    COALESCE(AVG((julianday(j.finished_at) - julianday(j.started_at)) * 86400), 0),
    ` + speedupAgg("j.") + `
  FROM jobs j LEFT JOIN nodes n ON n.id = j.node_id` +
		whereFinishedAt("j.finished_at", cutoff) +
		` GROUP BY j.node_id ORDER BY COUNT(CASE WHEN j.status='done' THEN 1 END) DESC`
	rows, err := s.db.QueryContext(ctx, q, rangeArgs(cutoff)...)
	if err != nil {
		return fmt.Errorf("stats per node: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r model.StatsNodeRow
		var avg sql.NullFloat64
		if err := rows.Scan(&r.NodeID, &r.Name, &r.Done, &r.Failed, &avg, &r.AvgSpeedup); err != nil {
			return err
		}
		r.AvgDurationSec = avg.Float64
		out.PerNode = append(out.PerNode, r)
	}
	return rows.Err()
}

// statsPerFlow fills the per-flow aggregate rows (LEFT JOIN flows for the
// name). Same conditional-COUNT + julianday-AVG shape as per-node.
func (s *Store) statsPerFlow(ctx context.Context, out *model.Stats, cutoff string) error {
	q := `SELECT j.flow_id, COALESCE(f.name, ''),
    COUNT(CASE WHEN j.status='done' THEN 1 END),
    COUNT(CASE WHEN j.status='failed' THEN 1 END),
    COALESCE(AVG((julianday(j.finished_at) - julianday(j.started_at)) * 86400), 0),
    ` + speedupAgg("j.") + `
  FROM jobs j LEFT JOIN flows f ON f.id = j.flow_id` +
		whereFinishedAt("j.finished_at", cutoff) +
		` GROUP BY j.flow_id ORDER BY COUNT(CASE WHEN j.status='done' THEN 1 END) DESC`
	rows, err := s.db.QueryContext(ctx, q, rangeArgs(cutoff)...)
	if err != nil {
		return fmt.Errorf("stats per flow: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r model.StatsFlowRow
		var avg sql.NullFloat64
		if err := rows.Scan(&r.FlowID, &r.Name, &r.Done, &r.Failed, &avg, &r.AvgSpeedup); err != nil {
			return err
		}
		r.AvgDurationSec = avg.Float64
		out.PerFlow = append(out.PerFlow, r)
	}
	return rows.Err()
}

// statsFailuresByStep fills the top-10 steps where jobs died. The step column
// holds the last/current step a job reported, so filtering status='failed'
// attributes the failure to the step it was on. The status predicate seeks
// idx_jobs_status; GROUP BY step is a bounded scan over the matched failed
// rows. Ordered by count DESC so the most failure-prone step is first.
func (s *Store) statsFailuresByStep(ctx context.Context, out *model.Stats, cutoff string) error {
	q := `SELECT step, COUNT(*) FROM jobs WHERE status='failed'` +
		andFinishedAt(cutoff) +
		` GROUP BY step ORDER BY COUNT(*) DESC LIMIT 10`
	rows, err := s.db.QueryContext(ctx, q, rangeArgs(cutoff)...)
	if err != nil {
		return fmt.Errorf("stats failures by step: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r model.StatsStepRow
		if err := rows.Scan(&r.Step, &r.Count); err != nil {
			return err
		}
		out.FailuresByStep = append(out.FailuresByStep, r)
	}
	return rows.Err()
}

// statsPerDay fills the day buckets of completed jobs for the throughput
// trend. date(finished_at) is the SQLite "YYYY-MM-DD" slice of the text
// timestamp; GROUP BY + ORDER BY d give an ascending day series. Only
// status='done' jobs count as throughput (failed/cancelled aren't "throughput").
func (s *Store) statsPerDay(ctx context.Context, out *model.Stats, cutoff string) error {
	q := `SELECT date(finished_at) d, COUNT(*) FROM jobs WHERE status='done'` +
		andFinishedAt(cutoff) +
		` GROUP BY d ORDER BY d`
	rows, err := s.db.QueryContext(ctx, q, rangeArgs(cutoff)...)
	if err != nil {
		return fmt.Errorf("stats per day: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r model.StatsDayRow
		if err := rows.Scan(&r.Date, &r.Count); err != nil {
			return err
		}
		out.PerDay = append(out.PerDay, r)
	}
	return rows.Err()
}

// statsRepeatFailures fills the stuck-episode triage list: failed jobs in
// range whose retry_count >= 1 (they burned at least one auto-retry attempt
// and still failed). Ordered worst-first (most attempts, then most recent)
// and capped at 25 — this is a "look at these" list, not a full failure
// history. The status predicate seeks idx_jobs_status; the retry_count
// filter is a cheap scan over the matched failed rows. The error text is
// SUBSTR-capped in SQL so a pathological multi-KB message cannot bloat the
// payload.
func (s *Store) statsRepeatFailures(ctx context.Context, out *model.Stats, cutoff string) error {
	q := `SELECT j.id, j.series, j.episode, j.node_id, COALESCE(n.name, ''),
    j.step, j.retry_count + 1, SUBSTR(j.error, 1, 200), COALESCE(j.finished_at, '')
  FROM jobs j LEFT JOIN nodes n ON n.id = j.node_id
  WHERE j.status='failed' AND j.retry_count >= 1` +
		andFinishedAt(cutoff) +
		` ORDER BY j.retry_count DESC, j.finished_at DESC LIMIT 25`
	rows, err := s.db.QueryContext(ctx, q, rangeArgs(cutoff)...)
	if err != nil {
		return fmt.Errorf("stats repeat failures: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r model.StatsRepeatRow
		if err := rows.Scan(&r.JobID, &r.Series, &r.Episode, &r.NodeID, &r.NodeName,
			&r.Step, &r.Attempts, &r.Error, &r.FinishedAt); err != nil {
			return err
		}
		out.RepeatFailures = append(out.RepeatFailures, r)
	}
	return rows.Err()
}

// whereFinishedAt returns " WHERE <col> >= ?" (qualified for joined queries)
// or "" when cutoff is empty ("all"). Every aggregate applies the identical
// range predicate so all sections agree on the window.
func whereFinishedAt(col, cutoff string) string {
	if cutoff == "" {
		return ""
	}
	return " WHERE " + col + " >= ?"
}

// speedupQual is the WHERE-style boolean qualifying a row for the speedup
// aggregate: a done job with both timestamps, positive wall time, and a
// positive duration_sec metric. pfx qualifies the columns for joined
// queries ("j." in per-node/per-flow, "" in totals).
//
// json_extract returns NULL (not 0) when the key is absent, so a single
// > 0 comparison covers both the missing-key and explicit-zero cases.
func speedupQual(pfx string) string {
	return pfx + `status='done'
       AND ` + pfx + `started_at IS NOT NULL AND ` + pfx + `finished_at IS NOT NULL
       AND (julianday(` + pfx + `finished_at) - julianday(` + pfx + `started_at)) * 86400 > 0
       AND json_extract(` + pfx + `metrics_json, '$.duration_sec') > 0`
}

// speedupAgg builds the DURATION-WEIGHTED speedup aggregate for a group:
// SUM(media seconds) / SUM(wall seconds) over qualifying rows, NULL-safe to
// 0 when no row qualifies. Weighted sums — not AVG(per-job ratio) — so a
// 5-second job with a noisy ratio cannot outweigh a 3-hour encode; the
// result reads as "media seconds produced per wall second on this
// node/flow/fleet", which is the comparison that matters (GPU vs CPU).
func speedupAgg(pfx string) string {
	return `COALESCE(
      SUM(CASE WHEN ` + speedupQual(pfx) + `
           THEN json_extract(` + pfx + `metrics_json, '$.duration_sec') END)
      / NULLIF(SUM(CASE WHEN ` + speedupQual(pfx) + `
           THEN (julianday(` + pfx + `finished_at) - julianday(` + pfx + `started_at)) * 86400 END), 0),
    0)`
}

// andFinishedAt returns " AND finished_at >= ?" for queries that already have
// a WHERE clause (the failures-by-step and per-day queries start with
// WHERE status=...). Empty for "all".
func andFinishedAt(cutoff string) string {
	if cutoff == "" {
		return ""
	}
	return " AND finished_at >= ?"
}

// rangeArgs returns the cutoff arg slice (nil for "all"). The conditional
// helpers above always pair with this so the arg count matches the "?" count.
func rangeArgs(cutoff string) []any {
	if cutoff == "" {
		return nil
	}
	return []any{cutoff}
}

// SeedFinishedJob inserts a terminal job row with explicit started_at and
// finished_at timestamps, for tests that need controlled durations and range
// boundaries. The normal CreateJob/AssignJob/FinishJob path stamps
// finished_at=datetime('now'), which is uncontrolled; this helper lets a test
// backdate. It is exported so cross-package API tests can use it too.
func (s *Store) SeedFinishedJob(ctx context.Context, flowID, nodeID int64, status, step, startedAt, finishedAt string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (series, episode, episode_dir, script_type, flow_id, status, node_id, step, started_at, finished_at)
		 VALUES (?, ?, ?, 'vpy', ?, ?, ?, ?, ?, ?)`,
		"S", "01", "S/Ep 01", flowID, status, nodeID, step, startedAt, finishedAt)
	return err
}
