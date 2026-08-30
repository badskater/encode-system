package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// doRawGET issues a GET with only a bearer header (no Content-Type) so it can
// hit endpoints that return non-JSON bodies (the raw log endpoint).
func doRawGET(t *testing.T, url, token string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

// TestJobLogEndpoint is the Phase B2 integration: an agent POSTs a completion
// report carrying log_full + step_timings; the controller must persist both,
// expose step_timings in the GET /api/jobs/{id} response, and serve the raw
// full log at GET /api/jobs/{id}/log as text/plain.
func TestJobLogEndpoint(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	job, _ := e.server.Store.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID})
	e.server.Store.AssignJob(ctx, job.ID, e.node.ID)

	timings := []model.StepTiming{
		{Step: "encode", StartedAt: time.Date(2026, 8, 30, 11, 0, 0, 0, time.UTC), DurationSec: 12.5},
		{Step: "mux", StartedAt: time.Date(2026, 8, 30, 11, 0, 12, 500000000, time.UTC), DurationSec: 3.25},
	}
	rep := map[string]any{
		"status":       "done",
		"exit_code":    0,
		"outputs":      []string{"S - 01 [1080p].mkv"},
		"log_tail":     "ENCODE_JOB_DONE",
		"log_full":     "line one\nline two\nENCODE_JOB_DONE\n",
		"step_timings": timings,
	}
	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/job/"+itoa(job.ID)+"/complete", e.token, rep)
	if resp.StatusCode != 200 {
		t.Fatalf("complete: %d %s", resp.StatusCode, body)
	}

	// GET /api/jobs/{id} carries the persisted step_timings (the full_log
	// field is omitempty-empty-string so it appears too).
	resp2, body2 := doJSON(t, "GET", ts.URL+"/api/jobs/"+itoa(job.ID), adminTok, nil)
	if resp2.StatusCode != 200 {
		t.Fatalf("get job: %d %s", resp2.StatusCode, body2)
	}
	var got model.Job
	if err := json.Unmarshal(body2, &got); err != nil {
		t.Fatalf("unmarshal job: %v (%s)", err, body2)
	}
	if len(got.StepTimings) != 2 {
		t.Fatalf("step_timings len=%d want 2 (%s)", len(got.StepTimings), body2)
	}
	if got.StepTimings[0].Step != "encode" || got.StepTimings[1].Step != "mux" {
		t.Fatalf("step_timings wrong: %+v", got.StepTimings)
	}
	if got.FullLog == "" {
		t.Fatalf("full_log empty in job response: %s", body2)
	}

	// GET /api/jobs/{id}/log returns the raw full log as text/plain.
	resp3, body3 := doRawGET(t, ts.URL+"/api/jobs/"+itoa(job.ID)+"/log", adminTok)
	if resp3.StatusCode != 200 {
		t.Fatalf("log endpoint: %d %s", resp3.StatusCode, body3)
	}
	if ct := resp3.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type=%q want text/plain", ct)
	}
	if string(body3) != "line one\nline two\nENCODE_JOB_DONE\n" {
		t.Fatalf("log body mismatch: %q", body3)
	}
}

// TestJobLogEndpointOldAgentCompat asserts an OLD agent (pre-Phase-A report
// that omits log_full and step_timings) still completes successfully and the
// log endpoint 404s with the no-log-recorded message instead of an empty 200.
func TestJobLogEndpointOldAgentCompat(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	job, _ := e.server.Store.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID})
	e.server.Store.AssignJob(ctx, job.ID, e.node.ID)

	// Old-agent report: only the four pre-v2 fields. No log_full, no
	// step_timings — both decode to zero values and the finish must still
	// succeed.
	rep := map[string]any{
		"status":    "done",
		"exit_code": 0,
		"outputs":   []string{"S - 01 [1080p].mkv"},
		"log_tail":  "ENCODE_JOB_DONE",
	}
	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/job/"+itoa(job.ID)+"/complete", e.token, rep)
	if resp.StatusCode != 200 {
		t.Fatalf("old-agent complete must succeed: %d %s", resp.StatusCode, body)
	}
	got, _ := e.server.Store.GetJob(ctx, job.ID)
	if got.Status != model.JobDone {
		t.Fatalf("old-agent job not done: %+v", got)
	}

	// No full log recorded → log endpoint 404s with the explicit message.
	resp2, body2 := doRawGET(t, ts.URL+"/api/jobs/"+itoa(job.ID)+"/log", adminTok)
	if resp2.StatusCode != 404 {
		t.Fatalf("want 404 for job with no log, got %d", resp2.StatusCode)
	}
	if !bytes.Contains(body2, []byte("no log recorded")) {
		t.Fatalf("404 body must explain no log: %s", body2)
	}
}

// TestJobLogEndpointUnknownJob404 asserts a request for a job id that does
// not exist is a clean 404, not a 500 or empty 200.
func TestJobLogEndpointUnknownJob404(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, _ := doRawGET(t, ts.URL+"/api/jobs/999999/log", adminTok)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown job: want 404, got %d", resp.StatusCode)
	}
}

// TestJobLogEndpointRequiresAdmin pins the withAdmin auth wrapper on the new
// route: job logs can hold paths and errors, so the endpoint must reject a
// request with no (or bad) session the same way GET /api/jobs/{id} does.
func TestJobLogEndpointRequiresAdmin(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	job, _ := e.server.Store.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID})
	e.server.Store.AssignJob(ctx, job.ID, e.node.ID)

	// Complete with a log so there IS a log to read — the auth gate must
	// fire before the log-present check.
	rep := map[string]any{"status": "done", "exit_code": 0, "log_full": "has a log"}
	resp, _ := doJSON(t, "POST", ts.URL+"/api/agent/job/"+itoa(job.ID)+"/complete", e.token, rep)
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}

	// No token → 401 (matches TestAdminAuthRequired's pattern).
	resp2, _ := doRawGET(t, ts.URL+"/api/jobs/"+itoa(job.ID)+"/log", "")
	if resp2.StatusCode != 401 {
		t.Fatalf("no-token: want 401, got %d", resp2.StatusCode)
	}
	// Bad token → 401.
	resp3, _ := doRawGET(t, ts.URL+"/api/jobs/"+itoa(job.ID)+"/log", "not-a-real-session")
	if resp3.StatusCode != 401 {
		t.Fatalf("bad-token: want 401, got %d", resp3.StatusCode)
	}
}
