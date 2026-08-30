package api

import (
	"net/http"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// ---------- Phase E: fleet stats ----------

// handleStats returns the fleet-wide aggregate snapshot over the job
// history, scoped to a range window (?range=24h|7d|30d|all, default 7d).
//
// The range mirrors handleNodeMetrics's style: default 7d, an unknown value
// falls back to 7d (not a 400) because the links are UI-generated and a bad
// value is a stale bookmark, not a user typo worth hard-stopping on. The
// aggregates are computed in SQL from the existing jobs columns + joins —
// no new storage. An empty/never-used fleet returns a zeroed shape, not an
// error, so the Stats page renders cleanly on a fresh install.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	// Parse the range param. Default 7d; accepted values 24h/7d/30d/all.
	// An unknown value falls back to 7d rather than 400-ing — mirrors
	// handleNodeMetrics's forgiving stance on UI-generated links.
	rng := 7 * 24 * time.Hour
	switch r.URL.Query().Get("range") {
	case "24h":
		rng = 24 * time.Hour
	case "7d":
		rng = 7 * 24 * time.Hour
	case "30d":
		rng = 30 * 24 * time.Hour
	case "all":
		rng = 0 // no lower bound
	}
	stats, err := s.Store.JobStats(r.Context(), rng)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "compute stats")
		return
	}
	// Guarantee the sub-slices marshal as [] not null even on a fresh
	// install (the store already returns empty slices, but belt-and-suspenders
	// so the frontend never has to null-guard).
	if stats.PerNode == nil {
		stats.PerNode = []model.StatsNodeRow{}
	}
	if stats.PerFlow == nil {
		stats.PerFlow = []model.StatsFlowRow{}
	}
	if stats.FailuresByStep == nil {
		stats.FailuresByStep = []model.StatsStepRow{}
	}
	if stats.PerDay == nil {
		stats.PerDay = []model.StatsDayRow{}
	}
	writeJSON(w, http.StatusOK, stats)
}
