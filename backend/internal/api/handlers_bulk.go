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

// handleBulkJobs applies one action (retry|cancel) to a list of job ids.
// Each id goes through the SAME guarded store call as the single-job
// endpoints (RetryJob/CancelJob), so the lifecycle rules stay in one place:
// retry re-queues failed/cancelled/done and backoff-gated jobs only; cancel
// touches pending/assigned only. Ids that do not match their guard (wrong
// state, nonexistent) are reported in "skipped" instead of failing the whole
// request — a bulk op over a mixed selection is the common case (the UI
// selects rows across statuses).
func (s *Server) handleBulkJobs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string  `json:"action"` // retry | cancel
		IDs    []int64 `json:"ids"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Action != "retry" && req.Action != "cancel" {
		writeErr(w, http.StatusBadRequest, "action must be retry or cancel")
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

	ctx := r.Context()
	affected := 0
	skipped := make([]int64, 0)
	for _, id := range req.IDs {
		var (
			n   int64
			err error
		)
		if req.Action == "retry" {
			n, err = s.Store.RetryJob(ctx, id)
		} else {
			n, err = s.Store.CancelJob(ctx, id)
		}
		if err != nil {
			// A DB error mid-batch: report what landed so far plus the
			// error. Partial application is acceptable (each id is an
			// independent guarded UPDATE) and the response says so.
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error":    err.Error(),
				"action":   req.Action,
				"affected": affected,
				"skipped":  skipped,
			})
			return
		}
		if n > 0 {
			affected++
		} else {
			skipped = append(skipped, id)
		}
	}
	s.Log.Info("bulk job action", "action", req.Action, "affected", affected, "skipped", len(skipped))
	s.audit(r, "jobs.bulk", req.Action, map[string]any{"ids": req.IDs, "affected": affected, "skipped": skipped})
	writeJSON(w, http.StatusOK, map[string]any{
		"action":   req.Action,
		"affected": affected,
		"skipped":  skipped,
	})
}
