package api

// Regressions for the P4 quality review (offsite hook):
//   - Q5: a panicking AfterSnapshot hook must not crash the controller —
//     the detached wrapper recovers, logs Error and audits
//     backup.offsite_failed with reason=panic.
//   - Q3: destination errors embedding credential-bearing URLs are redacted
//     in ALL THREE sinks: structured log, audit detail, alert text.
//   - Q10: an unreadable (non-missing) shares key fails the offsite run with
//     backup.offsite_failed + alert, while the local backup still succeeds.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/offsite"
)

// TestBackupHookPanicContained: a hook that panics must not take down the
// controller (synchronous seam AND the default detached goroutine), and the
// failure must be audited as backup.offsite_failed with reason=panic.
func TestBackupHookPanicContained(t *testing.T) {
	e, ts := backupEnv(t)
	e.server.Backup.AfterSnapshot = func(ctx context.Context, info *BackupInfo) {
		panic("hook exploded: " + info.Name)
	}
	e.server.Backup.afterSnapshotGo = func(fn func()) { fn() }

	resp, body := doJSON(t, "POST", ts.URL+"/api/backup", adminTok, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("panicking hook must not fail the local backup: %d %s", resp.StatusCode, body)
	}
	assertPanicAudit(t, e)

	// Default launcher (detached goroutine): the recover must live INSIDE
	// the launched closure, not in the seam.
	e2, ts2 := backupEnv(t)
	e2.server.Backup.AfterSnapshot = func(ctx context.Context, info *BackupInfo) {
		panic("detached hook exploded")
	}
	// afterSnapshotGo stays nil → production `go fn()` launcher.
	resp, body = doJSON(t, "POST", ts2.URL+"/api/backup", adminTok, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("detached panicking hook must not fail the backup: %d %s", resp.StatusCode, body)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if hasPanicAudit(t, e2) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("detached hook panic must be audited as backup.offsite_failed reason=panic")
}

func hasPanicAudit(t *testing.T, e *testEnv) bool {
	t.Helper()
	events, err := e.server.Store.ListAudit(ctxBg(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Action == "backup.offsite_failed" && strings.Contains(ev.Detail, `"reason":"panic"`) {
			return true
		}
	}
	return false
}

func assertPanicAudit(t *testing.T, e *testEnv) {
	t.Helper()
	if !hasPanicAudit(t, e) {
		t.Fatalf("missing panic audit (backup.offsite_failed reason=panic): %v", auditActions(t, e))
	}
}

// TestOffsiteErrorRedactsEmbeddedCredentials: a transport error carrying an
// embedded credential URL must not leak the secret into the log, the audit
// detail, or the alert text.
func TestOffsiteErrorRedactsEmbeddedCredentials(t *testing.T) {
	e, ts := syncHookEnv(t)
	seedOffsiteSettings(t, e, true, 30)
	var logBuf bytes.Buffer
	e.server.Log = slog.New(slog.NewTextHandler(&logBuf, nil))
	rec := &recordingNotifier{}
	e.server.Notifier = rec

	user := "AKIA" + "HOOKKEY9"
	pass := "hook" + "s3cret-pw-5"
	query := "hook" + "s3cret-q-6"
	fake := newAPIFakeDest("s3")
	fake.putErr = errors.New(`Put "https://` + user + `:` + pass + `@s3.example.com/b/k?auth=` + query + `": dial tcp 192.0.2.9:443: connect: connection refused`)
	e.server.OffsiteDestinations = func(st *model.Settings) ([]offsite.Destination, []offsite.Result) {
		return []offsite.Destination{fake}, nil
	}

	resp, body := doJSON(t, "POST", ts+"/api/backup", adminTok, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("local backup must succeed: %d %s", resp.StatusCode, body)
	}
	secrets := []string{user, pass, query}
	// Log sink.
	logged := logBuf.String()
	for _, s := range secrets {
		if strings.Contains(logged, s) {
			t.Fatalf("offsite error log leaked embedded credential: %s", logged)
		}
	}
	if !strings.Contains(logged, "REDACTED") {
		t.Fatalf("log must carry the redacted URL: %s", logged)
	}
	// Audit sink.
	events, err := e.server.Store.ListAudit(ctxBg(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var failedDetail string
	for _, ev := range events {
		if ev.Action == "backup.offsite_failed" {
			failedDetail = ev.Detail
		}
	}
	if failedDetail == "" {
		t.Fatal("missing backup.offsite_failed audit")
	}
	for _, s := range secrets {
		if strings.Contains(failedDetail, s) {
			t.Fatalf("audit detail leaked embedded credential: %s", failedDetail)
		}
	}
	// Alert sink.
	rec.mu.Lock()
	texts := rec.alertTexts
	rec.mu.Unlock()
	if len(texts) != 1 {
		t.Fatalf("want 1 alert, got %d", len(texts))
	}
	for _, s := range secrets {
		if strings.Contains(texts[0], s) {
			t.Fatalf("alert leaked embedded credential: %s", texts[0])
		}
	}
	if !strings.Contains(texts[0], "REDACTED") {
		t.Fatalf("alert must carry the redacted URL: %s", texts[0])
	}
}

// TestOffsiteUnreadableKeyFailsRun: the shares key exists but is unreadable
// (a directory sits at its path) — the offsite run must fail with an audit
// and an alert, NOT silently succeed snapshot-only. The local backup stays
// successful. The settings cipher is already cached, so GetSettings still
// works — the failure is isolated to the key READ, as intended.
func TestOffsiteUnreadableKeyFailsRun(t *testing.T) {
	e, ts := syncHookEnv(t)
	seedOffsiteSettings(t, e, true, 30)
	rec := &recordingNotifier{}
	e.server.Notifier = rec
	fake := newAPIFakeDest("s3")
	e.server.OffsiteDestinations = func(st *model.Settings) ([]offsite.Destination, []offsite.Result) {
		return []offsite.Destination{fake}, nil
	}

	keyPath := e.server.Store.SharesKeyPath()
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(keyPath, 0o700); err != nil { // EISDIR on read, any OS/uid
		t.Fatal(err)
	}

	resp, body := doJSON(t, "POST", ts+"/api/backup", adminTok, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("local backup must survive an unreadable key: %d %s", resp.StatusCode, body)
	}
	fake.mu.Lock()
	uploads := len(fake.objects)
	fake.mu.Unlock()
	if uploads != 0 {
		t.Fatalf("nothing may upload with an unreadable key: %d objects", uploads)
	}
	if !containsAction(auditActions(t, e), "backup.offsite_failed") {
		t.Fatalf("unreadable key must audit backup.offsite_failed: %v", auditActions(t, e))
	}
	if containsAction(auditActions(t, e), "backup.offsite") {
		t.Fatal("an unreadable-key run must NOT audit success")
	}
	rec.mu.Lock()
	alerts := rec.alertCount
	rec.mu.Unlock()
	if alerts != 1 {
		t.Fatalf("unreadable key must fire the operational alert, got %d", alerts)
	}
}
