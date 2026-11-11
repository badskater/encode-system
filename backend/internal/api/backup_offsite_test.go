package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/offsite"
)

// ---------- Offsite backup hook (P4): scheduled + manual integration ----------

// apiFakeDest is an in-memory offsite.Destination for the API-level hook
// tests (the offsite package has its own richer fakes).
type apiFakeDest struct {
	mu      sync.Mutex
	name    string
	objects map[string][]byte
	putErr  error
}

func newAPIFakeDest(name string) *apiFakeDest {
	return &apiFakeDest{name: name, objects: map[string][]byte{}}
}

func (f *apiFakeDest) Name() string { return f.name }
func (f *apiFakeDest) Put(ctx context.Context, name, localPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	b, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	f.objects[name] = b
	return nil
}
func (f *apiFakeDest) List(ctx context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for n := range f.objects {
		names = append(names, n)
	}
	return names, nil
}
func (f *apiFakeDest) Delete(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, name)
	return nil
}
func (f *apiFakeDest) has(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[name]
	return ok
}

// syncHookEnv wires the real offsite hook with a SYNCHRONOUS launch seam so
// assertions after the HTTP response are deterministic.
func syncHookEnv(t *testing.T) (*testEnv, string) {
	t.Helper()
	e, ts := backupEnv(t)
	e.server.Backup.AfterSnapshot = e.server.OffsiteAfterSnapshot
	e.server.Backup.afterSnapshotGo = func(fn func()) { fn() }
	return e, ts.URL
}

// seedOffsiteSettings stores a valid settings row with the S3 destination
// enabled. Saving a secret creates the shares-key file the bundle needs.
func seedOffsiteSettings(t *testing.T, e *testEnv, includeKey bool, keepN int) {
	t.Helper()
	st := &model.Settings{
		ScriptsRoot: "/data/scripts", ReleaseRoot: "/data/release",
		NodeBinDir: `C:\bin`, NodeScriptsDir: `C:\Encodes\scripts`, NodeReleaseDir: `C:\Encodes\ReleaseFolders`,
		ScanIntervalSeconds: 30, TasksBeforeReboot: 10, Group: "G", Tag: "1080p",
		DiscordWebhook: "https://discord.com/api/webhooks/test/hook",
	}
	st.BackupOffsite = model.BackupOffsite{
		KeepN: keepN, IncludeSharesKey: includeKey,
		S3: model.BackupOffsiteS3{
			Enabled: true, Endpoint: "minio.example.com:9000", Bucket: "backups",
			AccessKey: "AK" + "TEST", SecretKey: "SK" + "TEST",
		},
	}
	if err := e.server.Store.SaveSettings(ctxBg(), st); err != nil {
		t.Fatal(err)
	}
}

// auditActions returns the audit actions recorded so far.
func auditActions(t *testing.T, e *testEnv) []string {
	t.Helper()
	events, err := e.server.Store.ListAudit(ctxBg(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range events {
		out = append(out, ev.Action)
	}
	return out
}

func containsAction(actions []string, want string) bool {
	for _, a := range actions {
		if a == want {
			return true
		}
	}
	return false
}

// TestBackupHookFiresOnManualAndScheduled: the AfterSnapshot seam fires for
// BOTH paths with the created snapshot's name; a failed snapshot never
// fires it.
func TestBackupHookFiresOnManualAndScheduled(t *testing.T) {
	e, tsrv := backupEnv(t)
	ts := tsrv.URL
	var mu sync.Mutex
	var got []BackupInfo
	e.server.Backup.AfterSnapshot = func(ctx context.Context, info *BackupInfo) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, *info)
	}
	e.server.Backup.afterSnapshotGo = func(fn func()) { fn() }

	// Manual via the HTTP handler.
	resp, body := doJSON(t, "POST", ts+"/api/backup", adminTok, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("manual backup: %d %s", resp.StatusCode, body)
	}
	// Scheduled via the manager directly (the Run loop calls the same path).
	if _, err := e.server.Backup.TakeSnapshot(ctxBg(), true); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("hook must fire for manual AND scheduled: %d calls", len(got))
	}
	if got[0].Scheduled || !got[1].Scheduled {
		t.Fatalf("scheduled flags wrong: %+v", got)
	}
	if !strings.HasPrefix(got[0].Name, "encode-") || !strings.HasSuffix(got[0].Name, ".db") {
		t.Fatalf("hook got wrong snapshot name: %q", got[0].Name)
	}

	// Failed snapshot: PG mode with no dump handle errors before the hook.
	e.server.Backup.Postgres = true
	e.server.Backup.PGDump = nil
	if _, err := e.server.Backup.TakeSnapshot(ctxBg(), true); err == nil {
		t.Fatal("snapshot must fail without a PG dump handle")
	}
	if len(got) != 2 {
		t.Fatalf("hook must NOT fire on a failed snapshot: %d calls", len(got))
	}
}

