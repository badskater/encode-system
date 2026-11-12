package api

// Regressions for P4 review Q8: same-second snapshot name collisions.
// dumpPG used os.Create (truncate) — two replicas dumping in the same
// second silently overwrote each other, and the docs claimed idempotent
// overwrites that SFTP did not honor pre-Q7. The dump file is now created
// O_EXCL (exactly one writer wins) and TakeSnapshot advances the counter
// suffix on collision, matching the SQLite VACUUM INTO behavior.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCreateDumpFileExclusive: the create helper refuses to clobber an
// existing file (the old os.Create truncated it — data loss under HA).
func TestCreateDumpFileExclusive(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "encode-20261111-120000-sched.dump")
	f, err := createDumpFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("first-writer"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	// Second create must fail with ErrExist, NOT truncate.
	f2, err := createDumpFile(dest)
	if err == nil {
		f2.Close()
		t.Fatal("createDumpFile must refuse an existing file")
	}
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("want os.ErrExist, got %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "first-writer" {
		t.Fatalf("existing dump must be untouched: %q %v", got, err)
	}
}

// TestDumpPGRefusesExistingFile: dumpPG surfaces the O_EXCL collision so
// TakeSnapshot can advance the counter (PG-gated).
func TestDumpPGRefusesExistingFile(t *testing.T) {
	e := newPGBackupTestEnv(t)
	defer e.cleanup()
	dest := filepath.Join(e.dir, "encode-20261111-120000-sched.dump")
	if err := os.WriteFile(dest, []byte("existing-dump"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := e.server.Backup.dumpPG(context.Background(), dest)
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("dumpPG over an existing file must return ErrExist, got %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "existing-dump" {
		t.Fatalf("existing dump clobbered: %q", got)
	}
}

// TestTakeSnapshotSameSecondCollision: with the base names for the current
// AND next second pre-created (deterministic collision regardless of clock
// alignment), TakeSnapshot must advance the counter suffix and produce a
// valid, distinct dump (PG-gated).
func TestTakeSnapshotSameSecondCollision(t *testing.T) {
	e := newPGBackupTestEnv(t)
	defer e.cleanup()
	b := e.server.Backup
	if err := os.MkdirAll(b.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, ts := range []string{now.Format("20060102-150405"), now.Add(time.Second).Format("20060102-150405")} {
		for _, kind := range []string{"sched", "manual"} {
			p := filepath.Join(b.Dir, "encode-"+ts+"-"+kind+".dump")
			if err := os.WriteFile(p, []byte("occupied"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	info, err := b.TakeSnapshot(context.Background(), true)
	if err != nil {
		t.Fatalf("collision must advance the counter, not fail: %v", err)
	}
	if !strings.Contains(info.Name, "-1.dump") {
		t.Fatalf("expected counter suffix -1, got %q", info.Name)
	}
	got, err := os.ReadFile(filepath.Join(b.Dir, info.Name))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "encode-pg-dump") {
		t.Fatalf("counter-suffixed file must be a real dump: %q", string(got[:min(80, len(got))]))
	}
	// The occupied base files are untouched.
	for _, ts := range []string{now.Format("20060102-150405"), now.Add(time.Second).Format("20060102-150405")} {
		p := filepath.Join(b.Dir, "encode-"+ts+"-sched.dump")
		if g, _ := os.ReadFile(p); string(g) != "occupied" {
			t.Fatalf("occupied base file clobbered: %s", p)
		}
	}
}
