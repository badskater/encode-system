package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// backupEnv builds a testEnv with a wired BackupManager (routes 503 when
// Backup is nil, so these tests need it set).
func backupEnv(t *testing.T) (*testEnv, *httptest.Server) {
	t.Helper()
	e := newTestEnv(t)
	ts := e.serve(t)
	e.server.Backup = &BackupManager{
		Store:   e.server.Store,
		Dir:     filepath.Join(t.TempDir(), "backups"),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Enabled: false,
		Every:   6 * time.Hour,
	}
	return e, ts
}

// TestBackupManualLifecycle: manual snapshot → list → download (valid
// SQLite bytes) → delete → 404.
func TestBackupManualLifecycle(t *testing.T) {
	_, ts := backupEnv(t)

	// Empty list first.
	resp, body := doJSON(t, "GET", ts.URL+"/api/backup", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var st struct {
		Status    map[string]any `json:"status"`
		Snapshots []struct {
			Name      string `json:"name"`
			SizeBytes int64  `json:"size_bytes"`
			Scheduled bool   `json:"scheduled"`
		} `json:"snapshots"`
	}
	json.Unmarshal(body, &st)
	if len(st.Snapshots) != 0 {
		t.Fatalf("want 0 snapshots, got %d", len(st.Snapshots))
	}
	if st.Status["enabled"] != false || st.Status["every_seconds"] != float64(21600) {
		t.Fatalf("status = %v", st.Status)
	}

	// Manual snapshot.
	resp, body = doJSON(t, "POST", ts.URL+"/api/backup", adminTok, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("take: %d %s", resp.StatusCode, body)
	}
	var info struct {
		Name      string `json:"name"`
		SizeBytes int64  `json:"size_bytes"`
		Scheduled bool   `json:"scheduled"`
	}
	json.Unmarshal(body, &info)
	if info.SizeBytes == 0 || info.Scheduled {
		t.Fatalf("bad info: %+v", info)
	}
	if !strings.Contains(info.Name, "-manual") {
		t.Fatalf("manual name = %q", info.Name)
	}

	// List shows it.
	_, body = doJSON(t, "GET", ts.URL+"/api/backup", adminTok, nil)
	if !strings.Contains(string(body), info.Name) {
		t.Fatalf("list missing %q: %s", info.Name, body)
	}

	// Download returns the SQLite header ("SQLite format 3").
	req, _ := http.NewRequest("GET", ts.URL+"/api/backup/"+info.Name, nil)
	req.Header.Set("Authorization", "Bearer "+adminTok)
	dl, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Body.Close()
	if dl.StatusCode != 200 {
		t.Fatalf("download: %d", dl.StatusCode)
	}
	head := make([]byte, 16)
	io.ReadFull(dl.Body, head)
	if string(head) != "SQLite format 3\x00" {
		t.Fatalf("not a SQLite file: %q", head)
	}
	if cd := dl.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("content-disposition = %q", cd)
	}

	// Path traversal rejected.
	resp, _ = doJSON(t, "GET", ts.URL+"/api/backup/..%2Ftest.db", adminTok, nil)
	if resp.StatusCode != 400 && resp.StatusCode != 404 {
		t.Fatalf("traversal: %d, want 400/404", resp.StatusCode)
	}

	// Delete → 204, then 404.
	if r2, _ := doJSON(t, "DELETE", ts.URL+"/api/backup/"+info.Name, adminTok, nil); r2.StatusCode != 204 {
		t.Fatalf("delete: %d", r2.StatusCode)
	}
	if r3, _ := doJSON(t, "GET", ts.URL+"/api/backup/"+info.Name, adminTok, nil); r3.StatusCode != 404 {
		t.Fatalf("after delete: %d, want 404", r3.StatusCode)
	}
	if r4, _ := doJSON(t, "DELETE", ts.URL+"/api/backup/"+info.Name, adminTok, nil); r4.StatusCode != 404 {
		t.Fatalf("delete twice: %d, want 404", r4.StatusCode)
	}
}

// TestBackupSettingsValidation: interval bounds + toggle persistence.
func TestBackupSettingsValidation(t *testing.T) {
	e, ts := backupEnv(t)

	// Out of bounds.
	if r1, _ := doJSON(t, "PUT", ts.URL+"/api/backup/settings", adminTok, map[string]any{"every_seconds": 60}); r1.StatusCode != 400 {
		t.Fatalf("60s: %d, want 400", r1.StatusCode)
	}
	if r2, _ := doJSON(t, "PUT", ts.URL+"/api/backup/settings", adminTok, map[string]any{"every_seconds": 100000}); r2.StatusCode != 400 {
		t.Fatalf("100000s: %d, want 400", r2.StatusCode)
	}

	// Valid toggle + interval.
	r3, b3 := doJSON(t, "PUT", ts.URL+"/api/backup/settings", adminTok,
		map[string]any{"enabled": true, "every_seconds": 7200})
	if r3.StatusCode != 200 {
		t.Fatalf("valid put: %d %s", r3.StatusCode, b3)
	}
	if !e.server.Backup.Enabled || e.server.Backup.Every != 2*time.Hour {
		t.Fatalf("not applied: %v %v", e.server.Backup.Enabled, e.server.Backup.Every)
	}
	if !strings.Contains(string(b3), `"enabled":true`) {
		t.Fatalf("response = %s", b3)
	}

	// Unauthenticated.
	if r4, _ := doJSON(t, "PUT", ts.URL+"/api/backup/settings", "", map[string]any{"enabled": true}); r4.StatusCode != 401 {
		t.Fatalf("no auth: %d, want 401", r4.StatusCode)
	}
}

// TestBackupScheduledPrune: TakeSnapshot(scheduled) marks files for pruning
// and the cap keeps only maxBackups scheduled snapshots.
func TestBackupScheduledPrune(t *testing.T) {
	e, _ := backupEnv(t)
	ctx := context.Background()

	for i := 0; i < maxBackups+5; i++ {
		if _, err := e.server.Backup.TakeSnapshot(ctx, true); err != nil {
			t.Fatal(err)
		}
		// Names embed a second-resolution timestamp; collisions get a
		// counter suffix, but pruning must still see maxBackups+5 files.
	}
	list, err := e.server.Backup.List()
	if err != nil {
		t.Fatal(err)
	}
	sched := 0
	for _, bi := range list {
		if bi.Scheduled {
			sched++
		}
	}
	if sched > maxBackups {
		t.Fatalf("scheduled snapshots = %d, want <= %d", sched, maxBackups)
	}
	// Newest survive; verify the newest file exists on disk.
	newest := list[0]
	if _, err := os.Stat(filepath.Join(e.server.Backup.Dir, newest.Name)); err != nil {
		t.Fatalf("newest missing: %v", err)
	}
}

// TestBackupNilManagerUnavailable: routes 503 when Backup isn't wired
// (protects older test envs and misconfigured boots).
func TestBackupNilManagerUnavailable(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	if r1, _ := doJSON(t, "GET", ts.URL+"/api/backup", adminTok, nil); r1.StatusCode != 503 {
		t.Fatalf("nil GET: %d, want 503", r1.StatusCode)
	}
	if r2, _ := doJSON(t, "POST", ts.URL+"/api/backup", adminTok, nil); r2.StatusCode != 503 {
		t.Fatalf("nil POST: %d, want 503", r2.StatusCode)
	}
}
