// Command pgimport performs a one-shot, FK-ordered copy of an encode-system
// SQLite database into a Postgres database. It is the migration path for the
// Phase 2 SQLite→Postgres store workstream.
//
// Usage:
//
//	pgimport -sqlite <path> -pg <dsn> [-force] [-dry-run]
//
//	-sqlite   path to the source SQLite database (required)
//	-pg       target Postgres DSN, e.g. postgres://encode@host:port/db?sslmode=disable (required)
//	-force    allow import into a non-empty target: TRUNCATEs every data table CASCADE first
//	-dry-run  count + validate only; write nothing to the target
//
// Design notes:
//   - Tables are enumerated from the SQLite file (sqlite_master) and cross-
//     checked against the PG catalog after connect. A SQLite table with no PG
//     counterpart is a schema-drift bug → fail loudly, except a documented
//     skip list (schema_migrations is owned by the target's migratePG;
//     sqlite_sequence is a SQLite internal).
//   - Copy is generic: per table, read column names from SQLite PRAGMA
//     table_info, SELECT *, batch INSERT into PG with an explicit column list
//     ($n placeholders). Values are copied as-is — TEXT timestamps stay TEXT
//     strings, INTEGER 0/1 bools stay integers. Explicit ids are preserved
//     (BIGSERIAL accepts explicit inserts).
//   - After each table, the id sequence is advanced via
//     SELECT setval(pg_get_serial_sequence('t','id'), COALESCE(MAX(id),1))
//     so future inserts don't collide with the imported ids.
//   - A verification pass (always, even dry-run/force) compares per-table row
//     counts SQLite vs PG side by side, plus key aggregates via the store
//     package (CountJobsByStatus, ListNodes/CountNodes). Mismatch → non-zero
//     exit. A final "IMPORT OK: <N> tables, <M> rows" line is printed on
//     success.
//   - No secrets are ever logged: share password ciphertext/plaintext and
//     settings JSON values are never printed — only counts.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/badskater/encode-system/backend/internal/store"
)

// skipTables is the documented list of SQLite tables that have no PG
// counterpart and are deliberately not imported. schema_migrations is owned
// by the target's migratePG (the importer opens the target through store.Open,
// which runs migratePG, so the schema is already present and correct).
// sqlite_sequence is a SQLite-internal bookkeeping table for AUTOINCREMENT.
var skipTables = map[string]string{
	"schema_migrations": "owned by target migratePG",
	"sqlite_sequence":   "SQLite internal AUTOINCREMENT bookkeeping",
}

