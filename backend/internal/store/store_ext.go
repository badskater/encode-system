package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// migrateExt applies the phase-2 schema: flows.is_default, series,
// step_templates, pairing_codes. Tolerant of databases created by the
// original schema (ALTERs) and of fresh databases (CREATE TABLE).
func (s *Store) migrateExt() error {
	schema := `
CREATE TABLE IF NOT EXISTS series (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  flow_id INTEGER NOT NULL DEFAULT 0,
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS step_templates (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  key TEXT NOT NULL UNIQUE,
  label TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  params_json TEXT NOT NULL DEFAULT '[]',
  powershell TEXT NOT NULL,
  builtin INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS pairing_codes (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  code_hash TEXT NOT NULL UNIQUE,
  name_hint TEXT NOT NULL DEFAULT '',
  expires_at TEXT NOT NULL,
  used_by INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS settings (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  json TEXT NOT NULL,
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS provision_runs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  host TEXT NOT NULL,
  port INTEGER NOT NULL DEFAULT 5985,
  scheme TEXT NOT NULL DEFAULT 'http',
  winrm_user TEXT NOT NULL DEFAULT 'Administrator',
  node_name TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'queued',
  options_json TEXT NOT NULL DEFAULT '{}',
  log TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  finished_at TEXT
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate ext schema: %w", err)
	}
	// flows.is_default for databases created before phase 2.
	if _, err := s.db.Exec(`ALTER TABLE flows ADD COLUMN is_default INTEGER NOT NULL DEFAULT 0`); err != nil {
		if !isDuplicateColumnErr(err) {
			return fmt.Errorf("migrate flows.is_default: %w", err)
		}
	}
	// nodes.bin_version tracks the deployed bin-package version.
	if _, err := s.db.Exec(`ALTER TABLE nodes ADD COLUMN bin_version INTEGER NOT NULL DEFAULT 0`); err != nil {
		if !isDuplicateColumnErr(err) {
			return fmt.Errorf("migrate nodes.bin_version: %w", err)
		}
	}
	return nil
}

// ---------- Settings ----------

// GetSettings loads the persisted settings row. Returns nil (not an error)
// when no row exists yet — callers merge environment defaults in that case.
func (s *Store) GetSettings(ctx context.Context) (*model.Settings, error) {
	row := s.db.QueryRowContext(ctx, `SELECT json, updated_at FROM settings WHERE id = 1`)
	var js, updatedAt string
	if err := row.Scan(&js, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	var st model.Settings
	if err := json.Unmarshal([]byte(js), &st); err != nil {
		return nil, fmt.Errorf("unmarshal settings: %w", err)
	}
	st.UpdatedAt = ptrTime(updatedAt)
	return &st, nil
}

// ---------- Provision runs ----------

// CreateProvisionRun inserts a new run row and returns it with its ID.
func (s *Store) CreateProvisionRun(ctx context.Context, pr *model.ProvisionRun) (*model.ProvisionRun, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO provision_runs (host, port, scheme, winrm_user, node_name, status, options_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		pr.Host, pr.Port, pr.Scheme, pr.WinRMUser, pr.NodeName, pr.Status, pr.OptionsJSON)
	if err != nil {
		return nil, fmt.Errorf("create provision run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.GetProvisionRun(ctx, id)
}

// GetProvisionRun loads one run (log included — callers tail it).
func (s *Store) GetProvisionRun(ctx context.Context, id int64) (*model.ProvisionRun, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, host, port, scheme, winrm_user, node_name, status, options_json, log, error, created_at, finished_at
		 FROM provision_runs WHERE id = ?`, id)
	var pr model.ProvisionRun
	var createdAt string
	var finishedAt sql.NullString
	err := row.Scan(&pr.ID, &pr.Host, &pr.Port, &pr.Scheme, &pr.WinRMUser, &pr.NodeName,
		&pr.Status, &pr.OptionsJSON, &pr.Log, &pr.Error, &createdAt, &finishedAt)
	if err != nil {
		return nil, err
	}
	pr.CreatedAt = parseTime(createdAt)
	if finishedAt.Valid {
		pr.FinishedAt = ptrTime(finishedAt.String)
	}
	return &pr, nil
}

