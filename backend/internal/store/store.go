// Package store persists controller state (nodes, jobs, flows) in SQLite.
package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/badskater/encode-system/backend/internal/model"
)

// Store wraps the SQLite database with typed operations.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and applies
// migrations. The pragmas keep SQLite safe for a single-writer controller.
//
// journal_mode is DELETE (rollback journal), NOT WAL: on the Docker host's
// Ceph-backed volume the WAL/shm path corrupts its read view — sessions
// written through the WAL become invisible to subsequent lookups
// (reproduced on a pristine DB with this exact binary; the incident of
// 2026-08-28 turned every login into a 401). The rollback journal uses no
// shm/mmap and is the safe choice for a single-writer, low-volume store.
func Open(path string) (*Store, error) {
	// The _pragma query params apply to EVERY connection the pool opens,
	// unlike a one-shot Exec PRAGMA which a replacement connection would lose.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(DELETE)&_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite: single writer avoids lock contention
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// migrate applies the schema v1. Idempotent via IF NOT EXISTS. When the
// schema changes, add an explicit versioned migration here.
func (s *Store) migrate() error {
	schema := `
CREATE TABLE IF NOT EXISTS nodes (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  token_hash TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  status TEXT NOT NULL DEFAULT 'idle',
  agent_version TEXT NOT NULL DEFAULT '',
  lib_version INTEGER NOT NULL DEFAULT 0,
  bin_version INTEGER NOT NULL DEFAULT 0,
  tasks_since_boot INTEGER NOT NULL DEFAULT 0,
  reboot_pending INTEGER NOT NULL DEFAULT 0,
  reboot_issued_at_tasks INTEGER NOT NULL DEFAULT 0,
  reboot_issued_at TEXT,
  last_seen TEXT,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS flows (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  steps_json TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS jobs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  series TEXT NOT NULL,
  episode TEXT NOT NULL,
  episode_dir TEXT NOT NULL,
  script_type TEXT NOT NULL,
  script_file TEXT NOT NULL DEFAULT '',
  flow_id INTEGER NOT NULL REFERENCES flows(id),
  status TEXT NOT NULL DEFAULT 'pending',
  node_id INTEGER NOT NULL DEFAULT 0,
  step TEXT NOT NULL DEFAULT '',
  progress REAL NOT NULL DEFAULT 0,
  exit_code INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  log_tail TEXT NOT NULL DEFAULT '',
  outputs_json TEXT NOT NULL DEFAULT '[]',
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  started_at TEXT,
  finished_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_jobs_episode_dir ON jobs(episode_dir);
CREATE INDEX IF NOT EXISTS idx_jobs_node_status ON jobs(node_id, status);
CREATE INDEX IF NOT EXISTS idx_nodes_token_hash ON nodes(token_hash);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if err := s.migrateExt(); err != nil {
		return err
	}
	if err := s.migrateAuth(context.Background()); err != nil {
		return err
	}
	// Schema v1 -> v1.1: add reboot_issued_at_tasks to databases created
	// before this column existed. Tolerant of the already-migrated case.
	for _, alt := range []string{
		`ALTER TABLE nodes ADD COLUMN reboot_issued_at_tasks INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE nodes ADD COLUMN reboot_issued_at TEXT`,
	} {
		if _, err := s.db.Exec(alt); err != nil {
			// duplicate column name => already migrated; anything else is fatal
			if !isDuplicateColumnErr(err) {
				return fmt.Errorf("migrate nodes: %w", err)
			}
		}
	}
	// Schema v2: observability/queue feature set — job logs/timings/
	// priority/retry, series notify, node metrics ring. Reuses the same
	// tolerant-ALTER pattern (duplicate-column = already applied).
	if err := s.migrateV2(); err != nil {
		return err
	}
	return nil
}

// migrateV2 applies the schema v2 additions. Each ALTER is tolerant of the
// already-migrated case (duplicate-column error) so opening an existing v2
// database is a no-op. The node_metrics table uses CREATE TABLE IF NOT EXISTS.
func (s *Store) migrateV2() error {
	// jobs: full_log (complete capture), step_timings_json (per-step
	// wall-clock samples), priority (queue dispatch weight), retry_count
	// (re-queue count for backoff), next_retry_at (backoff gate).
	jobAlters := []string{
		`ALTER TABLE jobs ADD COLUMN full_log TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE jobs ADD COLUMN step_timings_json TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE jobs ADD COLUMN priority INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE jobs ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE jobs ADD COLUMN next_retry_at TEXT`,
	}
	for _, alt := range jobAlters {
		if _, err := s.db.Exec(alt); err != nil {
			if !isDuplicateColumnErr(err) {
				return fmt.Errorf("migrate v2 jobs: %w", err)
			}
		}
	}
	// series: notify controls per-series Discord alerts. Defaults 1 (true)
	// so existing series keep alerting (pre-v2 behavior).
	if _, err := s.db.Exec(`ALTER TABLE series ADD COLUMN notify INTEGER NOT NULL DEFAULT 1`); err != nil {
		if !isDuplicateColumnErr(err) {
			return fmt.Errorf("migrate v2 series.notify: %w", err)
		}
	}
	// node_metrics: rolling resource samples (one row per heartbeat) for
	// the observability dashboard. Index on (node_id, ts) supports the
	// "latest N samples for node" query shape.
	metricsSchema := `
CREATE TABLE IF NOT EXISTS node_metrics (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  node_id INTEGER NOT NULL,
  ts TEXT NOT NULL,
  cpu_pct REAL NOT NULL DEFAULT 0,
  mem_used_mb INTEGER NOT NULL DEFAULT 0,
  mem_total_mb INTEGER NOT NULL DEFAULT 0,
  disk_free_gb INTEGER NOT NULL DEFAULT 0,
  gpu_util INTEGER NOT NULL DEFAULT -1,
  gpu_temp INTEGER NOT NULL DEFAULT -1,
  gpu_mem_used_mb INTEGER NOT NULL DEFAULT -1,
  encode_fps REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_metrics_node_ts ON node_metrics(node_id, ts);
`
	if _, err := s.db.Exec(metricsSchema); err != nil {
		return fmt.Errorf("migrate v2 node_metrics: %w", err)
	}
	// settings: the table is a single-row JSON blob (id, json, updated_at),
	// so drain_mode and notify_digest live as fields inside the marshaled
	// Settings struct — no SQL column is needed. Their defaults (false) are
	// the zero value of the bool fields, so old JSON rows that lack the keys
	// unmarshal cleanly to "off". SaveSettings/GetSettings round-trip them.
	return nil
}

// isDuplicateColumnErr reports the SQLite "duplicate column" error shape.
func isDuplicateColumnErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column")
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		if t2, err2 := time.Parse(time.RFC3339Nano, s); err2 == nil {
			return t2.UTC()
		}
		return time.Time{}
	}
	return t.UTC()
}

func fmtTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

// ptrTime parses a stored timestamp into a *time.Time, nil when unset — keeps
// unset timestamps out of API responses instead of emitting year-1 dates.
func ptrTime(s string) *time.Time {
	t := parseTime(s)
	if t.IsZero() {
		return nil
	}
	return &t
}

// fmtPtrTime writes a nullable timestamp back to the store.
func fmtPtrTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return fmtTime(*t)
}

// ---------- Nodes ----------

// CreateNode registers a node with a bcrypt hash of its token.
func (s *Store) CreateNode(ctx context.Context, name, tokenHash string) (*model.Node, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO nodes (name, token_hash) VALUES (?, ?)`, name, tokenHash)
	if err != nil {
		return nil, fmt.Errorf("create node: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("CreateNode: last insert id: %w", err)
	}
	return s.GetNode(ctx, id)
}

// GetNode loads a node by ID.
func (s *Store) GetNode(ctx context.Context, id int64) (*model.Node, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, token_hash, enabled, status, agent_version,
  lib_version, bin_version, tasks_since_boot, reboot_pending, reboot_issued_at_tasks, reboot_issued_at, last_seen, last_error, created_at FROM nodes WHERE id = ?`, id)
	return scanNode(row)
}

// NodeByName loads a node by unique name.
func (s *Store) NodeByName(ctx context.Context, name string) (*model.Node, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, token_hash, enabled, status, agent_version,
  lib_version, bin_version, tasks_since_boot, reboot_pending, reboot_issued_at_tasks, reboot_issued_at, last_seen, last_error, created_at FROM nodes WHERE name = ?`, name)
	return scanNode(row)
}

func scanNode(row *sql.Row) (*model.Node, error) {
	var n model.Node
	var enabled, reboot int
	var lastSeen, issuedAt sql.NullString
	var createdAt string
	err := row.Scan(&n.ID, &n.Name, &n.TokenHash, &enabled, &n.Status, &n.AgentVersion,
		&n.LibVersion, &n.BinVersion, &n.TasksSinceBoot, &reboot, &n.RebootIssuedAtTasks, &issuedAt, &lastSeen, &n.LastError, &createdAt)
	if err != nil {
		return nil, err
	}
	n.Enabled = enabled == 1
	n.RebootPending = reboot == 1
	if lastSeen.Valid {
		n.LastSeen = ptrTime(lastSeen.String)
	}
	if issuedAt.Valid {
		n.RebootIssuedAt = ptrTime(issuedAt.String)
	}
	n.CreatedAt = parseTime(createdAt)
	return &n, nil
}

// ListNodes returns all nodes ordered by name.
func (s *Store) ListNodes(ctx context.Context) ([]*model.Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, token_hash, enabled, status, agent_version,
  lib_version, bin_version, tasks_since_boot, reboot_pending, reboot_issued_at_tasks, reboot_issued_at, last_seen, last_error, created_at FROM nodes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Node
	for rows.Next() {
		var n model.Node
		var enabled, reboot int
		var lastSeen, issuedAt sql.NullString
		var createdAt string
		if err := rows.Scan(&n.ID, &n.Name, &n.TokenHash, &enabled, &n.Status, &n.AgentVersion,
			&n.LibVersion, &n.BinVersion, &n.TasksSinceBoot, &reboot, &n.RebootIssuedAtTasks, &issuedAt, &lastSeen, &n.LastError, &createdAt); err != nil {
			return nil, err
		}
		n.Enabled = enabled == 1
		n.RebootPending = reboot == 1
		if lastSeen.Valid {
			n.LastSeen = ptrTime(lastSeen.String)
		}
		if issuedAt.Valid {
			n.RebootIssuedAt = ptrTime(issuedAt.String)
		}
		n.CreatedAt = parseTime(createdAt)
		out = append(out, &n)
	}
	return out, rows.Err()
}

// UpdateNode persists mutable node fields after a heartbeat or UI action.
func (s *Store) UpdateNode(ctx context.Context, n *model.Node) error {
	_, err := s.db.ExecContext(ctx, `UPDATE nodes SET enabled=?, status=?, agent_version=?,
  lib_version=?, bin_version=?, tasks_since_boot=?, reboot_pending=?, reboot_issued_at_tasks=?, reboot_issued_at=?, last_seen=?, last_error=? WHERE id=?`,
		boolToInt(n.Enabled), string(n.Status), n.AgentVersion, n.LibVersion, n.BinVersion, n.TasksSinceBoot,
		boolToInt(n.RebootPending), n.RebootIssuedAtTasks, fmtPtrTime(n.RebootIssuedAt), fmtPtrTime(n.LastSeen), n.LastError, n.ID)
	return err
}

// DeleteNode removes a node row (used to roll back failed pairing).
func (s *Store) DeleteNode(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, id)
	return err
}

