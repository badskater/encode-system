package api

import (
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/auth"
	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/store"
	"github.com/badskater/encode-system/backend/internal/update"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// newBackupTestEnv builds a Server with a BackupManager pointing at a temp
// backups dir, suitable for SQLite restore round-trip tests. exitFn is
// overridden so the restore handler doesn't kill the test binary.
func newBackupTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	up, err := update.NewStore(dir + "/updates")
	if err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		AdminUsername: "admin", AdminPassword: adminPassword,
		ScriptsRoot: dir + "/scripts", ReleaseRoot: dir + "/release",
		NodeBinDir: `C:\bin`, NodeScriptsDir: `C:\Encodes\scripts`, NodeReleaseDir: `C:\Encodes\ReleaseFolders`,
		Group: "OldFartsSubs", Tag: "1080p", TasksBeforeReboot: 10,
	}
	s, err := New(st, up, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Mint admin session.
	u, err := st.UserByUsername(ctxBg(), "admin")
	if err != nil || u == nil {
		t.Fatal("admin not seeded")
	}
	sessTok, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctxBg(), auth.HashToken(sessTok), u.ID, time.Now().UTC().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	adminTok = sessTok

	s.Backup = &BackupManager{
		Store:    st,
		Postgres: false,
		Dir:      filepath.Join(dir, "backups"),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Enabled:  false,
		Every:    time.Hour,
	}

	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)
	return &testEnv{server: s}
}

// TestBackupRestoreSQLiteRoundTrip: seed → backup → mutate → restore →
// old rows are back and a pre-restore snapshot exists.
func TestBackupRestoreSQLiteRoundTrip(t *testing.T) {
	// Override exitFn so the restore handler doesn't kill the test binary.
	origExit := exitFn
	exitFn = func(int) {} // no-op
	t.Cleanup(func() { exitFn = origExit })

	e := newBackupTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Seed: create a node.
	node, err := e.server.Store.CreateNode(ctx, "node-restore-1", auth.HashToken("tok-1"))
	if err != nil {
		t.Fatal(err)
	}

	// Take a backup.
	r1, b1 := doJSON(t, "POST", ts.URL+"/api/backup", adminTok, nil)
	if r1.StatusCode != 201 {
		t.Fatalf("backup now: %d %s", r1.StatusCode, b1)
	}
	var snap struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b1, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Name == "" {
		t.Fatal("no backup name in response")
	}

	// Mutate: delete the node.
	if err := e.server.Store.DeleteNode(ctx, node.ID); err != nil {
		t.Fatal(err)
	}
	// Verify it's gone.
	if n, _ := e.server.Store.GetNode(ctx, node.ID); n != nil {
		t.Fatal("node should be gone after delete")
	}

	// Restore the backup.
	r2, b2 := doJSON(t, "POST", ts.URL+"/api/backup/"+snap.Name+"/restore", adminTok, nil)
	if r2.StatusCode != 200 {
		t.Fatalf("restore: %d %s", r2.StatusCode, b2)
	}

	// Reopen the store from the same DB path to read the restored data.
	// The restore swapped the file; the old *sql.DB handle may have cached
	// pages. Re-open a fresh connection to the file.
	dbPath := e.server.Store.DBPath()
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	// The node should be back (the backup was taken before the delete).
	n, err := st2.GetNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("get node after restore: %v", err)
	}
	if n == nil {
		t.Fatal("node not restored — backup should have contained it")
	}
	if n.Name != "node-restore-1" {
		t.Fatalf("restored node name: want node-restore-1, got %q", n.Name)
	}

	// Pre-restore snapshot should exist beside the db file.
	pattern := dbPath + ".pre-restore-*"
	matches, _ := filepath.Glob(pattern)
	if len(matches) == 0 {
		t.Fatalf("no pre-restore snapshot found matching %s", pattern)
	}
}

