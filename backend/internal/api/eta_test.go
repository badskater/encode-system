package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestJobETA verifies GET /api/jobs/{id}/eta: with >=2 done jobs on the
// flow the estimate is avg - elapsed; with no history it reports
// eta_sec=-1 and samples=0.
func TestJobETA(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")

	// Two done jobs, each 20 minutes wall clock (exact timestamps via the
	// stats seeding helper — the ETA query only trusts started/finished).
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		start := now.Add(-time.Duration(60+i) * time.Minute)
		fin := start.Add(20 * time.Minute)
		if err := e.server.Store.SeedFinishedJob(ctx, fl.ID, e.node.ID, "done", "",
			start.Format("2006-01-02 15:04:05"), fin.Format("2006-01-02 15:04:05")); err != nil {
			t.Fatal(err)
		}
	}

	// A live job just assigned: elapsed≈0, so eta≈avg≈1200s.
	live, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "ETA Show", Episode: "03", EpisodeDir: "ETA Show/Ep 03",
		ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.AssignJob(ctx, live.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}
	e.server.Store.UpdateJobStatus(ctx, live.ID, model.JobRunning, "encode", 40, "")

	resp, body := doJSON(t, "GET", ts.URL+"/api/jobs/"+i64s(live.ID)+"/eta", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("eta: %d %s", resp.StatusCode, body)
	}
	var got etaResponse
	json.Unmarshal(body, &got)
	if got.Samples != 2 {
		t.Fatalf("samples = %d, want 2", got.Samples)
	}
	if got.AvgSec < 1190 || got.AvgSec > 1210 {
		t.Fatalf("avg = %.0fs, want ~1200s", got.AvgSec)
	}
	// elapsed is a couple of seconds at most in a test.
	if got.ETASec < 1150 || got.ETASec > 1210 {
		t.Fatalf("eta = %.0fs, want ~1200s (avg %.0f elapsed %.0f)", got.ETASec, got.AvgSec, got.ElapsedSec)
	}
	if got.Progress != 40 {
		t.Fatalf("progress = %v, want 40", got.Progress)
	}

	// No history: a fresh flow with zero done jobs.
	fl2, _ := e.server.Store.CreateFlow(ctx, &model.Flow{Name: "no-history"})
	fresh, _ := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "Fresh Show", Episode: "01", EpisodeDir: "Fresh Show/Ep 01",
		ScriptType: "avs", FlowID: fl2.ID, Status: model.JobPending})
	resp, body = doJSON(t, "GET", ts.URL+"/api/jobs/"+i64s(fresh.ID)+"/eta", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("eta (no history): %d %s", resp.StatusCode, body)
	}
	got = etaResponse{}
	json.Unmarshal(body, &got)
	if got.Samples != 0 || got.ETASec != -1 {
		t.Fatalf("no-history eta = %+v, want samples=0 eta=-1", got)
	}
}