// NodeByTokenHash finds the node holding this token hash (auth lookup).
func (s *Store) NodeByTokenHash(ctx context.Context, hash string) (*model.Node, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, token_hash, enabled, status, agent_version,
  lib_version, bin_version, tasks_since_boot, reboot_pending, reboot_issued_at_tasks, reboot_issued_at, last_seen, last_error, created_at FROM nodes WHERE token_hash = ?`, hash)
	return scanNode(row)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------- Flows ----------

// CreateFlow inserts a new flow.
func (s *Store) CreateFlow(ctx context.Context, f *model.Flow) (*model.Flow, error) {
	b, err := json.Marshal(f.Steps)
	if err != nil {
		return nil, fmt.Errorf("marshal steps: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO flows (name, steps_json) VALUES (?, ?)`, f.Name, string(b))
	if err != nil {
		return nil, fmt.Errorf("create flow: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("CreateFlow: last insert id: %w", err)
	}
	return s.GetFlow(ctx, id)
}

// GetFlow loads a flow by ID.
func (s *Store) GetFlow(ctx context.Context, id int64) (*model.Flow, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, steps_json, is_default, created_at, updated_at FROM flows WHERE id = ?`, id)
	return scanFlow(row)
}

// FlowByName loads a flow by unique name.
func (s *Store) FlowByName(ctx context.Context, name string) (*model.Flow, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, steps_json, is_default, created_at, updated_at FROM flows WHERE name = ?`, name)
	return scanFlow(row)
}

