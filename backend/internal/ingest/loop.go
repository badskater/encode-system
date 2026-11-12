package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// Ledger is the dedupe surface the poll loop needs (kept narrow for
// tests; the production implementation is store.Store).
type Ledger interface {
	IngestSeen(ctx context.Context, provider, hash string) (bool, error)
	RecordIngest(ctx context.Context, provider, hash, name, series, episode, stagedPath string) (bool, error)
}

// Config is one poll cycle's live configuration.
type Config struct {
	Settings    model.IngestSettings
	ScriptsRoot string // controller-side scripts share root
}

// LiveConfig supplies the runtime parameters on every cycle so
// Settings-page edits apply without a controller restart (same pattern
// as scanner.LiveConfig).
type LiveConfig func(ctx context.Context) Config

// ClientFactory builds a download client from live settings. The API
// layer injects the production factory (or a fake in tests); building
// per-cycle means credential rotation applies without a restart.
type ClientFactory func(s model.IngestSettings) (Client, error)

// StagedEvent reports one successfully staged torrent to the caller
// (the API layer audits it as ingest.staged and fans out an alert).
type StagedEvent struct {
	Provider   string
	Hash       string
	Name       string
	Series     string
	Episode    string
	StagedPath string
	Files      []string
}

// DefaultPollInterval is the cadence when poll_interval_seconds is 0.
const DefaultPollInterval = 60 * time.Second

// Poll bounds mirror validateIngest so a hand-edited DB row cannot push
// the cadence outside the sane range even if it bypassed the API.
const (
	MinPollInterval = 10 * time.Second
	MaxPollInterval = 3600 * time.Second
)

// RunLoop polls the configured download client on the live interval
// until ctx is cancelled. Each tick: list completed torrents in the
// configured categories → resolve series/episode (category map overrides
// the parsed series; the episode is ALWAYS parsed, never guessed) →
// skip ledger-seen torrents → stage source files → record the ledger →
// report the event. Errors are isolated per torrent: one bad download
// never stops the batch or the loop.
//
// The loop body is a no-op while ingestion is disabled (the default), so
// wrapping it in the HA supervisor costs nothing on installs that never
// enable the feature. The ticker is rebuilt only when the interval
// changes (scanner.RunLoop cadence semantics).
func RunLoop(ctx context.Context, log *slog.Logger, ledger Ledger, cfg LiveConfig, factory ClientFactory, onStaged func(StagedEvent)) {
	if log == nil {
		log = slog.Default()
	}
	var tick *time.Ticker
	var curInterval time.Duration
	defer func() {
		if tick != nil {
			tick.Stop()
		}
	}()
	for {
		c := cfg(ctx)
		interval := pollInterval(c.Settings.PollIntervalSeconds)
		if tick == nil || interval != curInterval {
			if tick != nil {
				tick.Stop()
			}
			tick = time.NewTicker(interval)
			curInterval = interval
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			PollOnce(ctx, log, ledger, c, factory, onStaged)
		}
	}
}

// pollInterval resolves the configured cadence with the same bounds the
// API validation enforces (0 → default; out-of-range → default).
func pollInterval(seconds int) time.Duration {
	d := time.Duration(seconds) * time.Second
	if seconds == 0 || d < MinPollInterval || d > MaxPollInterval {
		return DefaultPollInterval
	}
	return d
}

// PollOnce performs one ingestion pass. Exported so tests (and a future
// manual-trigger endpoint) can run a cycle without the ticker.
func PollOnce(ctx context.Context, log *slog.Logger, ledger Ledger, c Config, factory ClientFactory, onStaged func(StagedEvent)) {
	if log == nil {
		log = slog.Default()
	}
	in := c.Settings
	if !in.Enabled {
		return // off by default — the tick is a no-op
	}
	if c.ScriptsRoot == "" {
		log.Warn("ingest: scripts_root not configured; skipping poll")
		return
	}
	client, err := factory(in)
	if err != nil {
		log.Warn("ingest: client construction failed", "provider", in.Provider, "err", err)
		return
	}
	for _, category := range pollCategories(in) {
		if ctx.Err() != nil {
			return
		}
		torrents, err := client.Completed(ctx, category)
		if err != nil {
			log.Warn("ingest: completed-list failed", "provider", in.Provider, "category", category, "err", err)
			continue // next category / next tick
		}
		for _, t := range torrents {
			if ctx.Err() != nil {
				return
			}
			stageTorrent(ctx, log, ledger, in, c.ScriptsRoot, t, onStaged)
		}
	}
}

