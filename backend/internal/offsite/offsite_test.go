package offsite

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// fakeDest is an in-memory Destination recording every call. Content is
// read from the local file at Put time so tests assert real bytes traveled.
type fakeDest struct {
	name     string
	objects  map[string][]byte // remote name -> content
	puts     []string          // ordered put names
	deletes  []string          // ordered deleted names
	listed   int
	putErr   error
	listErr  error
	delErr   error
	closed   int
	failKeys bool // fail only the key companion upload
}

func newFakeDest(name string) *fakeDest {
	return &fakeDest{name: name, objects: map[string][]byte{}}
}

func (f *fakeDest) Name() string { return f.name }
func (f *fakeDest) Put(ctx context.Context, name, localPath string) error {
	if f.putErr != nil {
		return f.putErr
	}
	if f.failKeys && strings.HasSuffix(name, KeySuffix) {
		return errors.New("key upload denied")
	}
	b, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	f.objects[name] = b
	f.puts = append(f.puts, name)
	return nil
}
func (f *fakeDest) List(ctx context.Context) ([]string, error) {
	f.listed++
	if f.listErr != nil {
		return nil, f.listErr
	}
	var names []string
	for n := range f.objects {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}
func (f *fakeDest) Delete(ctx context.Context, name string) error {
	if f.delErr != nil {
		return f.delErr
	}
	delete(f.objects, name)
	f.deletes = append(f.deletes, name)
	return nil
}
func (f *fakeDest) Close() error { f.closed++; return nil }

// snapFile writes a fake snapshot + key file into a temp dir and returns
// their paths. Names mirror the real scheduler naming (UTC timestamp).
func snapFiles(t *testing.T) (snapPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()
	snapPath = filepath.Join(dir, "encode-20261111-120000-sched.db")
	if err := os.WriteFile(snapPath, []byte("snapshot-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyPath = filepath.Join(dir, "test.db.shares-key")
	if err := os.WriteFile(keyPath, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	return snapPath, keyPath
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestRunUploadsSnapshotAndKey: both destinations receive the snapshot and
// the key companion with the exact local bytes; keyBundled reports true.
func TestRunUploadsSnapshotAndKey(t *testing.T) {
	snap, key := snapFiles(t)
	a, b := newFakeDest("s3"), newFakeDest("sftp")
	results, bundled := Run(context.Background(), discardLog(), []Destination{a, b}, snap, key, true, 30)
	if !bundled {
		t.Fatal("keyBundled must be true when the key traveled")
	}
	for _, f := range []*fakeDest{a, b} {
		if len(results) != 2 {
			t.Fatalf("want 2 results, got %d", len(results))
		}
		if got := string(f.objects["encode-20261111-120000-sched.db"]); got != "snapshot-bytes" {
			t.Errorf("%s: snapshot content = %q", f.name, got)
		}
		if got := string(f.objects["encode-20261111-120000-sched.db"+KeySuffix]); got != "0123456789abcdef0123456789abcdef" {
			t.Errorf("%s: key content = %q", f.name, got)
		}
	}
	for _, r := range results {
		if r.Err != "" || r.PruneErr != "" {
			t.Errorf("unexpected error: %+v", r)
		}
		if len(r.Uploaded) != 2 {
			t.Errorf("want 2 uploaded objects, got %v", r.Uploaded)
		}
	}
	// Connections are closed after the run (SFTP owns its session).
	if a.closed != 1 || b.closed != 1 {
		t.Errorf("destinations must be closed once: %d %d", a.closed, b.closed)
	}
}

// TestRunKeyOptOut: includeKey=false uploads the snapshot only.
func TestRunKeyOptOut(t *testing.T) {
	snap, key := snapFiles(t)
	f := newFakeDest("s3")
	results, bundled := Run(context.Background(), discardLog(), []Destination{f}, snap, key, false, 30)
	if bundled {
		t.Fatal("keyBundled must be false on opt-out")
	}
	if len(f.puts) != 1 || strings.HasSuffix(f.puts[0], KeySuffix) {
		t.Fatalf("opt-out must upload only the snapshot: %v", f.puts)
	}
	if results[0].Err != "" {
		t.Fatalf("unexpected err: %v", results[0])
	}
}

// TestRunKeyMissing: a missing key file (fresh install, no secrets yet) is
// not an error — the snapshot still travels, keyBundled=false.
func TestRunKeyMissing(t *testing.T) {
	snap, _ := snapFiles(t)
	f := newFakeDest("s3")
	results, bundled := Run(context.Background(), discardLog(), []Destination{f}, snap, filepath.Join(t.TempDir(), "absent.key"), true, 30)
	if bundled {
		t.Fatal("keyBundled must be false when the key file is absent")
	}
	if results[0].Err != "" || len(f.puts) != 1 {
		t.Fatalf("missing key must not fail the upload: %+v puts=%v", results[0], f.puts)
	}
}

// TestRunKeyUploadFailure: snapshot lands but the key companion fails →
// the destination result is an error (incomplete bundle).
func TestRunKeyUploadFailure(t *testing.T) {
	snap, key := snapFiles(t)
	f := newFakeDest("s3")
	f.failKeys = true
	results, _ := Run(context.Background(), discardLog(), []Destination{f}, snap, key, true, 30)
	if results[0].Err == "" || !strings.Contains(results[0].Err, "shares key upload") {
		t.Fatalf("key upload failure must surface: %+v", results[0])
	}
	if _, ok := f.objects["encode-20261111-120000-sched.db"]; !ok {
		t.Fatal("snapshot must remain uploaded when the key fails")
	}
}

// TestRunUploadFailureIsolated: one broken destination must not block the
// other; results carry per-destination outcomes.
func TestRunUploadFailureIsolated(t *testing.T) {
	snap, key := snapFiles(t)
	bad, good := newFakeDest("s3"), newFakeDest("sftp")
	bad.putErr = errors.New("endpoint unreachable")
	results, _ := Run(context.Background(), discardLog(), []Destination{bad, good}, snap, key, true, 30)
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	if results[0].Err == "" {
		t.Error("failed destination must report Err")
	}
	if results[1].Err != "" || len(good.puts) != 2 {
		t.Errorf("healthy destination must still upload: %+v", results[1])
	}
}

// TestRunPrunesBeyondKeepN: with 5 generations present and keepN=2, the 3
// oldest go — snapshot AND key companion together; newest 2 stay.
func TestRunPrunesBeyondKeepN(t *testing.T) {
	snap, key := snapFiles(t)
	f := newFakeDest("s3")
	// Seed 4 older generations (the run adds the 5th, newest).
	for _, ts := range []string{"20261101-000000", "20261102-000000", "20261103-000000", "20261104-000000"} {
		base := "encode-" + ts + "-sched.db"
		f.objects[base] = []byte("old")
		f.objects[base+KeySuffix] = []byte("old-key")
	}
	// A foreign object under the same root must survive pruning.
	f.objects["notes.txt"] = []byte("keep me")
	results, _ := Run(context.Background(), discardLog(), []Destination{f}, snap, key, true, 2)
	if results[0].Err != "" || results[0].PruneErr != "" {
		t.Fatalf("unexpected errors: %+v", results[0])
	}
	if len(results[0].Pruned) != 6 {
		t.Fatalf("want 6 pruned objects (3 gens x 2), got %v", results[0].Pruned)
	}
	for _, kept := range []string{"encode-20261104-000000-sched.db", "encode-20261111-120000-sched.db", "notes.txt"} {
		if _, ok := f.objects[kept]; !ok {
			t.Errorf("must keep %s", kept)
		}
	}
	if _, ok := f.objects["encode-20261101-000000-sched.db"]; ok {
		t.Error("oldest generation must be pruned")
	}
	if _, ok := f.objects["encode-20261101-000000-sched.db"+KeySuffix]; ok {
		t.Error("oldest key companion must be pruned with its snapshot")
	}
}

// TestRunPruneFailureReported: a listing failure is a PruneErr, never an
// upload failure — the snapshot stays.
func TestRunPruneFailureReported(t *testing.T) {
	snap, key := snapFiles(t)
	f := newFakeDest("s3")
	f.listErr = errors.New("list denied")
	results, _ := Run(context.Background(), discardLog(), []Destination{f}, snap, key, true, 2)
	if results[0].Err != "" {
		t.Fatalf("prune failure must not fail the upload: %+v", results[0])
	}
	if results[0].PruneErr == "" {
		t.Fatal("list failure must surface as PruneErr")
	}
	if len(f.puts) != 2 {
		t.Fatalf("upload must complete: %v", f.puts)
	}
}

// TestRunKeepNDefault: keepN=0 falls back to DefaultKeepN (no pruning of a
// small listing).
func TestRunKeepNDefault(t *testing.T) {
	snap, key := snapFiles(t)
	f := newFakeDest("s3")
	results, _ := Run(context.Background(), discardLog(), []Destination{f}, snap, key, true, 0)
	if results[0].Err != "" || len(results[0].Pruned) != 0 {
		t.Fatalf("keepN=0 must default, not prune everything: %+v", results[0])
	}
}

// TestRunContextCancelled: a cancelled context fails destinations cleanly
// (no panic, no partial-upload claim).
func TestRunContextCancelled(t *testing.T) {
	snap, key := snapFiles(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := newFakeDest("s3")
	results, _ := Run(ctx, discardLog(), []Destination{f}, snap, key, true, 2)
	if results[0].Err == "" {
		t.Fatal("cancelled ctx must produce an error result")
	}
	if len(f.puts) != 0 {
		t.Fatalf("nothing must upload under a cancelled ctx: %v", f.puts)
	}
}

// TestPruneList: pure retention logic — timestamp ordering, companion
// pairing, foreign-object safety, keepN >= count no-op.
func TestPruneList(t *testing.T) {
	names := []string{
		"encode-20261103-000000-sched.db",
		"encode-20261101-000000-manual.db",
		"encode-20261101-000000-manual.db" + KeySuffix,
		"encode-20261102-000000-sched.dump", // PG backend naming sorts too
		"encode-20261102-000000-sched.dump" + KeySuffix,
		"unrelated.txt",
		"encode-not-a-snapshot.txt",
	}
	got := pruneList(names, 2, "")
	// Companion-first (G2): a failed pair delete leaves snapshot-only.
	want := []string{
		"encode-20261101-000000-manual.db" + KeySuffix,
		"encode-20261101-000000-manual.db",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pruneList = %v, want %v", got, want)
	}
	if pruneList(names, 10, "") != nil {
		t.Fatal("keepN >= generations must prune nothing")
	}
	if pruneList(nil, 1, "") != nil {
		t.Fatal("empty listing must prune nothing")
	}
}

// TestValidName: traversal and separators are rejected on both transports.
func TestValidName(t *testing.T) {
	for _, bad := range []string{"", "../x", "a/b", `a\b`, ".."} {
		if err := validName(bad); err == nil {
			t.Errorf("validName(%q) must fail", bad)
		}
	}
	if err := validName("encode-20261111-120000-sched.db"); err != nil {
		t.Errorf("plain snapshot name must pass: %v", err)
	}
}
