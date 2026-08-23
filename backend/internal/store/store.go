// Package store persists controller state (nodes, jobs, flows) in SQLite.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite: single writer avoids lock contention
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("set pragmas: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// migrate applies the schema. Idempotent via IF NOT EXISTS / user_version check.
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
  tasks_since_boot INTEGER NOT NULL DEFAULT 0,
  reboot_pending INTEGER NOT NULL DEFAULT 0,
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
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		if t2, err2 := time.Parse(time.RFC3339Nano, s); err2 == nil {
			return t2
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

// ---------- Nodes ----------

// CreateNode registers a node with a bcrypt hash of its token.
func (s *Store) CreateNode(ctx context.Context, name, tokenHash string) (*model.Node, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO nodes (name, token_hash) VALUES (?, ?)`, name, tokenHash)
	if err != nil {
		return nil, fmt.Errorf("create node: %w", err)
	}
	id, _ := res.LastInsertId()
	return s.GetNode(ctx, id)
}

// GetNode loads a node by ID.
func (s *Store) GetNode(ctx context.Context, id int64) (*model.Node, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, token_hash, enabled, status, agent_version,
  lib_version, tasks_since_boot, reboot_pending, last_seen, last_error, created_at FROM nodes WHERE id = ?`, id)
	return scanNode(row)
}

// NodeByName loads a node by unique name.
func (s *Store) NodeByName(ctx context.Context, name string) (*model.Node, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, token_hash, enabled, status, agent_version,
  lib_version, tasks_since_boot, reboot_pending, last_seen, last_error, created_at FROM nodes WHERE name = ?`, name)
	return scanNode(row)
}

func scanNode(row *sql.Row) (*model.Node, error) {
	var n model.Node
	var enabled, reboot int
	var lastSeen sql.NullString
	var createdAt string
	err := row.Scan(&n.ID, &n.Name, &n.TokenHash, &enabled, &n.Status, &n.AgentVersion,
		&n.LibVersion, &n.TasksSinceBoot, &reboot, &lastSeen, &n.LastError, &createdAt)
	if err != nil {
		return nil, err
	}
	n.Enabled = enabled == 1
	n.RebootPending = reboot == 1
	if lastSeen.Valid {
		n.LastSeen = parseTime(lastSeen.String)
	}
	n.CreatedAt = parseTime(createdAt)
	return &n, nil
}

// ListNodes returns all nodes ordered by name.
func (s *Store) ListNodes(ctx context.Context) ([]*model.Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, token_hash, enabled, status, agent_version,
  lib_version, tasks_since_boot, reboot_pending, last_seen, last_error, created_at FROM nodes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Node
	for rows.Next() {
		var n model.Node
		var enabled, reboot int
		var lastSeen sql.NullString
		var createdAt string
		if err := rows.Scan(&n.ID, &n.Name, &n.TokenHash, &enabled, &n.Status, &n.AgentVersion,
			&n.LibVersion, &n.TasksSinceBoot, &reboot, &lastSeen, &n.LastError, &createdAt); err != nil {
			return nil, err
		}
		n.Enabled = enabled == 1
		n.RebootPending = reboot == 1
		if lastSeen.Valid {
			n.LastSeen = parseTime(lastSeen.String)
		}
		n.CreatedAt = parseTime(createdAt)
		out = append(out, &n)
	}
	return out, rows.Err()
}

// UpdateNode persists mutable node fields after a heartbeat or UI action.
func (s *Store) UpdateNode(ctx context.Context, n *model.Node) error {
	_, err := s.db.ExecContext(ctx, `UPDATE nodes SET enabled=?, status=?, agent_version=?,
  lib_version=?, tasks_since_boot=?, reboot_pending=?, last_seen=?, last_error=? WHERE id=?`,
		boolToInt(n.Enabled), string(n.Status), n.AgentVersion, n.LibVersion, n.TasksSinceBoot,
		boolToInt(n.RebootPending), fmtTime(n.LastSeen), n.LastError, n.ID)
	return err
}

// NodeByTokenHash finds the node holding this token hash (auth lookup).
func (s *Store) NodeByTokenHash(ctx context.Context, hash string) (*model.Node, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, token_hash, enabled, status, agent_version,
  lib_version, tasks_since_boot, reboot_pending, last_seen, last_error, created_at FROM nodes WHERE token_hash = ?`, hash)
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
	id, _ := res.LastInsertId()
	return s.GetFlow(ctx, id)
}

// GetFlow loads a flow by ID.
func (s *Store) GetFlow(ctx context.Context, id int64) (*model.Flow, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, steps_json, created_at, updated_at FROM flows WHERE id = ?`, id)
	return scanFlow(row)
}

// FlowByName loads a flow by unique name.
func (s *Store) FlowByName(ctx context.Context, name string) (*model.Flow, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, steps_json, created_at, updated_at FROM flows WHERE name = ?`, name)
	return scanFlow(row)
}

func scanFlow(row *sql.Row) (*model.Flow, error) {
	var f model.Flow
	var stepsJSON string
	var createdAt, updatedAt string
	if err := row.Scan(&f.ID, &f.Name, &stepsJSON, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(stepsJSON), &f.Steps); err != nil {
		return nil, fmt.Errorf("unmarshal steps: %w", err)
	}
	f.CreatedAt = parseTime(createdAt)
	f.UpdatedAt = parseTime(updatedAt)
	return &f, nil
}