func scanFlow(row *sql.Row) (*model.Flow, error) {
	var f model.Flow
	var stepsJSON string
	var isDefault int
	var createdAt, updatedAt string
	if err := row.Scan(&f.ID, &f.Name, &stepsJSON, &isDefault, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(stepsJSON), &f.Steps); err != nil {
		return nil, fmt.Errorf("unmarshal steps: %w", err)
	}
	f.IsDefault = isDefault == 1
	f.CreatedAt = parseTime(createdAt)
	f.UpdatedAt = parseTime(updatedAt)
	return &f, nil
}

// ListFlows returns all flows ordered by name.
func (s *Store) ListFlows(ctx context.Context) ([]*model.Flow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, steps_json, is_default, created_at, updated_at FROM flows ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Flow
	for rows.Next() {
		var f model.Flow
		var stepsJSON string
		var isDefault int
		var createdAt, updatedAt string
		if err := rows.Scan(&f.ID, &f.Name, &stepsJSON, &isDefault, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(stepsJSON), &f.Steps); err != nil {
			return nil, err
		}
		f.IsDefault = isDefault == 1
		f.CreatedAt = parseTime(createdAt)
		f.UpdatedAt = parseTime(updatedAt)
		out = append(out, &f)
	}
	return out, rows.Err()
}

