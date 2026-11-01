package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestDeleteTerminalJob verifies DELETE /api/jobs/{id} removes a terminal job
// and returns {"deleted":true}.
func TestDeleteTerminalJob(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	done := bulkJob(t, e, "S/Done 01", model.JobDone)
	failed := bulkJob(t, e, "S/Failed 01", model.JobFailed)
	cancelled := bulkJob(t, e, "S/Cancelled 01", model.JobDone)
	// Force cancelled status via store lifecycle.
	if _, err := e.server.Store.CancelJob(ctxBg(), cancelled); err != nil {
		t.Fatal(err)
	}

	for _, id := range []int64{done, failed, cancelled} {
		resp, body := doJSON(t, "DELETE", ts.URL+"/api/jobs/"+itoa(id), adminTok, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("delete job %d: status %d: %s", id, resp.StatusCode, body)
		}
		var out struct {
			Deleted bool `json:"deleted"`
		}
		json.Unmarshal(body, &out)
		if !out.Deleted {
			t.Errorf("job %d: deleted = false, want true", id)
		}
		// Job is gone from the store.
		if _, err := e.server.Store.GetJob(ctxBg(), id); err == nil {
			t.Errorf("job %d still exists after delete", id)
		}
	}
}

// TestDeleteJobMissing returns 404 for a nonexistent job ID.
func TestDeleteJobMissing(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "DELETE", ts.URL+"/api/jobs/999999", adminTok, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}

// TestDeleteJobNonTerminal returns 409 for a running/pending job — only
// terminal jobs (done/failed/cancelled) can be deleted.
func TestDeleteJobNonTerminal(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	pending := bulkJob(t, e, "S/Pending 01", model.JobPending)
	running := bulkJob(t, e, "S/Running 01", model.JobRunning)

	for _, id := range []int64{pending, running} {
		resp, body := doJSON(t, "DELETE", ts.URL+"/api/jobs/"+itoa(id), adminTok, nil)
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("delete job %d: status %d, want 409 (%s)", id, resp.StatusCode, body)
		}
		// Job still exists.
		if j, err := e.server.Store.GetJob(ctxBg(), id); err != nil || j == nil {
			t.Errorf("job %d was deleted despite being non-terminal", id)
		}
	}
}

// TestDeleteJobRejectsNonAdmin verifies the endpoint is admin-only.
func TestDeleteJobRejectsNonAdmin(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	done := bulkJob(t, e, "S/Done Auth", model.JobDone)

	resp, _ := doJSON(t, "DELETE", ts.URL+"/api/jobs/"+itoa(done), "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous: status %d, want 401", resp.StatusCode)
	}
}