// ListFlows returns all flows ordered by name.
func (s *Store) ListFlows(ctx context.Context) ([]*model.Flow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, steps_json, created_at, updated_at FROM flows ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Flow
	for rows.Next() {
		var f model.Flow
		var stepsJSON string
		var createdAt, updatedAt string
		if err := rows.Scan(&f.ID, &f.Name, &stepsJSON, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(stepsJSON), &f.Steps); err != nil {
			return nil, err
		}
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

// DeleteFlow removes a flow; jobs referencing it keep their flow_id (rendered script already delivered).
func (s *Store) DeleteFlow(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM flows WHERE id = ?`, id)
	return err
}

// ---------- Jobs ----------

// CreateJob inserts a new pending job.
func (s *Store) CreateJob(ctx context.Context, j *model.Job) (*model.Job, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (series, episode, episode_dir, script_type, flow_id, status)
     VALUES (?, ?, ?, ?, ?, ?)`,
		j.Series, j.Episode, j.EpisodeDir, j.ScriptType, j.FlowID, string(model.JobPending))
	if err != nil {
		return nil, fmt.Errorf("create job: %w", err)
	}
	id, _ := res.LastInsertId()
	return s.GetJob(ctx, id)
}

// GetJob loads a job by ID.
func (s *Store) GetJob(ctx context.Context, id int64) (*model.Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, series, episode, episode_dir, script_type, flow_id, status,
  node_id, step, progress, exit_code, error, log_tail, outputs_json, created_at, started_at, finished_at
  FROM jobs WHERE id = ?`, id)
	return scanJob(row)
}

func scanJob(row *sql.Row) (*model.Job, error) {
	var j model.Job
	var status string
	var outputsJSON string
	var started, finished sql.NullString
	var createdAt string
	err := row.Scan(&j.ID, &j.Series, &j.Episode, &j.EpisodeDir, &j.ScriptType, &j.FlowID, &status,
		&j.NodeID, &j.Step, &j.Progress, &j.ExitCode, &j.Error, &j.LogTail, &outputsJSON,
		&createdAt, &started, &finished)
	if err != nil {
		return nil, err
	}
	j.CreatedAt = parseTime(createdAt)
	j.Status = model.JobStatus(status)
	_ = json.Unmarshal([]byte(outputsJSON), &j.Outputs)
	if started.Valid {
		j.StartedAt = parseTime(started.String)
	}
	if finished.Valid {
		j.FinishedAt = parseTime(finished.String)
	}
	return &j, nil
}

// ListJobs returns jobs, optionally filtered by status, newest first.
func (s *Store) ListJobs(ctx context.Context, status model.JobStatus, limit int) ([]*model.Job, error) {
	q := `SELECT id, series, episode, episode_dir, script_type, flow_id, status,
  node_id, step, progress, exit_code, error, log_tail, outputs_json, created_at, started_at, finished_at FROM jobs`
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
	var outputsJSON string
	var started, finished sql.NullString
	var createdAt string
	err := rows.Scan(&j.ID, &j.Series, &j.Episode, &j.EpisodeDir, &j.ScriptType, &j.FlowID, &status,
		&j.NodeID, &j.Step, &j.Progress, &j.ExitCode, &j.Error, &j.LogTail, &outputsJSON,
		&createdAt, &started, &finished)
	if err != nil {
		return nil, err
	}
	j.CreatedAt = parseTime(createdAt)
	j.Status = model.JobStatus(status)
	_ = json.Unmarshal([]byte(outputsJSON), &j.Outputs)
	if started.Valid {
		j.StartedAt = parseTime(started.String)
	}
	if finished.Valid {
		j.FinishedAt = parseTime(finished.String)
	}
	return &j, nil
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

	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET status='assigned', node_id=?, started_at=datetime('now') WHERE id=? AND status='pending'`,
		nodeID, jobID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET status='busy' WHERE id=?`, nodeID); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateJobStatus records progress from a heartbeat or completion report.
func (s *Store) UpdateJobStatus(ctx context.Context, id int64, status model.JobStatus, step string, progress float64, logTail string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status=?, step=?, progress=?, log_tail=? WHERE id=?`,
		string(status), step, progress, logTail, id)
	return err
}

// FinishJob marks a job done or failed with exit code, error, and outputs.
func (s *Store) FinishJob(ctx context.Context, id int64, status model.JobStatus, exitCode int, errMsg string, outputs []string, logTail string) error {
	if outputs == nil {
		outputs = []string{}
	}
	b, _ := json.Marshal(outputs)
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status=?, exit_code=?, error=?, outputs_json=?, log_tail=?, finished_at=datetime('now'), progress=100 WHERE id=?`,
		string(status), exitCode, errMsg, string(b), logTail, id)
	return err
}

// ActiveJobForNode returns the node's assigned/running job, if any.
func (s *Store) ActiveJobForNode(ctx context.Context, nodeID int64) (*model.Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, series, episode, episode_dir, script_type, flow_id, status,
  node_id, step, progress, exit_code, error, log_tail, outputs_json, created_at, started_at, finished_at
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

// RetryJob re-queues a failed/cancelled job as pending on no node.
func (s *Store) RetryJob(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status='pending', node_id=0, step='', progress=0, exit_code=0, error='', started_at=NULL, finished_at=NULL WHERE id=? AND status IN ('failed','cancelled','done')`,
		id)
	return err
}

// CountFinishedTasksForNode counts terminal jobs completed by a node — used
// only for display; the reboot counter itself comes from the agent heartbeat.
func (s *Store) CountFinishedTasksForNode(ctx context.Context, nodeID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE node_id = ? AND status = 'done'`, nodeID).Scan(&n)
	return n, err
}
