package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestBackupToProducesValidSnapshot verifies VACUUM INTO creates a
// readable standalone SQLite file carrying the current data — the whole
// point of the backup: restore = drop the file in and open it.
func TestBackupToProducesValidSnapshot(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Seed a recognizable row.
	if _, err := s.UpsertSeriesByName(ctx, "Backup Show"); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "snap.db")
	if err := s.BackupTo(ctx, dest); err != nil {
		t.Fatalf("BackupTo: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil || info.Size() == 0 {
		t.Fatalf("snapshot missing/empty: %v %v", info, err)
	}

	// Open the snapshot independently and read the row back.
	snap, err := sql.Open("sqlite", "file:"+dest)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	var name string
	if err := snap.QueryRowContext(ctx, `SELECT name FROM series WHERE name = 'Backup Show'`).Scan(&name); err != nil {
		t.Fatalf("snapshot not readable: %v", err)
	}

	// Refuses to clobber an existing file (VACUUM INTO semantics) so a
	// caller bug can't overwrite a previous good backup.
	if err := s.BackupTo(ctx, dest); err == nil {
		t.Fatal("BackupTo onto existing file should fail")
	}
}

// TestBackupToRejectsBadDest verifies errors surface (missing dir).
func TestBackupToRejectsBadDest(t *testing.T) {
	s := newTestStore(t)
	err := s.BackupTo(context.Background(), "/nonexistent-dir-xyz/snap.db")
	if err == nil {
		t.Fatal("want error for bad destination")
	}
}

var _ = model.Series{} // keep model import if unused above