// UpdateFlow replaces name/steps of an existing flow.
func (s *Store) UpdateFlow(ctx context.Context, f *model.Flow) error {
	b, err := json.Marshal(f.Steps)
	if err != nil {
		return fmt.Errorf("marshal steps: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `UPDATE flows SET name=?, steps_json=?, updated_at=datetime('now') WHERE id=?`,
		f.Name, string(b), f.ID)
	return err
}

// DeleteFlow removes a flow only when no job references it. The schema FK
// (jobs.flow_id REFERENCES flows(id)) enforces this; refusing up front gives
// an honest error instead of a constraint violation.
func (s *Store) DeleteFlow(ctx context.Context, id int64) error {
	var refs int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE flow_id = ?`, id).Scan(&refs); err != nil {
		return err
	}
	if refs > 0 {
		return fmt.Errorf("flow %d has %d job(s) referencing it", id, refs)
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM flows WHERE id = ?`, id)
	return err
}

// ---------- Jobs ----------

// CreateJob inserts a new pending job. Priority is honored so the queue can
// dispatch higher-priority jobs first; the other v2 fields (full_log,
// step_timings_json, retry_count, next_retry_at) default in the schema and
// are populated later by the completion/report paths.
func (s *Store) CreateJob(ctx context.Context, j *model.Job) (*model.Job, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (series, episode, episode_dir, script_type, script_file, flow_id, status, priority)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		j.Series, j.Episode, j.EpisodeDir, j.ScriptType, j.ScriptFile, j.FlowID, string(model.JobPending), j.Priority)
	if err != nil {
		return nil, fmt.Errorf("create job: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("CreateJob: last insert id: %w", err)
	}
	return s.GetJob(ctx, id)
}

