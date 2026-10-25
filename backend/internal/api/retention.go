package api

import (
	"context"
	"net/http"
	"time"

	"github.com/badskater/encode-system/backend/internal/store"
)

// retentionInterval is the retention loop's tick cadence. Hourly matches the
// digest loop: retention is a background hygiene task, not a latency-
// sensitive one — an hour's slack on a days-granular cutoff is invisible.
const retentionInterval = time.Hour

// StartRetentionLoop launches the hourly job-history pruner as a background
// goroutine bound to ctx (lifecycle mirrors StartDigestLoop). Each tick
// reads the LIVE settings so a Settings-page edit applies on the next tick
// without a restart; JobRetentionDays=0 (default) makes the tick a no-op.
//
// HA: on Postgres the loop body runs under store.RunLoopSupervisor with
// LockKeyRetention so exactly one replica prunes per tick (two replicas
// pruning concurrently is harmless — PruneOldJobs is a DELETE with a WHERE
// clause — but the lock avoids redundant work and log noise). On SQLite
// the supervisor is a transparent always-locked wrapper and the loop runs
// unchanged; ctx-cancel/SIGTERM stop is preserved.
func (s *Server) StartRetentionLoop(ctx context.Context) {
	go store.RunLoopSupervisor(ctx, s.Store, s.Log, store.LockKeyRetention, "retention", func(ctx context.Context) {
		s.runRetentionLoop(ctx)
	})
}

func (s *Server) runRetentionLoop(ctx context.Context) {
	tick := time.NewTicker(retentionInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.pruneJobsTick(ctx)
		}
	}
}

// pruneJobsTick performs one retention pass. Errors are logged, not fatal:
// retention is hygiene, and a transient DB error must not kill the loop (it
// retries next tick).
func (s *Server) pruneJobsTick(ctx context.Context) {
	days := s.currentSettings(ctx).JobRetentionDays
	if days <= 0 {
		return // retention disabled
	}
	n, err := s.Store.PruneOldJobs(ctx, days)
	if err != nil {
		s.Log.Warn("job retention prune failed", "err", err, "days", days)
		return
	}
	if n > 0 {
		s.Log.Info("job retention pruned old jobs", "deleted", n, "retention_days", days)
	}
}

// handlePruneJobs runs an immediate retention pass on demand
// (POST /api/jobs/prune). Body: {"days": N} — required, same bounds as the
// setting (1-3650). Explicit days instead of reading the setting: a manual
// prune is a deliberate one-shot cleanup that an operator may want deeper
// than the standing policy; 0/negative is a 400, never a silent no-op, so a
// fat-fingered request cannot be mistaken for success.
func (s *Server) handlePruneJobs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Days *int `json:"days"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Days == nil {
		writeErr(w, http.StatusBadRequest, "days is required")
		return
	}
	if *req.Days < 1 || *req.Days > 3650 {
		writeErr(w, http.StatusBadRequest, "days must be 1-3650")
		return
	}
	n, err := s.Store.PruneOldJobs(r.Context(), *req.Days)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "prune jobs")
		return
	}
	s.Log.Info("manual job prune", "deleted", n, "days", *req.Days)
	s.audit(r, "jobs.prune", "jobs", map[string]any{"deleted": n, "days": *req.Days})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n, "days": *req.Days})
}