// TestOffsiteUploadOnManualBackup: a manual backup uploads snapshot + key
// to the configured destination and audits backup.offsite.
func TestOffsiteUploadOnManualBackup(t *testing.T) {
	e, ts := syncHookEnv(t)
	seedOffsiteSettings(t, e, true, 30)
	fake := newAPIFakeDest("s3")
	e.server.OffsiteDestinations = func(st *model.Settings) ([]offsite.Destination, []offsite.Result) {
		return []offsite.Destination{fake}, nil
	}

	resp, body := doJSON(t, "POST", ts+"/api/backup", adminTok, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("manual backup: %d %s", resp.StatusCode, body)
	}
	var info BackupInfo
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatal(err)
	}
	if !fake.has(info.Name) {
		t.Fatalf("snapshot %s not uploaded", info.Name)
	}
	if !fake.has(info.Name + offsite.KeySuffix) {
		t.Fatal("shares key companion not uploaded")
	}
	// The uploaded key bytes match the local key file.
	keyBytes, err := os.ReadFile(e.server.Store.SharesKeyPath())
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	uploadedKey := fake.objects[info.Name+offsite.KeySuffix]
	fake.mu.Unlock()
	if string(uploadedKey) != string(keyBytes) {
		t.Fatal("uploaded key bytes differ from the local shares key")
	}
	if !containsAction(auditActions(t, e), "backup.offsite") {
		t.Fatalf("missing backup.offsite audit: %v", auditActions(t, e))
	}
}

// TestOffsiteUploadFailureDoesNotFailBackup: a broken destination leaves
// the local backup successful (201), audits backup.offsite_failed and
// fires the operational alert fan-out.
func TestOffsiteUploadFailureDoesNotFailBackup(t *testing.T) {
	e, ts := syncHookEnv(t)
	seedOffsiteSettings(t, e, true, 30)
	rec := &recordingNotifier{}
	e.server.Notifier = rec
	fake := newAPIFakeDest("s3")
	fake.putErr = errors.New("endpoint unreachable")
	e.server.OffsiteDestinations = func(st *model.Settings) ([]offsite.Destination, []offsite.Result) {
		return []offsite.Destination{fake}, nil
	}

	resp, body := doJSON(t, "POST", ts+"/api/backup", adminTok, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("upload failure must not fail the local backup: %d %s", resp.StatusCode, body)
	}
	actions := auditActions(t, e)
	if !containsAction(actions, "backup.offsite_failed") {
		t.Fatalf("missing failure audit: %v", actions)
	}
	rec.mu.Lock()
	alerts := rec.alertCount
	texts := rec.alertTexts
	rec.mu.Unlock()
	if alerts != 1 {
		t.Fatalf("want 1 operational alert, got %d", alerts)
	}
	if !strings.Contains(texts[0], "Offsite backup failed") || !strings.Contains(texts[0], "s3") {
		t.Fatalf("alert text wrong: %q", texts[0])
	}
	// The local snapshot still exists and is listed.
	resp, body = doJSON(t, "GET", ts+"/api/backup", adminTok, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "encode-") {
		t.Fatalf("local snapshot must survive: %d %s", resp.StatusCode, body)
	}
}

// TestOffsiteKeyOptOut: include_shares_key=false uploads the snapshot only.
func TestOffsiteKeyOptOut(t *testing.T) {
	e, ts := syncHookEnv(t)
	seedOffsiteSettings(t, e, false, 30)
	fake := newAPIFakeDest("s3")
	e.server.OffsiteDestinations = func(st *model.Settings) ([]offsite.Destination, []offsite.Result) {
		return []offsite.Destination{fake}, nil
	}
	resp, body := doJSON(t, "POST", ts+"/api/backup", adminTok, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("backup: %d %s", resp.StatusCode, body)
	}
	var info BackupInfo
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatal(err)
	}
	if !fake.has(info.Name) {
		t.Fatal("snapshot must upload on key opt-out")
	}
	if fake.has(info.Name + offsite.KeySuffix) {
		t.Fatal("key must NOT upload when include_shares_key=false")
	}
}

