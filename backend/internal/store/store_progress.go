package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// This file holds the stall-watchdog persistence surface (resilience batch):
// the jobs.last_progress_at stamp written on claim (AssignJob) and refreshed
// by heartbeats that report changed step/progress, plus the queries the
// controller watchdog and the Prometheus exposition read.

// TouchJobProgress stamps jobs.last_progress_at=now for a live job. The
// heartbeat handler calls it ONLY when the reported step or progress differs
// from the stored row — an unchanged heartbeat must not refresh the stamp,
// or a wedged encode reporting the same percentage forever would never be
// detected as stalled. Guarded to assigned/running so a stale heartbeat
// cannot touch a terminal job.
func (s *Store) TouchJobProgress(ctx context.Context, id int64) error {
	_, err := s.qExec(ctx,
		`UPDATE jobs SET last_progress_at=datetime('now') WHERE id=? AND status IN ('assigned','running')`,
		id)
	return err
}

// JobLastProgressAt reads the job's last progress stamp (nil when unset —
// rows that predate the column and were never re-claimed).
func (s *Store) JobLastProgressAt(ctx context.Context, id int64) (*time.Time, error) {
	var ts sql.NullString
	err := s.qRow(ctx, `SELECT last_progress_at FROM jobs WHERE id=?`, id).Scan(&ts)
	if err != nil {
		return nil, err
	}
	if !ts.Valid {
		return nil, nil
	}
	return ptrTime(ts.String), nil
}

// StalledRunningJobs returns RUNNING jobs whose last progress is older than
// cutoff, oldest first. Rows with a NULL last_progress_at (running jobs that
// predate the column) fall back to started_at so an upgrade does not exempt
// them forever. Pending/assigned jobs are never returned — the watchdog only
// fails jobs that actually started executing.
func (s *Store) StalledRunningJobs(ctx context.Context, cutoff time.Time) ([]*model.Job, error) {
	rows, err := s.qQuery(ctx, `SELECT id, series, episode, episode_dir, script_type, script_file, flow_id, status,
  node_id, step, progress, exit_code, error, log_tail, outputs_json, created_at, started_at, finished_at,
  full_log, step_timings_json, metrics_json, priority, retry_count, next_retry_at, last_failed_node_id
  FROM jobs WHERE status='running' AND COALESCE(last_progress_at, started_at) < ? ORDER BY id`, fmtTime(cutoff))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Job
	for rows.Next() {
		j, err := scanJobRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// FailStalledJob atomically fails a job ONLY IF it is still live
// (running/assigned) AND still stale (last progress at or before cutoff).
// It returns true when this call performed the transition, false when the
// guard matched zero rows — the job already completed, was cancelled, was
// failed by someone else, or made progress since the watchdog's snapshot.
//
// This is the watchdog's guard against the snapshot race: StalledRunningJobs
// is a point-in-time read, and the agent may complete the job before the
// watchdog acts on it. An unguarded finish would overwrite done→failed,
// wipe the outputs, and auto-retry a finished encode. The conditional
// UPDATE makes the transition atomic on BOTH backends (SQLite's single
// writer and PG's row lock serialize it), so concurrent watchdog passes /
// orphan recovery can never double-fail one job.
//
// Column set mirrors FinishJob's failure path (status/exit_code=-1/error/
// finished_at + the last_failed_node_id steering stamp for retry) but
// deliberately does NOT write outputs_json, log_tail or progress: a stalled
// job keeps whatever partial state it had, and a row that raced to done is
// never touched at all.
func (s *Store) FailStalledJob(ctx context.Context, id int64, cutoff time.Time, errMsg string) (bool, error) {
	res, err := s.qExec(ctx,
		`UPDATE jobs SET status='failed', exit_code=-1, error=?, finished_at=datetime('now'),
		  last_failed_node_id=(SELECT node_id FROM jobs WHERE id=?)
		 WHERE id=? AND status IN ('running','assigned') AND COALESCE(last_progress_at, started_at) <= ?`,
		errMsg, id, id, fmtTime(cutoff))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// CountStalledJobs counts failed jobs whose recorded error carries the
// watchdog's stall prefix — the DB-derived basis for the
// encode_jobs_stalled_total Prometheus counter (same derivation pattern as
// encode_jobs_failed_total: a count over the jobs table, not an in-memory
// tally, so every replica serves the same number).
//
// Known derivation limits (deliberate, no behavior change):
//   - Undercount on auto-retry: a stalled job that is re-queued and later
//     succeeds (or fails with a different error) no longer carries a
//     'stalled:' error on its row, so the counter reflects jobs CURRENTLY
//     failed-stalled, not the total number of stall events.
//   - The LIKE prefix is not exclusive to the watchdog: an agent-reported
//     error that happens to start with 'stalled:' also matches.
func (s *Store) CountStalledJobs(ctx context.Context) (int, error) {
	var n int
	err := s.qRow(ctx, `SELECT COUNT(*) FROM jobs WHERE status='failed' AND error LIKE 'stalled:%'`).Scan(&n)
	return n, err
}

// SeedJobLastProgress backdates a job's last_progress_at stamp, for tests
// that need a controlled stall age (the live paths stamp datetime('now'),
// which is uncontrolled). Exported so cross-package API tests can use it,
// mirroring SeedFinishedJob.
func (s *Store) SeedJobLastProgress(ctx context.Context, id int64, at time.Time) error {
	_, err := s.qExec(ctx, `UPDATE jobs SET last_progress_at=? WHERE id=?`, fmtTime(at), id)
	return err
}
