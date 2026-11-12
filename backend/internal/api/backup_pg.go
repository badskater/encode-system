package api

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
)

// pgBackupTables is the FK-respecting dump order for the Postgres logical
// backup, mirroring cmd/pgimport's importOrder (parents before children).
// Read from pgimport for reference, implemented locally here (never imports
// cmd/) so the backup endpoint does not take a build dependency on the
// importer binary. schema_migrations is skipped — migratePG owns it on
// restore. A table absent from a given PG database (older schema) is skipped
// silently by the per-table column lookup.
var pgBackupTables = []string{
	"settings",
	"nodes",
	"flows",
	"users",
	"api_tokens",
	"pairing_codes",
	"audit_events",
	"step_templates",
	"series",
	"shares",
	"provision_runs",
	"sessions",
	"jobs",
	"node_metrics",
}

// dumpPG writes a logical, table-walk dump of the Postgres database to dest
// as newline-delimited JSON. Each line is either a control record marking the
// start of a table ({"_table":"<name>"}) or a row record
// ({"_table":"<name>","_row":{...}}). Tables are walked in FK order so the
// dump can be re-imported with FKs satisfied.
//
// Format rationale: JSON-lines is human-inspectable, streamable (the writer
// never holds the whole DB in memory), and trivially restorable via a small
// INSERT loop. SQL-INSERT format was rejected because it would require
// per-column literal escaping in Go; JSON delegates that to encoding/json.
//
// No-secrets contract: this copies the shares.password column verbatim, which
// holds the AES-GCM ciphertext ("v1:<hex>") — exactly what a SQLite VACUUM
// INTO backup already contains. The encryption key file lives beside the DB
// (SQLite) / at ENCODE_SHARES_KEY_FILE (PG) and is NEVER in the database, so
// neither backup format leaks plaintext share passwords. See TakeSnapshot's
// doc comment for the full parity rationale.
func (b *BackupManager) dumpPG(ctx context.Context, dest string) error {
	if b.PGDump == nil {
		return fmt.Errorf("PGDump handle not set on BackupManager")
	}
	f, err := createDumpFile(dest)
	if err != nil {
		return fmt.Errorf("create dump file: %w", err)
	}
	// A failed dump must not leave a partial .dump behind (it would be
	// listed as a valid snapshot). The ErrExist path returns BEFORE this
	// defer is armed, so a file owned by another writer is never removed.
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(dest)
		}
	}()
	enc := json.NewEncoder(f)
	// Header control record so a restore tool can validate format/version.
	if err := enc.Encode(map[string]any{
		"_format":  "encode-pg-dump",
		"_version": 1,
	}); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	for _, table := range pgBackupTables {
		// Table-start marker (even for empty tables, so a restore knows the
		// table exists in the schema).
		if err := enc.Encode(map[string]string{"_table": table}); err != nil {
			return fmt.Errorf("write table marker %s: %w", table, err)
		}
		if err := dumpPGTable(ctx, b.PGDump, table, enc); err != nil {
			return fmt.Errorf("dump table %s: %w", table, err)
		}
	}
	ok = true
	return nil
}

// createDumpFile creates the dump file EXCLUSIVELY (O_CREATE|O_EXCL, 0600).
// Two HA replicas snapshotting in the same second must not truncate each
// other's dump (P4 review Q8 — os.Create silently clobbered). The loser
// gets os.ErrExist; TakeSnapshot advances the counter suffix and retries,
// matching SQLite's VACUUM INTO behavior (which also refuses an existing
// output file).
func createDumpFile(dest string) (*os.File, error) {
	return os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
}

// dumpPGTable reads every row+column of one table and writes each as a
// {"_table":..., "_row":{...}} JSON line. Columns come from the PG catalog
// (information_schema.columns) so the dump adapts to the live schema rather
// than hardcoding columns. Values are scanned into []any and JSON-encoded
// as-is: TEXT stays string, INTEGER 0/1 stays int, timestamps stay the TEXT
// format the store uses on both backends. A table not present in the schema
// returns zero columns and is a no-op.
func dumpPGTable(ctx context.Context, db *sql.DB, table string, enc *json.Encoder) error {
	cols, err := pgColumns(ctx, db, table)
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		// Table not present in this schema (older DB) — skip silently.
		return nil
	}
	// Build SELECT with an explicit column list. The quoting guards against
	// reserved words like "group"/"order" rather than injection — column
	// names come from the catalog, never user input.
	q := fmt.Sprintf("SELECT %s FROM %s", quoteIdentList(cols), quoteIdent(table))
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("select: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = normalizePGValue(vals[i])
		}
		if err := enc.Encode(map[string]any{"_table": table, "_row": row}); err != nil {
			return fmt.Errorf("encode row: %w", err)
		}
	}
	return rows.Err()
}

