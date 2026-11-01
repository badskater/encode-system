package api

import (
	"net/http"
	"strconv"
)

// maxBulkJobIDs caps one bulk request. The IN-list is processed per id
// (each call reuses the single-job guarded store UPDATE), so the cap bounds
// the handler's work and keeps the request/response shapes small. The Jobs
// page never selects more than one page of jobs at a time, so 500 is far
// beyond real use — the guard exists against script abuse, not the UI.
const maxBulkJobIDs = 500

// handleBulkJobs applies one action to a list of job ids. Each id goes
// through the SAME guarded store call as the single-job endpoints, so the
// lifecycle rules stay in one place. Ids that do not match their guard (wrong
// state, nonexistent, or a validation failure) are reported in "skipped"
// instead of failing the whole request — a bulk op over a mixed selection is
// the common case (the UI selects rows across statuses).
//
// Actions:
//   - retry: re-queue failed/cancelled/done jobs (RetryJob)
//   - cancel: cancel pending/assigned jobs (CancelJob)
//   - delete: remove terminal jobs (DeleteJob); non-terminal/missing → skipped
//   - set_priority: patch priority on pending jobs (PatchPendingJob); requires
//     body.priority (int); non-pending/missing → skipped
//   - set_flow: patch flow_id on pending jobs (PatchPendingJob); requires
//     body.flow_id (int64, validated to exist); non-pending/missing → skipped
//
// Max 500 ids per request. Audit: jobs.bulk.
func (s *Server) handleBulkJobs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action   string  `json:"action"` // retry | cancel | delete | set_priority | set_flow
		IDs      []int64 `json:"ids"`
		Priority *int    `json:"priority"` // required for set_priority
		FlowID   *int64  `json:"flow_id"`  // required for set_flow
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	switch req.Action {
	case "retry", "cancel", "delete", "set_priority", "set_flow":
	default:
		writeErr(w, http.StatusBadRequest, "action must be retry, cancel, delete, set_priority, or set_flow")
		return
	}
	if len(req.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, "ids must not be empty")
		return
	}
	if len(req.IDs) > maxBulkJobIDs {
		writeErr(w, http.StatusBadRequest, "too many ids (max "+strconv.Itoa(maxBulkJobIDs)+")")
		return
	}
	// set_priority requires a priority value; set_flow requires a flow_id
	// that resolves to an existing flow. Both are validated BEFORE the loop
	// so a 400 is returned up front rather than mid-batch.
	if req.Action == "set_priority" && req.Priority == nil {
		writeErr(w, http.StatusBadRequest, "priority is required for set_priority")
		return
	}
	if req.Action == "set_flow" {
		if req.FlowID == nil {
			writeErr(w, http.StatusBadRequest, "flow_id is required for set_flow")
			return
		}
		if _, err := s.Store.GetFlow(r.Context(), *req.FlowID); err != nil {
			writeErr(w, http.StatusBadRequest, "flow not found")
			return
		}
	}

	ctx := r.Context()
	affected := 0
	skipped := make([]int64, 0)
	for _, id := range req.IDs {
		var ok bool
		switch req.Action {
		case "retry":
			n, err := s.Store.RetryJob(ctx, id)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error": err.Error(), "action": req.Action, "affected": affected, "skipped": skipped,
				})
				return
			}
			ok = n > 0
		case "cancel":
			n, err := s.Store.CancelJob(ctx, id)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error": err.Error(), "action": req.Action, "affected": affected, "skipped": skipped,
				})
				return
			}
			ok = n > 0
		case "delete":
			deleted, err := s.Store.DeleteJob(ctx, id)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error": err.Error(), "action": req.Action, "affected": affected, "skipped": skipped,
				})
				return
			}
			ok = deleted
		case "set_priority":
			changed, err := s.Store.PatchPendingJob(ctx, id, nil, req.Priority)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error": err.Error(), "action": req.Action, "affected": affected, "skipped": skipped,
				})
				return
			}
			ok = changed
		case "set_flow":
			changed, err := s.Store.PatchPendingJob(ctx, id, req.FlowID, nil)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error": err.Error(), "action": req.Action, "affected": affected, "skipped": skipped,
				})
				return
			}
			ok = changed
		}
		if ok {
			affected++
		} else {
			skipped = append(skipped, id)
		}
	}
	s.Log.Info("bulk job action", "action", req.Action, "affected", affected, "skipped", len(skipped))
	auditDetail := map[string]any{"ids": req.IDs, "affected": affected, "skipped": skipped}
	if req.Action == "set_priority" {
		auditDetail["priority"] = req.Priority
	}
	if req.Action == "set_flow" {
		auditDetail["flow_id"] = req.FlowID
	}
	s.audit(r, "jobs.bulk", req.Action, auditDetail)
	writeJSON(w, http.StatusOK, map[string]any{
		"action":   req.Action,
		"affected": affected,
		"skipped":  skipped,
	})
}
