package store

import (
	"context"
	"fmt"
	"strings"
)

// pgTimestampDefault is the Postgres column DEFAULT that produces a TEXT
// timestamp in the exact "YYYY-MM-DD HH:MM:SS" UTC format SQLite's
// datetime('now') produces — so every scan path (parseTime, which parses
// "2006-01-02 15:04:05") works identically on both backends.
const pgTimestampDefault = "to_char(now() AT TIME ZONE 'utc', 'YYYY-MM-DD HH24:MI:SS')"

// pgSchemaTemplate is the full Postgres schema. Every %s is replaced with
// pgTimestampDefault by migratePG. The schema mirrors the SQLite end-state
// AFTER all migrations complete (not the ALTER chain).
//
// Key type mappings (all chosen so scan code is unchanged on PG):
//   - INTEGER PRIMARY KEY AUTOINCREMENT → BIGSERIAL PRIMARY KEY
//   - TEXT columns stay TEXT (timestamps included — NOT timestamptz, by
//     design: keeping TEXT lets fmtTime/parseTime and every scan path run
//     unchanged on both backends)
//   - INTEGER/SMALLINT 0/1 for booleans (SQLite has no bool type; PG mirrors
//     this so boolToInt/==1 scan code is identical)
//   - REAL → DOUBLE PRECISION
//   - DEFAULT (datetime('now')) → DEFAULT to_char(now() AT TIME ZONE 'utc', ...)
const pgSchemaTemplate = `
-- Bookkeeping: tracks which schema version this database is on.
CREATE TABLE IF NOT EXISTS schema_migrations (
  name TEXT PRIMARY KEY,
  applied_at TEXT NOT NULL DEFAULT %s
);

CREATE TABLE IF NOT EXISTS nodes (
  id BIGSERIAL PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  token_hash TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  status TEXT NOT NULL DEFAULT 'idle',
  "group" TEXT NOT NULL DEFAULT '',
  max_concurrent_jobs INTEGER NOT NULL DEFAULT 1,
  agent_version TEXT NOT NULL DEFAULT '',
  lib_version INTEGER NOT NULL DEFAULT 0,
  bin_version INTEGER NOT NULL DEFAULT 0,
  tasks_since_boot INTEGER NOT NULL DEFAULT 0,
  reboot_pending INTEGER NOT NULL DEFAULT 0,
  reboot_issued_at_tasks INTEGER NOT NULL DEFAULT 0,
  reboot_issued_at TEXT,
  last_seen TEXT,
  last_error TEXT NOT NULL DEFAULT '',
  agent_log TEXT NOT NULL DEFAULT '',
  platform TEXT NOT NULL DEFAULT 'windows',
  created_at TEXT NOT NULL DEFAULT %s
);
CREATE INDEX IF NOT EXISTS idx_nodes_token_hash ON nodes(token_hash);

CREATE TABLE IF NOT EXISTS flows (
  id BIGSERIAL PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  steps_json TEXT NOT NULL,
  is_default INTEGER NOT NULL DEFAULT 0,
  options_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL DEFAULT %s,
  updated_at TEXT NOT NULL DEFAULT %s
);

CREATE TABLE IF NOT EXISTS jobs (
  id BIGSERIAL PRIMARY KEY,
  series TEXT NOT NULL,
  episode TEXT NOT NULL,
  episode_dir TEXT NOT NULL,
  script_type TEXT NOT NULL,
  script_file TEXT NOT NULL DEFAULT '',
  flow_id BIGINT NOT NULL REFERENCES flows(id),
  status TEXT NOT NULL DEFAULT 'pending',
  node_id BIGINT NOT NULL DEFAULT 0,
  step TEXT NOT NULL DEFAULT '',
  progress DOUBLE PRECISION NOT NULL DEFAULT 0,
  exit_code INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  log_tail TEXT NOT NULL DEFAULT '',
  outputs_json TEXT NOT NULL DEFAULT '[]',
  full_log TEXT NOT NULL DEFAULT '',
  step_timings_json TEXT NOT NULL DEFAULT '[]',
  metrics_json TEXT NOT NULL DEFAULT '{}',
  priority INTEGER NOT NULL DEFAULT 0,
  retry_count INTEGER NOT NULL DEFAULT 0,
  next_retry_at TEXT,
  last_failed_node_id BIGINT NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT %s,
  started_at TEXT,
  finished_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_jobs_episode_dir ON jobs(episode_dir);
CREATE INDEX IF NOT EXISTS idx_jobs_node_status ON jobs(node_id, status);

CREATE TABLE IF NOT EXISTS series (
  id BIGSERIAL PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  flow_id BIGINT NOT NULL DEFAULT 0,
  tag TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  notify INTEGER NOT NULL DEFAULT 1,
  paused INTEGER NOT NULL DEFAULT 0,
  node_group TEXT NOT NULL DEFAULT '',
  webhook_url TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT %s,
  updated_at TEXT NOT NULL DEFAULT %s
);

CREATE TABLE IF NOT EXISTS api_tokens (
  id BIGSERIAL PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  scope TEXT NOT NULL DEFAULT 'read',
  token_hash TEXT NOT NULL UNIQUE,
  last_used_at TEXT,
  created_at TEXT NOT NULL DEFAULT %s
);

CREATE TABLE IF NOT EXISTS audit_events (
  id BIGSERIAL PRIMARY KEY,
  at TEXT NOT NULL DEFAULT %s,
  actor TEXT NOT NULL DEFAULT '',
  action TEXT NOT NULL,
  object TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS step_templates (
  id BIGSERIAL PRIMARY KEY,
  key TEXT NOT NULL UNIQUE,
  label TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  params_json TEXT NOT NULL DEFAULT '[]',
  powershell TEXT NOT NULL,
  builtin INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT %s,
  updated_at TEXT NOT NULL DEFAULT %s
);

CREATE TABLE IF NOT EXISTS pairing_codes (
  id BIGSERIAL PRIMARY KEY,
  code_hash TEXT NOT NULL UNIQUE,
  name_hint TEXT NOT NULL DEFAULT '',
  expires_at TEXT NOT NULL,
  used_by BIGINT NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT %s
);

CREATE TABLE IF NOT EXISTS settings (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  json TEXT NOT NULL,
  updated_at TEXT NOT NULL DEFAULT %s
);

CREATE TABLE IF NOT EXISTS provision_runs (
  id BIGSERIAL PRIMARY KEY,
  host TEXT NOT NULL,
  port INTEGER NOT NULL DEFAULT 5985,
  scheme TEXT NOT NULL DEFAULT 'http',
  winrm_user TEXT NOT NULL DEFAULT 'Administrator',
  node_name TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'queued',
  options_json TEXT NOT NULL DEFAULT '{}',
  log TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT %s,
  finished_at TEXT
);

CREATE TABLE IF NOT EXISTS node_metrics (
  id BIGSERIAL PRIMARY KEY,
  node_id BIGINT NOT NULL,
  ts TEXT NOT NULL,
  cpu_pct DOUBLE PRECISION NOT NULL DEFAULT 0,
  mem_used_mb INTEGER NOT NULL DEFAULT 0,
  mem_total_mb INTEGER NOT NULL DEFAULT 0,
  disk_free_gb INTEGER NOT NULL DEFAULT 0,
  gpu_util INTEGER NOT NULL DEFAULT -1,
  gpu_temp INTEGER NOT NULL DEFAULT -1,
  gpu_mem_used_mb INTEGER NOT NULL DEFAULT -1,
  encode_fps DOUBLE PRECISION NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_metrics_node_ts ON node_metrics(node_id, ts);

CREATE TABLE IF NOT EXISTS shares (
  id BIGSERIAL PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL,
  role TEXT NOT NULL,
  server TEXT NOT NULL DEFAULT '',
  path TEXT NOT NULL DEFAULT '',
  port INTEGER NOT NULL DEFAULT 0,
  username TEXT NOT NULL DEFAULT '',
  password TEXT NOT NULL DEFAULT '',
  region TEXT NOT NULL DEFAULT '',
  use_tls INTEGER NOT NULL DEFAULT 0,
  endpoint TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  mount_path TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT %s,
  updated_at TEXT NOT NULL DEFAULT %s
);

CREATE TABLE IF NOT EXISTS users (
  id BIGSERIAL PRIMARY KEY,
  username TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'admin',
  created_at TEXT NOT NULL DEFAULT %s
);

CREATE TABLE IF NOT EXISTS sessions (
  token_hash TEXT PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
`

