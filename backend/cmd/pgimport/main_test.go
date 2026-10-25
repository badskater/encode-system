package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/store"
)

// TestImportGolden is the golden SQLite→Postgres import test. It is gated on
// ENCODE_TEST_PG_DSN: when unset, the test is skipped so the default
// `go test ./...` needs no Postgres. When set, the DSN must point at the
// encode_test database (which the test truncates before each run for
// isolation — NOT dropped+recreated, because dropping the whole DB
// terminates other tests' pool connections when this runs alongside the
// store PG suite under one `go test` invocation).
//
// The test:
//  1. Builds a SQLite fixture via store.Open (nodes incl. one linux, a flow,
//     jobs in several statuses with step timings, settings with linux_*
//     fields, a share with password, audit rows, metrics rows, users/tokens).
//  2. Truncates all PG target tables, runs the importer (no -force: empty).
//  3. Asserts per-table row counts equal (SQLite vs PG).
//  4. Spot-checks a job row round-trips (status, timestamps parse, step
//     timings intact) via store.GetJob on PG.
//  5. Settings JSON round-trips via store.GetSettings on PG.
//  6. Share password decrypts via store.GetShare on PG — the test sets
//     ENCODE_SHARES_KEY_FILE to the SQLite fixture's key file so the PG
//     store resolves the same key.
//  7. Idempotence: a second run WITHOUT -force refuses (non-empty target);
//     WITH -force it truncates and re-imports cleanly.
//  8. -dry-run writes nothing (target counts stay 0 after a fresh truncate).
func TestImportGolden(t *testing.T) {
	pgDSN := os.Getenv("ENCODE_TEST_PG_DSN")
	if pgDSN == "" {
		t.Skip("ENCODE_TEST_PG_DSN not set; skipping PG import golden test")
	}

	ctx := context.Background()
	fixtureDir := t.TempDir()
	sqlitePath := filepath.Join(fixtureDir, "fixture.db")

	// --- Build the SQLite fixture via the real store API ---
	fixture := buildSQLiteFixture(t, ctx, sqlitePath)

	// The fixture's share key file is beside the SQLite DB. Point the PG
	// store at it so imported share ciphertext decrypts under PG.
	keyFile := sqlitePath + ".shares-key"
	t.Setenv("ENCODE_SHARES_KEY_FILE", keyFile)

	// --- Use a DEDICATED test database (encode_import_test), NOT the
	// encode_test DB the env var points at. Reason: `go test ./cmd/pgimport/
	// ./internal/store/ -run 'Import|PG'` runs the two packages in PARALLEL,
	// and the store's TestMigratePGFresh drops+recreates encode_test
	// mid-suite — which would kill this test's connections if it shared
	// the same DB. A separate database isolates this test from that drop.
	// Created from the env DSN by swapping the database name.
	importDSN, err := dedicatedImportDSN(pgDSN)
	if err != nil {
		t.Fatalf("derive import DSN: %v", err)
	}
	if err := dropAndRecreateDB(pgDSN, importDSN); err != nil {
		t.Fatalf("create encode_import_test: %v", err)
	}
	// Drop the dedicated DB in cleanup so repeated runs start clean and no
	// stray DB accumulates on the test cluster.
	t.Cleanup(func() { _ = dropDB(importDSN) })

	tgtDB, err := sql.Open("pgx", importDSN)
	if err != nil {
		t.Fatalf("open pg raw: %v", err)
	}
	defer tgtDB.Close()
	// The dedicated DB was just created (empty). store.Open below (run by
	// the importer) creates the schema via migratePG, so no pre-truncate is
	// needed. truncateAll would fail here (no tables yet).

	// --- Run the importer (fresh target, no -force needed) ---
	code, err := run(ctx, sqlitePath, importDSN, false /*force*/, false /*dryRun*/)
	if err != nil {
		t.Fatalf("first import run: code=%d err=%v", code, err)
	}
	if code != 0 {
		t.Fatalf("first import run: non-zero exit %d", code)
	}

	// --- Open raw SQLite handle for count comparisons ---
	srcDB, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		t.Fatalf("open sqlite raw: %v", err)
	}
	defer srcDB.Close()

	// --- Verify per-table row counts match ---
	for _, tbl := range fixture.tables {
		s := countRowsT(t, ctx, srcDB, tbl)
		g := countRowsT(t, ctx, tgtDB, tbl)
		if s != g {
			t.Errorf("table %s: sqlite=%d pg=%d (mismatch)", tbl, s, g)
		}
	}

	// --- Spot-check a job round-trips via store.GetJob ---
	pgStore, err := store.Open(importDSN)
	if err != nil {
		t.Fatalf("open pg store for verify: %v", err)
	}
	defer pgStore.Close()
	job, err := pgStore.GetJob(ctx, fixture.doneJobID)
	if err != nil {
		t.Fatalf("GetJob(%d) on PG: %v", fixture.doneJobID, err)
	}
	if job.Status != model.JobDone {
		t.Fatalf("imported job status = %s, want done", job.Status)
	}
	if len(job.StepTimings) != 1 || job.StepTimings[0].Step != "encode" {
		t.Fatalf("imported job step timings lost: %+v", job.StepTimings)
	}
	if job.Metrics["duration_sec"] != 300 {
		t.Fatalf("imported job metrics lost: %+v", job.Metrics)
	}
	// Timestamps parse: GetJob parses TEXT timestamps into *time.Time; for a
	// done job both StartedAt and FinishedAt must be non-nil.
	if job.StartedAt == nil || job.FinishedAt == nil {
		t.Fatalf("imported job timestamps nil: started=%v finished=%v", job.StartedAt, job.FinishedAt)
	}

	// --- Settings JSON round-trips via store.GetSettings ---
	st, err := pgStore.GetSettings(ctx)
	if err != nil || st == nil {
		t.Fatalf("GetSettings on PG: %v %v", st, err)
	}
	if st.ControllerURL != "http://ctrl:8080" {
		t.Fatalf("settings controller_url = %q", st.ControllerURL)
	}
	if st.LinuxBinDir != "/opt/encode/bin" {
		t.Fatalf("settings linux_bin_dir = %q", st.LinuxBinDir)
	}
	if st.LinuxScriptsDir != "/opt/encode/scripts" {
		t.Fatalf("settings linux_scripts_dir = %q", st.LinuxScriptsDir)
	}

	// --- Share password decrypts via store.GetShare on PG ---
	sh, err := pgStore.GetShare(ctx, fixture.shareID)
	if err != nil {
		t.Fatalf("GetShare(%d) on PG: %v", fixture.shareID, err)
	}
	if sh.Password != "s3cret" {
		t.Fatalf("imported share password = %q, want s3cret (key file not wired?)", sh.Password)
	}

	// --- Key aggregate check: CountJobsByStatus on PG ---
	counts, err := pgStore.CountJobsByStatus(ctx)
	if err != nil {
		t.Fatalf("CountJobsByStatus on PG: %v", err)
	}
	jobTotal := 0
	for _, n := range counts {
		jobTotal += n
	}
	if jobTotal != 3 {
		t.Fatalf("CountJobsByStatus total = %d, want 3 jobs", jobTotal)
	}

	// --- Idempotence: second run WITHOUT -force must refuse ---
	code, err = run(ctx, sqlitePath, importDSN, false, false)
	if err == nil || code == 0 {
		t.Fatalf("second import without -force should have refused; got code=%d err=%v", code, err)
	}

	// --- Second run WITH -force truncates and re-imports cleanly ---
	code, err = run(ctx, sqlitePath, importDSN, true /*force*/, false /*dryRun*/)
	if err != nil {
		t.Fatalf("second import with -force: code=%d err=%v", code, err)
	}
	if code != 0 {
		t.Fatalf("second import with -force: non-zero exit %d", code)
	}
	// Counts still match after the force re-import.
	for _, tbl := range fixture.tables {
		s := countRowsT(t, ctx, srcDB, tbl)
		g := countRowsT(t, ctx, tgtDB, tbl)
		if s != g {
			t.Errorf("after -force, table %s: sqlite=%d pg=%d (mismatch)", tbl, s, g)
		}
	}

	// --- dry-run writes nothing ---
	// Truncate the target so it's empty, then dry-run must leave it empty.
	truncateAll(t, ctx, tgtDB)
	code, err = run(ctx, sqlitePath, importDSN, false, true /*dryRun*/)
	if err != nil {
		t.Fatalf("dry-run: code=%d err=%v", code, err)
	}
	if code != 0 {
		t.Fatalf("dry-run: non-zero exit %d", code)
	}
	for _, tbl := range fixture.tables {
		n := countRowsT(t, ctx, tgtDB, tbl)
		if n != 0 {
			t.Fatalf("dry-run wrote rows to %s: %d", tbl, n)
		}
	}

	// --- The dedicated DB is dropped in cleanup (dropDB registered above),
	// so no truncate is needed here. The drop+recreate on the next run
	// starts from a pristine empty DB. ---
}

