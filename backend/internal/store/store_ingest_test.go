package store

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// storeContractIngestLedger pins the ingest dedupe ledger contract on
// both backends: record/seen round-trip, idempotent duplicate record,
// provider scoping, and listing.
func storeContractIngestLedger(t *testing.T, openFn func(t *testing.T) *Store) {
	ctx := context.Background()
	s := openFn(t)

	// Fresh ledger: nothing seen.
	seen, err := s.IngestSeen(ctx, "qbittorrent", "hash-a")
	if err != nil {
		t.Fatalf("IngestSeen on empty ledger: %v", err)
	}
	if seen {
		t.Fatal("empty ledger must report unseen")
	}

	// Record + seen round-trip.
	inserted, err := s.RecordIngest(ctx, "qbittorrent", "hash-a", "Show.S01E01", "Show", "01", "Show/Ep 01")
	if err != nil {
		t.Fatalf("RecordIngest: %v", err)
	}
	if !inserted {
		t.Fatal("first record must insert")
	}
	seen, err = s.IngestSeen(ctx, "qbittorrent", "hash-a")
	if err != nil || !seen {
		t.Fatalf("recorded torrent must be seen: seen=%v err=%v", seen, err)
	}

	// Duplicate record is an idempotent no-op (ON CONFLICT DO NOTHING).
	inserted, err = s.RecordIngest(ctx, "qbittorrent", "hash-a", "Show.S01E01", "Show", "01", "Show/Ep 01")
	if err != nil {
		t.Fatalf("duplicate RecordIngest must not error: %v", err)
	}
	if inserted {
		t.Fatal("duplicate record must not insert")
	}

	// Provider scoping: the same hash under another provider is unseen.
	seen, err = s.IngestSeen(ctx, "rutorrent", "hash-a")
	if err != nil || seen {
		t.Fatalf("provider scoping: seen=%v err=%v", seen, err)
	}
	if _, err := s.RecordIngest(ctx, "rutorrent", "hash-a", "n", "s", "02", "s/Ep 02"); err != nil {
		t.Fatal(err)
	}

	// Listing: newest first, fields intact.
	entries, err := s.ListIngestLedger(ctx, 10)
	if err != nil {
		t.Fatalf("ListIngestLedger: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 ledger entries, got %d", len(entries))
	}
	if entries[0].Provider != "rutorrent" || entries[0].TorrentHash != "hash-a" {
		t.Fatalf("newest-first order: %+v", entries[0])
	}
	if entries[1].Series != "Show" || entries[1].Episode != "01" || entries[1].StagedPath != "Show/Ep 01" {
		t.Fatalf("entry fields: %+v", entries[1])
	}
	if entries[1].StagedAt == "" {
		t.Fatal("staged_at must be stamped")
	}
}