// TestBackupRestorePendingJob409: a pending job blocks restore.
func TestBackupRestorePendingJob409(t *testing.T) {
	origExit := exitFn
	exitFn = func(int) {}
	t.Cleanup(func() { exitFn = origExit })

	e := newBackupTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Take a backup.
	r1, b1 := doJSON(t, "POST", ts.URL+"/api/backup", adminTok, nil)
	if r1.StatusCode != 201 {
		t.Fatalf("backup now: %d %s", r1.StatusCode, b1)
	}
	var snap struct {
		Name string `json:"name"`
	}
	json.Unmarshal(b1, &snap)

	// Create a pending job.
	fl, err := e.server.Store.FlowByName(ctx, e.server.Cfg.DefaultFlowName)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.server.Store.CreateJob(ctx, &model.Job{
		Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Restore should be blocked.
	r2, b2 := doJSON(t, "POST", ts.URL+"/api/backup/"+snap.Name+"/restore", adminTok, nil)
	if r2.StatusCode != 409 {
		t.Fatalf("restore with pending job: want 409, got %d %s", r2.StatusCode, b2)
	}
}

// TestBackupRestoreWrongExtension409: a .dump file can't restore on SQLite.
func TestBackupRestoreWrongExtension409(t *testing.T) {
	e := newBackupTestEnv(t)
	ts := e.serve(t)

	// Create a fake .dump file in the backups dir.
	if err := os.MkdirAll(e.server.Backup.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakePath := filepath.Join(e.server.Backup.Dir, "fake.dump")
	if err := os.WriteFile(fakePath, []byte("not a real dump"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, b := doJSON(t, "POST", ts.URL+"/api/backup/fake.dump/restore", adminTok, nil)
	if r.StatusCode != 409 {
		t.Fatalf("wrong extension restore: want 409, got %d %s", r.StatusCode, b)
	}
}

// TestBackupRestoreTraversal400: path traversal in the name is rejected.
func TestBackupRestoreTraversal400(t *testing.T) {
	e := newBackupTestEnv(t)
	ts := e.serve(t)

	// Various traversal attempts.
	for _, name := range []string{"../etc/passwd", "..%2fetc%2fpasswd", "foo/../bar.db"} {
		r, _ := doJSON(t, "POST", ts.URL+"/api/backup/"+name+"/restore", adminTok, nil)
		if r.StatusCode != 400 && r.StatusCode != 404 {
			t.Errorf("traversal %q: want 400 or 404, got %d", name, r.StatusCode)
		}
	}
}

// TestBackupRestoreNotFound404: nonexistent backup name → 404.
func TestBackupRestoreNotFound404(t *testing.T) {
	e := newBackupTestEnv(t)
	ts := e.serve(t)

	r, _ := doJSON(t, "POST", ts.URL+"/api/backup/nonexistent.db/restore", adminTok, nil)
	if r.StatusCode != 404 {
		t.Fatalf("nonexistent backup: want 404, got %d", r.StatusCode)
	}
}

// TestBackupRestorePGRoundTrip: seed → dump → mutate → restore → old rows
// back. Only runs under ENCODE_TEST_PG_DSN.
func TestBackupRestorePGRoundTrip(t *testing.T) {
	dsn := os.Getenv("ENCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("ENCODE_TEST_PG_DSN not set; skipping PG restore test")
	}

	dir := t.TempDir()
	st, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// Clean slate: truncate all tables so the test is deterministic.
	pgDump0, err := openPGDump(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cleanPGTables(t, pgDump0)
	pgDump0.Close()

	up, err := update.NewStore(dir + "/updates")
	if err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		AdminUsername: "admin", AdminPassword: adminPassword,
		ScriptsRoot: dir + "/scripts", ReleaseRoot: dir + "/release",
		NodeBinDir: `C:\bin`, NodeScriptsDir: `C:\Encodes\scripts`, NodeReleaseDir: `C:\Encodes\ReleaseFolders`,
		Group: "OldFartsSubs", Tag: "1080p", TasksBeforeReboot: 10,
	}
	s, err := New(st, up, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Mint admin session.
	u, err := st.UserByUsername(ctxBg(), "admin")
	if err != nil || u == nil {
		t.Fatal("admin not seeded")
	}
	sessTok, _ := auth.NewToken()
	if err := st.CreateSession(ctxBg(), auth.HashToken(sessTok), u.ID, time.Now().UTC().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Open a second *sql.DB for the PG dump/restore handle.
	pgDump, err := openPGDump(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pgDump.Close() })

	s.Backup = &BackupManager{
		Store:    st,
		Postgres: true,
		PGDump:   pgDump,
		Dir:      filepath.Join(dir, "backups"),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Enabled:  false,
		Every:    time.Hour,
	}

	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)
	ctx := ctxBg()

	// Seed: create a node.
	node, err := st.CreateNode(ctx, "pg-restore-node", auth.HashToken("tok-pg-1"))
	if err != nil {
		t.Fatal(err)
	}

	// Take a backup (PG dump).
	info, err := s.Backup.TakeSnapshot(ctx, false)
	if err != nil {
		t.Fatalf("take snapshot: %v", err)
	}

	// Mutate: delete the node.
	if err := st.DeleteNode(ctx, node.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.GetNode(ctx, node.ID); n != nil {
		t.Fatal("node should be gone after delete")
	}

	// Restore the dump.
	r2, b2 := doJSON(t, "POST", ts.URL+"/api/backup/"+info.Name+"/restore", sessTok, nil)
	if r2.StatusCode != 200 {
		t.Fatalf("PG restore: %d %s", r2.StatusCode, b2)
	}

	// The node should be back.
	n, err := st.GetNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("get node after PG restore: %v", err)
	}
	if n == nil {
		t.Fatal("node not restored after PG restore")
	}
	if n.Name != "pg-restore-node" {
		t.Fatalf("restored node name: want pg-restore-node, got %q", n.Name)
	}
}

// cleanPGTables truncates all tables in the PG store for a clean test slate.
func cleanPGTables(t *testing.T, db *sql.DB) {
	t.Helper()
	for i := len(pgBackupTables) - 1; i >= 0; i-- {
		table := pgBackupTables[i]
		_, err := db.Exec("TRUNCATE TABLE " + quoteIdent(table) + " RESTART IDENTITY CASCADE")
		if err != nil {
			t.Logf("truncate %s: %v (ok if table absent)", table, err)
		}
	}
}

// openPGDump opens a *sql.DB over the DSN for the PG backup dump/restore
// handle (same pattern as main.go's pgDump).
func openPGDump(dsn string) (*sql.DB, error) {
	return sql.Open("pgx", dsn)
}