// fixtureData holds the ids and table list the assertions need.
type fixtureData struct {
	doneJobID int64
	shareID   int64
	tables    []string
}

// buildSQLiteFixture creates a SQLite store at sqlitePath via the real store
// API and populates it with one row of each kind the importer must carry:
// nodes (incl. a linux node), a flow, jobs in several statuses with step
// timings, settings with linux_* fields, a share with a password, audit rows,
// metrics rows, a user, and an API token. Returns the fixture data.
func buildSQLiteFixture(t *testing.T, ctx context.Context, sqlitePath string) fixtureData {
	t.Helper()
	s, err := store.Open(sqlitePath)
	if err != nil {
		t.Fatalf("open sqlite fixture: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// Nodes: one Windows, one Linux.
	winNode, err := s.CreateNode(ctx, "fix-win", "hash-win")
	if err != nil {
		t.Fatalf("create win node: %v", err)
	}
	linuxNode, err := s.CreateNode(ctx, "fix-linux", "hash-linux")
	if err != nil {
		t.Fatalf("create linux node: %v", err)
	}
	linuxNode.Platform = model.PlatformLinux
	if err := s.UpdateNode(ctx, linuxNode); err != nil {
		t.Fatalf("set linux platform: %v", err)
	}

	// Flow.
	flow, err := s.CreateFlow(ctx, &model.Flow{
		Name:  "fix-flow",
		Steps: []model.Step{{Type: model.StepEncode}},
	})
	if err != nil {
		t.Fatalf("create flow: %v", err)
	}

	// Job 1: done with step timings + metrics (the spot-checked job).
	doneJob, err := s.CreateJob(ctx, &model.Job{
		Series: "FixShow", Episode: "01", EpisodeDir: "FixShow/Ep 01",
		ScriptType: "vpy", FlowID: flow.ID,
	})
	if err != nil {
		t.Fatalf("create done job: %v", err)
	}
	if err := s.AssignJob(ctx, doneJob.ID, winNode.ID); err != nil {
		t.Fatalf("assign done job: %v", err)
	}
	if err := s.UpdateJobStatus(ctx, doneJob.ID, model.JobRunning, "encode", 42.5, "tail"); err != nil {
		t.Fatalf("update done job to running: %v", err)
	}
	timings := []model.StepTiming{{Step: "encode", DurationSec: 120.5}}
	metrics := map[string]float64{"duration_sec": 300, "fps": 25}
	if err := s.FinishJobWithReport(ctx, doneJob.ID, model.JobDone, 0, "", []string{"out.mkv"}, "tail", "fulllog", timings, metrics); err != nil {
		t.Fatalf("finish done job: %v", err)
	}

	// Job 2: failed.
	failJob, err := s.CreateJob(ctx, &model.Job{
		Series: "FixShow", Episode: "02", EpisodeDir: "FixShow/Ep 02",
		ScriptType: "vpy", FlowID: flow.ID,
	})
	if err != nil {
		t.Fatalf("create fail job: %v", err)
	}
	if err := s.AssignJob(ctx, failJob.ID, linuxNode.ID); err != nil {
		t.Fatalf("assign fail job: %v", err)
	}
	if err := s.FinishJob(ctx, failJob.ID, model.JobFailed, 1, "boom", nil, "tail"); err != nil {
		t.Fatalf("finish fail job: %v", err)
	}

	// Job 3: pending (the queue state).
	if _, err := s.CreateJob(ctx, &model.Job{
		Series: "FixShow", Episode: "03", EpisodeDir: "FixShow/Ep 03",
		ScriptType: "vpy", FlowID: flow.ID,
	}); err != nil {
		t.Fatalf("create pending job: %v", err)
	}

	// Settings with linux_* fields.
	if err := s.SaveSettings(ctx, &model.Settings{
		ControllerURL:       "http://ctrl:8080",
		NFSServer:           "nas01",
		ScriptsShare:        "/scripts",
		ReleaseShare:        "/release",
		LinuxBinDir:         "/opt/encode/bin",
		LinuxScriptsDir:     "/opt/encode/scripts",
		LinuxReleaseDir:     "/opt/encode/release",
		ScanIntervalSeconds: 30,
		TasksBeforeReboot:   10,
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	// Share with a password (exercises the AES-GCM key file path).
	sh, err := s.CreateShare(ctx, &model.Share{
		Name: "fix-share", Kind: model.ShareSMB, Role: model.ShareRoleScripts,
		Server: "smb-host", Path: "share", Username: "u", Password: "s3cret",
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create share: %v", err)
	}

	// Audit rows.
	if err := s.AppendAudit(ctx, "admin", "create", "node/fix-win", "created fix-win"); err != nil {
		t.Fatalf("append audit 1: %v", err)
	}
	if err := s.AppendAudit(ctx, "admin", "create", "node/fix-linux", "created fix-linux"); err != nil {
		t.Fatalf("append audit 2: %v", err)
	}

	// Metrics rows.
	if err := s.InsertNodeMetric(ctx, winNode.ID, &model.NodeMetrics{
		CPUPct: 55.0, MemUsedMB: 1024, MemTotalMB: 4096, DiskFreeGB: 100,
	}); err != nil {
		t.Fatalf("insert metric: %v", err)
	}

	// User + API token.
	if _, err := s.CreateUser(ctx, "admin", "$bcrypt$hash", "admin"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := s.CreateAPIToken(ctx, &model.APIToken{
		Name: "fix-token", Scope: model.TokenScopeAdmin, TokenHash: "tok-hash-1",
	}); err != nil {
		t.Fatalf("create api token: %v", err)
	}

	return fixtureData{
		doneJobID: doneJob.ID,
		shareID:   sh.ID,
		tables: []string{
			"settings", "nodes", "flows", "users", "api_tokens",
			"pairing_codes", "audit_events", "step_templates", "series",
			"shares", "provision_runs", "sessions", "jobs", "node_metrics",
		},
	}
}

// allPGTables is every data table in the PG schema (reverse FK order so
// TRUNCATE CASCADE clears dependents). Matches the list in
// store_contract_test.go's openTestStorePG cleanup.
var allPGTables = []string{
	"node_metrics", "audit_events", "pairing_codes", "provision_runs",
	"sessions", "users", "api_tokens", "shares", "settings",
	"step_templates", "series", "jobs", "flows", "nodes",
}

// truncateAll truncates every data table (RESTART IDENTITY CASCADE). Used to
// reset the target between import runs without dropping the database (which
// would race with other tests' pool connections).
func truncateAll(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, tbl := range allPGTables {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("TRUNCATE TABLE %s RESTART IDENTITY CASCADE", tbl)); err != nil {
			t.Fatalf("truncate %s: %v", tbl, err)
		}
	}
}

// countRowsT counts the rows in a table, failing the test on error.
func countRowsT(t *testing.T, ctx context.Context, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %q", table)).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// dedicatedImportDSN returns a DSN pointing at a dedicated
// "encode_import_test" database (parsed from the source DSN by replacing
// the database name). This isolates the import test from the store's
// encode_test database, which TestMigratePGFresh drops+recreates mid-suite
// when both packages run in parallel under `go test ./pkg1/ ./pkg2/`.
func dedicatedImportDSN(dsn string) (string, error) {
	rest := dsn
	if i := stringsIndex(rest, "://"); i < 0 {
		return "", fmt.Errorf("not a postgres DSN: %q", dsn)
	} else {
		rest = rest[i+3:]
	}
	// userinfo@hostport/dbname?params
	slash := stringsIndex(rest, "/")
	if slash < 0 {
		return "", fmt.Errorf("no database in DSN: %q", dsn)
	}
	prefix := dsn[:stringsIndex(dsn, "://")+3] // scheme://
	// Rebuild: prefix + rest[:slash] + "/encode_import_test" + querystring
	hostAndUser := rest[:slash]
	pathAndQuery := rest[slash+1:]
	query := ""
	if q := stringsIndex(pathAndQuery, "?"); q >= 0 {
		query = pathAndQuery[q:]
	}
	return prefix + hostAndUser + "/encode_import_test" + query, nil
}

// dropAndRecreateDB connects to the maintenance ("postgres") database via
// the env DSN, terminates any open connections to the target DB, then drops
// and recreates it. The target DSN is the dedicated import DSN (its db
// name is encode_import_test).
func dropAndRecreateDB(envDSN, targetDSN string) error {
	dbName, maintDSN, err := maintenanceConn(envDSN)
	if err != nil {
		return err
	}
	// The target db name comes from the target DSN (encode_import_test), not
	// the env DSN (encode_test).
	targetName, err := dbNameFromDSN(targetDSN)
	if err != nil {
		return err
	}
	maintDB, err := sql.Open("pgx", maintDSN)
	if err != nil {
		return fmt.Errorf("open maintenance: %w", err)
	}
	defer maintDB.Close()
	ctx := context.Background()
	// Terminate lingering connections to the target DB before dropping it
	// (else DROP DATABASE fails with "in use"). Safe on a test cluster.
	_, _ = maintDB.ExecContext(ctx, fmt.Sprintf(
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='%s' AND pid <> pg_backend_pid()`,
		targetName))
	_, _ = maintDB.ExecContext(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS "%s"`, targetName))
	if _, err := maintDB.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE "%s"`, targetName)); err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	_ = dbName // env db name, unused (kept for clarity)
	return nil
}

// dropDB drops the target DB (used in cleanup so repeated runs start clean).
func dropDB(targetDSN string) error {
	// Re-derive the env DSN's maintenance connection from the target DSN by
	// swapping its db name back to "postgres".
	maintDSN, err := postgresMaintDSN(targetDSN)
	if err != nil {
		return err
	}
	targetName, err := dbNameFromDSN(targetDSN)
	if err != nil {
		return err
	}
	maintDB, err := sql.Open("pgx", maintDSN)
	if err != nil {
		return fmt.Errorf("open maintenance: %w", err)
	}
	defer maintDB.Close()
	ctx := context.Background()
	_, _ = maintDB.ExecContext(ctx, fmt.Sprintf(
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='%s' AND pid <> pg_backend_pid()`,
		targetName))
	_, _ = maintDB.ExecContext(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS "%s"`, targetName))
	return nil
}

// maintenanceConn returns (envDbName, maintDSN) by parsing the env DSN and
// pointing a maintenance connection at the "postgres" database.
func maintenanceConn(dsn string) (dbName, maintDSN string, err error) {
	dbName, err = dbNameFromDSN(dsn)
	if err != nil {
		return "", "", err
	}
	maintDSN, err = postgresMaintDSN(dsn)
	if err != nil {
		return "", "", err
	}
	return dbName, maintDSN, nil
}

// dbNameFromDSN extracts the database name from a postgres:// DSN.
func dbNameFromDSN(dsn string) (string, error) {
	rest := dsn
	if i := stringsIndex(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	slash := stringsIndex(rest, "/")
	if slash < 0 {
		return "", fmt.Errorf("no database in DSN: %q", dsn)
	}
	pathPart := rest[slash+1:]
	if q := stringsIndex(pathPart, "?"); q >= 0 {
		pathPart = pathPart[:q]
	}
	if pathPart == "" {
		return "", fmt.Errorf("empty database name in DSN: %q", dsn)
	}
	return pathPart, nil
}

// postgresMaintDSN builds a maintenance DSN pointing at the "postgres"
// database, preserving the env DSN's user/host/params.
func postgresMaintDSN(dsn string) (string, error) {
	scheme := "postgres://"
	if i := stringsIndex(dsn, "://"); i >= 0 {
		scheme = dsn[:i+3]
		rest := dsn[i+3:]
		// userinfo@hostport
		slash := stringsIndex(rest, "/")
		if slash < 0 {
			// no path — just append /postgres
			query := ""
			if q := stringsIndex(rest, "?"); q >= 0 {
				query = rest[q:]
			}
			host := rest
			if q := stringsIndex(host, "?"); q >= 0 {
				host = host[:q]
			}
			return scheme + host + "/postgres" + query, nil
		}
		hostAndUser := rest[:slash]
		pathAndQuery := rest[slash+1:]
		query := ""
		if q := stringsIndex(pathAndQuery, "?"); q >= 0 {
			query = pathAndQuery[q:]
		}
		return scheme + hostAndUser + "/postgres" + query, nil
	}
	return "", fmt.Errorf("not a postgres DSN: %q", dsn)
}

// stringsIndex finds the first index of substr in s, or -1.
func stringsIndex(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