// migratePG creates the full Postgres schema from scratch. Unlike the SQLite
// path (which replays an idempotent ALTER chain), PG gets the end-state schema
// in one shot — every table with its final columns, indexes, and constraints.
// This is CREATE ... IF NOT EXISTS so opening an existing PG database is a
// no-op (the schema is already there).
//
// The schema mirrors the SQLite end-state AFTER all migrations complete:
// nodes, flows, jobs, series, api_tokens, audit_events, step_templates,
// pairing_codes, settings, provision_runs, node_metrics, shares, users,
// sessions — plus the schema_migrations bookkeeping table.
//
// Key type mappings (all chosen so scan code is unchanged on PG):
//   - INTEGER PRIMARY KEY AUTOINCREMENT → BIGSERIAL PRIMARY KEY
//   - TEXT columns stay TEXT (timestamps included — NOT timestamptz, by
//     design: keeping TEXT lets fmtTime/parseTime and every scan path run
//     unchanged on both backends)
//   - INTEGER/SMALLINT 0/1 for booleans (SQLite has no bool type; PG mirrors
//     this so boolToInt/==1 scan code is identical)
//   - REAL → DOUBLE PRECISION
//   - DEFAULT (datetime('now')) → DEFAULT to_char(now() AT TIME ZONE 'utc', ...)
func (s *Store) migratePG() error {
	// Substitute the PG timestamp DEFAULT into every %s marker in the schema.
	// The literal is the same on every table, so a global replace fills all
	// placeholders without a fmt.Sprintf arg-count mismatch.
	schema := strings.ReplaceAll(pgSchemaTemplate, "%s", pgTimestampDefault)
	if _, err := s.db.ExecContext(context.Background(), schema); err != nil {
		return fmt.Errorf("migratePG schema: %w", err)
	}
	// SQLite compatibility functions: the stats queries use SQLite-specific
	// functions (julianday, strftime, date, SUBSTR, json_extract, substr)
	// that Postgres doesn't have. Rather than branching every stats query by
	// dialect, we create PL/pgSQL functions with the same names so the
	// identical SQL text runs on both backends. CREATE OR REPLACE makes this
	// idempotent (opening an existing PG database is a no-op).
	if err := s.migratePGCompatFuncs(); err != nil {
		return fmt.Errorf("migratePG compat funcs: %w", err)
	}
	return nil
}

