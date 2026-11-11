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

// CountStalledJobs counts failed jobs whose recorded error carries the
// watchdog's stall prefix — the DB-derived basis for the
// encode_jobs_stalled_total Prometheus counter (same derivation pattern as
// encode_jobs_failed_total: a count over the jobs table, not an in-memory
// tally, so every replica serves the same number).
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
