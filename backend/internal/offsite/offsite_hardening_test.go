package offsite

// Regressions for the P4 quality review: retention must never delete the
// just-uploaded generation (Q6), an UNREADABLE shares key must fail the run
// instead of silently uploading snapshot-only (Q10), snapshot-name matching
// must be exact so nested/foreign objects can neither be counted nor wedge
// pruning (Q11), and key companions are deleted BEFORE their snapshot so a
// failed delete leaves snapshot-only, never key-only (G2).

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestRunPruneNeverDeletesCurrent: keep_n=1 with a NEWER foreign generation
// present (clock skew / another HA replica uploaded ahead) — the snapshot
// this run just uploaded must survive even though it sorts oldest.
func TestRunPruneNeverDeletesCurrent(t *testing.T) {
	snap, key := snapFiles(t) // encode-20261111-120000-sched.db
	f := newFakeDest("s3")
	// A newer generation from another replica (clock skew).
	newer := "encode-20261112-000000-sched.db"
	f.objects[newer] = []byte("newer")
	f.objects[newer+KeySuffix] = []byte("newer-key")
	results, _ := Run(context.Background(), discardLog(), []Destination{f}, snap, key, true, 1)
	if results[0].Err != "" {
		t.Fatalf("unexpected err: %+v", results[0])
	}
	if _, ok := f.objects["encode-20261111-120000-sched.db"]; !ok {
		t.Fatal("the just-uploaded snapshot must NEVER be pruned (keep_n=1 + newer gen)")
	}
	if _, ok := f.objects["encode-20261111-120000-sched.db"+KeySuffix]; !ok {
		t.Fatal("the current key companion must survive with its snapshot")
	}
	for _, del := range results[0].Pruned {
		if strings.HasPrefix(del, "encode-20261111-120000") {
			t.Fatalf("current generation listed for deletion: %v", results[0].Pruned)
		}
	}
}

// TestRunKeyUnreadableFails: a key file that EXISTS but cannot be read is
// not the fresh-install case — the run must fail (Result.Err) so the caller
// audits backup.offsite_failed and alerts, instead of a silent snapshot-only
// success. A directory at the key path yields a non-NotExist read error on
// every platform and privilege level.
func TestRunKeyUnreadableFails(t *testing.T) {
	snap, _ := snapFiles(t)
	f := newFakeDest("s3")
	keyDir := t.TempDir() // a directory: ReadFile fails with EISDIR, not NotExist
	results, bundled := Run(context.Background(), discardLog(), []Destination{f}, snap, keyDir, true, 30)
	if bundled {
		t.Fatal("keyBundled must be false when the key is unreadable")
	}
	if len(results) != 1 || results[0].Err == "" {
		t.Fatalf("unreadable key must fail the run: %+v", results)
	}
	if !strings.Contains(results[0].Err, "shares key") {
		t.Fatalf("error must name the shares key: %q", results[0].Err)
	}
	if len(f.puts) != 0 {
		t.Fatalf("nothing may upload when the key is unreadable: %v", f.puts)
	}
}

// TestRunKeyMissingStillSnapshotOnly: the fresh-install branch is unchanged —
// a MISSING key file uploads the snapshot without error (contrast with the
// unreadable case above).
func TestRunKeyMissingStillSnapshotOnly(t *testing.T) {
	snap, _ := snapFiles(t)
	f := newFakeDest("s3")
	results, bundled := Run(context.Background(), discardLog(), []Destination{f}, snap, filepath.Join(t.TempDir(), "absent.key"), true, 30)
	if bundled || results[0].Err != "" || len(f.puts) != 1 {
		t.Fatalf("missing key must stay snapshot-only success: %+v puts=%v", results[0], f.puts)
	}
}

// TestPruneListStrictNames: only the EXACT snapshot naming counts as a
// generation. Nested paths and foreign encode-* files must be ignored —
// otherwise they inflate the generation count and (because Delete rejects
// them) wedge retention with a permanent PruneErr.
func TestPruneListStrictNames(t *testing.T) {
	valid := []string{
		"encode-20261101-000000-sched.db",
		"encode-20261102-000000-manual.db",
		"encode-20261103-000000-sched-1.dump",
		"encode-20261104-000000-manual-12.db",
	}
	foreign := []string{
		"encode-20261105-000000-sched.db/nested.db", // nested: not a generation
		"encode-notes.db",                         // foreign encode-* at root
		"encode-20261105-000000-sched.dbx",        // wrong extension
		"encode-20261105-000000-other.db",         // unknown kind
		"encode-20261105-000000-sched",            // no extension
		"encode-2026110-000000-sched.db",          // short timestamp
		"encode-20261105-000000-sched.db.partial", // in-flight upload
		"encode-20261399-000000-sched.db",         // impossible date
		"other-20261105-000000-sched.db",          // wrong prefix
	}
	names := append(append([]string{}, valid...), foreign...)
	// keepN=2: only the 4 valid names count; the 2 oldest valid ones go
	// (newest-first order, matching the sorted candidate walk).
	got := pruneList(names, 2, "")
	want := []string{
		"encode-20261102-000000-manual.db",
		"encode-20261101-000000-sched.db",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pruneList = %v, want %v (foreign/nested names must not count)", got, want)
	}
	// With only foreign names present nothing is ever deleted.
	if del := pruneList(foreign, 1, ""); del != nil {
		t.Fatalf("foreign-only listing must prune nothing: %v", del)
	}
}

// TestPruneListCompanionFirst: the key companion is deleted BEFORE its
// snapshot — a mid-pair delete failure then leaves a snapshot-only
// generation (restorable with an out-of-band key), never a key-only orphan
// (useless AND secret material).
func TestPruneListCompanionFirst(t *testing.T) {
	base := "encode-20261101-000000-sched.db"
	names := []string{
		base, base + KeySuffix,
		"encode-20261102-000000-sched.db",
		"encode-20261103-000000-sched.db",
	}
	got := pruneList(names, 2, "")
	want := []string{base + KeySuffix, base}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pruneList = %v, want companion-first %v", got, want)
	}
}

// TestRunUnreadableKeyFailsEveryDestination: the failure is reported per
// destination so each one audits/alerts; healthy destinations cannot mask
// the broken bundle.
func TestRunUnreadableKeyFailsEveryDestination(t *testing.T) {
	snap, _ := snapFiles(t)
	a, b := newFakeDest("s3"), newFakeDest("sftp")
	results, _ := Run(context.Background(), discardLog(), []Destination{a, b}, snap, t.TempDir(), true, 30)
	if len(results) != 2 {
		t.Fatalf("want one result per destination, got %d", len(results))
	}
	for _, r := range results {
		if r.Err == "" {
			t.Fatalf("destination %s must report the unreadable key: %+v", r.Destination, r)
		}
	}
	if len(a.puts)+len(b.puts) != 0 {
		t.Fatal("no uploads may happen when the key is unreadable")
	}
	// include_shares_key=false bypasses the key entirely — still uploads.
	results, _ = Run(context.Background(), discardLog(), []Destination{a}, snap, t.TempDir(), false, 30)
	if results[0].Err != "" || len(a.puts) != 1 {
		t.Fatalf("key opt-out must not read the key at all: %+v", results[0])
	}
}