// ListProvisionRuns returns runs newest-first (the UI shows the recent ones).
func (s *Store) ListProvisionRuns(ctx context.Context) ([]*model.ProvisionRun, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, host, port, scheme, winrm_user, node_name, status, error, created_at, finished_at
		 FROM provision_runs ORDER BY id DESC LIMIT 25`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ProvisionRun
	for rows.Next() {
		var pr model.ProvisionRun
		var createdAt string
		var finishedAt sql.NullString
		if err := rows.Scan(&pr.ID, &pr.Host, &pr.Port, &pr.Scheme, &pr.WinRMUser, &pr.NodeName,
			&pr.Status, &pr.Error, &createdAt, &finishedAt); err != nil {
			return nil, err
		}
		pr.CreatedAt = parseTime(createdAt)
		if finishedAt.Valid {
			pr.FinishedAt = ptrTime(finishedAt.String)
		}
		out = append(out, &pr)
	}
	return out, rows.Err()
}

// SetProvisionRunStatus updates status + optional error/finished markers.
func (s *Store) SetProvisionRunStatus(ctx context.Context, id int64, status, errMsg string, finished bool) error {
	if finished {
		_, err := s.db.ExecContext(ctx,
			`UPDATE provision_runs SET status = ?, error = ?, finished_at = datetime('now') WHERE id = ?`,
			status, errMsg, id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE provision_runs SET status = ? WHERE id = ?`, status, id)
	return err
}

// UpgradeStepTemplateIfFactory replaces a builtin template's factory version
// with a new one — but ONLY when the stored script still matches the old
// factory version byte-for-byte. A template the user edited in the UI never
// matches, so customizations survive factory upgrades. Returns whether the
// upgrade was applied.
func (s *Store) UpgradeStepTemplateIfFactory(ctx context.Context, key, oldPowerShell string, t *model.StepTemplate) (bool, error) {
	if t.Params == nil {
		t.Params = []model.ParamDef{}
	}
	pb, err := json.Marshal(t.Params)
	if err != nil {
		return false, fmt.Errorf("marshal params: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE step_templates
		 SET label = ?, description = ?, params_json = ?, powershell = ?, updated_at = datetime('now')
		 WHERE key = ? AND builtin = 1 AND powershell = ?`,
		t.Label, t.Description, string(pb), t.PowerShell, key, oldPowerShell)
	if err != nil {
		return false, fmt.Errorf("upgrade step template: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// PruneProvisionRuns keeps only the most recent keep rows. Each row carries
// a full run log, so the table must not grow without bound across the
// controller's lifetime. Finished runs are pruned first (never a live one).
func (s *Store) PruneProvisionRuns(ctx context.Context, keep int) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM provision_runs
		WHERE id NOT IN (SELECT id FROM provision_runs ORDER BY id DESC LIMIT ?)
		  AND status IN ('success', 'failed')`, keep)
	return err
}

// DeleteNodeIfNotBusy removes a node only if it is not busy, atomically.
// Returns rows affected: 0 means the node is busy (or gone) — the caller
// must not proceed, otherwise a job dispatched between check and delete
// would be orphaned mid-encode (the TOCTOU the old check-then-act had).
func (s *Store) DeleteNodeIfNotBusy(ctx context.Context, id int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM nodes WHERE id = ? AND status != 'busy'`, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AppendProvisionRunLog atomically appends output to the run's log column.
// SQLite's || concat handles the append; the UI tails via GetProvisionRun.
// When capBytes > 0 the persisted log is bounded to the last capBytes bytes
// (substr with a negative offset keeps the tail — where the failure is) so a
// long run cannot grow the row without limit.
func (s *Store) AppendProvisionRunLog(ctx context.Context, id int64, chunk string, capBytes int) error {
	if capBytes > 0 {
		_, err := s.db.ExecContext(ctx,
			`UPDATE provision_runs SET log = substr(log || ?, -?) WHERE id = ?`, chunk, capBytes, id)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE provision_runs SET log = log || ? WHERE id = ?`, chunk, id)
	return err
}

