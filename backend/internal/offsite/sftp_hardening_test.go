package offsite

// Regressions for P4 review Q1/Q7: SFTP uploads go to a temp name, are
// chmodded 0600 (the bundle carries the shares key — server-default 0644
// would expose it), and are renamed onto the final name only on success.
// Any mid-upload failure removes the partial and leaves NO final-name
// object, so a corrupt upload can never be counted as a valid generation.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func putSrc(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "snap.db")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	return src
}

// TestSFTPPutChmodsSnapshotAndKey: BOTH the snapshot and the key companion
// are chmodded 0600 (consistent hardening, not key-only).
func TestSFTPPutChmodsSnapshotAndKey(t *testing.T) {
	f := newFakeSFTP()
	d := &SFTPDestination{Client: f, Root: "/b"}
	src := putSrc(t)
	for _, name := range []string{"encode-20261111-120000-sched.db", "encode-20261111-120000-sched.db" + KeySuffix} {
		if err := d.Put(context.Background(), name, src); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.chmods) != 2 {
		t.Fatalf("want 2 chmods, got %v", f.chmods)
	}
	for _, c := range f.chmods {
		if c.mode != 0o600 {
			t.Fatalf("chmod mode = %o, want 0600", c.mode)
		}
		if !strings.Contains(c.path, ".partial-") {
			t.Fatalf("chmod must target the temp file before rename: %q", c.path)
		}
	}
	for _, final := range []string{"/b/encode-20261111-120000-sched.db", "/b/encode-20261111-120000-sched.db" + KeySuffix} {
		if f.modes[final] != 0o600 {
			t.Fatalf("final object %s must carry mode 0600, got %o", final, f.modes[final])
		}
	}
}

// TestSFTPPutFailureLeavesNoFinalObject: every failure stage (write, flush,
// chmod, rename) must clean the partial and leave no final-name object.
func TestSFTPPutFailureLeavesNoFinalObject(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject func(f *fakeSFTP)
	}{
		{"mid-copy write failure", func(f *fakeSFTP) { f.writeErr = errors.New("connection reset") }},
		{"flush-on-close failure", func(f *fakeSFTP) { f.closeErr = errors.New("disk full") }},
		{"chmod failure", func(f *fakeSFTP) { f.chmodErr = errors.New("operation unsupported") }},
		{"rename failure", func(f *fakeSFTP) { f.renameErr = errors.New("permission denied") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSFTP()
			tc.inject(f)
			d := &SFTPDestination{Client: f, Root: "/b"}
			err := d.Put(context.Background(), "encode-20261111-120000-sched.db", putSrc(t))
			if err == nil {
				t.Fatal("injected failure must propagate")
			}
			if _, ok := f.files["/b/encode-20261111-120000-sched.db"]; ok {
				t.Fatal("a failed upload must NOT leave a final-name object")
			}
			for p := range f.files {
				if strings.Contains(p, ".partial-") {
					t.Fatalf("partial object must be cleaned up: %q", p)
				}
			}
			// The cleanup Remove actually ran (best-effort is fine, but the
			// happy path of the fake must show it).
			if len(f.removed) != 1 || !strings.Contains(f.removed[0], ".partial-") {
				t.Fatalf("partial must be removed: %v", f.removed)
			}
		})
	}
}

// TestSFTPPutOverwritesIdempotently: re-uploading the same name (HA overlap)
// replaces the content via rename — no error, no leftover partial.
func TestSFTPPutOverwritesIdempotently(t *testing.T) {
	f := newFakeSFTP()
	d := &SFTPDestination{Client: f, Root: "/b"}
	src := putSrc(t)
	name := "encode-20261111-120000-sched.db"
	if err := d.Put(context.Background(), name, src); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("second-replica-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Put(context.Background(), name, src); err != nil {
		t.Fatalf("same-name re-upload must succeed (idempotent): %v", err)
	}
	if string(f.files["/b/"+name]) != "second-replica-bytes" {
		t.Fatalf("re-upload must replace content: %q", f.files["/b/"+name])
	}
	if len(f.files) != 1 {
		t.Fatalf("no partials may remain: %v", f.files)
	}
}
