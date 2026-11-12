package store

import (
	"context"
	"fmt"
)

// ---------- Ingest ledger (P11, experimental download ingestion) ----------

// ensureIngestLedger creates the ingest dedupe ledger (idempotent; called
// from migrate so every SQLite Open gets it). One row per staged torrent:
// the (provider, torrent_hash) pair is unique so the same download is
// never staged twice, and the row survives controller restarts.
func (s *Store) ensureIngestLedger(ctx context.Context) error {
	_, err := s.qExec(ctx, `
CREATE TABLE IF NOT EXISTS ingest_ledger (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  provider TEXT NOT NULL,
  torrent_hash TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  series TEXT NOT NULL DEFAULT '',
  episode TEXT NOT NULL DEFAULT '',
  staged_path TEXT NOT NULL,
  staged_at TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE(provider, torrent_hash)
)`)
	return err
}

// IngestSeen reports whether a torrent was already staged by the given
// provider. The poll loop consults it BEFORE staging so a completed
// download that stays in the client's list is processed exactly once.
func (s *Store) IngestSeen(ctx context.Context, provider, hash string) (bool, error) {
	var n int
	err := s.qRow(ctx,
		`SELECT COUNT(*) FROM ingest_ledger WHERE provider = ? AND torrent_hash = ?`,
		provider, hash).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// RecordIngest writes the ledger row for a successfully staged torrent.
// ON CONFLICT DO NOTHING is the dual-backend idempotent upsert (same as
// UpsertSeriesByName): a duplicate record — e.g. a retry after a partial
// failure between staging and recording — is a silent no-op, never an
// error. Returns whether a NEW row was inserted.
func (s *Store) RecordIngest(ctx context.Context, provider, hash, name, series, episode, stagedPath string) (bool, error) {
	res, err := s.qExec(ctx, `
INSERT INTO ingest_ledger (provider, torrent_hash, name, series, episode, staged_path)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(provider, torrent_hash) DO NOTHING`,
		provider, hash, name, series, episode, stagedPath)
	if err != nil {
		return false, fmt.Errorf("record ingest: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// IngestLedgerEntry is one staged-torrent row, served for diagnostics.
type IngestLedgerEntry struct {
	Provider    string `json:"provider"`
	TorrentHash string `json:"torrent_hash"`
	Name        string `json:"name"`
	Series      string `json:"series"`
	Episode     string `json:"episode"`
	StagedPath  string `json:"staged_path"`
	StagedAt    string `json:"staged_at"`
}

// ListIngestLedger returns the newest limit ledger rows (diagnostics /
// future UI). limit is clamped 1-500 by the caller; the store applies a
// hard cap of 500 so a hand-built query cannot dump the whole table.
func (s *Store) ListIngestLedger(ctx context.Context, limit int) ([]IngestLedgerEntry, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.qQuery(ctx, `
SELECT provider, torrent_hash, name, series, episode, staged_path, staged_at
FROM ingest_ledger ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IngestLedgerEntry
	for rows.Next() {
		var e IngestLedgerEntry
		if err := rows.Scan(&e.Provider, &e.TorrentHash, &e.Name, &e.Series, &e.Episode, &e.StagedPath, &e.StagedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