// SaveSettings persists the settings row (single-row upsert on id=1).
func (s *Store) SaveSettings(ctx context.Context, st *model.Settings) error {
	b, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO settings (id, json, updated_at) VALUES (1, ?, datetime('now'))
		 ON CONFLICT(id) DO UPDATE SET json = excluded.json, updated_at = excluded.updated_at`,
		string(b))
	return err
}

// ---------- Series ----------

// UpsertSeriesByName returns the series row for name, creating it enabled
// with no flow override when first seen. Existing rows are returned as-is.
func (s *Store) UpsertSeriesByName(ctx context.Context, name string) (*model.Series, error) {
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO series (name) VALUES (?)`, name); err != nil {
		return nil, fmt.Errorf("upsert series: %w", err)
	}
	return s.SeriesByName(ctx, name)
}

// SeriesByName loads a series by exact folder name.
func (s *Store) SeriesByName(ctx context.Context, name string) (*model.Series, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, flow_id, enabled, created_at, updated_at FROM series WHERE name = ?`, name)
	return scanSeries(row)
}

// GetSeries loads a series by ID.
func (s *Store) GetSeries(ctx context.Context, id int64) (*model.Series, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, flow_id, enabled, created_at, updated_at FROM series WHERE id = ?`, id)
	return scanSeries(row)
}

func scanSeries(row *sql.Row) (*model.Series, error) {
	var sr model.Series
	var enabled int
	var createdAt, updatedAt string
	if err := row.Scan(&sr.ID, &sr.Name, &sr.FlowID, &enabled, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	sr.Enabled = enabled == 1
	sr.CreatedAt = parseTime(createdAt)
	sr.UpdatedAt = parseTime(updatedAt)
	return &sr, nil
}

// ListSeries returns all series ordered by name.
func (s *Store) ListSeries(ctx context.Context) ([]*model.Series, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, flow_id, enabled, created_at, updated_at FROM series ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Series
	for rows.Next() {
		var sr model.Series
		var enabled int
		var createdAt, updatedAt string
		if err := rows.Scan(&sr.ID, &sr.Name, &sr.FlowID, &enabled, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		sr.Enabled = enabled == 1
		sr.CreatedAt = parseTime(createdAt)
		sr.UpdatedAt = parseTime(updatedAt)
		out = append(out, &sr)
	}
	return out, rows.Err()
}

// UpdateSeries persists flow selection and enabled state.
func (s *Store) UpdateSeries(ctx context.Context, sr *model.Series) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE series SET flow_id=?, enabled=?, updated_at=datetime('now') WHERE id=?`,
		sr.FlowID, boolToInt(sr.Enabled), sr.ID)
	return err
}

// SetSeriesFlow updates ONLY the flow selection — field-scoped SQL so a
// concurrent enable/disable patch cannot be clobbered by a stale full-row
// read-modify-write.
func (s *Store) SetSeriesFlow(ctx context.Context, id, flowID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE series SET flow_id=?, updated_at=datetime('now') WHERE id=?`, flowID, id)
	return err
}

// SetSeriesEnabled updates ONLY the enabled flag (same rationale).
func (s *Store) SetSeriesEnabled(ctx context.Context, id int64, enabled bool) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE series SET enabled=?, updated_at=datetime('now') WHERE id=?`, boolToInt(enabled), id)
	return err
}

// ---------- Flows: default management ----------

// DefaultFlow returns the flow marked default, or an error when none exists.
func (s *Store) DefaultFlow(ctx context.Context) (*model.Flow, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, steps_json, is_default, created_at, updated_at FROM flows WHERE is_default = 1 LIMIT 1`)
	return scanFlowV2(row)
}