// TestOffsiteRetentionViaHook: keep_n from settings prunes old generations
// on the destination after a successful upload.
func TestOffsiteRetentionViaHook(t *testing.T) {
	e, ts := syncHookEnv(t)
	seedOffsiteSettings(t, e, true, 2)
	fake := newAPIFakeDest("s3")
	// Seed 3 older generations; the new snapshot makes 4 → 2 must go.
	for _, ts := range []string{"20261101-000000", "20261102-000000", "20261103-000000"} {
		base := "encode-" + ts + "-sched.db"
		fake.objects[base] = []byte("old")
		fake.objects[base+offsite.KeySuffix] = []byte("old-key")
	}
	e.server.OffsiteDestinations = func(st *model.Settings) ([]offsite.Destination, []offsite.Result) {
		return []offsite.Destination{fake}, nil
	}
	resp, body := doJSON(t, "POST", ts+"/api/backup", adminTok, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("backup: %d %s", resp.StatusCode, body)
	}
	if fake.has("encode-20261101-000000-sched.db") || fake.has("encode-20261102-000000-sched.db") {
		t.Fatal("oldest generations must be pruned to keep_n=2")
	}
	if fake.has("encode-20261101-000000-sched.db" + offsite.KeySuffix) {
		t.Fatal("key companions must be pruned with their snapshots")
	}
	if !fake.has("encode-20261103-000000-sched.db") {
		t.Fatal("newest old generation must survive keep_n=2")
	}
}

// TestOffsiteSkippedWhenDisabled: no enabled destination → the builder is
// never consulted and no offsite audit rows appear.
func TestOffsiteSkippedWhenDisabled(t *testing.T) {
	e, ts := syncHookEnv(t)
	// Settings row exists but both destinations are disabled (default).
	st := &model.Settings{
		ScriptsRoot: "/s", ReleaseRoot: "/r",
		NodeBinDir: `C:\bin`, NodeScriptsDir: `C:\s`, NodeReleaseDir: `C:\r`,
		ScanIntervalSeconds: 30, TasksBeforeReboot: 10, Group: "G", Tag: "T",
	}
	if err := e.server.Store.SaveSettings(ctxBg(), st); err != nil {
		t.Fatal(err)
	}
	called := false
	e.server.OffsiteDestinations = func(st *model.Settings) ([]offsite.Destination, []offsite.Result) {
		called = true
		return nil, nil
	}
	resp, body := doJSON(t, "POST", ts+"/api/backup", adminTok, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("backup: %d %s", resp.StatusCode, body)
	}
	if called {
		t.Fatal("destinations must not build when offsite is disabled")
	}
	for _, a := range auditActions(t, e) {
		if strings.HasPrefix(a, "backup.offsite") {
			t.Fatalf("no offsite audit expected when disabled: %s", a)
		}
	}
}

// TestOffsiteSettingsUnreadable: with the shares key lost the hook cannot
// read its config — it logs/audits the skip and the local backup still
// succeeds (fail-closed, no silent wipe).
func TestOffsiteSettingsUnreadable(t *testing.T) {
	e, ts := syncHookEnv(t)
	seedOffsiteSettings(t, e, true, 30)
	orig := swapStoreWithLostKey(t, e)
	defer os.WriteFile(filepath.Join(e.dir, "test.db.shares-key"), orig, 0o600)
	// The reopen replaced the server's store handle; the BackupManager
	// captured the old one at construction — re-point it.
	e.server.Backup.Store = e.server.Store

	resp, body := doJSON(t, "POST", ts+"/api/backup", adminTok, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("local backup must succeed with unreadable settings: %d %s", resp.StatusCode, body)
	}
	if !containsAction(auditActions(t, e), "backup.offsite_failed") {
		t.Fatalf("unreadable settings must be audited: %v", auditActions(t, e))
	}
}