// GetJob loads a job by ID.
func (s *Store) GetJob(ctx context.Context, id int64) (*model.Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, series, episode, episode_dir, script_type, script_file, flow_id, status,
  node_id, step, progress, exit_code, error, log_tail, outputs_json, created_at, started_at, finished_at,
  full_log, step_timings_json, priority, retry_count, next_retry_at
  FROM jobs WHERE id = ?`, id)
	return scanJob(row)
}

func scanJob(row *sql.Row) (*model.Job, error) {
	var j model.Job
	var status string
	var outputsJSON, timingsJSON string
	var started, finished, nextRetry sql.NullString
	var createdAt string
	err := row.Scan(&j.ID, &j.Series, &j.Episode, &j.EpisodeDir, &j.ScriptType, &j.ScriptFile, &j.FlowID, &status,
		&j.NodeID, &j.Step, &j.Progress, &j.ExitCode, &j.Error, &j.LogTail, &outputsJSON,
		&createdAt, &started, &finished,
		&j.FullLog, &timingsJSON, &j.Priority, &j.RetryCount, &nextRetry)
	if err != nil {
		return nil, err
	}
	j.CreatedAt = parseTime(createdAt)
	j.Status = model.JobStatus(status)
	if err := json.Unmarshal([]byte(outputsJSON), &j.Outputs); err != nil {
		return nil, fmt.Errorf("job %d: outputs_json: %w", j.ID, err)
	}
	if err := json.Unmarshal([]byte(timingsJSON), &j.StepTimings); err != nil {
		return nil, fmt.Errorf("job %d: step_timings_json: %w", j.ID, err)
	}
	if started.Valid {
		j.StartedAt = ptrTime(started.String)
	}
	if finished.Valid {
		j.FinishedAt = ptrTime(finished.String)
	}
	if nextRetry.Valid {
		j.NextRetryAt = ptrTime(nextRetry.String)
	}
	return &j, nil
}

// ListJobs returns jobs, optionally filtered by status, newest first.
func (s *Store) ListJobs(ctx context.Context, status model.JobStatus, limit int) ([]*model.Job, error) {
	q := `SELECT id, series, episode, episode_dir, script_type, script_file, flow_id, status,
  node_id, step, progress, exit_code, error, log_tail, outputs_json, created_at, started_at, finished_at,
  full_log, step_timings_json, priority, retry_count, next_retry_at FROM jobs`
	args := []any{}
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, string(status))
	}
	q += ` ORDER BY id DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Job
	for rows.Next() {
		row := rows
		j, err := scanJobRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// scanJobRow scans a job from a Rows-positioned row.
func scanJobRow(rows *sql.Rows) (*model.Job, error) {
	var j model.Job
	var status string
	var outputsJSON, timingsJSON string
	var started, finished, nextRetry sql.NullString
	var createdAt string
	err := rows.Scan(&j.ID, &j.Series, &j.Episode, &j.EpisodeDir, &j.ScriptType, &j.ScriptFile, &j.FlowID, &status,
		&j.NodeID, &j.Step, &j.Progress, &j.ExitCode, &j.Error, &j.LogTail, &outputsJSON,
		&createdAt, &started, &finished,
		&j.FullLog, &timingsJSON, &j.Priority, &j.RetryCount, &nextRetry)
	if err != nil {
		return nil, err
	}
	j.CreatedAt = parseTime(createdAt)
	j.Status = model.JobStatus(status)
	if err := json.Unmarshal([]byte(outputsJSON), &j.Outputs); err != nil {
		return nil, fmt.Errorf("job %d: outputs_json: %w", j.ID, err)
	}
	if err := json.Unmarshal([]byte(timingsJSON), &j.StepTimings); err != nil {
		return nil, fmt.Errorf("job %d: step_timings_json: %w", j.ID, err)
	}
	if started.Valid {
		j.StartedAt = ptrTime(started.String)
	}
	if finished.Valid {
		j.FinishedAt = ptrTime(finished.String)
	}
	if nextRetry.Valid {
		j.NextRetryAt = ptrTime(nextRetry.String)
	}
	return &j, nil
}

// SetJobFlow changes the flow of a pending job (guarded by the caller).
func (s *Store) SetJobFlow(ctx context.Context, id, flowID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET flow_id = ? WHERE id = ? AND status = 'pending'`, flowID, id)
	return err
}

// JobExistsForEpisode reports whether any job (non-cancelled) already covers this episode dir.
func (s *Store) JobExistsForEpisode(ctx context.Context, episodeDir string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE episode_dir = ? AND status != ?`, episodeDir, string(model.JobCancelled)).Scan(&n)
	return n > 0, err
}

// AssignJob atomically hands a pending job to a node. It fails if the node
// already has an active (assigned/running) job — the one-job-per-node rule.
func (s *Store) AssignJob(ctx context.Context, jobID, nodeID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var active int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE node_id = ? AND status IN ('assigned','running')`, nodeID).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return fmt.Errorf("node %d already has an active job", nodeID)
	}

	// The node must exist and be enabled; otherwise the job would be handed
	// to a box that never runs it and the node row could be wrongly flipped.
	var enabled int
	if err := tx.QueryRowContext(ctx, `SELECT enabled FROM nodes WHERE id = ?`, nodeID).Scan(&enabled); err != nil {
		return fmt.Errorf("assign to unknown node %d: %w", nodeID, err)
	}
	if enabled == 0 {
		return fmt.Errorf("node %d is disabled", nodeID)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE jobs SET status='assigned', node_id=?, started_at=datetime('now') WHERE id=? AND status='pending'`,
		nodeID, jobID)
	if err != nil {
		return err
	}
	// RowsAffected must be 1: a job that is no longer pending (race, retry
	// loop, bad id) matches zero rows, and marking the node busy anyway
	// would strand it in busy state with no job — the exact invariant this
	// function exists to protect.
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("job %d not pending (rows affected %d)", jobID, n)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET status='busy' WHERE id=?`, nodeID); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateJobStatus records progress from a heartbeat or completion report.
// Only live jobs (assigned/running) are updatable — a stale heartbeat must
// not resurrect a terminal job back into the queue.
func (s *Store) UpdateJobStatus(ctx context.Context, id int64, status model.JobStatus, step string, progress float64, logTail string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status=?, step=?, progress=?, log_tail=? WHERE id=? AND status IN ('assigned','running')`,
		string(status), step, progress, logTail, id)
	return err
}

