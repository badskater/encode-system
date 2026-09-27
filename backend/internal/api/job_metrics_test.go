package api

import (
	"encoding/json"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestCompletionCarriesMetrics verifies the agent completion report's
// "metrics" object lands on the job and is exposed by GET /api/jobs/{id}.
// Old agents omit the key entirely — that must stay a no-op (nil metrics).
func TestCompletionCarriesMetrics(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "Met Show", Episode: "01", EpisodeDir: "Met Show/Ep 01",
		ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}

	rep := map[string]any{
		"status": "done", "exit_code": 0, "error": "",
		"outputs": []string{"out.mkv"}, "log_tail": "ENCODE_JOB_DONE",
		"log_full": "full", "step_timings": []any{},
		"metrics": map[string]float64{"vmaf": 94.21, "output_bitrate_kbps": 8123},
	}
	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/job/"+i64s(job.ID)+"/complete", e.token, rep)
	if resp.StatusCode != 200 {
		t.Fatalf("complete: %d %s", resp.StatusCode, body)
	}

	resp, body = doJSON(t, "GET", ts.URL+"/api/jobs/"+i64s(job.ID), adminTok, nil)
	var got model.Job
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Metrics["vmaf"] != 94.21 || got.Metrics["output_bitrate_kbps"] != 8123 {
		t.Fatalf("metrics = %v, want vmaf+bitrate", got.Metrics)
	}

	// Old-agent shape: no metrics key at all.
	job2, _ := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "Met Show", Episode: "02", EpisodeDir: "Met Show/Ep 02",
		ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending})
	e.server.Store.AssignJob(ctx, job2.ID, e.node.ID)
	old := map[string]any{
		"status": "done", "exit_code": 0, "error": "",
		"outputs": []string{}, "log_tail": "", "log_full": "", "step_timings": []any{},
	}
	resp, body = doJSON(t, "POST", ts.URL+"/api/agent/job/"+i64s(job2.ID)+"/complete", e.token, old)
	if resp.StatusCode != 200 {
		t.Fatalf("old-agent complete: %d %s", resp.StatusCode, body)
	}
	resp, body = doJSON(t, "GET", ts.URL+"/api/jobs/"+i64s(job2.ID), adminTok, nil)
	var got2 model.Job
	json.Unmarshal(body, &got2)
	if len(got2.Metrics) != 0 {
		t.Fatalf("old-agent metrics = %v, want empty", got2.Metrics)
	}
}
