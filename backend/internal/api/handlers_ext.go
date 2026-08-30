package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/store"
)

// ---------- Series ----------

// seriesView is the JSON shape of GET /api/series: the embedded Series row
// plus the legacy jobs count and the Phase-F1 progress counters.
type seriesView struct {
	*model.Series
	Jobs int `json:"jobs"`
	// Phase F1: per-series encode progress. Done counts distinct episodes
	// with a successful job; Failed counts episodes that never succeeded
	// (an episode that eventually succeeded is done, not failed); Active
	// counts episodes with an in-flight job; Total is the best available
	// denominator (scaffolded episode folders on disk, or the distinct
	// episode_dir count from jobs when the scripts root is not readable).
	EpisodesDone   int `json:"episodes_done"`
	EpisodesFailed int `json:"episodes_failed"`
	EpisodesActive int `json:"episodes_active"`
	EpisodesTotal  int `json:"episodes_total"`
}

// handleListSeries returns all registered series with job counts and
// per-series encode progress for the UI. Progress counts are derived from
// the jobs table in one query (done/failed/active with "eventually done
// wins"); the total uses scaffolded episode folders on disk when the
// scripts root is readable, falling back to the distinct episode_dir count
// from jobs.
func (s *Server) handleListSeries(w http.ResponseWriter, r *http.Request) {
	series, err := s.Store.ListSeries(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list series")
		return
	}
	jobs, err := s.Store.ListJobs(r.Context(), "", 0)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list jobs")
		return
	}
	// Count jobs per series for the overview column (legacy field).
	counts := map[string]int{}
	for _, j := range jobs {
		counts[j.Series]++
	}
	// Phase F1: per-series progress from job history (one query, no N+1).
	progress, err := s.Store.SeriesProgressCounts(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "series progress")
		return
	}
	// Best-effort scaffolded-folder totals from the scripts root. The root
	// is the controller-side mount; if it is missing or unreadable we fall
	// back to the jobs-derived distinct count (progress.Seen). Wrapped
	// defensively — a missing/unreadable root must never break the list.
	scaffolded := s.countScaffoldedEpisodes(r.Context(), series, progress)

	out := make([]seriesView, 0, len(series))
	for _, sr := range series {
		p := progress[sr.Name] // absent → zero-value (no jobs)
		total := scaffolded[sr.Name]
		if total == 0 {
			total = p.Seen // fallback: distinct dirs seen in jobs
		}
		out = append(out, seriesView{
			Series:         sr,
			Jobs:           counts[sr.Name],
			EpisodesDone:   p.Done,
			EpisodesFailed: p.Failed,
			EpisodesActive: p.Active,
			EpisodesTotal:  total,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// countScaffoldedEpisodes reads the scripts root and counts subdirectories
// matching "Ep *" under each series' folder. Returns a map keyed by series
// name. Best-effort: a missing/unreadable root or series dir yields 0 for
// that series (the caller falls back to the jobs-derived count). This is
// the best available denominator — model.Series has no persisted episode
// count column, so the filesystem is the only source of truth for the
// scaffolded total.
func (s *Server) countScaffoldedEpisodes(ctx context.Context, series []*model.Series, progress map[string]store.SeriesProgress) map[string]int {
	out := make(map[string]int, len(series))
	root := s.currentSettings(ctx).ScriptsRoot
	if root == "" {
		return out
	}
	for _, sr := range series {
		dir := filepath.Join(root, sr.Name)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // missing/unreadable → fallback to jobs-derived count
		}
		n := 0
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if strings.HasPrefix(e.Name(), "Ep ") {
				n++
			}
		}
		out[sr.Name] = n
	}
	return out
}

// handlePatchSeries updates a series' flow selection, enabled state, notify
// (mute) flag, and tag override. A series with flow_id 0 falls back to the
// default flow; disabled series are skipped by the scanner (no new jobs are
// created for them).
func (s *Server) handlePatchSeries(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad series id")
		return
	}
	var req struct {
		FlowID  *int64  `json:"flow_id"`
		Enabled *bool   `json:"enabled"`
		Notify  *bool   `json:"notify"`
		Tag     *string `json:"tag"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid patch")
		return
	}
	ctx := r.Context()
	sr, err := s.Store.GetSeries(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "series not found")
		return
	}
	// Field-scoped updates: each field is written by its own SQL statement,
	// so two concurrent PATCHes touching different fields cannot lose writes.
	if req.FlowID != nil {
		if *req.FlowID != 0 {
			if _, err := s.Store.GetFlow(ctx, *req.FlowID); err != nil {
				writeErr(w, http.StatusBadRequest, "flow not found")
				return
			}
		}
		if err := s.Store.SetSeriesFlow(ctx, id, *req.FlowID); err != nil {
			writeErr(w, http.StatusInternalServerError, "update series flow")
			return
		}
	}
	if req.Tag != nil {
		tag := strings.TrimSpace(*req.Tag)
		if err := validateSeriesName(tag); tag != "" && err != nil {
			writeErr(w, http.StatusBadRequest, "invalid tag: "+err.Error())
			return
		}
		if err := s.Store.SetSeriesTag(ctx, id, tag); err != nil {
			writeErr(w, http.StatusInternalServerError, "update series tag")
			return
		}
	}
	if req.Enabled != nil {
		if err := s.Store.SetSeriesEnabled(ctx, id, *req.Enabled); err != nil {
			writeErr(w, http.StatusInternalServerError, "update series enabled")
			return
		}
	}
	if req.Notify != nil {
		if err := s.Store.SetSeriesNotify(ctx, id, *req.Notify); err != nil {
			writeErr(w, http.StatusInternalServerError, "update series notify")
			return
		}
	}
	sr, err = s.Store.GetSeries(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "series not found")
		return
	}
	writeJSON(w, http.StatusOK, sr)
}

// ---------- Flow default / export / import ----------

// handleSetDefaultFlow marks one flow as THE default (all others cleared).
func (s *Server) handleSetDefaultFlow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad flow id")
		return
	}
	if err := s.Store.SetDefaultFlow(r.Context(), id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeErr(w, http.StatusNotFound, "flow not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "set default flow")
		return
	}
	fl, err := s.Store.GetFlow(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "flow not found")
		return
	}
	s.Log.Info("default flow changed", "flow", fl.Name)
	writeJSON(w, http.StatusOK, fl)
}

// flowExport is the portable JSON shape of a flow: the flow itself plus every
// custom step template it references, so an import on another controller is
// self-contained. Built-in templates are not embedded (they ship with the
// controller); an import warns when a custom template is missing.
type flowExport struct {
	Flow      model.Flow            `json:"flow"`
	Templates []*model.StepTemplate `json:"templates"`
}

// handleExportFlow returns the portable JSON for one flow.
func (s *Server) handleExportFlow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad flow id")
		return
	}
	ctx := r.Context()
	fl, err := s.Store.GetFlow(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "flow not found")
		return
	}
	exp := flowExport{Flow: *fl}
	// The exported flow never carries the default flag — defaultness is a
	// per-controller decision made at import time or via "make default".
	exp.Flow.IsDefault = false
	for _, st := range fl.Steps {
		t, err := s.Store.StepTemplateByKey(ctx, st.TemplateKey())
		if err != nil {
			continue // built-in or missing: built-ins resolve at import time
		}
		if !t.Builtin {
			exp.Templates = append(exp.Templates, t)
		}
	}
	// Sanitize the filename: user-controlled flow names must not inject
	// quotes/CR/LF into the response header.
	w.Header().Set("Content-Disposition", "attachment; filename=\""+exportFileName(fl.Name)+".json\"")
	writeJSON(w, http.StatusOK, exp)
}

// exportFileName restricts a flow name to filename-safe characters for the
// Content-Disposition header.
func exportFileName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "flow"
	}
	return b.String()
}

// handleImportFlow accepts a flowExport JSON. Behavior:
//   - custom templates are created/updated (by key)
//   - the flow is created with a new name when the name is taken
//   - steps referencing unknown templates are rejected up front
func (s *Server) handleImportFlow(w http.ResponseWriter, r *http.Request) {
	var exp flowExport
	if err := decodeJSON(r, &exp); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid import payload: "+err.Error())
		return
	}
	if exp.Flow.Name == "" || len(exp.Flow.Steps) == 0 {
		writeErr(w, http.StatusBadRequest, "import needs a named flow with steps")
		return
	}
	ctx := r.Context()

	// Install embedded custom templates first so validation can see them.
	// An import NEVER overwrites an existing template with different content:
	// other flows may reference it, and template bodies execute on agents.
	// Identical re-imports (same key + same script) pass through as no-ops.
	for _, t := range exp.Templates {
		t.ID = 0
		t.Builtin = false // imported templates are never built-ins
		if existing, err := s.Store.StepTemplateByKey(ctx, t.Key); err == nil {
			if existing.PowerShell != t.PowerShell {
				writeErr(w, http.StatusConflict,
					"template "+t.Key+" already exists with different content; delete or rename before importing")
				return
			}
			continue
		}
		if _, err := s.Store.UpsertStepTemplate(ctx, t); err != nil {
			writeErr(w, http.StatusBadRequest, "import template "+t.Key+": "+err.Error())
			return
		}
	}

	// Validate every step resolves to a known template.
	for _, st := range exp.Flow.Steps {
		if _, err := s.Store.StepTemplateByKey(ctx, st.TemplateKey()); err != nil {
			writeErr(w, http.StatusBadRequest,
				"step "+string(st.Type)+" references unknown template "+st.TemplateKey())
			return
		}
	}

	// Pick a free name (bounded attempts; a hostile import cannot force an
	// unbounded lookup loop).
	name := exp.Flow.Name
	picked := false
	for i := 2; i <= 50; i++ {
		if _, err := s.Store.FlowByName(ctx, name); err != nil {
			picked = true
			break
		}
		name = exp.Flow.Name + "-" + strconv.Itoa(i)
	}
	if !picked {
		writeErr(w, http.StatusConflict, "too many name collisions during import")
		return
	}
	exp.Flow.ID = 0
	exp.Flow.Name = name
	exp.Flow.IsDefault = false
	created, err := s.Store.CreateFlow(ctx, &exp.Flow)
	if err != nil {
		writeErr(w, http.StatusConflict, "create flow: "+err.Error())
		return
	}
	s.Log.Info("flow imported", "flow", created.Name, "steps", len(created.Steps))
	writeJSON(w, http.StatusCreated, created)
}