// SetDefaultFlow marks one flow default and clears all others, atomically.
func (s *Store) SetDefaultFlow(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM flows WHERE id = ?`, id).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("flow %d not found", id)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE flows SET is_default = 0`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE flows SET is_default = 1 WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// scanFlowV2 reads a flow row including is_default.
func scanFlowV2(row *sql.Row) (*model.Flow, error) {
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

// ---------- Step templates ----------

// InsertStepTemplateIfAbsent creates a template only when its key is not yet
// in the store. Boot seeding uses this so edits made through the UI survive
// controller restarts (re-seeding only adds brand-new templates).
func (s *Store) InsertStepTemplateIfAbsent(ctx context.Context, t *model.StepTemplate) (*model.StepTemplate, error) {
	if t.Params == nil {
		t.Params = []model.ParamDef{}
	}
	pb, err := json.Marshal(t.Params)
	if err != nil {
		return nil, fmt.Errorf("marshal params: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO step_templates (key, label, description, params_json, powershell, builtin)
     VALUES (?, ?, ?, ?, ?, ?)
     ON CONFLICT(key) DO NOTHING`,
		t.Key, t.Label, t.Description, string(pb), t.PowerShell, boolToInt(t.Builtin))
	if err != nil {
		return nil, fmt.Errorf("insert step template: %w", err)
	}
	return s.StepTemplateByKey(ctx, t.Key)
}

// UpsertStepTemplate inserts a template by key or updates it when present.
// Used by the UI for edits and custom templates; boot seeding uses
// InsertStepTemplateIfAbsent instead to preserve those edits.
func (s *Store) UpsertStepTemplate(ctx context.Context, t *model.StepTemplate) (*model.StepTemplate, error) {
	if t.Params == nil {
		t.Params = []model.ParamDef{} // never persist "null" — schema default is '[]'
	}
	pb, err := json.Marshal(t.Params)
	if err != nil {
		return nil, fmt.Errorf("marshal params: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO step_templates (key, label, description, params_json, powershell, builtin)
     VALUES (?, ?, ?, ?, ?, ?)
     ON CONFLICT(key) DO UPDATE SET label=excluded.label, description=excluded.description,
       params_json=excluded.params_json, powershell=excluded.powershell, updated_at=datetime('now')`,
		t.Key, t.Label, t.Description, string(pb), t.PowerShell, boolToInt(t.Builtin)); err != nil {
		return nil, fmt.Errorf("upsert step template: %w", err)
	}
	return s.StepTemplateByKey(ctx, t.Key)
}

// StepTemplateByKey loads a template by unique key.
func (s *Store) StepTemplateByKey(ctx context.Context, key string) (*model.StepTemplate, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, key, label, description, params_json, powershell, builtin, created_at, updated_at
     FROM step_templates WHERE key = ?`, key)
	return scanTemplate(row)
}

// GetStepTemplate loads a template by ID.
func (s *Store) GetStepTemplate(ctx context.Context, id int64) (*model.StepTemplate, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, key, label, description, params_json, powershell, builtin, created_at, updated_at
     FROM step_templates WHERE id = ?`, id)
	return scanTemplate(row)
}

func scanTemplate(row *sql.Row) (*model.StepTemplate, error) {
	var t model.StepTemplate
	var paramsJSON string
	var builtin int
	var createdAt, updatedAt string
	if err := row.Scan(&t.ID, &t.Key, &t.Label, &t.Description, &paramsJSON, &t.PowerShell,
		&builtin, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(paramsJSON), &t.Params); err != nil {
		return nil, fmt.Errorf("unmarshal params: %w", err)
	}
	t.Builtin = builtin == 1
	t.CreatedAt = parseTime(createdAt)
	t.UpdatedAt = parseTime(updatedAt)
	return &t, nil
}

// ListStepTemplates returns all templates ordered by key.
func (s *Store) ListStepTemplates(ctx context.Context) ([]*model.StepTemplate, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, key, label, description, params_json, powershell, builtin, created_at, updated_at
     FROM step_templates ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.StepTemplate
	for rows.Next() {
		var t model.StepTemplate
		var paramsJSON string
		var builtin int
		var createdAt, updatedAt string
		if err := rows.Scan(&t.ID, &t.Key, &t.Label, &t.Description, &paramsJSON, &t.PowerShell,
			&builtin, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(paramsJSON), &t.Params); err != nil {
			return nil, err
		}
		t.Builtin = builtin == 1
		t.CreatedAt = parseTime(createdAt)
		t.UpdatedAt = parseTime(updatedAt)
		out = append(out, &t)
	}
	return out, rows.Err()
}

// DeleteStepTemplate removes a custom template. Built-ins and templates
// referenced by flows are refused.
func (s *Store) DeleteStepTemplate(ctx context.Context, id int64) error {
	t, err := s.GetStepTemplate(ctx, id)
	if err != nil {
		return fmt.Errorf("template %d: %w", id, err)
	}
	if t.Builtin {
		return fmt.Errorf("built-in template %q cannot be deleted", t.Key)
	}
	refs, err := s.flowRefsTemplate(ctx, t.Key)
	if err != nil {
		return err
	}
	if refs > 0 {
		return fmt.Errorf("template %q is used by %d flow(s)", t.Key, refs)
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM step_templates WHERE id = ?`, id)
	return err
}

// flowRefsTemplate counts flows whose step list references the template key.
func (s *Store) flowRefsTemplate(ctx context.Context, key string) (int, error) {
	flows, err := s.ListFlows(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, f := range flows {
		for _, st := range f.Steps {
			if st.TemplateKey() == key {
				n++
				break
			}
		}
	}
	return n, nil
}

// ---------- Pairing codes ----------

// CreatePairingCode stores a hashed one-shot code expiring after ttl.
func (s *Store) CreatePairingCode(ctx context.Context, codeHash, nameHint string, ttl time.Duration) (*model.PairingCode, error) {
	exp := time.Now().UTC().Add(ttl)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO pairing_codes (code_hash, name_hint, expires_at) VALUES (?, ?, ?)`,
		codeHash, nameHint, fmtTime(exp))
	if err != nil {
		return nil, fmt.Errorf("create pairing code: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.GetPairingCode(ctx, id)
}

// GetPairingCode loads one code by ID.
func (s *Store) GetPairingCode(ctx context.Context, id int64) (*model.PairingCode, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, code_hash, name_hint, expires_at, used_by, created_at FROM pairing_codes WHERE id = ?`, id)
	return scanPairing(row)
}

func scanPairing(row *sql.Row) (*model.PairingCode, error) {
	var p model.PairingCode
	var expires, createdAt string
	if err := row.Scan(&p.ID, &p.CodeHash, &p.NameHint, &expires, &p.UsedBy, &createdAt); err != nil {
		return nil, err
	}
	p.ExpiresAt = parseTime(expires)
	p.CreatedAt = parseTime(createdAt)
	return &p, nil
}

// PeekPairingCode validates a code without consuming it: unknown/expired/
// used codes are reported distinctly, and transient DB errors propagate as
// such instead of being disguised as auth failures.
func (s *Store) PeekPairingCode(ctx context.Context, codeHash string) (*model.PairingCode, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, code_hash, name_hint, expires_at, used_by, created_at FROM pairing_codes WHERE code_hash = ?`,
		codeHash)
	p, err := scanPairing(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("unknown pairing code")
	}
	if err != nil {
		return nil, fmt.Errorf("read pairing code: %w", err)
	}
	if p.UsedBy != 0 {
		return nil, fmt.Errorf("pairing code already used")
	}
	if time.Now().UTC().After(p.ExpiresAt) {
		return nil, fmt.Errorf("pairing code expired")
	}
	return p, nil
}

// ConsumePairingCode atomically marks a code used by nodeID. It fails when
// the code is unknown, expired, or already consumed (one-shot semantics).
func (s *Store) ConsumePairingCode(ctx context.Context, codeHash string, nodeID int64) (*model.PairingCode, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var p model.PairingCode
	var expires, createdAt string
	err = tx.QueryRowContext(ctx,
		`SELECT id, code_hash, name_hint, expires_at, used_by, created_at FROM pairing_codes WHERE code_hash = ?`,
		codeHash).Scan(&p.ID, &p.CodeHash, &p.NameHint, &expires, &p.UsedBy, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("unknown pairing code")
	}
	if err != nil {
		return nil, fmt.Errorf("read pairing code: %w", err)
	}
	p.ExpiresAt = parseTime(expires)
	p.CreatedAt = parseTime(createdAt)

	if p.UsedBy != 0 {
		return nil, fmt.Errorf("pairing code already used")
	}
	if time.Now().UTC().After(p.ExpiresAt) {
		return nil, fmt.Errorf("pairing code expired")
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE pairing_codes SET used_by = ? WHERE id = ?`, nodeID, p.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	p.UsedBy = nodeID
	return &p, nil
}

// ListPairingCodes returns active (unused, unexpired) codes for the UI.
func (s *Store) ListPairingCodes(ctx context.Context) ([]*model.PairingCode, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, code_hash, name_hint, expires_at, used_by, created_at FROM pairing_codes
     WHERE used_by = 0 AND expires_at > ? ORDER BY id DESC`, fmtTime(time.Now().UTC()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.PairingCode
	for rows.Next() {
		var p model.PairingCode
		var expires, createdAt string
		if err := rows.Scan(&p.ID, &p.CodeHash, &p.NameHint, &expires, &p.UsedBy, &createdAt); err != nil {
			return nil, err
		}
		p.ExpiresAt = parseTime(expires)
		p.CreatedAt = parseTime(createdAt)
		out = append(out, &p)
	}
	return out, rows.Err()
}

// ---------- Users & sessions (management-plane login) ----------

func (s *Store) migrateAuth(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	username TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	role TEXT NOT NULL DEFAULT 'admin',
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
`)
	return err
}

// CreateUser inserts a user with a pre-hashed (bcrypt) password.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash, role string) (*model.User, error) {
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO users(username, password_hash, role, created_at) VALUES (?, ?, ?, datetime('now'))",
		username, passwordHash, role)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &model.User{ID: id, Username: username, Role: role, PasswordHash: passwordHash}, nil
}

// UserByUsername loads one account (nil when absent).
func (s *Store) UserByUsername(ctx context.Context, username string) (*model.User, error) {
	var u model.User
	var created sql.NullString
	err := s.db.QueryRowContext(ctx,
		"SELECT id, username, password_hash, role, created_at FROM users WHERE username = ?", username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &created)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt = ptrTime(created.String)
	return &u, nil
}

// Users lists accounts (no hashes).
func (s *Store) Users(ctx context.Context) ([]*model.User, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, username, role, created_at FROM users ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.User{}
	for rows.Next() {
		var u model.User
		var created sql.NullString
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &created); err != nil {
			return nil, err
		}
		u.CreatedAt = ptrTime(created.String)
		out = append(out, &u)
	}
	return out, rows.Err()
}

// CreateSession stores a hashed session token with its expiry.
func (s *Store) CreateSession(ctx context.Context, tokenHash string, userID int64, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO sessions(token_hash, user_id, created_at, expires_at) VALUES (?, ?, datetime('now'), ?)",
		tokenHash, userID, fmtTime(expiresAt))
	return err
}

// SessionByTokenHash loads an UNEXPIRED session joined with its user.
func (s *Store) SessionByTokenHash(ctx context.Context, tokenHash string) (*model.Session, error) {
	var sess model.Session
	var created, expires sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT s.token_hash, s.user_id, u.username, s.created_at, s.expires_at
FROM sessions s JOIN users u ON u.id = s.user_id
WHERE s.token_hash = ? AND s.expires_at > datetime('now')`, tokenHash).
		Scan(&sess.TokenHash, &sess.UserID, &sess.Username, &created, &expires)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sess.CreatedAt = ptrTime(created.String)
	sess.ExpiresAt = ptrTime(expires.String)
	return &sess, nil
}

// SlideSession extends an active session's expiry.
func (s *Store) SlideSession(ctx context.Context, tokenHash string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE sessions SET expires_at = ? WHERE token_hash = ?",
		fmtTime(expiresAt), tokenHash)
	return err
}

// DeleteSession revokes one session (logout).
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash = ?", tokenHash)
	return err
}

// PruneSessions removes expired sessions.
func (s *Store) PruneSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= datetime('now')")
	return err
}

// UpdateUserPassword replaces a user's bcrypt hash.
func (s *Store) UpdateUserPassword(ctx context.Context, userID int64, passwordHash string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE users SET password_hash = ? WHERE id = ?", passwordHash, userID)
	return err
}

// DeleteUserSessions revokes every session of a user except the one
// identified by keepTokenHash (the session performing the action). Used on
// password change so stale or stolen sessions die immediately.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64, keepTokenHash string) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?", userID, keepTokenHash)
	return err
}
