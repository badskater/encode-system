// Package scanner's controller integration: RunLoop periodically scans the
// scripts share and creates jobs for new ready episode folders.
package scanner

import (
	"context"
	"log/slog"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// JobCreator is the subset of store the loop needs (kept narrow for tests).
type JobCreator interface {
	JobExistsForEpisode(ctx context.Context, episodeDir string) (bool, error)
	CreateJob(ctx context.Context, j *model.Job) (*model.Job, error)
	FlowByName(ctx context.Context, name string) (*model.Flow, error)
}

// RunLoop scans every interval until ctx is cancelled. It logs each created
// job and skips folders that already have a job (any non-cancelled status).
func RunLoop(ctx context.Context, log *slog.Logger, st JobCreator, root, defaultFlow string, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			scanOnce(ctx, log, st, root, defaultFlow)
		}
	}
}

// scanOnce performs a single scan + job creation pass.
func scanOnce(ctx context.Context, log *slog.Logger, st JobCreator, root, defaultFlow string) {
	cands, err := Scan(root)
	if err != nil {
		log.Warn("scan failed", "root", root, "err", err)
		return
	}
	fl, err := st.FlowByName(ctx, defaultFlow)
	if err != nil {
		log.Warn("default flow missing", "flow", defaultFlow, "err", err)
		return
	}
	for _, c := range cands {
		exists, err := st.JobExistsForEpisode(ctx, c.EpisodeDir)
		if err != nil {
			log.Warn("dedupe check failed", "episode_dir", c.EpisodeDir, "err", err)
			continue
		}
		if exists {
			continue
		}
		job, err := st.CreateJob(ctx, &model.Job{
			Series:     c.Series,
			Episode:    episodeFromDir(c.EpisodeDir),
			EpisodeDir: c.EpisodeDir,
			ScriptType: c.ScriptType,
			FlowID:     fl.ID,
		})
		if err != nil {
			log.Warn("create job failed", "episode_dir", c.EpisodeDir, "err", err)
			continue
		}
		log.Info("job created from scan", "job", job.ID, "episode_dir", c.EpisodeDir,
			"script", c.ScriptFile, "flow", defaultFlow)
	}
}

// episodeFromDir extracts the episode token from "Series/Ep NN".
func episodeFromDir(dir string) string {
	for i := len(dir) - 1; i >= 0; i-- {
		if dir[i] == '/' {
			return epToken(dir[i+1:])
		}
	}
	return epToken(dir)
}

// epToken strips the "Ep " prefix when present.
func epToken(s string) string {
	const p = "Ep "
	if len(s) > len(p) && s[:len(p)] == p {
		return s[len(p):]
	}
	return s
}