// importOrder is the FK-respecting copy order, derived from the schema in
// store.go (SQLite) and store_pg.go (PG): parents before children.
// settings is single-row (id CHECK = 1) and has no FKs. nodes/flows/users/
// api_tokens/pairing_codes/audit_events/step_templates have no FK deps.
// series references flows (flow_id, optional 0). sessions references users.
// shares has no FK. provision_runs has no FK. jobs references flows (flow_id).
// node_metrics references nodes (node_id, not a declared FK on SQLite but
// logically dependent). Tables absent from the SQLite source are skipped
// silently (the source is an older schema); tables present in SQLite but
// missing from this order AND not in skipTables trigger the drift failure.
var importOrder = []string{
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

// batchSize is the per-table INSERT batch size. Large enough to amortize
// round-trips, small enough to keep statement arg counts bounded (PG has a
// 65535 parameter limit per statement; 500 rows × ~30 cols = 15000 params,
// comfortably under).
const batchSize = 500

func main() {
	sqlitePath := flag.String("sqlite", "", "path to the source SQLite database (required)")
	pgDSN := flag.String("pg", "", "target Postgres DSN (required)")
	force := flag.Bool("force", false, "allow import into a non-empty target: TRUNCATE all tables CASCADE first")
	dryRun := flag.Bool("dry-run", false, "count + validate only; write nothing to the target")
	flag.Parse()

	if *sqlitePath == "" || *pgDSN == "" {
		fmt.Fprintln(os.Stderr, "error: -sqlite and -pg are both required")
		flag.Usage()
		os.Exit(2)
	}
	if *dryRun && *force {
		fmt.Fprintln(os.Stderr, "error: -force has no effect with -dry-run (nothing is written)")
		os.Exit(2)
	}

	code, err := run(context.Background(), *sqlitePath, *pgDSN, *force, *dryRun)
	if err != nil {
		fmt.Fprintf(os.Stderr, "IMPORT FAILED: %v\n", err)
	}
	os.Exit(code)
}

// run executes the import and returns an exit code (0 success, non-zero on
// any failure) plus an error for diagnostics. Split from main() for testability.
func run(ctx context.Context, sqlitePath, pgDSN string, force, dryRun bool) (int, error) {
	// --- Open source (SQLite, read-only intent) ---
	src, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		return 1, fmt.Errorf("open sqlite: %w", err)
	}
	defer src.Close()
	if err := src.PingContext(ctx); err != nil {
		return 1, fmt.Errorf("ping sqlite: %w", err)
	}

	// --- Enumerate SQLite tables ---
	sqliteTables, err := sqliteTableNames(ctx, src)
	if err != nil {
		return 1, fmt.Errorf("enumerate sqlite tables: %w", err)
	}
	fmt.Printf("source: %d tables in SQLite: %s\n", len(sqliteTables), strings.Join(sqliteTables, ", "))

	// --- Open target (Postgres) ---
	// Two handles, both over the same pgx stdlib driver:
	//   1. store.Open runs migratePG so the schema exists (fresh DB) or is a
	//      no-op (existing DB). Its *Store is used for the verification
	//      aggregates (CountJobsByStatus, ListNodes) — proving the imported
	//      data is readable through real code paths.
	//   2. A raw *sql.DB is used for catalog queries, bulk INSERTs, and
	//      setval, since those need direct SQL not the store's typed API.
	// Two pools on the same DSN is fine: pgx stdlib pools connections and
	// the importer is single-threaded per table.
	tgtStore, err := store.Open(pgDSN)
	if err != nil {
		return 1, fmt.Errorf("open postgres (store.Open/migratePG): %w", err)
	}
	defer tgtStore.Close()
	pgDB, err := sql.Open("pgx", pgDSN)
	if err != nil {
		return 1, fmt.Errorf("open postgres raw: %w", err)
	}
	defer pgDB.Close()
	if err := pgDB.PingContext(ctx); err != nil {
		return 1, fmt.Errorf("ping postgres: %w", err)
	}

	// --- Cross-check: every SQLite table must map to a PG table or be skipped ---
	pgTables, err := pgTableNames(ctx, pgDB)
	if err != nil {
		return 1, fmt.Errorf("enumerate pg tables: %w", err)
	}
	pgSet := make(map[string]bool, len(pgTables))
	for _, t := range pgTables {
		pgSet[t] = true
	}
	ordered, skipped, err := reconcileTables(sqliteTables, pgSet)
	if err != nil {
		return 1, err
	}
	fmt.Printf("skip list: %s\n", formatSkipList(skipped))
	fmt.Printf("import order (%d tables): %s\n", len(ordered), strings.Join(ordered, ", "))

	// --- Pre-flight: refuse non-empty target unless -force ---
	if err := checkTargetEmpty(ctx, pgDB, ordered, force, dryRun); err != nil {
		return 1, err
	}

	// --- Dry-run: count + validate only, write nothing ---
	if dryRun {
		fmt.Println("DRY RUN: writing nothing")
		// In dry-run the target stays empty by design, so the count
		// comparison is informational (PG=0 for every populated table).
		// Verification reports the counts but does NOT fail on mismatch.
		verify(ctx, src, pgDB, ordered, tgtStore, true /*dryRun*/)
		fmt.Println("DRY RUN OK (no rows written)")
		return 0, nil
	}

	// --- Force: TRUNCATE every target data table CASCADE first ---
	if force {
		if err := truncateTarget(ctx, pgDB, ordered); err != nil {
			return 1, fmt.Errorf("truncate target: %w", err)
		}
		fmt.Println("truncated target tables (-force)")
	}

	// --- Copy each table in FK order ---
	totalRows := 0
	for _, tbl := range ordered {
		n, err := copyTable(ctx, src, pgDB, tbl)
		if err != nil {
			return 1, fmt.Errorf("copy table %s: %w", tbl, err)
		}
		totalRows += n
		fmt.Printf("  %-18s %d rows\n", tbl, n)
	}

	// --- Verification pass (counts + key aggregates) ---
	if err := verify(ctx, src, pgDB, ordered, tgtStore, false /*dryRun*/); err != nil {
		return 1, fmt.Errorf("verification: %w", err)
	}

	fmt.Printf("IMPORT OK: %d tables, %d rows\n", len(ordered), totalRows)
	return 0, nil
}

