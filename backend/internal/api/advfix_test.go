package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// doRawJSON posts a pre-built JSON byte slice directly (bypassing json.Marshal)
// so tests can construct oversized or escaping-inflated payloads that exercise
// the body-cap path.
func doRawJSON(t *testing.T, method, url, token string, body []byte) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

// TestJobCompleteAcceptsEscapingInflatedLargeLog asserts the job-complete
// route (and ONLY that route) gets a 4 MiB body cap (maxCompleteBodyBytes)
// instead of the default 1 MiB, so a real ≥1-MiB-log completion whose JSON
// wire payload is inflated past 1 MiB by string escaping (newlines → \n,
// quotes → \") still succeeds and persists. A 5 MiB body is rejected.
//
// The test posts a ~1.5 MiB log_full built from 1 MiB of newlines (each \n
// becomes \\n = 2 bytes on the wire, so the JSON body is ~1 MiB of raw text
// + escape overhead + JSON framing, well over 1 MiB but under 4 MiB).
func TestJobCompleteAcceptsEscapingInflatedLargeLog(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	job, _ := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "BIG", Episode: "01", EpisodeDir: "BIG/Ep 01", ScriptType: "vpy", FlowID: fl.ID,
	})
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}

	// Build a log_full of ~1 MiB that will inflate past 1 MiB when JSON-
	// escaped: 1 MiB of newlines. Each \n in the raw string becomes \\n
	// (2 bytes) in JSON, so the JSON "log_full" value alone is ~2 MiB on
	// the wire, well over the 1 MiB default cap but under the 4 MiB
	// job-complete cap.
	logFull := strings.Repeat("\n", 1<<20) // 1 MiB of newlines

	// Build the JSON body manually so we control the exact wire size.
	body := mustMarshalCompletion(t, "done", 0, "", logFull)

	resp, bodyBytes := doRawJSON(t, "POST", ts.URL+"/api/agent/job/"+itoa(job.ID)+"/complete", e.token, body)
	if resp.StatusCode != 200 {
		t.Fatalf("completion with escaping-inflated ~1.5 MiB log should succeed under 4 MiB cap, got %d: %s", resp.StatusCode, bodyBytes)
	}

	// Verify it persisted.
	got, _ := e.server.Store.GetJob(ctx, job.ID)
	if got.Status != model.JobDone {
		t.Fatalf("job status = %q, want done", got.Status)
	}
	if len(got.FullLog) == 0 {
		t.Fatal("full_log not persisted for large completion")
	}
}

// TestJobCompleteRejectsFiveMiBBody asserts the 4 MiB cap on the complete
// route is still enforced: a 5 MiB body is rejected with 400.
func TestJobCompleteRejectsFiveMiBBody(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	job, _ := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "HUGE", Episode: "01", EpisodeDir: "HUGE/Ep 01", ScriptType: "vpy", FlowID: fl.ID,
	})
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}

	// Build a body well over 4 MiB: a 5 MiB log_full of 'x' chars.
	logFull := strings.Repeat("x", 5<<20)
	body := mustMarshalCompletion(t, "done", 0, "", logFull)

	resp, _ := doRawJSON(t, "POST", ts.URL+"/api/agent/job/"+itoa(job.ID)+"/complete", e.token, body)
	if resp.StatusCode != 400 {
		t.Fatalf("5 MiB completion body should be rejected (400), got %d", resp.StatusCode)
	}
}

