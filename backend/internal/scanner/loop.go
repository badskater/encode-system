// Package scanner's controller integration: RunLoop periodically scans the
// scripts share and creates jobs for new ready episode folders.
package scanner

import (
	"context"
	"log/slog"
	"time"

	"github.com/badskater/encode-system/backend/internal/flow"
	"github.com/badskater/encode-system/backend/internal/model"
)

// JobCreator is the subset of store the loop needs (kept narrow for tests).
type JobCreator interface {
	JobExistsForEpisode(ctx context.Context, episodeDir string) (bool, error)
	CreateJob(ctx context.Context, j *model.Job) (*model.Job, error)
	// Series registry: auto-register on first sight, honor flow selection.
	UpsertSeriesByName(ctx context.Context, name string) (*model.Series, error)
	GetFlow(ctx context.Context, id int64) (*model.Flow, error)
	DefaultFlow(ctx context.Context) (*model.Flow, error)
	FlowByName(ctx context.Context, name string) (*model.Flow, error)
}

// SourceStableFor defers job creation for sources modified more recently than
// this window — large NFS copies look "ready" by existence long before the
// last bytes land.
const SourceStableFor = 2 * time.Minute

// LiveConfig supplies the scanner's runtime parameters on every cycle so
// Settings-page edits (watch root, cadence) apply without restarting the
// controller. Returns the scripts root, the scan interval, and the default
// flow name.
type LiveConfig func(ctx context.Context) (root string, interval time.Duration, defaultFlow string)

// RunLoop scans on the live-configured interval until ctx is cancelled. It
// logs each created job and skips folders that already have a job (any
// non-cancelled status). The interval is re-read each cycle so Settings-page
// edits apply on the next cycle without a restart; the ticker is stopped via
// defer on every iteration so a panic in scanOnce cannot leak it.
func RunLoop(ctx context.Context, log *slog.Logger, st JobCreator, cfg LiveConfig) {
	for {
		root, interval, defaultFlow := cfg(ctx)
		if interval <= 0 {
			interval = 30 * time.Second
		}
		scanWithTicker(ctx, log, st, root, defaultFlow, interval)
		if ctx.Err() != nil {
			return
		}
	}
}

// scanWithTicker runs exactly one scan after interval elapses (or returns
// early on ctx cancel). Deferred Stop keeps the ticker leak-safe.
func scanWithTicker(ctx context.Context, log *slog.Logger, st JobCreator, root, defaultFlow string, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	select {
	case <-ctx.Done():
	case <-tick.C:
		scanOnce(ctx, log, st, root, defaultFlow)
	}
}

// scanOnce performs a single scan + job creation pass. Series are
// auto-registered on first sight; disabled series are skipped; each series'
// flow selection wins over the default flow.
func scanOnce(ctx context.Context, log *slog.Logger, st JobCreator, root, defaultFlow string) {
	cands, skipped, err := Scan(root, SourceStableFor)
	if err != nil {
		log.Warn("scan failed", "root", root, "err", err)
		return
	}
	if skipped > 0 {
		log.Warn("scan skipped unreadable dirs", "count", skipped, "root", root)
	}
	for _, c := range cands {
		sr, err := st.UpsertSeriesByName(ctx, c.Series)
		if err != nil {
			log.Warn("series registration failed", "series", c.Series, "err", err)
			continue
		}
		if !sr.Enabled {
			continue // operator paused this series; folder stays unprocessed
		}
		exists, err := st.JobExistsForEpisode(ctx, c.EpisodeDir)
		if err != nil {
			log.Warn("dedupe check failed", "episode_dir", c.EpisodeDir, "err", err)
			continue
		}
		if exists {
			continue
		}
		fl, err := resolveSeriesFlow(ctx, st, sr, defaultFlow)
		if err != nil {
			log.Warn("flow resolution failed", "series", c.Series, "err", err)
			continue
		}
		job, err := st.CreateJob(ctx, &model.Job{
			Series:     c.Series,
			Episode:    flow.EpisodeNumber(c.EpisodeDir),
			EpisodeDir: c.EpisodeDir,
			ScriptType: c.ScriptType,
			ScriptFile: c.ScriptFile,
			FlowID:     fl.ID,
		})
		if err != nil {
			log.Warn("create job failed", "episode_dir", c.EpisodeDir, "err", err)
			continue
		}
		log.Info("job created from scan", "job", job.ID, "episode_dir", c.EpisodeDir,
			"script", c.ScriptFile, "flow", fl.Name)
	}
}

// resolveSeriesFlow picks the series' explicit flow, else the flagged default
// flow, else the configured default name.
func resolveSeriesFlow(ctx context.Context, st JobCreator, sr *model.Series, defaultName string) (*model.Flow, error) {
	if sr.FlowID > 0 {
		if fl, err := st.GetFlow(ctx, sr.FlowID); err == nil {
			return fl, nil
		}
	}
	if fl, err := st.DefaultFlow(ctx); err == nil {
		return fl, nil
	}
	return st.FlowByName(ctx, defaultName)
}