// migratePGCompatFuncs creates PL/pgSQL functions that mirror the SQLite
// functions used in the store's stats queries. This lets the same query text
// run on both SQLite and Postgres without per-dialect SQL strings.
//
// Functions created:
//   - julianday(text) → numeric: converts a "YYYY-MM-DD HH:MM:SS" text to a
//     Julian day number. SQLite's julianday(finished_at) - julianday(started_at)
//     gives day-fraction differences; * 86400 converts to seconds.
//   - strftime(text, text) → text: SQLite's strftime. We only implement
//     '%s' (Unix epoch seconds) since that's the only format the store uses.
//   - date(text) → text: SQLite's date() returns "YYYY-MM-DD" from a
//     "YYYY-MM-DD HH:MM:SS" text.
//   - SUBSTR(text, int, int) → text: SQLite's 3-arg substring (1-indexed).
//     PG's SUBSTRING uses different syntax; this shim matches SQLite's.
//   - substr(text, int) → text: SQLite's 2-arg substr with negative offset
//     (keep last N chars). Used by AppendProvisionRunLog.
//   - substr(text, int, int) → text: 3-arg form for completeness.
//   - json_extract(text, text) → numeric: SQLite's json_extract for
//     '$.key' paths. Returns numeric for the stats comparison context.
func (s *Store) migratePGCompatFuncs() error {
	funcs := []string{
		// julianday: convert "YYYY-MM-DD HH:MM:SS" text to Julian day number.
		// Uses PG's to_timestamp + epoch math. SQLite's julianday returns
		// days since -4714-11-24 12:00 GMT; PG's epoch is seconds since
		// 1970-01-01. Julian day = epoch/86400 + 2440587.5.
		`CREATE OR REPLACE FUNCTION julianday(ts text) RETURNS numeric AS $$
		BEGIN
			RETURN (EXTRACT(EPOCH FROM to_timestamp(ts, 'YYYY-MM-DD HH24:MI:SS')) / 86400.0) + 2440587.5;
		END;
		$$ LANGUAGE plpgsql IMMUTABLE;`,

		// strftime: only '%s' (epoch seconds) is implemented — the sole
		// format the store uses (AvgFlowDuration). Returns text to match
		// SQLite's return type; the caller subtracts two and AVGs.
		`CREATE OR REPLACE FUNCTION strftime(fmt text, ts text) RETURNS text AS $$
		BEGIN
			IF fmt = '%s' THEN
				RETURN EXTRACT(EPOCH FROM to_timestamp(ts, 'YYYY-MM-DD HH24:MI:SS'))::text;
			END IF;
			RETURN '';
		END;
		$$ LANGUAGE plpgsql IMMUTABLE;`,

		// date: return "YYYY-MM-DD" from a timestamp text.
		`CREATE OR REPLACE FUNCTION date(ts text) RETURNS text AS $$
		BEGIN
			RETURN SUBSTRING(ts FROM 1 FOR 10);
		END;
		$$ LANGUAGE plpgsql IMMUTABLE;`,

		// SUBSTR (uppercase): 3-arg substring. SQLite is 1-indexed;
		// PG's substring(FROM n FOR m) is also 1-indexed.
		`CREATE OR REPLACE FUNCTION "SUBSTR"(s text, pos int, len int) RETURNS text AS $$
		BEGIN
			RETURN SUBSTRING(s FROM pos FOR len);
		END;
		$$ LANGUAGE plpgsql IMMUTABLE;`,

		// substr (lowercase): 2-arg with negative offset. SQLite's
		// substr(x, -n) returns the last n chars. Used by
		// AppendProvisionRunLog: substr(log || ?, -?).
		`CREATE OR REPLACE FUNCTION substr(s text, pos int) RETURNS text AS $$
		BEGIN
			IF pos < 0 THEN
				RETURN RIGHT(s, -pos);
			END IF;
			RETURN SUBSTRING(s FROM pos);
		END;
		$$ LANGUAGE plpgsql IMMUTABLE;`,

		// substr: 3-arg form (text, int, int). SQLite's substr(x, pos, len).
		`CREATE OR REPLACE FUNCTION substr(s text, pos int, len int) RETURNS text AS $$
		BEGIN
			RETURN SUBSTRING(s FROM pos FOR len);
		END;
		$$ LANGUAGE plpgsql IMMUTABLE;`,

		// json_extract: extract a value from a JSON text by '$.key' path.
		// Returns numeric (the stats context compares > 0 and sums).
		`CREATE OR REPLACE FUNCTION json_extract(j text, path text) RETURNS numeric AS $$
		DECLARE
			key text;
		BEGIN
			key := REPLACE(path, '$.', '');
			IF j IS NULL OR j = '' THEN
				RETURN NULL;
			END IF;
			RETURN (j::jsonb ->> key)::numeric;
		END;
		$$ LANGUAGE plpgsql IMMUTABLE;`,
	}
	for _, fn := range funcs {
		if _, err := s.db.ExecContext(context.Background(), fn); err != nil {
			return fmt.Errorf("create compat func: %w", err)
		}
	}
	return nil
}