// pollCategories resolves which categories to poll: the explicit filter
// wins; else the category-map keys (configured categories); else one
// empty category = all completed downloads.
func pollCategories(in model.IngestSettings) []string {
	if in.Category != "" {
		return []string{in.Category}
	}
	if len(in.CategoryMap) > 0 {
		cats := make([]string, 0, len(in.CategoryMap))
		for k := range in.CategoryMap {
			cats = append(cats, k)
		}
		sort.Strings(cats) // deterministic order for logs/tests
		return cats
	}
	return []string{""}
}

// stageTorrent processes ONE torrent with full error isolation: every
// failure logs and returns, never propagates to the batch.
func stageTorrent(ctx context.Context, log *slog.Logger, ledger Ledger, in model.IngestSettings, scriptsRoot string, t Torrent, onStaged func(StagedEvent)) {
	seen, err := ledger.IngestSeen(ctx, in.Provider, t.Hash)
	if err != nil {
		log.Warn("ingest: ledger check failed", "provider", in.Provider, "hash", t.Hash, "err", err)
		return
	}
	if seen {
		return // already staged (ledger survives restarts)
	}
	series, episode, ok := resolveSeriesEpisode(in, t)
	if !ok {
		log.Info("ingest: release name unparseable; skipped (never guessed)",
			"provider", in.Provider, "hash", t.Hash, "name", t.Name, "category", t.Category)
		return
	}
	destDir := filepath.Join(scriptsRoot, series, EpisodeDirName(episode))
	staged, err := Stage(ctx, in.DownloadRoot, t, destDir)
	if err != nil {
		log.Warn("ingest: staging failed", "provider", in.Provider, "hash", t.Hash,
			"name", t.Name, "series", series, "episode", episode, "err", err)
		return
	}
	relPath := filepath.Join(series, EpisodeDirName(episode))
	if _, err := ledger.RecordIngest(ctx, in.Provider, t.Hash, t.Name, series, fmt.Sprintf("%02d", episode), relPath); err != nil {
		// Files are staged but the ledger write failed: the next poll
		// re-runs staging (existing-file skip makes it idempotent) and
		// retries the record. Never report the event twice.
		log.Warn("ingest: ledger record failed (files staged; will retry)",
			"provider", in.Provider, "hash", t.Hash, "err", err)
		return
	}
	log.Info("ingest staged", "provider", in.Provider, "hash", t.Hash, "name", t.Name,
		"series", series, "episode", episode, "staged_path", relPath, "files", len(staged))
	if onStaged != nil {
		onStaged(StagedEvent{
			Provider:   in.Provider,
			Hash:       t.Hash,
			Name:       t.Name,
			Series:     series,
			Episode:    fmt.Sprintf("%02d", episode),
			StagedPath: relPath,
			Files:      staged,
		})
	}
}

// resolveSeriesEpisode applies the category→series override and parses
// the release name. The map supplies the SERIES only — the episode must
// still parse from the name (a mapped category with an unparseable name
// is skipped, never guessed).
func resolveSeriesEpisode(in model.IngestSettings, t Torrent) (series string, episode int, ok bool) {
	parsedSeries, ep, parsed := ParseRelease(t.Name)
	if mapped, has := in.CategoryMap[t.Category]; has && mapped != "" {
		if !parsed || !ValidSeriesName(mapped) {
			return "", 0, false
		}
		return mapped, ep, true
	}
	if !parsed {
		return "", 0, false
	}
	return parsedSeries, ep, true
}