// pgColumns returns the column names of a Postgres table in ordinal order,
// from information_schema.columns. An empty result means the table is absent
// from the current schema (older DB) — the dump treats that as a skip.
func pgColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT column_name FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = $1
		 ORDER BY ordinal_position`, table)
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// normalizePGValue makes pgx-scanned values JSON-clean: []byte (TEXT/BYTEA
// come back as []byte via pgx) becomes string; sql.Null* is unwrapped so null
// renders as JSON null. nil passes through. Everything else (int, float,
// bool) is left for encoding/json to handle.
func normalizePGValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(t)
	case sql.NullString:
		if !t.Valid {
			return nil
		}
		return t.String
	case sql.NullInt64:
		if !t.Valid {
			return nil
		}
		return t.Int64
	case sql.NullFloat64:
		if !t.Valid {
			return nil
		}
		return sanitizeFloat(t.Float64)
	case float64:
		// REAL columns (node_metrics progress/cpu_pct/encode_fps) scan as
		// float64; json.Marshal aborts on NaN/±Inf, so a single malformed
		// metric row would fail the whole backup. Render them as null.
		return sanitizeFloat(t)
	case sql.NullBool:
		if !t.Valid {
			return nil
		}
		return t.Bool
	default:
		return v
	}
}

// sanitizeFloat maps NaN/±Inf to nil (JSON null): encoding/json refuses to
// marshal them, and one corrupt REAL value must not abort the whole dump.
func sanitizeFloat(f float64) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return f
}

// quoteIdent quotes a SQL identifier for safe interpolation — defensive
// against reserved words ("group", "order") rather than injection (the names
// come from the catalog, never user input). Double-quote form with embedded
// quotes doubled; valid on Postgres.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// quoteIdentList quotes and comma-joins a list of identifiers.
func quoteIdentList(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = quoteIdent(c)
	}
	return strings.Join(q, ", ")
}

// ---------- PG restore ----------

// restorePG replays a JSON-lines dump file into the Postgres database in a
// single transaction. It TRUNCATEs every table in reverse FK order (children
// before parents), INSERTs each row with parameterized columns (rebound via
// the store's q() helper for $N placeholders), and advances each table's id
// serial sequence via setval/pg_get_serial_sequence (skipping non-serial
// tables like settings). The shares-key file is never touched — it lives
// outside the DB and is unaffected by table data.
//
// The dump format is one JSON object per line: {"_table":"name"} marks a
// table boundary, {"_table":"name","_row":{...}} is a data row. The header
// {"_format":"encode-pg-dump","_version":1} is skipped.
func (s *Server) restorePG(ctx context.Context, srcPath string) error {
	if s.Backup == nil || s.Backup.PGDump == nil {
		return fmt.Errorf("PGDump handle not set on BackupManager")
	}
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open dump file: %w", err)
	}
	defer f.Close()

	// Read all lines first so we can process within a single transaction
	// without holding the file open across the tx (cleaner rollback).
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // 4 MiB max line
	type rowEntry struct {
		table string
		row   map[string]any
	}
	var rows []rowEntry
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			return fmt.Errorf("parse dump line: %w", err)
		}
		if _, ok := rec["_format"]; ok {
			continue // header record
		}
		table, _ := rec["_table"].(string)
		if table == "" {
			continue
		}
		if row, ok := rec["_row"].(map[string]any); ok {
			rows = append(rows, rowEntry{table: table, row: row})
		}
		// A bare {"_table":"name"} marker (no _row) just signals the table
		// boundary — nothing to do per-row; TRUNCATE handles the table.
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read dump file: %w", err)
	}

	db := s.Backup.PGDump
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() // safe: a committed tx makes this a no-op

	// TRUNCATE all tables in one statement with CASCADE so FK constraints
	// don't block. We list every table explicitly (not CASCADE on one) so
	// only the known tables are affected. RESTART IDENTITY resets sequences,
	// but we setval explicitly below to match the imported max id.
	// Tables absent from the schema (older DB) are filtered out first.
	var presentTables []string
	for _, table := range pgBackupTables {
		cols, err := pgColumnsTx(ctx, tx, table)
		if err != nil {
			return fmt.Errorf("columns for %s: %w", table, err)
		}
		if len(cols) > 0 {
			presentTables = append(presentTables, quoteIdent(table))
		}
	}
	if len(presentTables) > 0 {
		truncSQL := "TRUNCATE TABLE " + strings.Join(presentTables, ", ") + " RESTART IDENTITY"
		if _, err := tx.ExecContext(ctx, truncSQL); err != nil {
			return fmt.Errorf("truncate tables: %w", err)
		}
	}

	// INSERT rows grouped by table (the dump writes them in FK order, but
	// grouping is not required — FK constraints are satisfied as long as
	// parents come before children, which the dump guarantees). We insert
	// in the order they appear in the file.
	for _, entry := range rows {
		cols, err := pgColumnsTx(ctx, tx, entry.table)
		if err != nil {
			return fmt.Errorf("columns for %s: %w", entry.table, err)
		}
		if len(cols) == 0 {
			continue
		}
		// Build the column list from the row's keys (intersect with catalog
		// to guard against schema drift). Values are positional in catalog
		// order — a column absent from the row inserts as NULL.
		colList := make([]string, 0, len(cols))
		vals := make([]any, 0, len(cols))
		for _, c := range cols {
			if v, ok := entry.row[c]; ok {
				colList = append(colList, quoteIdent(c))
				vals = append(vals, v)
			}
			// Columns absent from the row are omitted from the INSERT
			// (defaults apply).
		}
		if len(colList) == 0 {
			continue // empty row, nothing to insert
		}
		// Build parameterized INSERT: INSERT INTO "t" ("c1","c2") VALUES ($1,$2)
		ph := make([]string, len(vals))
		for i := range vals {
			ph[i] = fmt.Sprintf("$%d", i+1)
		}
		query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
			quoteIdent(entry.table), strings.Join(colList, ", "), strings.Join(ph, ", "))
		if _, err := tx.ExecContext(ctx, query, vals...); err != nil {
			return fmt.Errorf("insert into %s: %w", entry.table, err)
		}
	}

	// Advance id sequences so the next controller insert gets an id above
	// the imported max. Tables without a serial (settings uses INTEGER PK)
	// are skipped — pg_get_serial_sequence returns NULL.
	for _, table := range pgBackupTables {
		if err := advanceSequenceTx(ctx, tx, table); err != nil {
			return fmt.Errorf("advance sequence %s: %w", table, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit restore: %w", err)
	}
	// Q9: the restored rows may be sealed with a DIFFERENT shares key (the
	// operator places the bundle's key file before restoring). Drop the
	// cached cipher so the next secret read re-reads the key from disk —
	// no controller restart needed for the in-place path.
	s.Store.InvalidateShareCipher()
	return nil
}

// pgColumnsTx is the transaction-scoped version of pgColumns.
func pgColumnsTx(ctx context.Context, tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT column_name FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = $1
		 ORDER BY ordinal_position`, table)
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// advanceSequenceTx runs setval so the serial's next value is above the max
// imported id, within the restore transaction. Mirrors cmd/pgimport's
// advanceSequence pattern. Tables without a serial sequence (settings uses
// INTEGER PK, sessions has no id column) are no-ops — pg_get_serial_sequence
// returns NULL or the column lookup fails. We check the column exists first
// to avoid a hard error on tables without an id column.
func advanceSequenceTx(ctx context.Context, tx *sql.Tx, table string) error {
	// Check the table has an "id" column before querying pg_get_serial_sequence,
	// which errors on a missing column (sessions has token_hash PK, no id).
	hasID, err := txHasColumn(ctx, tx, table, "id")
	if err != nil {
		return fmt.Errorf("check id column for %s: %w", table, err)
	}
	if !hasID {
		return nil // no id column — no sequence to advance
	}
	var seqName sql.NullString
	if err := tx.QueryRowContext(ctx,
		"SELECT pg_get_serial_sequence($1, 'id')", table).Scan(&seqName); err != nil {
		return fmt.Errorf("lookup sequence for %s: %w", table, err)
	}
	if !seqName.Valid || seqName.String == "" {
		return nil // no serial sequence (e.g. settings uses INTEGER PK)
	}
	var result int64
	if err := tx.QueryRowContext(ctx,
		fmt.Sprintf("SELECT setval('%s'::regclass, COALESCE((SELECT MAX(id) FROM %s), 1))",
			seqName.String, quoteIdent(table))).Scan(&result); err != nil {
		return fmt.Errorf("setval %s: %w", table, err)
	}
	return nil
}

// txHasColumn reports whether a table has a column with the given name.
func txHasColumn(ctx context.Context, tx *sql.Tx, table, col string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`,
		table, col).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
