package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// bulkJob seeds one job in the given status (via store primitives) and
// returns its id. Status transitions use the real lifecycle paths so the
// guard semantics match production rows.
func bulkJob(t *testing.T, e *testEnv, dir string, status model.JobStatus) int64 {
	t.Helper()
	ctx := ctxBg()
	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "S", Episode: "01", EpisodeDir: dir,
		ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	switch status {
	case model.JobPending:
		// already pending
	case model.JobFailed:
		if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
			t.Fatal(err)
		}
		if err := e.server.Store.FinishJob(ctx, job.ID, model.JobFailed, 1, "boom", nil, "tail"); err != nil {
			t.Fatal(err)
		}
	case model.JobDone:
		if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
			t.Fatal(err)
		}
		if err := e.server.Store.FinishJob(ctx, job.ID, model.JobDone, 0, "", []string{"out.mkv"}, "tail"); err != nil {
			t.Fatal(err)
		}
	case model.JobRunning:
		if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
			t.Fatal(err)
		}
		if err := e.server.Store.UpdateJobStatus(ctx, job.ID, model.JobRunning, "encode", 50, "tail"); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("bulkJob: unsupported seed status %s", status)
	}
	return job.ID
}

// TestBulkRetryFailedJobs verifies the bulk endpoint re-queues every failed
// job in the id list and reports the affected count. A done job in the same
// list is ALSO retryable (matching single-job retry semantics: done is
// re-runnable), while a running job is skipped — the guard must not regress
// a live job.
func TestBulkRetry(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	f1 := bulkJob(t, e, "S/Ep 01", model.JobFailed)
	f2 := bulkJob(t, e, "S/Ep 02", model.JobFailed)
	running := bulkJob(t, e, "S/Ep 03", model.JobRunning)

	resp, body := doJSON(t, "POST", ts.URL+"/api/jobs/bulk", adminTok, map[string]any{
		"action": "retry",
		"ids":    []int64{f1, f2, running},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Action   string  `json:"action"`
		Affected int     `json:"affected"`
		Skipped  []int64 `json:"skipped"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("bad response %s: %v", body, err)
	}
	if out.Affected != 2 {
		t.Errorf("affected = %d, want 2", out.Affected)
	}
	if len(out.Skipped) != 1 || out.Skipped[0] != running {
		t.Errorf("skipped = %v, want [%d]", out.Skipped, running)
	}

	// The failed jobs are pending again; the running job is untouched.
	ctx := ctxBg()
	for _, id := range []int64{f1, f2} {
		j, err := e.server.Store.GetJob(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status != model.JobPending {
			t.Errorf("job %d status = %s, want pending", id, j.Status)
		}
	}
	j, _ := e.server.Store.GetJob(ctx, running)
	if j.Status != model.JobRunning {
		t.Errorf("running job regressed to %s", j.Status)
	}
}

// TestBulkCancelPendingJobs verifies bulk cancel only touches
// pending/assigned jobs (same guard as single cancel) and reports skips for
// terminal/running rows.
func TestBulkCancel(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	p1 := bulkJob(t, e, "S/Ep 01", model.JobPending)
	p2 := bulkJob(t, e, "S/Ep 02", model.JobPending)
	done := bulkJob(t, e, "S/Ep 03", model.JobDone)
	running := bulkJob(t, e, "S/Ep 04", model.JobRunning)

	resp, body := doJSON(t, "POST", ts.URL+"/api/jobs/bulk", adminTok, map[string]any{
		"action": "cancel",
		"ids":    []int64{p1, p2, done, running},
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
	if len(out.Skipped) != 2 {
		t.Errorf("skipped = %v, want 2 entries (done, running)", out.Skipped)
	}
	ctx := ctxBg()
	for _, id := range []int64{p1, p2} {
		j, _ := e.server.Store.GetJob(ctx, id)
		if j.Status != model.JobCancelled {
			t.Errorf("job %d status = %s, want cancelled", id, j.Status)
		}
	}
	j, _ := e.server.Store.GetJob(ctx, done)
	if j.Status != model.JobDone {
		t.Errorf("done job changed to %s", j.Status)
	}
	j, _ = e.server.Store.GetJob(ctx, running)
	if j.Status != model.JobRunning {
		t.Errorf("running job changed to %s", j.Status)
	}
}

// TestBulkRejectsBadRequests pins the validation shape: unknown action,
// empty id list, oversized list, and non-admin callers all fail cleanly.
func TestBulkRejectsBadRequests(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"unknown action", map[string]any{"action": "explode", "ids": []int64{1}}, http.StatusBadRequest},
		{"empty ids", map[string]any{"action": "retry", "ids": []int64{}}, http.StatusBadRequest},
		{"missing ids", map[string]any{"action": "retry"}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		resp, body := doJSON(t, "POST", ts.URL+"/api/jobs/bulk", adminTok, tc.body)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, resp.StatusCode, tc.want, body)
		}
	}

	// Oversized list (guard against absurd IN clauses).
	big := make([]int64, 501)
	for i := range big {
		big[i] = int64(i + 1)
	}
	resp, _ := doJSON(t, "POST", ts.URL+"/api/jobs/bulk", adminTok, map[string]any{"action": "retry", "ids": big})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("oversized ids: status %d, want 400", resp.StatusCode)
	}

	// Anonymous and node-token callers are refused.
	resp, _ = doJSON(t, "POST", ts.URL+"/api/jobs/bulk", "", map[string]any{"action": "retry", "ids": []int64{1}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous: status %d, want 401", resp.StatusCode)
	}
	resp, _ = doJSON(t, "POST", ts.URL+"/api/jobs/bulk", e.token, map[string]any{"action": "retry", "ids": []int64{1}})
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("node token: status %d, want 401/403", resp.StatusCode)
	}
}

// TestBulkUnknownIdsSkipped verifies ids that do not exist are reported as
// skipped rather than silently inflating affected.
func TestBulkUnknownIdsSkipped(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	f1 := bulkJob(t, e, "S/Ep 01", model.JobFailed)

	resp, body := doJSON(t, "POST", ts.URL+"/api/jobs/bulk", adminTok, map[string]any{
		"action": "retry",
		"ids":    []int64{f1, 9999},
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
	found := false
	for _, id := range out.Skipped {
		if id == 9999 {
			found = true
		}
	}
	if !found {
		t.Errorf("unknown id 9999 not reported in skipped: %v", out.Skipped)
	}
}
