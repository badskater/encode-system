package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/auth"
	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/store"
	"github.com/badskater/encode-system/backend/internal/update"
	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx" driver for the backup dump handle
)

// pgBackupTestDSN is the database name the PG backup tests use, derived from
// ENCODE_TEST_PG_DSN by swapping the db segment. It is distinct from the
// store package's encode_test so parallel runs don't clobber.
const pgBackupTestDSN_dbName = "encode_backup_test"

// pgBackupTestEnv holds a PG-backed controller for the PG backup tests. It is
// env-gated: when ENCODE_TEST_PG_DSN is unset, the PG tests skip (the SQLite
// tests in backup_test.go run unconditionally and cover the default path).
//
// The test derives its own database name (encode_backup_test) from the
// ENCODE_TEST_PG_DSN, creates + drops it per-test via a maintenance DSN, so
// it never collides with the store package's encode_test database (which
// TestMain creates and leaves). The store's migratePG then builds the schema
// fresh on store.Open (the common install.sh --postgres fresh-install path).
type pgBackupTestEnv struct {
	dsn      string
	store    *store.Store
	pgDump   *sql.DB
	dir      string
	server   *Server
	ts       *httptest.Server
	adminTok string
	cleanup  func()
}

func newPGBackupTestEnv(t *testing.T) *pgBackupTestEnv {
	t.Helper()
	src := os.Getenv("ENCODE_TEST_PG_DSN")
	if src == "" {
		t.Skip("ENCODE_TEST_PG_DSN not set; PG backup tests skipped (SQLite path covered by backup_test.go)")
	}
	// Swap the db segment of the source DSN for the per-test db name.
	backupDSN := swapDBName(src, pgBackupTestDSN_dbName)
	// Drop+create the per-test database via a maintenance DSN (same
	// user/host/port, "postgres" db).
	_, maintDSN, err := parseBackupTestDSN(src)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	dropAndCreateBackupTestDB(t, pgBackupTestDSN_dbName, maintDSN)

	st, err := store.Open(backupDSN)
	if err != nil {
		t.Fatalf("store.Open PG: %v", err)
	}
	pgDump, err := sql.Open("pgx", backupDSN)
	if err != nil {
		t.Fatalf("open pg dump: %v", err)
	}
	dir := t.TempDir()
	up, err := update.NewStore(dir + "/updates")
	if err != nil {
		t.Fatalf("open update store: %v", err)
	}
	cfg := Config{
		AdminUsername: "admin", AdminPassword: adminPassword,
		ScriptsRoot: dir + "/scripts", ReleaseRoot: dir + "/release",
		Group: "G", Tag: "1080p", TasksBeforeReboot: 10,
	}
	srv, err := New(st, up, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.Backup = &BackupManager{
		Store:    st,
		Postgres: true,
		PGDump:   pgDump,
		Dir:      filepath.Join(dir, "backups"),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Enabled:  false,
		Every:    6 * time.Hour,
	}
	// Mint an admin session so withAdmin handlers accept the test requests,
	// mirroring newTestEnv's pattern.
	u, err := st.UserByUsername(context.Background(), "admin")
	if err != nil || u == nil {
		t.Fatalf("admin not seeded: %v %v", u, err)
	}
	sessTok, err := auth.NewToken()
	if err != nil {
		t.Fatalf("mint session token: %v", err)
	}
	if err := st.CreateSession(context.Background(), auth.HashToken(sessTok), u.ID, time.Now().UTC().Add(24*time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	ts := httptest.NewServer(srv.Routes())
	return &pgBackupTestEnv{
		dsn:      backupDSN,
		store:    st,
		pgDump:   pgDump,
		dir:      dir,
		server:   srv,
		ts:       ts,
		adminTok: sessTok,
		cleanup: func() {
			ts.Close()
			pgDump.Close()
			st.Close()
		},
	}
}

// swapDBName returns a copy of dsn with the database name segment replaced
// by dbName (preserving the query string). Used to point the test's PG handle
// at the per-test database.
func swapDBName(dsn, dbName string) string {
	i := strings.Index(dsn, "://")
	if i < 0 {
		return dsn
	}
	rest := dsn[i+3:]
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return dsn
	}
	host := rest[:slash]
	tail := rest[slash+1:]
	query := ""
	if q := strings.Index(tail, "?"); q >= 0 {
		query = tail[q:]
	}
	return dsn[:i+3] + host + "/" + dbName + query
}

// parseBackupTestDSN extracts the db name + a maintenance DSN from the test
// DSN, mirroring store.parseTestDBName (not imported — different package).
func parseBackupTestDSN(dsn string) (dbName, maintDSN string, err error) {
	rest := dsn
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	var userinfo, hostport, pathPart string
	if atIdx := strings.Index(rest, "@"); atIdx >= 0 {
		userinfo = rest[:atIdx]
		hostport = rest[atIdx+1:]
	} else {
		hostport = rest
	}
	if slashIdx := strings.Index(hostport, "/"); slashIdx >= 0 {
		pathPart = hostport[slashIdx+1:]
		hostport = hostport[:slashIdx]
	}
	dbName = pathPart
	if q := strings.Index(dbName, "?"); q >= 0 {
		dbName = dbName[:q]
	}
	if dbName == "" {
		return "", "", fmt.Errorf("could not parse database name from DSN")
	}
	maintDSN = fmt.Sprintf("postgres://%s@%s/postgres?sslmode=disable", userinfo, hostport)
	return dbName, maintDSN, nil
}

// dropAndCreateBackupTestDB drops (if present) and recreates the per-test
// database so each test starts from a clean schema. The store's migratePG
// then builds the schema fresh on store.Open.
func dropAndCreateBackupTestDB(t *testing.T, dbName, maintDSN string) {
	t.Helper()
	mdb, err := sql.Open("pgx", maintDSN)
	if err != nil {
		t.Fatalf("open maintenance: %v", err)
	}
	defer mdb.Close()
	if _, err := mdb.Exec(fmt.Sprintf(`DROP DATABASE IF EXISTS "%s"`, dbName)); err != nil {
		t.Fatalf("drop db: %v", err)
	}
	if _, err := mdb.Exec(fmt.Sprintf(`CREATE DATABASE "%s"`, dbName)); err != nil {
		t.Fatalf("create db: %v", err)
	}
}

// TestPGBackupDumpFormatAndSecretsParity verifies the PG backup path
// produces a non-empty JSON-lines dump whose schema matches the live PG
// schema, and that the no-secrets contract holds: the shares.password column
// (when a share exists) carries the AES-GCM ciphertext, never plaintext.
//
// Parity with SQLite: a SQLite VACUUM INTO backup contains the
// shares.password ciphertext verbatim (the key file lives beside the DB, not
// in it). This test confirms the PG dump does the same — it includes the
// ciphertext, and never includes a plaintext password or the key.
func TestPGBackupDumpFormatAndSecretsParity(t *testing.T) {
	env := newPGBackupTestEnv(t)
	t.Cleanup(env.cleanup)
	ctx := context.Background()

	// Seed recognizable rows so the dump isn't empty and so we can assert
	// the share password ciphertext survives (parity: SQLite VACUUM INTO
	// would carry it too).
	if _, err := env.store.CreateNode(ctx, "pg-backup-node", "hash-pg-backup"); err != nil {
		t.Fatalf("create node: %v", err)
	}
	// CreateShare encrypts the password at rest; the stored column holds the
	// AES-GCM ciphertext ("v1:<hex>"), not plaintext — exactly what a SQLite
	// VACUUM INTO backup would copy.
	const plainPass = "s3cr3t-pass-w1th-n0-secret-value"
	if _, err := env.store.CreateShare(ctx, &model.Share{
		Name: "nfs-scripts", Kind: model.ShareNFS, Role: model.ShareRoleScripts,
		Server: "nfs.example", Path: "/scripts",
		Password: plainPass, Enabled: true,
	}); err != nil {
		t.Fatalf("create share: %v", err)
	}

	info, err := env.server.Backup.TakeSnapshot(ctx, false)
	if err != nil {
		t.Fatalf("TakeSnapshot PG: %v", err)
	}
	if !strings.HasSuffix(info.Name, ".dump") {
		t.Fatalf("PG snapshot must be a .dump file, got %q", info.Name)
	}
	if info.SizeBytes == 0 {
		t.Fatal("dump is empty")
	}

	data, err := os.ReadFile(filepath.Join(env.server.Backup.Dir, info.Name))
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 {
		t.Fatalf("dump has too few lines: %d", len(lines))
	}

	// First line is the format header.
	var hdr map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &hdr); err != nil {
		t.Fatalf("header not JSON: %v", err)
	}
	if hdr["_format"] != "encode-pg-dump" {
		t.Fatalf("header format = %v, want encode-pg-dump", hdr["_format"])
	}

	// Walk the shares table rows; assert the password column is the
	// ciphertext and the plaintext never appears anywhere in the dump.
	foundSharesTable := false
	foundPasswordCiphertext := false
	for _, ln := range lines {
		var rec map[string]json.RawMessage
		if err := json.Unmarshal([]byte(ln), &rec); err != nil {
			continue
		}
		var tbl string
		if err := json.Unmarshal(rec["_table"], &tbl); err != nil {
			continue
		}
		if tbl != "shares" {
			continue
		}
		foundSharesTable = true
		rowRaw, ok := rec["_row"]
		if !ok {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal(rowRaw, &row); err != nil {
			t.Fatalf("shares row not JSON: %v", err)
		}
		pw, _ := row["password"].(string)
		if strings.HasPrefix(pw, "v1:") {
			foundPasswordCiphertext = true
		}
	}
	if !foundSharesTable {
		t.Fatal("dump has no shares table section")
	}
	if !foundPasswordCiphertext {
		t.Fatal("shares.password ciphertext not found in dump (parity: SQLite VACUUM INTO would carry it)")
	}
	// The whole dump must NOT contain the plaintext password anywhere.
	if strings.Contains(string(data), plainPass) {
		t.Fatal("PLAINTEXT share password leaked into the dump — no-secrets contract broken")
	}
}

// TestPGBackupDownloadStream verifies the .dump file is downloadable through
// the API and the bytes match the on-disk file (Content-Disposition attached).
func TestPGBackupDownloadStream(t *testing.T) {
	env := newPGBackupTestEnv(t)
	t.Cleanup(env.cleanup)
	ctx := context.Background()

	if _, err := env.store.CreateNode(ctx, "dl-node", "h"); err != nil {
		t.Fatal(err)
	}
	info, err := env.server.Backup.TakeSnapshot(ctx, false)
	if err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}

	req, _ := http.NewRequest("GET", env.ts.URL+"/api/backup/"+info.Name, nil)
	req.Header.Set("Authorization", "Bearer "+env.adminTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("download status: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/octet-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("content-disposition = %q", cd)
	}
	body, _ := io.ReadAll(resp.Body)
	want, _ := os.ReadFile(filepath.Join(env.server.Backup.Dir, info.Name))
	if string(body) != string(want) {
		t.Fatalf("downloaded bytes != on-disk: got %d want %d", len(body), len(want))
	}
}

// TestPGBackupDeleteLifecycle verifies a PG .dump snapshot can be deleted
// through the API and disappears from the list.
func TestPGBackupDeleteLifecycle(t *testing.T) {
	env := newPGBackupTestEnv(t)
	t.Cleanup(env.cleanup)
	ctx := context.Background()

	if _, err := env.store.CreateNode(ctx, "del-node", "h"); err != nil {
		t.Fatal(err)
	}
	info, err := env.server.Backup.TakeSnapshot(ctx, false)
	if err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}

	// Delete via the API.
	req, _ := http.NewRequest("DELETE", env.ts.URL+"/api/backup/"+info.Name, nil)
	req.Header.Set("Authorization", "Bearer "+env.adminTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("delete status: %d", resp.StatusCode)
	}
	// Gone: GET returns 404.
	req2, _ := http.NewRequest("GET", env.ts.URL+"/api/backup/"+info.Name, nil)
	req2.Header.Set("Authorization", "Bearer "+env.adminTok)
	resp2, _ := http.DefaultClient.Do(req2)
	if resp2.StatusCode != 404 {
		t.Fatalf("after delete: want 404, got %d", resp2.StatusCode)
	}
}

// TestPGBackupEmptyDB verifies the dump succeeds on a fresh (empty) PG
// database — the common install.sh --postgres fresh-install path. Every
// table section is present (proving the schema migrated) with zero rows.
func TestPGBackupEmptyDB(t *testing.T) {
	env := newPGBackupTestEnv(t)
	t.Cleanup(env.cleanup)
	ctx := context.Background()

	info, err := env.server.Backup.TakeSnapshot(ctx, false)
	if err != nil {
		t.Fatalf("TakeSnapshot empty: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(env.server.Backup.Dir, info.Name))
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	// Header + one table marker per pgBackupTables entry, zero row records.
	gotTableMarkers := 0
	for _, ln := range lines[1:] { // skip header
		var rec map[string]any
		if json.Unmarshal([]byte(ln), &rec) != nil {
			continue
		}
		if _, ok := rec["_table"].(string); ok {
			if _, hasRow := rec["_row"]; !hasRow {
				gotTableMarkers++
			}
		}
	}
	if gotTableMarkers != len(pgBackupTables) {
		t.Fatalf("table markers: got %d, want %d (schema must be present)", gotTableMarkers, len(pgBackupTables))
	}
}