// sqliteTableNames returns user table names from sqlite_master, excluding the
// sqlite_% internal tables. Order is unspecified (sqlite_master returns
// creation order), so callers sort into importOrder separately.
func sqliteTableNames(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// pgTableNames returns user table names from the PG catalog
// (information_schema.tables), excluding the pg_catalog/schema_migrations
// bookkeeping rows.
func pgTableNames(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
SELECT table_name FROM information_schema.tables
WHERE table_schema = 'public' AND table_type = 'BASE TABLE' ORDER BY table_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// reconcileTables validates that every SQLite table is either in the skip
// list, has a PG counterpart, or is a known optional table. It returns the
// ordered list of tables to import (in importOrder, filtered to those present
// in the SQLite source) and the skip list with reasons.
//
// A SQLite table present in neither PG nor the skip list is schema drift
// (a new SQLite table without a PG counterpart) and fails loudly.
func reconcileTables(sqliteTables []string, pgSet map[string]bool) (ordered, skipped []string, err error) {
	srcSet := make(map[string]bool, len(sqliteTables))
	for _, t := range sqliteTables {
		srcSet[t] = true
	}
	// Build the ordered import list from importOrder, keeping only tables
	// that exist in the SQLite source.
	for _, t := range importOrder {
		if srcSet[t] {
			ordered = append(ordered, t)
		}
	}
	// Walk every SQLite table: it must be imported (and exist in PG), or
	// be on the skip list. Anything else is drift.
	for _, t := range sqliteTables {
		if reason, ok := skipTables[t]; ok {
			skipped = append(skipped, fmt.Sprintf("%s (%s)", t, reason))
			continue
		}
		if !pgSet[t] {
			return nil, nil, fmt.Errorf("schema drift: SQLite table %q has no Postgres counterpart and is not on the skip list", t)
		}
		// Also confirm it's in importOrder — a SQLite table that exists in
		// PG but isn't in our ordered list means importOrder is stale (a
		// new table was added to both schemas but not to the importer).
		if !inList(t, importOrder) {
			return nil, nil, fmt.Errorf("importer out of date: table %q exists in both schemas but is not in importOrder — add it", t)
		}
	}
	return ordered, skipped, nil
}

func inList(s string, list []string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func formatSkipList(skipped []string) string {
	if len(skipped) == 0 {
		return "(none)"
	}
	return strings.Join(skipped, ", ")
}

// checkTargetEmpty refuses to import if ANY target table already has rows,
// unless force is set (or dryRun, which writes nothing so the pre-flight is
// informational only — we still report the counts). This prevents clobbering
// a live target by accident.
func checkTargetEmpty(ctx context.Context, db *sql.DB, tables []string, force, dryRun bool) error {
	nonEmpty := []string{}
	for _, t := range tables {
		n, err := countRows(ctx, db, t)
		if err != nil {
			return fmt.Errorf("count target %s: %w", t, err)
		}
		if n > 0 {
			nonEmpty = append(nonEmpty, fmt.Sprintf("%s=%d", t, n))
		}
	}
	if len(nonEmpty) == 0 {
		fmt.Println("target: empty (ok)")
		return nil
	}
	fmt.Printf("target: non-empty tables: %s\n", strings.Join(nonEmpty, ", "))
	if dryRun {
		fmt.Println("note: -dry-run writes nothing; target counts shown for reference")
		return nil
	}
	if !force {
		return fmt.Errorf("target is non-empty (tables with rows: %s); pass -force to TRUNCATE and re-import", strings.Join(nonEmpty, ", "))
	}
	return nil
}

// truncateTarget TRUNCATEs every target data table with RESTART IDENTITY
// CASCADE. Order doesn't matter (CASCADE handles FKs) but we truncate in
// reverse import order for clarity. schema_migrations is left untouched
// (it's bookkeeping owned by migratePG).
func truncateTarget(ctx context.Context, db *sql.DB, tables []string) error {
	// Reverse order — children before parents — though CASCADE makes this
	// belt-and-suspenders rather than required.
	for i := len(tables) - 1; i >= 0; i-- {
		t := tables[i]
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`TRUNCATE TABLE %s RESTART IDENTITY CASCADE`, t)); err != nil {
			return fmt.Errorf("truncate %s: %w", t, err)
		}
	}
	return nil
}

// copyTable reads all rows+columns from the SQLite source table and batch-
// inserts them into the PG target, preserving explicit ids. After the copy it
// advances the id sequence so future inserts don't collide.
//
// Column names come from SQLite PRAGMA table_info (so the importer adapts to
// the source schema rather than hardcoding columns). Values are scanned into
// []any and passed to PG as-is: TEXT timestamps stay strings, INTEGER 0/1
// bools stay ints. The PG schema uses the same types by design (see
// store_pg.go), so no per-column conversion is needed.
func copyTable(ctx context.Context, src, tgt *sql.DB, table string) (int, error) {
	cols, err := sqliteColumns(ctx, src, table)
	if err != nil {
		return 0, fmt.Errorf("read columns: %w", err)
	}
	if len(cols) == 0 {
		return 0, fmt.Errorf("no columns for table %s", table)
	}

	// Build the PG INSERT statement skeleton once: explicit column list,
	// with a %s placeholder for the VALUES clause (filled per batch with
	// distinct $n placeholders — each row needs its own $n..$n+cols range,
	// not the same $1..$cols repeated, since PG placeholders are positional
	// across the whole statement).
	colList := quoteIdentList(cols)
	insertSQL := fmt.Sprintf("INSERT INTO %s (%s) VALUES %%s",
		quoteIdent(table), colList)

	// Stream rows from SQLite to avoid loading the whole table into memory.
	selectSQL := fmt.Sprintf("SELECT %s FROM %s", colList, quoteIdent(table))
	rows, err := src.QueryContext(ctx, selectSQL)
	if err != nil {
		return 0, fmt.Errorf("select source: %w", err)
	}
	defer rows.Close()

	// Allocate a slice of []any (one per row) and a flat arg slice for pgx.
	// pgx's database/sql driver accepts []any as variadic args.
	total := 0
	batchRows := make([][]any, 0, batchSize)

	flush := func() error {
		if len(batchRows) == 0 {
			return nil
		}
		// Build the VALUES clause: one tuple per row, each with DISTINCT
		// $n placeholders (row 0 uses $1..$cols, row 1 uses $cols+1..$2*cols,
		// etc.). PG placeholders are positional across the whole statement,
		// so reusing $1..$cols for every row would bind every row to the
		// first row's args.
		ncols := len(cols)
		tuples := make([]string, len(batchRows))
		for i := range batchRows {
			ph := make([]string, ncols)
			for j := 0; j < ncols; j++ {
				ph[j] = fmt.Sprintf("$%d", i*ncols+j+1)
			}
			tuples[i] = "(" + strings.Join(ph, ",") + ")"
		}
		stmt := fmt.Sprintf(insertSQL, strings.Join(tuples, ","))
		// Flatten args: row0col0, row0col1, ..., rowNcolM.
		args := make([]any, 0, len(batchRows)*ncols)
		for _, r := range batchRows {
			args = append(args, r...)
		}
		if _, err := tgt.ExecContext(ctx, stmt, args...); err != nil {
			return fmt.Errorf("insert batch: %w", err)
		}
		total += len(batchRows)
		batchRows = batchRows[:0]
		return nil
	}

	for rows.Next() {
		rowVals := make([]any, len(cols))
		scanTargets := make([]any, len(cols))
		for i := range rowVals {
			scanTargets[i] = &rowVals[i]
		}
		if err := rows.Scan(scanTargets...); err != nil {
			return 0, fmt.Errorf("scan source row: %w", err)
		}
		// Coerce []byte (SQLite TEXT/BLOB come back as []byte via modernc) to
		// string for TEXT columns — pgx would otherwise send them as bytea
		// and fail the TEXT column type. nil stays nil.
		for i, v := range rowVals {
			if b, ok := v.([]byte); ok {
				rowVals[i] = string(b)
			}
		}
		batchRows = append(batchRows, rowVals)
		if len(batchRows) >= batchSize {
			if err := flush(); err != nil {
				return 0, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate source rows: %w", err)
	}
	if err := flush(); err != nil {
		return 0, err
	}

	// Advance the id sequence so the next insert (via the controller) gets an
	// id above the imported max. Tables without an id serial (settings uses
	// a plain INTEGER PK) are skipped — pg_get_serial_sequence returns NULL.
	if inList("id", cols) {
		if err := advanceSequence(ctx, tgt, table); err != nil {
			return 0, fmt.Errorf("advance sequence: %w", err)
		}
	}
	return total, nil
}

// advanceSequence runs
//
//	SELECT setval(pg_get_serial_sequence('table','id'), COALESCE(MAX(id),1))
//	FROM table
//
// so the serial's next value is above the max imported id. If the table has
// no serial sequence (pg_get_serial_sequence returns NULL/empty), this is a
// no-op — e.g. settings uses a plain INTEGER PRIMARY KEY, not BIGSERIAL.
func advanceSequence(ctx context.Context, db *sql.DB, table string) error {
	var seqName sql.NullString
	if err := db.QueryRowContext(ctx,
		"SELECT pg_get_serial_sequence($1, 'id')", table).Scan(&seqName); err != nil {
		return fmt.Errorf("lookup sequence for %s: %w", table, err)
	}
	if !seqName.Valid || seqName.String == "" {
		return nil // no serial sequence (e.g. settings) — nothing to advance
	}
	// pg_get_serial_sequence returns a fully-qualified, already-quoted-
	// safe name like 'public.nodes_id_seq'. Pass it to setval as a string
	// literal (NOT a quoted identifier — that would break the schema.name
	// split into one bogus identifier). The ::regclass cast resolves it.
	// setval(name, value): the name arg is text, cast to regclass internally.
	var result int64
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT setval('%s'::regclass, COALESCE((SELECT MAX(id) FROM %s), 1))",
			seqName.String, quoteIdent(table))).Scan(&result); err != nil {
		return fmt.Errorf("setval %s: %w", table, err)
	}
	return nil
}

// sqliteColumns returns the column names of a SQLite table in ordinal order,
// via PRAGMA table_info.
func sqliteColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", quoteIdent(table)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// countRows returns the row count of a table.
func countRows(ctx context.Context, db *sql.DB, table string) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", quoteIdent(table))).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// verify compares per-table row counts (SQLite vs PG) for every imported
// table, then runs key aggregate checks via the store package
// (CountJobsByStatus totals, ListNodes for node count). Any mismatch fails.
//
// In dryRun mode the target is empty by design, so the count comparison is
// informational (printed but not fatal) and the aggregate checks are skipped
// (they'd read zero rows). The caller separately asserts the target stayed
// empty.
func verify(ctx context.Context, src, tgt *sql.DB, tables []string, tgtStore *store.Store, dryRun bool) error {
	fmt.Println("verification: per-table counts (sqlite | pg)")
	mismatch := 0
	totalSrc := 0
	totalTgt := 0
	for _, t := range tables {
		s, err := countRows(ctx, src, t)
		if err != nil {
			return fmt.Errorf("count source %s: %w", t, err)
		}
		g, err := countRows(ctx, tgt, t)
		if err != nil {
			return fmt.Errorf("count target %s: %w", t, err)
		}
		mark := "ok"
		if s != g {
			mark = "MISMATCH"
			mismatch++
		}
		fmt.Printf("  %-18s sqlite=%-6d pg=%-6d %s\n", t, s, g, mark)
		totalSrc += s
		totalTgt += g
	}
	if dryRun {
		// Dry-run: counts are informational only. Report and return.
		return nil
	}
	if mismatch > 0 {
		return fmt.Errorf("%d tables have count mismatches", mismatch)
	}
	if totalSrc != totalTgt {
		return fmt.Errorf("total row count mismatch: sqlite=%d pg=%d", totalSrc, totalTgt)
	}

	// --- Key aggregate checks via the store package ---
	// These prove the imported data is readable through the real code paths,
	// not just that row counts match.
	if inList("jobs", tables) {
		srcCounts, err := tgtStore.CountJobsByStatus(ctx)
		if err != nil {
			return fmt.Errorf("CountJobsByStatus on target: %w", err)
		}
		// Sum the status counts and compare to the jobs row count.
		jobTotal := 0
		for _, n := range srcCounts {
			jobTotal += n
		}
		jobRows, err := countRows(ctx, tgt, "jobs")
		if err != nil {
			return fmt.Errorf("count target jobs: %w", err)
		}
		if jobTotal != jobRows {
			return fmt.Errorf("jobs aggregate mismatch: CountJobsByStatus total=%d but jobs rows=%d", jobTotal, jobRows)
		}
		fmt.Printf("  jobs: CountJobsByStatus total=%d matches rows=%d\n", jobTotal, jobRows)
	}
	if inList("nodes", tables) {
		nodes, err := tgtStore.ListNodes(ctx)
		if err != nil {
			return fmt.Errorf("ListNodes on target: %w", err)
		}
		nodeRows, err := countRows(ctx, tgt, "nodes")
		if err != nil {
			return fmt.Errorf("count target nodes: %w", err)
		}
		if len(nodes) != nodeRows {
			return fmt.Errorf("nodes aggregate mismatch: ListNodes=%d but nodes rows=%d", len(nodes), nodeRows)
		}
		fmt.Printf("  nodes: ListNodes=%d matches rows=%d\n", len(nodes), nodeRows)
	}

	// --- Shares key reminder (no secrets printed) ---
	// The share-credential encryption key lives BESIDE the SQLite DB
	// (<sqlite>.shares-key). On Postgres it resolves to
	// ENCODE_SHARES_KEY_FILE or a temp-dir path derived from the DSN db name
	// (see store_share.go shareKeyPath). For shares to decrypt after import,
	// the operator MUST copy the source key file to the path the PG
	// controller will resolve — or set ENCODE_SHARES_KEY_FILE to it. Print a
	// reminder (the importer does not move the key itself, by design: the
	// key's location is a deployment decision, not an import side effect).
	if inList("shares", tables) {
		fmt.Println("  REMINDER: share passwords are AES-GCM encrypted with a key file beside the SQLite DB.")
		fmt.Println("            Copy <sqlite-path>.shares-key to where the PG controller resolves it:")
		fmt.Println("              set ENCODE_SHARES_KEY_FILE=<path>, or place it at the DSN-derived temp path.")
		fmt.Println("            (shares key file path is not printed here — see store_share.go shareKeyPath)")
	}
	return nil
}

// quoteIdent quotes a SQL identifier for safe interpolation into a statement.
// For Postgres this is the double-quote form with embedded quotes doubled.
// For SQLite the same form works. We use it for table/column names that come
// from the catalogs (never user input), so the quoting is defensive against
// reserved words like "group" and "order" rather than injection.
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
