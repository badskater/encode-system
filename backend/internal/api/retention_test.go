package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestPruneEndpoint verifies POST /api/jobs/prune deletes old terminal jobs
// and reports the count, and that validation rejects missing/zero/oversized
// days values.
func TestPruneEndpoint(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	started := now.Add(-41 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	for i := 0; i < 3; i++ {
		if err := e.server.Store.SeedFinishedJob(ctx, fl.ID, 0, "done", "mux", started, old); err != nil {
			t.Fatal(err)
		}
	}

	resp, body := doJSON(t, "POST", ts.URL+"/api/jobs/prune", adminTok, map[string]any{"days": 30})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Deleted int64 `json:"deleted"`
		Days    int   `json:"days"`
	}
	json.Unmarshal(body, &out)
	if out.Deleted != 3 || out.Days != 30 {
		t.Errorf("out = %+v, want deleted=3 days=30", out)
	}

	// Validation: missing, zero, negative, oversized.
	for _, tc := range []map[string]any{
		{},
		{"days": 0},
		{"days": -1},
		{"days": 99999},
	} {
		resp, _ = doJSON(t, "POST", ts.URL+"/api/jobs/prune", adminTok, tc)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("prune %v: status %d, want 400", tc, resp.StatusCode)
		}
	}
	// Anonymous refused.
	resp, _ = doJSON(t, "POST", ts.URL+"/api/jobs/prune", "", map[string]any{"days": 30})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous prune: status %d, want 401", resp.StatusCode)
	}
}

// TestSettingsRetentionRoundTrip verifies job_retention_days survives the
// settings PUT/GET cycle and that out-of-range values are rejected.
func TestSettingsRetentionRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get settings: %d", resp.StatusCode)
	}
	var st model.Settings
	json.Unmarshal(body, &st)
	if st.JobRetentionDays != 0 {
		t.Errorf("default retention = %d, want 0 (keep forever)", st.JobRetentionDays)
	}

	st.JobRetentionDays = 90
	resp, body = doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, st)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put settings: %d %s", resp.StatusCode, body)
	}
	resp, body = doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	json.Unmarshal(body, &st)
	if st.JobRetentionDays != 90 {
		t.Errorf("after round-trip: retention = %d, want 90", st.JobRetentionDays)
	}

	st.JobRetentionDays = -5
	resp, _ = doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, st)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("negative retention: status %d, want 400", resp.StatusCode)
	}
	st.JobRetentionDays = 100000
	resp, _ = doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, st)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("oversized retention: status %d, want 400", resp.StatusCode)
	}
}

// TestPruneJobsTickRespectsSettings verifies the loop tick: with retention
// disabled (default 0) nothing is deleted; with a policy set, old terminal
// jobs are pruned on the next tick.
func TestPruneJobsTickRespectsSettings(t *testing.T) {
	e := newTestEnv(t)
	ctx := ctxBg()
	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	if err := e.server.Store.SeedFinishedJob(ctx, fl.ID, 0, "done", "mux", old, old); err != nil {
		t.Fatal(err)
	}

	// Default (0): tick is a no-op.
	e.server.pruneJobsTick(ctx)
	jobs, _ := e.server.Store.ListJobs(ctx, "", 10)
	if len(jobs) != 1 {
		t.Fatalf("disabled retention deleted jobs: %d remain", len(jobs))
	}

	// Enable a 30-day policy via the real settings path, then tick.
	st := e.server.currentSettings(ctx)
	st.JobRetentionDays = 30
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	e.server.pruneJobsTick(ctx)
	jobs, _ = e.server.Store.ListJobs(ctx, "", 10)
	if len(jobs) != 0 {
		t.Fatalf("enabled retention kept old job: %d remain", len(jobs))
	}
}
