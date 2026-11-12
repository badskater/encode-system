package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/notify"
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

// MaxTorrentsPerPoll bounds one poll cycle's fanout (Q11): a client with
// thousands of completed torrents must not turn one tick into an
// unbounded staging marathon. Torrents beyond the cap are deferred to
// the next poll (the ledger makes progress incremental) and the
// truncation is logged as a warning.
const MaxTorrentsPerPoll = 500

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
	// One skip-log tracker for the loop's lifetime (Q12): the cooldown
	// must span polls, not reset every tick.
	skips := newSkipTracker()
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
			pollOnce(ctx, log, ledger, c, factory, onStaged, skips)
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
// manual-trigger endpoint) can run a cycle without the ticker. Each call
// gets a fresh skip-log tracker; RunLoop carries one across ticks so the
// cooldown (Q12) spans polls.
func PollOnce(ctx context.Context, log *slog.Logger, ledger Ledger, c Config, factory ClientFactory, onStaged func(StagedEvent)) {
	pollOnce(ctx, log, ledger, c, factory, onStaged, newSkipTracker())
}

// pollOnce is the poll body with an explicit skip-log tracker.
func pollOnce(ctx context.Context, log *slog.Logger, ledger Ledger, c Config, factory ClientFactory, onStaged func(StagedEvent), skips *skipTracker) {
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
	skips.purge(time.Now()) // bound the cooldown map (Q12)
	client, err := factory(in)
	if err != nil {
		// Construction errors can echo the configured URL (credentials
		// included) — render through RedactURL (Q4).
		log.Warn("ingest: client construction failed", "provider", in.Provider, "err", notify.RedactURL(err.Error()))
		return
	}
	processed := 0
	for _, category := range pollCategories(in) {
		if ctx.Err() != nil {
			return
		}
		torrents, err := client.Completed(ctx, category)
		if err != nil {
			// Transport errors embed the full request URL — redact (Q4).
			log.Warn("ingest: completed-list failed", "provider", in.Provider, "category", category, "err", notify.RedactURL(err.Error()))
			continue // next category / next tick
		}
		for _, t := range torrents {
			if ctx.Err() != nil {
				return
			}
			if processed >= MaxTorrentsPerPoll {
				// Per-poll fanout cap (Q11): defer the rest to the next
				// poll (the ledger makes progress incremental).
				log.Warn("ingest: per-poll torrent cap reached; remaining torrents deferred to the next poll",
					"provider", in.Provider, "cap", MaxTorrentsPerPoll)
				return
			}
			processed++
			stageTorrent(ctx, log, ledger, in, c.ScriptsRoot, t, onStaged, skips)
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
// failure logs and returns, never propagates to the batch. Repeated
// skips of the same hash log at full level once per cooldown window
// (Q12); repeats drop to debug.
func stageTorrent(ctx context.Context, log *slog.Logger, ledger Ledger, in model.IngestSettings, scriptsRoot string, t Torrent, onStaged func(StagedEvent), skips *skipTracker) {
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
		if skips.allow(t.Hash, time.Now()) {
			log.Info("ingest: release name unparseable; skipped (never guessed)",
				"provider", in.Provider, "hash", t.Hash, "name", t.Name, "category", t.Category)
		} else {
			log.Debug("ingest: release name unparseable; skipped (log cooldown)",
				"provider", in.Provider, "hash", t.Hash, "name", t.Name, "category", t.Category)
		}
		return
	}
	destDir := filepath.Join(scriptsRoot, series, EpisodeDirName(episode))
	staged, err := Stage(ctx, in.DownloadRoot, t, destDir)
	if err != nil {
		if skips.allow(t.Hash, time.Now()) {
			log.Warn("ingest: staging failed", "provider", in.Provider, "hash", t.Hash,
				"name", t.Name, "series", series, "episode", episode, "err", notify.RedactURL(err.Error()))
		} else {
			log.Debug("ingest: staging failed (log cooldown)", "provider", in.Provider, "hash", t.Hash,
				"name", t.Name, "series", series, "episode", episode, "err", notify.RedactURL(err.Error()))
		}
		return
	}
	relPath := filepath.Join(series, EpisodeDirName(episode))
	inserted, err := ledger.RecordIngest(ctx, in.Provider, t.Hash, t.Name, series, fmt.Sprintf("%02d", episode), relPath)
	if err != nil {
		// Files are staged but the ledger write failed: the next poll
		// re-runs staging (existing-file skip makes it idempotent) and
		// retries the record. Never report the event twice.
		log.Warn("ingest: ledger record failed (files staged; will retry)",
			"provider", in.Provider, "hash", t.Hash, "err", err)
		return
	}
	if !inserted {
		// Another poller recorded this torrent first (HA lock-loss
		// window): the ledger deduped the row — suppress the duplicate
		// event so the operator gets exactly one alert (Q6).
		log.Debug("ingest: torrent already recorded by another poller; duplicate event suppressed",
			"provider", in.Provider, "hash", t.Hash)
		return
	}
	if len(staged) == 0 {
		// Cross-torrent same-episode collision (Q9): every file was
		// already present (another release staged this episode first).
		// The ledger row stays (dedupe), but this is NOT a success —
		// the event carries zero files so the API layer audits a
		// duplicate and sends no alert.
		log.Info("ingest: duplicate release — all files already present",
			"provider", in.Provider, "hash", t.Hash, "name", t.Name,
			"series", series, "episode", episode, "staged_path", relPath)
	} else {
		log.Info("ingest staged", "provider", in.Provider, "hash", t.Hash, "name", t.Name,
			"series", series, "episode", episode, "staged_path", relPath, "files", len(staged))
	}
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

// skipLogCooldown bounds how often a repeatedly-skipped torrent may log
// at full level (Q12): an unparseable or failing torrent is re-listed by
// the client on EVERY poll, and logging each skip produced ~8640
// lines/day/torrent. One full-level log per hour per hash is enough to
// diagnose; the repeats drop to debug.
const skipLogCooldown = time.Hour

// skipTracker is the per-hash in-memory skip-log cooldown. It lives for
// the lifetime of the poll loop (RunLoop owns one); it is deliberately
// NOT persisted — a restart may re-log once, which is harmless.
type skipTracker struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newSkipTracker() *skipTracker {
	return &skipTracker{last: make(map[string]time.Time)}
}

// allow reports whether hash's skip should log at full level now: the
// first skip in a cooldown window logs, repeats inside the window do not.
func (s *skipTracker) allow(hash string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.last[hash]; ok && now.Sub(t) < skipLogCooldown {
		return false
	}
	s.last[hash] = now
	return true
}

// purge drops expired entries so the map stays bounded by the torrents
// skipped within the last cooldown window (called once per poll).
func (s *skipTracker) purge(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, t := range s.last {
		if now.Sub(t) >= skipLogCooldown {
			delete(s.last, h)
		}
	}
}
