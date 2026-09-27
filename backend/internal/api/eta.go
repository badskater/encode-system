package api

import (
	"net/http"
	"strconv"
	"time"
)

// etaResponse is the ETA estimate for a live job.
type etaResponse struct {
	// AvgSec is the historical average wall-clock duration of done jobs on
	// the same flow; Samples is how many jobs that average is over.
	AvgSec  float64 `json:"avg_sec"`
	Samples int     `json:"samples"`
	// ElapsedSec is how long this job has been running (since started_at).
	ElapsedSec float64 `json:"elapsed_sec"`
	// ETASec is the predicted REMAINING time (avg - elapsed), never
	// negative. -1 means "no estimate" (no history or job not started).
	ETASec float64 `json:"eta_sec"`
	// Progress is the job's last reported step percentage, echoed so the
	// UI can blend the two signals.
	Progress float64 `json:"progress"`
}

// etaMinSamples gates when a flow's average is trustworthy enough to show.
// One sample can be an outlier (a tiny episode); two starts to converge.
const etaMinSamples = 2

// handleJobETA predicts the remaining time for a running job from the
// average duration of done jobs on the same flow. The store does the
// aggregation (indexed on flow_id+status); this handler only shapes the
// response. Works for assigned/running jobs; terminal jobs report eta -1
// (nothing remains).
func (s *Server) handleJobETA(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad job id")
		return
	}
	ctx := r.Context()
	job, err := s.Store.GetJob(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	resp := etaResponse{ETASec: -1, Progress: job.Progress}
	avg, samples, err := s.Store.AvgFlowDuration(ctx, job.FlowID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "eta history")
		return
	}
	resp.AvgSec = avg
	resp.Samples = samples
	if job.StartedAt != nil && !job.StartedAt.IsZero() {
		resp.ElapsedSec = time.Since(*job.StartedAt).Seconds()
	}
	if samples >= etaMinSamples && avg > 0 {
		rem := avg - resp.ElapsedSec
		if rem < 0 {
			rem = 0
		}
		resp.ETASec = rem
	}
	writeJSON(w, http.StatusOK, resp)
}