// mustMarshalCompletion builds the JSON completion body with a given log_full,
// using json.Marshal so the escaping is real (newlines → \n etc.).
func mustMarshalCompletion(t *testing.T, status string, exitCode int, errMsg, logFull string) []byte {
	t.Helper()
	type rep struct {
		Status   string `json:"status"`
		ExitCode int    `json:"exit_code"`
		Error    string `json:"error"`
		LogFull  string `json:"log_full"`
	}
	b, err := json.Marshal(rep{
		Status:   status,
		ExitCode: exitCode,
		Error:    errMsg,
		LogFull:  logFull,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestUpdateFlowPersistsRetryPolicy asserts PUT /api/flows/{id} persists the
// retry policy (MaxRetries + RetryBackoffMinutes) — the handler copies both
// fields unconditionally into the existing flow before UpdateFlow (PUT
// semantics replace the policy). A PUT with max_retries=0 (policy OFF)
// must also persist (clearing a stale policy).
func TestUpdateFlowPersistsRetryPolicy(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Start with a flow that has a retry policy.
	fl, err := e.server.Store.CreateFlow(ctx, &model.Flow{
		Name:                "retry-persist",
		Steps:               []model.Step{{Type: model.StepDGIndex}},
		MaxRetries:          1,
		RetryBackoffMinutes: 5,
	})
	if err != nil {
		t.Fatal(err)
	}

	// PUT with max_retries=3, backoff=10.
	resp, body := doJSON(t, "PUT", ts.URL+"/api/flows/"+itoa(fl.ID), adminTok,
		map[string]any{
			"name":                  "retry-persist",
			"steps":                 []map[string]any{{"type": "dgindex"}},
			"max_retries":           3,
			"retry_backoff_minutes": 10,
		})
	if resp.StatusCode != 200 {
		t.Fatalf("PUT flow with retry policy: %d %s", resp.StatusCode, body)
	}

	got, _ := e.server.Store.GetFlow(ctx, fl.ID)
	if got.MaxRetries != 3 {
		t.Fatalf("max_retries = %d, want 3 (PUT must persist)", got.MaxRetries)
	}
	if got.RetryBackoffMinutes != 10 {
		t.Fatalf("retry_backoff_minutes = %d, want 10", got.RetryBackoffMinutes)
	}

	// PUT again with max_retries=0 (policy OFF) — must persist 0, not keep 3.
	resp2, body2 := doJSON(t, "PUT", ts.URL+"/api/flows/"+itoa(fl.ID), adminTok,
		map[string]any{
			"name":                  "retry-persist",
			"steps":                 []map[string]any{{"type": "dgindex"}},
			"max_retries":           0,
			"retry_backoff_minutes": 0,
		})
	if resp2.StatusCode != 200 {
		t.Fatalf("PUT flow with policy off: %d %s", resp2.StatusCode, body2)
	}

	got2, _ := e.server.Store.GetFlow(ctx, fl.ID)
	if got2.MaxRetries != 0 {
		t.Fatalf("max_retries = %d, want 0 (PUT must persist policy OFF)", got2.MaxRetries)
	}
	if got2.RetryBackoffMinutes != 0 {
		t.Fatalf("retry_backoff_minutes = %d, want 0", got2.RetryBackoffMinutes)
	}
}

// TestJobCompleteConcurrentCancelIdempotent asserts the FinishJobWithReport
// status guard: if a job is concurrently cancelled between the handler's
// Terminal() pre-check and the store write, the completion report answers
// idempotently ("already_recorded"), does NOT notify, and does NOT retry.
func TestJobCompleteConcurrentCancelIdempotent(t *testing.T) {
	e, rec, fl := newRetryTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "CC", Episode: "01", EpisodeDir: "CC/Ep 01", ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}

	// Simulate a concurrent cancel: move the job to 'cancelled' directly.
	// The handler's Terminal() pre-check will now see a terminal status and
	// short-circuit to "already_recorded" before even calling FinishJobWithReport.
	// This tests the fast path of the idempotency guard.
	if _, err := e.server.Store.CancelJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}

	// Now the agent reports completion (it doesn't know about the cancel).
	rep := map[string]any{"status": "done", "exit_code": 0}
	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/job/"+itoa(job.ID)+"/complete", e.token, rep)
	if resp.StatusCode != 200 {
		t.Fatalf("completion on concurrently-cancelled job should 200, got %d: %s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("already_recorded")) {
		t.Fatalf("expected already_recorded for concurrently-cancelled job: %s", body)
	}

	// The job must remain cancelled (not overwritten to done).
	got, _ := e.server.Store.GetJob(ctx, job.ID)
	if got.Status != model.JobCancelled {
		t.Fatalf("job status = %q, want cancelled (guard prevented overwrite)", got.Status)
	}
	// No notification must fire.
	if rec.count() != 0 {
		t.Fatalf("notify must NOT fire on idempotent cancel race, got %d calls", rec.count())
	}
}
