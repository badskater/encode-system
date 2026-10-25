package api

import (
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
	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("create dump file: %w", err)
	}
	defer f.Close()
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
	return nil
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
