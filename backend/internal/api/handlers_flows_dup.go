package api

import (
	"net/http"
	"strconv"

	"github.com/badskater/encode-system/backend/internal/model"
)

// handleDuplicateFlow copies a flow and its steps verbatim under a new name.
// The default name is "<original> (copy)"; the request body may override it
// with {"name":"..."}. The copy is never the default flow (is_default=false)
// so duplicating the flagged default does not create a second default. The
// retry policy (MaxRetries, RetryBackoffMinutes) is copied alongside the
// steps so a duplicated flow preserves its failure-handling behavior.
//
// Audit: flow.duplicate.
func (s *Server) handleDuplicateFlow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad flow id")
		return
	}
	src, err := s.Store.GetFlow(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "flow not found")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	// Body is optional; decode tolerantly when empty.
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	name := req.Name
	if name == "" {
		name = src.Name + " (copy)"
	}
	dup := &model.Flow{
		Name:                name,
		Steps:               src.Steps,
		MaxRetries:          src.MaxRetries,
		RetryBackoffMinutes: src.RetryBackoffMinutes,
	}
	created, err := s.Store.CreateFlow(r.Context(), dup)
	if err != nil {
		if !isUniqueConstraintErr(err) {
			s.Log.Error("duplicate flow failed", "err", err, "source_id", src.ID)
			writeErr(w, http.StatusInternalServerError, "duplicate failed: "+err.Error())
			return
		}
		writeErr(w, http.StatusConflict, "flow name taken")
		return
	}
	s.audit(r, "flow.duplicate", auditObject("flow", created.ID), map[string]any{
		"source_id": src.ID, "name": created.Name, "steps": len(created.Steps),
	})
	writeJSON(w, http.StatusOK, created)
}