// FinishJob marks a job done or failed with exit code, error, and outputs.
func (s *Store) FinishJob(ctx context.Context, id int64, status model.JobStatus, exitCode int, errMsg string, outputs []string, logTail string) error {
	if outputs == nil {
		outputs = []string{}
	}
	b, _ := json.Marshal(outputs)
	// progress=100 only for done jobs; a failed/cancelled job keeps its last
	// progress so dashboards don't render failure as completion.
	progress := ""
	if status == model.JobDone {
		progress = ", progress=100"
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status=?, exit_code=?, error=?, outputs_json=?, log_tail=?, finished_at=datetime('now')`+progress+` WHERE id=?`,
		string(status), exitCode, errMsg, string(b), logTail, id)
	return err
}

// maxFullLogBytes is the controller-side cap on a persisted full_log. The
// agent already caps its captureRunLog output at the same 1 MiB, but this is
// a defense-in-depth guard: a buggy or hostile POST (or a future caller that
// bypasses the agent path) must never write an unbounded log into the row
// and stall the single-writer SQLite store.
const maxFullLogBytes = 1 << 20 // 1 MiB

// truncationMarker is prepended when a full_log exceeds the cap so the UI can
// show that content was elided. Mirrors the agent's captureRunLog marker so
// both sides agree on shape — a capped log always starts with this prefix.
const truncationMarker = "[…truncated…]\n"

// capFullLog trims fullLog to at most maxFullLogBytes. If the input is within
// the cap it is returned verbatim. If it exceeds the cap, the content is cut
// at the first newline at/after len-1MiB (never splitting a line mid-way) and
// the truncation marker is prepended — the same rule as the agent's
// captureRunLog, so a controller-side cap produces the identical shape.
func capFullLog(fullLog string) string {
	if len(fullLog) <= maxFullLogBytes {
		return fullLog
	}
	data := []byte(fullLog)
	// Start the tail window at the first byte that would bring us under the
	// cap, then advance to the next newline so the body begins on a full line.
	cut := len(data) - maxFullLogBytes
	// Align the byte cut to a rune boundary: if it landed inside a multibyte
	// UTF-8 sequence, skip forward over the continuation bytes (0x80-0xBF) so
	// the truncated body never starts mid-rune. Encode logs carry CJK series
	// names, so a raw byte offset routinely splits a 3-byte rune and would
	// persist/serve invalid UTF-8. Advancing at most 3 bytes (a continuation
	// byte can never start a valid sequence); kept size only shrinks (≤ 1 MiB).
	for cut < len(data) && data[cut]&0xC0 == 0x80 {
		cut++
	}
	nl := bytes.IndexByte(data[cut:], '\n')
	if nl >= 0 {
		data = data[cut+nl+1:]
	} else {
		// No newline in the tail window: take the whole tail verbatim.
		data = data[cut:]
	}
	return truncationMarker + string(data)
}

// marshalStepTimings serializes the per-step timing slice to the JSON shape
// stored in step_timings_json. This is the single marshal contract for the
// column: a nil slice becomes the literal '[]' (never NULL or ”) so every
// read path unmarshals to a len-0 slice, and a marshal error — effectively
// impossible for this simple struct — degrades to '[]' rather than failing
// the finish over observability data.
func marshalStepTimings(timings []model.StepTiming) []byte {
	if timings == nil {
		return []byte("[]")
	}
	b, err := json.Marshal(timings)
	if err != nil {
		return []byte("[]")
	}
	return b
}

// FinishJobWithReport marks a job terminal and persists the agent's full
// completion report: the v1 finish columns (status/exit_code/error/outputs/
// log_tail/finished_at) PLUS the v2 observability fields (full_log +
// step_timings_json) the Phase-A agent report carries. It is the completion
// path for agents that report logs and timings; the older FinishJob remains
// for the two callers (orphan recovery, render-failed) that correctly pass
// no log. full_log is capped at 1 MiB via capFullLog; timings are marshaled
// via marshalStepTimings (the column's marshal contract).
func (s *Store) FinishJobWithReport(ctx context.Context, id int64, status model.JobStatus, exitCode int, errMsg string, outputs []string, logTail string, fullLog string, timings []model.StepTiming) error {
	if outputs == nil {
		outputs = []string{}
	}
	b, _ := json.Marshal(outputs)
	timingsJSON := marshalStepTimings(timings)
	capped := capFullLog(fullLog)
	// progress=100 only for done jobs; a failed/cancelled job keeps its last
	// progress so dashboards don't render failure as completion.
	progress := ""
	if status == model.JobDone {
		progress = ", progress=100"
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status=?, exit_code=?, error=?, outputs_json=?, log_tail=?, full_log=?, step_timings_json=?, finished_at=datetime('now')`+progress+` WHERE id=?`,
		string(status), exitCode, errMsg, string(b), logTail, capped, string(timingsJSON), id)
	return err
}