// TestIngestLedgerContractSQLite runs the ledger contract on SQLite.
func TestIngestLedgerContractSQLite(t *testing.T) {
	storeContractIngestLedger(t, func(t *testing.T) *Store {
		t.Helper()
		s, err := Open(filepath.Join(t.TempDir(), "ingest.db"))
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}

// TestIngestLedgerContractPostgres runs the ledger contract on Postgres
// when ENCODE_TEST_PG_DSN is set (fresh migratePG must create the table).
func TestIngestLedgerContractPostgres(t *testing.T) {
	dsn := os.Getenv("ENCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("ENCODE_TEST_PG_DSN not set; skipping PG ingest ledger contract")
	}
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() {
		s.db.ExecContext(context.Background(), "TRUNCATE TABLE ingest_ledger RESTART IDENTITY CASCADE")
		s.Close()
	})
	storeContractIngestLedger(t, func(t *testing.T) *Store { return s })
}

// TestIngestLedgerUpgradePG proves an EXISTING Postgres database created
// before P11 gains the ledger on the next Open (the fresh-schema
// CREATE TABLE IF NOT EXISTS runs on every open — the upgrade path for
// a new table, no ALTER chain needed).
func TestIngestLedgerUpgradePG(t *testing.T) {
	dsn := os.Getenv("ENCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("ENCODE_TEST_PG_DSN not set; skipping PG ingest upgrade test")
	}
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	ctx := context.Background()
	// Simulate a pre-P11 database: drop the ledger, then reopen.
	if _, err := s.db.ExecContext(ctx, "DROP TABLE IF EXISTS ingest_ledger"); err != nil {
		t.Fatalf("drop ledger: %v", err)
	}
	s.Close()
	s2, err := Open(dsn)
	if err != nil {
		t.Fatalf("reopen pg (upgrade): %v", err)
	}
	defer s2.Close()
	if _, err := s2.RecordIngest(ctx, "qbittorrent", "upgrade-hash", "n", "s", "01", "s/Ep 01"); err != nil {
		t.Fatalf("ledger must exist after upgrade open: %v", err)
	}
	seen, err := s2.IngestSeen(ctx, "qbittorrent", "upgrade-hash")
	if err != nil || !seen {
		t.Fatalf("upgrade ledger round-trip: seen=%v err=%v", seen, err)
	}
	if _, err := s2.db.ExecContext(ctx, "TRUNCATE TABLE ingest_ledger RESTART IDENTITY CASCADE"); err != nil {
		t.Logf("cleanup truncate: %v", err)
	}
}

// TestIngestLedgerRestartSQLite proves the ledger survives a controller
// restart on SQLite (close + reopen the same file).
func TestIngestLedgerRestartSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	ctx := context.Background()
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.RecordIngest(ctx, "qbittorrent", "persist-h", "n", "s", "01", "s/Ep 01"); err != nil {
		t.Fatal(err)
	}
	s1.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	seen, err := s2.IngestSeen(ctx, "qbittorrent", "persist-h")
	if err != nil || !seen {
		t.Fatalf("ledger must survive restart: seen=%v err=%v", seen, err)
	}
}

// TestIngestLockKeyFree proves lock key 1008 is distinct from every
// existing loop key (a collision would let two loops share one lock).
func TestIngestLockKeyFree(t *testing.T) {
	keys := []int64{
		LockKeyScanner, LockKeyRetention, LockKeyDigest, LockKeyDiskAlert,
		LockKeyBackup, LockKeyNodeAlert, LockKeyStallWatchdog,
	}
	for _, k := range keys {
		if k == LockKeyIngest {
			t.Fatalf("LockKeyIngest collides with existing key %d", k)
		}
	}
	if LockKeyIngest != 1008 {
		t.Fatalf("LockKeyIngest = %d, want 1008 (plan-assigned)", LockKeyIngest)
	}
}

// TestIngestLockStandbyPG proves the HA contract for the ingestion loop:
// while one replica holds lock 1008, a second replica's RunLoopSupervisor
// body must NOT run (non-leader does not poll). When the holder releases,
// the standby acquires and runs.
func TestIngestLockStandbyPG(t *testing.T) {
	dsn := os.Getenv("ENCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("ENCODE_TEST_PG_DSN not set; skipping PG ingest HA test")
	}
	leader, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer leader.Close()
	standby, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer standby.Close()

	ctx := context.Background()
	release, ok, err := leader.TryAdvisoryLock(ctx, LockKeyIngest)
	if err != nil || !ok {
		t.Fatalf("leader must acquire 1008: ok=%v err=%v", ok, err)
	}

	ran := make(chan struct{}, 1)
	supCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go RunLoopSupervisor(supCtx, standby, log, LockKeyIngest, "ingest-test", func(ctx context.Context) {
		ran <- struct{}{}
		<-ctx.Done()
	})

	// While the leader holds the lock, the standby body must not run.
	select {
	case <-ran:
		t.Fatal("non-leader ran the ingest loop while the lock was held")
	case <-time.After(400 * time.Millisecond):
		// expected: standby waits (reAcquireDelay is 15s)
	}

	// Release → the standby acquires on its next retry and runs.
	release()
	select {
	case <-ran:
		// expected: failover within the retry window
	case <-time.After(20 * time.Second):
		t.Fatal("standby never acquired the released lock")
	}
	cancel()
}
