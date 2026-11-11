package api

import (
	"context"
	"fmt"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/store"
)

// stallWatchdogInterval is the watchdog's tick cadence. 30s matches the
// scanner/node-alert cadence; stall thresholds are minute-scale, so one
// tick of detection slack is invisible.
const stallWatchdogInterval = 30 * time.Second

// StartStallWatchdogLoop launches the job stall watchdog as a background
// goroutine bound to ctx (lifecycle mirrors StartRetentionLoop). Each tick
// reads the LIVE settings so a Settings-page edit applies on the next tick
// without a restart; StallTimeoutMinutes=0 (default) makes the tick a no-op.
//
// The watchdog fails RUNNING jobs whose last_progress_at (stamped on claim,
// refreshed by heartbeats reporting changed step/progress) is older than the
// threshold. A wedged x265 that keeps heartbeating the same percentage never
// refreshes the stamp, so it is detected even though the node stays online —
// the gap repeat_failures and orphan recovery both miss.
//
// HA: on Postgres the loop body runs under store.RunLoopSupervisor with
// LockKeyStallWatchdog so exactly one replica fails stalled jobs (two
// replicas would double-count retries and double-notify). On SQLite the
// supervisor is a transparent always-locked wrapper; ctx-cancel/SIGTERM stop
// is preserved.
func (s *Server) StartStallWatchdogLoop(ctx context.Context) {
	go store.RunLoopSupervisor(ctx, s.Store, s.Log, store.LockKeyStallWatchdog, "stall-watchdog", func(ctx context.Context) {
		s.runStallWatchdogLoop(ctx)
	})
}

func (s *Server) runStallWatchdogLoop(ctx context.Context) {
	tick := time.NewTicker(stallWatchdogInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.stallWatchdogTick(ctx)
		}
	}
}

// stallWatchdogTick performs one watchdog pass. Errors are logged, not
// fatal: a transient DB error must not kill the loop (it retries next tick).
func (s *Server) stallWatchdogTick(ctx context.Context) {
	minutes := s.currentSettings(ctx).StallTimeoutMinutes
	if minutes <= 0 {
		return // watchdog disabled
	}
	cutoff := time.Now().UTC().Add(-time.Duration(minutes) * time.Minute)
	jobs, err := s.Store.StalledRunningJobs(ctx, cutoff)
	if err != nil {
		s.Log.Warn("stall watchdog: query failed", "err", err)
		return
	}
	for _, j := range jobs {
		s.failStalledJob(ctx, j, minutes)
	}
}

// failStalledJob fails one stalled job through the EXISTING job-failure
// path — the same sequence handleJobComplete and orphan recovery use — so
// the flow's max_retries/retry_backoff policy, node release, SSE final
// event, and Discord notification all apply exactly as for an agent-reported
// failure. The audit event and structured log fire regardless of the retry
// outcome (the stall happened either way); the notification is skipped on
// auto-retry, matching every other silent-retry path.
func (s *Server) failStalledJob(ctx context.Context, j *model.Job, minutes int) {
	errMsg := fmt.Sprintf("stalled: no progress for %d minutes", minutes)
	if err := s.Store.FinishJob(ctx, j.ID, model.JobFailed, -1, errMsg, nil, ""); err != nil {
		s.Log.Warn("stall watchdog: finish job failed", "err", err, "job", j.ID)
		return
	}
	// Free the node's slot (mirrors handleJobComplete): the job left the
	// running state, so the node must not stay phantom-busy.
	if j.NodeID > 0 {
		if err := s.Store.ReleaseNode(ctx, j.NodeID); err != nil {
			s.Log.Warn("stall watchdog: release node failed", "err", err, "job", j.ID, "node", j.NodeID)
		}
	}
	s.Log.Warn("job stalled — failed by watchdog", "job", j.ID, "node", j.NodeID,
		"series", j.Series, "episode_dir", j.EpisodeDir, "step", j.Step,
		"stall_timeout_minutes", minutes)
	s.auditSystem(ctx, "job.stalled", auditObject("job", j.ID), map[string]any{
		"episode_dir":           j.EpisodeDir,
		"node_id":               j.NodeID,
		"stall_timeout_minutes": minutes,
	})
	// Re-read so shouldAutoRetry sees the just-stamped failed state (same
	// pattern as orphan recovery).
	failed, err := s.Store.GetJob(ctx, j.ID)
	if err != nil {
		// The row is already failed and visible in the UI; skip retry/notify
		// this cycle rather than stranding the loop.
		s.Log.Error("stall watchdog: GetJob after finish failed (job already failed+visible; skipped retry/notify this cycle)",
			"job", j.ID, "err", err)
		return
	}
	if s.shouldAutoRetry(ctx, failed, j.ID) {
		s.Log.Info("stalled job auto-retried", "job", j.ID)
		return
	}
	// Terminal SSE event so open log streams close (mirrors the completion
	// and orphan paths), then the standard job-failure notification.
	s.publishFinal(j.ID, string(model.JobFailed), failed.Error, failed.ExitCode, failed.FullLog != "")
	s.notifyJobFinished(ctx, j.ID, "controller")
}
