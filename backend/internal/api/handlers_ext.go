package api

import (
	"net/http"
	"strconv"

	"github.com/badskater/encode-system/backend/internal/model"
)

// ---------- Series ----------

// handleListSeries returns all registered series with job counts for the UI.
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
	// Count jobs per series for the overview column.
	counts := map[string]int{}
	for _, j := range jobs {
		counts[j.Series]++
	}
	type seriesView struct {
		*model.Series
		Jobs int `json:"jobs"`
	}
	out := make([]seriesView, 0, len(series))
	for _, sr := range series {
		out = append(out, seriesView{Series: sr, Jobs: counts[sr.Name]})
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePatchSeries updates a series' flow selection and enabled state.
// A series with flow_id 0 falls back to the default flow; disabled series
// are skipped by the scanner (no new jobs are created for them).
func (s *Server) handlePatchSeries(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad series id")
		return
	}
	var req struct {
		FlowID  *int64 `json:"flow_id"`
		Enabled *bool  `json:"enabled"`
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
	if req.FlowID != nil {
		if *req.FlowID != 0 {
			if _, err := s.Store.GetFlow(ctx, *req.FlowID); err != nil {
				writeErr(w, http.StatusBadRequest, "flow not found")
				return
			}
		}
		sr.FlowID = *req.FlowID
	}
	if req.Enabled != nil {
		sr.Enabled = *req.Enabled
	}
	if err := s.Store.UpdateSeries(ctx, sr); err != nil {
		writeErr(w, http.StatusInternalServerError, "update series")
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
		writeErr(w, http.StatusNotFound, err.Error())
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
	w.Header().Set("Content-Disposition", `attachment; filename="flow-`+fl.Name+`.json"`)
	writeJSON(w, http.StatusOK, exp)
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
	for _, t := range exp.Templates {
		t.ID = 0
		t.Builtin = false // imported templates are never built-ins
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

	// Pick a free name.
	name := exp.Flow.Name
	for i := 2; ; i++ {
		if _, err := s.Store.FlowByName(ctx, name); err != nil {
			break
		}
		name = exp.Flow.Name + "-" + strconv.Itoa(i)
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
