package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestStatsEndpointShape seeds a couple finished jobs and confirms the
// endpoint returns the full Stats shape (totals + per_node + per_flow +
// failures_by_step + per_day) populated correctly.
func TestStatsEndpointShape(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	fin := time.Now().UTC().Add(-1 * time.Hour)
	start := fin.Add(-120 * time.Second)
	seedStatsAPIJob(t, e, fl.ID, e.node.ID, "done", "encode", start, fin)
	seedStatsAPIJob(t, e, fl.ID, e.node.ID, "failed", "encode", fin.Add(-60*time.Second), fin)

	resp, body := doJSON(t, "GET", ts.URL+"/api/stats?range=7d", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("stats: %d %s", resp.StatusCode, body)
	}
	var st model.Stats
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if st.RangeDays != 7 {
		t.Fatalf("range_days: want 7, got %d", st.RangeDays)
	}
	if st.Totals.Done != 1 || st.Totals.Failed != 1 {
		t.Fatalf("totals: %+v", st.Totals)
	}
	if len(st.PerNode) != 1 || st.PerNode[0].Done != 1 {
		t.Fatalf("per_node: %+v", st.PerNode)
	}
	if len(st.PerFlow) != 1 || st.PerFlow[0].Done != 1 {
		t.Fatalf("per_flow: %+v", st.PerFlow)
	}
	if len(st.FailuresByStep) != 1 || st.FailuresByStep[0].Step != "encode" {
		t.Fatalf("failures_by_step: %+v", st.FailuresByStep)
	}
	if len(st.PerDay) != 1 {
		t.Fatalf("per_day: want 1 bucket, got %d", len(st.PerDay))
	}
}

// TestStatsEndpointDefaultRange confirms the default range (no ?range param)
// is 7d.
func TestStatsEndpointDefaultRange(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "GET", ts.URL+"/api/stats", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("stats: %d %s", resp.StatusCode, body)
	}
	var st model.Stats
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if st.RangeDays != 7 {
		t.Fatalf("default range_days: want 7, got %d", st.RangeDays)
	}
}

// TestStatsEndpointUnknownRangeFallsBackTo7d confirms an unknown ?range value
// falls back to 7d (mirrors handleNodeMetrics), not a 400.
func TestStatsEndpointUnknownRangeFallsBackTo7d(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "GET", ts.URL+"/api/stats?range=bogus", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("unknown range: want 200, got %d %s", resp.StatusCode, body)
	}
	var st model.Stats
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if st.RangeDays != 7 {
		t.Fatalf("unknown range fallback: want 7, got %d", st.RangeDays)
	}
}

// TestStatsEndpointAllRange confirms ?range=all sets range_days=0 (no lower
// bound) and includes jobs of any age.
func TestStatsEndpointAllRange(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()
	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	// A job finished 8 days ago — excluded by 7d but included by "all".
	old := time.Now().UTC().Add(-8 * 24 * time.Hour)
	seedStatsAPIJob(t, e, fl.ID, e.node.ID, "done", "encode", old.Add(-60*time.Second), old)

	resp, body := doJSON(t, "GET", ts.URL+"/api/stats?range=all", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("stats all: %d %s", resp.StatusCode, body)
	}
	var st model.Stats
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if st.RangeDays != 0 {
		t.Fatalf("all range_days: want 0, got %d", st.RangeDays)
	}
	if st.Totals.Done != 1 {
		t.Fatalf("all range: want 1 done, got %d", st.Totals.Done)
	}
}

// TestStatsEndpointRequiresAdmin confirms a request without a session token
// is rejected with 401 (withAdmin guard).
func TestStatsEndpointRequiresAdmin(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, _ := doJSON(t, "GET", ts.URL+"/api/stats?range=7d", "", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("want 401 without admin token, got %d", resp.StatusCode)
	}
}

// TestStatsEndpointEmptyFleetReturnsZeroedShape confirms a fresh install
// (no jobs) returns a zeroed Stats shape (not an error), so the Stats page
// renders cleanly.
func TestStatsEndpointEmptyFleetReturnsZeroedShape(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "GET", ts.URL+"/api/stats?range=7d", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("empty stats: %d %s", resp.StatusCode, body)
	}
	var st model.Stats
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if st.Totals.Done != 0 || st.Totals.Failed != 0 || st.Totals.Cancelled != 0 {
		t.Fatalf("totals should be zeroed: %+v", st.Totals)
	}
	// Slices must be [] not null so the frontend doesn't have to null-guard.
	if st.PerNode == nil || st.PerFlow == nil || st.FailuresByStep == nil || st.PerDay == nil {
		t.Fatalf("slices should be non-nil (zeroed []), got per_node=%v per_flow=%v failures=%v per_day=%v",
			st.PerNode, st.PerFlow, st.FailuresByStep, st.PerDay)
	}
}

// seedStatsAPIJob inserts a finished job via the test env's store with
// explicit timestamps so the stats endpoint asserts can check exact counts.
// The normal CreateJob/FinishJob path stamps finished_at=datetime('now'),
// which is uncontrolled for tests; this helper uses the store's test-seed
// method to backdate.
func seedStatsAPIJob(t *testing.T, e *testEnv, flowID, nodeID int64, status, step string, started, finished time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := e.server.Store.SeedFinishedJob(ctx, flowID, nodeID, status, step,
		started.UTC().Format("2006-01-02 15:04:05"),
		finished.UTC().Format("2006-01-02 15:04:05")); err != nil {
		t.Fatalf("seed job: %v", err)
	}
}
