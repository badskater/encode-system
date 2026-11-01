package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestBulkDelete verifies the bulk delete action removes terminal jobs and
// skips non-terminal/missing ids.
func TestBulkDelete(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	done := bulkJob(t, e, "S/Del Done", model.JobDone)
	failed := bulkJob(t, e, "S/Del Failed", model.JobFailed)
	running := bulkJob(t, e, "S/Del Running", model.JobRunning)

	resp, body := doJSON(t, "POST", ts.URL+"/api/jobs/bulk", adminTok, map[string]any{
		"action": "delete",
		"ids":    []int64{done, failed, running, 99999},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Action   string  `json:"action"`
		Affected int     `json:"affected"`
		Skipped  []int64 `json:"skipped"`
	}
	json.Unmarshal(body, &out)
	if out.Affected != 2 {
		t.Errorf("affected = %d, want 2", out.Affected)
	}
	// running and 99999 are skipped.
	if len(out.Skipped) != 2 {
		t.Errorf("skipped = %v, want 2 entries", out.Skipped)
	}
	// Terminal jobs are gone.
	for _, id := range []int64{done, failed} {
		if _, err := e.server.Store.GetJob(ctxBg(), id); err == nil {
			t.Errorf("job %d still exists", id)
		}
	}
	// Running job is untouched.
	j, _ := e.server.Store.GetJob(ctxBg(), running)
	if j == nil || j.Status != model.JobRunning {
		t.Errorf("running job was affected: %+v", j)
	}
}

// TestBulkSetPriority verifies the bulk set_priority action patches the
// priority on pending jobs only, via PatchPendingJob.
func TestBulkSetPriority(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	p1 := bulkJob(t, e, "S/Prio 01", model.JobPending)
	p2 := bulkJob(t, e, "S/Prio 02", model.JobPending)
	done := bulkJob(t, e, "S/Prio Done", model.JobDone)

	prio := 5
	resp, body := doJSON(t, "POST", ts.URL+"/api/jobs/bulk", adminTok, map[string]any{
		"action":   "set_priority",
		"ids":      []int64{p1, p2, done},
		"priority": prio,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Affected int     `json:"affected"`
		Skipped  []int64 `json:"skipped"`
	}
	json.Unmarshal(body, &out)
	if out.Affected != 2 {
		t.Errorf("affected = %d, want 2", out.Affected)
	}
	if len(out.Skipped) != 1 || out.Skipped[0] != done {
		t.Errorf("skipped = %v, want [%d]", out.Skipped, done)
	}
	// Pending jobs have the new priority.
	for _, id := range []int64{p1, p2} {
		j, _ := e.server.Store.GetJob(ctxBg(), id)
		if j.Priority != 5 {
			t.Errorf("job %d priority = %d, want 5", id, j.Priority)
		}
	}
}

// TestBulkSetPriorityMissingValue verifies a missing priority value is a 400.
func TestBulkSetPriorityMissingValue(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	p1 := bulkJob(t, e, "S/Prio Miss", model.JobPending)

	resp, _ := doJSON(t, "POST", ts.URL+"/api/jobs/bulk", adminTok, map[string]any{
		"action": "set_priority",
		"ids":    []int64{p1},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing priority: status %d, want 400", resp.StatusCode)
	}
}

// TestBulkSetFlow verifies the bulk set_flow action patches the flow on
// pending jobs only, and validates the flow exists.
func TestBulkSetFlow(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Create a target flow.
	target, err := e.server.Store.CreateFlow(ctx, &model.Flow{
		Name:  "bulk-target-flow",
		Steps: []model.Step{{Type: model.StepEncode}},
	})
	if err != nil {
		t.Fatal(err)
	}

	p1 := bulkJob(t, e, "S/Flow 01", model.JobPending)
	done := bulkJob(t, e, "S/Flow Done", model.JobDone)

	resp, body := doJSON(t, "POST", ts.URL+"/api/jobs/bulk", adminTok, map[string]any{
		"action":  "set_flow",
		"ids":     []int64{p1, done},
		"flow_id": target.ID,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Affected int     `json:"affected"`
		Skipped  []int64 `json:"skipped"`
	}
	json.Unmarshal(body, &out)
	if out.Affected != 1 {
		t.Errorf("affected = %d, want 1", out.Affected)
	}
	if len(out.Skipped) != 1 || out.Skipped[0] != done {
		t.Errorf("skipped = %v, want [%d]", out.Skipped, done)
	}
	j, _ := e.server.Store.GetJob(ctxBg(), p1)
	if j.FlowID != target.ID {
		t.Errorf("job %d flow_id = %d, want %d", p1, j.FlowID, target.ID)
	}
}

// TestBulkSetFlowInvalidFlow verifies a nonexistent flow_id is a 400.
func TestBulkSetFlowInvalidFlow(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	p1 := bulkJob(t, e, "S/Flow Invalid", model.JobPending)

	resp, _ := doJSON(t, "POST", ts.URL+"/api/jobs/bulk", adminTok, map[string]any{
		"action":  "set_flow",
		"ids":     []int64{p1},
		"flow_id": 999999,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid flow: status %d, want 400", resp.StatusCode)
	}
}

// TestBulkSetFlowMissingValue verifies a missing flow_id is a 400.
func TestBulkSetFlowMissingValue(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	p1 := bulkJob(t, e, "S/Flow Miss", model.JobPending)

	resp, _ := doJSON(t, "POST", ts.URL+"/api/jobs/bulk", adminTok, map[string]any{
		"action": "set_flow",
		"ids":    []int64{p1},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing flow_id: status %d, want 400", resp.StatusCode)
	}
}