// ActiveJobForNode returns the node's assigned/running job, if any.
func (s *Store) ActiveJobForNode(ctx context.Context, nodeID int64) (*model.Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, series, episode, episode_dir, script_type, script_file, flow_id, status,
  node_id, step, progress, exit_code, error, log_tail, outputs_json, created_at, started_at, finished_at,
  full_log, step_timings_json, priority, retry_count, next_retry_at
  FROM jobs WHERE node_id = ? AND status IN ('assigned','running') ORDER BY id LIMIT 1`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if rows.Next() {
		return scanJobRow(rows)
	}
	return nil, nil
}

// ReleaseNode sets the node back to idle after a job finishes.
func (s *Store) ReleaseNode(ctx context.Context, nodeID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE nodes SET status='idle' WHERE id=?`, nodeID)
	return err
}

// CancelJob marks a pending/assigned job cancelled and returns rows affected.
// Terminal and running jobs are refused so the lifecycle cannot regress.
func (s *Store) CancelJob(ctx context.Context, id int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status='cancelled', finished_at=datetime('now') WHERE id=? AND status IN ('pending','assigned')`,
		id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RetryJob re-queues a failed/cancelled job as pending on no node. It returns
// the number of rows re-queued (0 when the job is not retryable, e.g. still
// running) so callers can distinguish success from a no-op. Stale v2 capture
// (full_log, step_timings_json, next_retry_at) is cleared alongside the v1
// state so a retry starts clean and is never blocked by a stale backoff gate
// once the queue gates dispatch on next_retry_at; retry_count is managed by
// the queue layer.
func (s *Store) RetryJob(ctx context.Context, id int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status='pending', node_id=0, step='', progress=0, exit_code=0, error='', log_tail='', outputs_json='[]', full_log='', step_timings_json='[]', started_at=NULL, finished_at=NULL, next_retry_at=NULL WHERE id=? AND status IN ('failed','cancelled','done')`,
		id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountFinishedTasksForNode counts terminal jobs completed by a node — used
// only for display; the reboot counter itself comes from the agent heartbeat.
func (s *Store) CountFinishedTasksForNode(ctx context.Context, nodeID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE node_id = ? AND status = 'done'`, nodeID).Scan(&n)
	return n, err
}
