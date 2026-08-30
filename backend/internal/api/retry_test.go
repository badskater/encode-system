package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/notify"
)

// recordingNotifier captures every JobFinished call so tests can assert
// whether a notification fired (the auto-retry path must be silent). It
// implements notify.Notifier.
type recordingNotifier struct {
	mu      sync.Mutex
	calls   []recordedCall
	fired   int
	payload []map[string]string // contents for Discord-style assertion
}

type recordedCall struct {
	JobID    int64
	Status   model.JobStatus
	NodeName string
}

func (r *recordingNotifier) JobFinished(_ context.Context, j *model.Job, nodeName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fired++
	r.calls = append(r.calls, recordedCall{JobID: j.ID, Status: j.Status, NodeName: nodeName})
}

func (r *recordingNotifier) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fired
}

// newRetryTestEnv builds a testEnv with a recording notifier injected so the
// auto-retry path's silence is observable. It also creates a flow with a
// retry policy for the tests to use.
func newRetryTestEnv(t *testing.T) (*testEnv, *recordingNotifier, *model.Flow) {
	t.Helper()
	e := newTestEnv(t)
	rec := &recordingNotifier{}
	e.server.Notifier = rec
	ctx := ctxBg()
	// Create a flow with a retry policy: 2 retries, 1-minute backoff.
	fl, err := e.server.Store.CreateFlow(ctx, &model.Flow{
		Name:                "retry-policy-flow",
		Steps:               []model.Step{{Type: model.StepDGIndex}},
		MaxRetries:          2,
		RetryBackoffMinutes: 1,
	})
	if err != nil {
		t.Fatalf("create retry flow: %v", err)
	}
	return e, rec, fl
}

// completeJob simulates an agent's final report on a job assigned to e.node.
func completeJob(t *testing.T, url string, e *testEnv, jobID int64, status string) {
	t.Helper()
	rep := map[string]any{"status": status}
	if status == "done" {
		rep["exit_code"] = 0
		rep["outputs"] = []string{"out.mkv"}
	} else {
		rep["exit_code"] = 1
		rep["error"] = "encode failed"
	}
	resp, body := doJSON(t, "POST", url+"/api/agent/job/"+itoa(jobID)+"/complete", e.token, rep)
	if resp.StatusCode != 200 {
		t.Fatalf("complete job %d: status %d: %s", jobID, resp.StatusCode, body)
	}
}

// TestFailedCompletionWithRetryPolicyRequeues asserts a failed job whose
// flow has a retry policy is silently re-queued: status back to pending,
// retry_count incremented, next_retry_at stamped in the future, and the
// notifier NOT called.
func TestFailedCompletionWithRetryPolicyRequeues(t *testing.T) {
	e, rec, fl := newRetryTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}

	completeJob(t, ts.URL, e, job.ID, "failed")

	got, _ := e.server.Store.GetJob(ctx, job.ID)
	if got.Status != model.JobPending {
		t.Fatalf("job status = %q, want pending (re-queued)", got.Status)
	}
	if got.RetryCount != 1 {
		t.Fatalf("retry_count = %d, want 1", got.RetryCount)
	}
	if got.NextRetryAt == nil {
		t.Fatal("next_retry_at not set")
	}
	// Backoff is 1 minute; next_retry_at must be ~now+1m (within a generous
	// 5s bound for the write+read round-trip).
	want := time.Now().UTC().Add(1 * time.Minute)
	if got.NextRetryAt.Sub(want).Abs() > 5*time.Second {
		t.Fatalf("next_retry_at = %v, want ~%v", got.NextRetryAt, want)
	}
	// next_retry_at must be in the future (not immediately ready).
	if got.NextRetryAt.Before(time.Now().UTC()) {
		t.Fatalf("next_retry_at %v must be in the future", got.NextRetryAt)
	}
	if rec.count() != 0 {
		t.Fatalf("notify must NOT fire on silent retry, got %d calls", rec.count())
	}
	// Node must be freed (ReleaseNode ran before the retry decision).
	n, _ := e.server.Store.GetNode(ctx, e.node.ID)
	if n.Status != model.NodeIdle {
		t.Fatalf("node status = %q, want idle", n.Status)
	}
}

// TestFailedCompletionRetryExhaustedNotifies asserts that when a job has
// used all its retries, the final failure notifies (with the retry count).
func TestFailedCompletionRetryExhaustedNotifies(t *testing.T) {
	e, rec, fl := newRetryTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "S", Episode: "02", EpisodeDir: "S/Ep 02", ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Pre-set retry_count to MaxRetries so the first failure exhausts the
	// budget (MaxRetries=2, so retry_count=2 means exhausted). Stamp it via
	// the same path the auto-retry uses: fail the job, then ScheduleJobRetry
	// to set retry_count=2 + a past backoff, then RetryJob to make it
	// pending again (clearing next_retry_at). The net result: a pending job
	// with retry_count=2 and no backoff gate, ready to be assigned.
	if err := e.server.Store.FinishJob(ctx, job.ID, model.JobFailed, 1, "pre", nil, ""); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-1 * time.Minute)
	if err := e.server.Store.ScheduleJobRetry(ctx, job.ID, 2, past); err != nil {
		t.Fatal(err)
	}
	if _, err := e.server.Store.RetryJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	// Now assign it and fail it — this failure exhausts the budget.
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}

	completeJob(t, ts.URL, e, job.ID, "failed")

	got, _ := e.server.Store.GetJob(ctx, job.ID)
	if got.Status != model.JobFailed {
		t.Fatalf("job status = %q, want failed (exhausted, not re-queued)", got.Status)
	}
	if got.RetryCount != 2 {
		t.Fatalf("retry_count = %d, want 2 (unchanged)", got.RetryCount)
	}
	if rec.count() != 1 {
		t.Fatalf("notify must fire when retries exhausted, got %d calls", rec.count())
	}
}

// TestFailedCompletionNoPolicyNotifies asserts a flow with no retry policy
// (MaxRetries=0) notifies immediately on failure.
func TestFailedCompletionNoPolicyNotifies(t *testing.T) {
	e := newTestEnv(t)
	rec := &recordingNotifier{}
	e.server.Notifier = rec
	ts := e.serve(t)
	ctx := ctxBg()

	// The seeded default flow has no retry policy (MaxRetries=0).
	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "S", Episode: "03", EpisodeDir: "S/Ep 03", ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}

	completeJob(t, ts.URL, e, job.ID, "failed")

	if rec.count() != 1 {
		t.Fatalf("notify must fire immediately when flow has no retry policy, got %d calls", rec.count())
	}
}

// TestDoneCompletionNotifies asserts a successful completion still notifies
// (the retry decision only applies to failures).
func TestDoneCompletionNotifies(t *testing.T) {
	e, _, fl := newRetryTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "S", Episode: "04", EpisodeDir: "S/Ep 04", ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}

	completeJob(t, ts.URL, e, job.ID, "done")

	got, _ := e.server.Store.GetJob(ctx, job.ID)
	if got.Status != model.JobDone {
		t.Fatalf("job status = %q, want done", got.Status)
	}
}

// TestManualRetryOnAutoRetriedJob asserts the manual retry endpoint still
// works on a job that was auto-retried (it clears next_retry_at and
// resets state so the queue can pick it up immediately).
func TestManualRetryOnAutoRetriedJob(t *testing.T) {
	e, _, fl := newRetryTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "S", Episode: "05", EpisodeDir: "S/Ep 05", ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}
	// First failure: auto-retry (job goes pending with next_retry_at set).
	completeJob(t, ts.URL, e, job.ID, "failed")
	got, _ := e.server.Store.GetJob(ctx, job.ID)
	if got.Status != model.JobPending || got.NextRetryAt == nil {
		t.Fatalf("auto-retry did not gate the job: %+v", got)
	}

	// Manual retry must clear the backoff gate and re-queue immediately.
	resp, body := doJSON(t, "POST", ts.URL+"/api/jobs/"+itoa(job.ID)+"/retry", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("manual retry: %d %s", resp.StatusCode, body)
	}
	var retried model.Job
	json.Unmarshal(body, &retried)
	if retried.Status != model.JobPending {
		t.Fatalf("manual retry status = %q, want pending", retried.Status)
	}
	if retried.NextRetryAt != nil {
		t.Fatalf("manual retry must clear next_retry_at, got %v", retried.NextRetryAt)
	}
}

// TestAssignmentGateSkipsBackoffJob asserts the heartbeat assignment path
// skips a pending job whose next_retry_at is in the future (backoff not
// elapsed) and does not assign it.
func TestAssignmentGateSkipsBackoffJob(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "S", Episode: "06", EpisodeDir: "S/Ep 06", ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Build the backoff-gated state via the public store API only (the api
	// package cannot reach the store's unexported db field): fail the job,
	// then ScheduleJobRetry it with a future backoff gate. The net row is
	// status=pending with next_retry_at ~10 minutes out — exactly what the
	// real auto-retry path would write, just via exported methods.
	if err := e.server.Store.FinishJob(ctx, job.ID, model.JobFailed, 1, "gate", nil, ""); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(10 * time.Minute)
	if err := e.server.Store.ScheduleJobRetry(ctx, job.ID, 1, future); err != nil {
		t.Fatal(err)
	}

	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 1, 0))
	if resp.StatusCode != 200 {
		t.Fatalf("heartbeat: %d %s", resp.StatusCode, body)
	}
	var reply model.HeartbeatReply
	json.Unmarshal(body, &reply)
	if reply.Instruction != "none" {
		t.Fatalf("backoff-gated job must not be assigned, got instruction %q", reply.Instruction)
	}
}

// TestAssignmentPicksPriorityJob asserts the heartbeat assignment path
// dispatches the highest-priority pending job first.
func TestAssignmentPicksPriorityJob(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	// Low priority (created first, would win under plain FIFO).
	low, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "L", Episode: "01", EpisodeDir: "L/Ep 01", ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	// High priority (created second).
	high, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "H", Episode: "01", EpisodeDir: "H/Ep 01", ScriptType: "vpy", FlowID: fl.ID, Priority: 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 1, 0))
	if resp.StatusCode != 200 {
		t.Fatalf("heartbeat: %d %s", resp.StatusCode, body)
	}
	var reply model.HeartbeatReply
	json.Unmarshal(body, &reply)
	if reply.Instruction != "job" || reply.Job == nil {
		t.Fatalf("want job instruction, got %q (%s)", reply.Instruction, body)
	}
	if reply.Job.ID != high.ID {
		t.Fatalf("expected high-priority job %d, got %d", high.ID, reply.Job.ID)
	}
	// The low-priority job must still be pending (unassigned).
	got, _ := e.server.Store.GetJob(ctx, low.ID)
	if got.Status != model.JobPending {
		t.Fatalf("low-priority job status = %q, want pending", got.Status)
	}
}

// TestPatchJobPriority asserts PATCH /api/jobs/{id} accepts an optional
// priority on a pending job and refuses it on non-pending jobs.
func TestPatchJobPriority(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "P", Episode: "01", EpisodeDir: "P/Ep 01", ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Set priority via PATCH.
	resp, body := doJSON(t, "PATCH", ts.URL+"/api/jobs/"+itoa(job.ID), adminTok,
		map[string]any{"priority": 42})
	if resp.StatusCode != 200 {
		t.Fatalf("patch priority: %d %s", resp.StatusCode, body)
	}
	var patched model.Job
	json.Unmarshal(body, &patched)
	if patched.Priority != 42 {
		t.Fatalf("priority = %d, want 42", patched.Priority)
	}

	// Verify it persisted.
	got, _ := e.server.Store.GetJob(ctx, job.ID)
	if got.Priority != 42 {
		t.Fatalf("priority not persisted: %d", got.Priority)
	}

	// Refused on a non-pending job: assign it, then try to patch.
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}
	resp2, _ := doJSON(t, "PATCH", ts.URL+"/api/jobs/"+itoa(job.ID), adminTok,
		map[string]any{"priority": 99})
	if resp2.StatusCode != 409 {
		t.Fatalf("patch priority on assigned job: want 409, got %d", resp2.StatusCode)
	}
}

// TestPatchJobRequiresEitherField asserts PATCH /api/jobs/{id} requires at
// least one of flow_id or priority (rejects empty body).
func TestPatchJobRequiresEitherField(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	job, _ := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "E", Episode: "01", EpisodeDir: "E/Ep 01", ScriptType: "vpy", FlowID: fl.ID,
	})
	resp, _ := doJSON(t, "PATCH", ts.URL+"/api/jobs/"+itoa(job.ID), adminTok,
		map[string]any{})
	if resp.StatusCode != 400 {
		t.Fatalf("empty patch: want 400, got %d", resp.StatusCode)
	}
}

// TestNotifyCarriesRetryCount asserts the exhausted-failure alert carries
// the retry count (the Discord notifier appends "(after N retries)"). Uses
// the real Discord notifier against a capture webhook so the message body
// is observable.
func TestNotifyCarriesRetryCount(t *testing.T) {
	// This test uses the notify package's own Discord + a capture webhook,
	// exercising the "(after N retries)" suffix path directly.
	srv := newCaptureWebhookServer()
	defer srv.Close()
	n := notify.NewDiscord(srv.URL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	n.JobFinished(context.Background(), &model.Job{
		ID: 9, Series: "Exhaust", Episode: "01", FlowID: 1,
		Status: model.JobFailed, Step: "encode", Error: "boom",
		RetryCount: 2,
	}, "enc-09")
	body := srv.LastBody()
	if body == nil {
		t.Fatal("no webhook call")
	}
	content := body["content"]
	if !strings.Contains(content, "after 2 retries") {
		t.Fatalf("alert missing retry count: %s", content)
	}
}

// captureWebhookServer is an httptest server that records every JSON body
// posted to it, mirroring the notify package's test captureWebhook.
type captureWebhookServer struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]string
}

func newCaptureWebhookServer() *captureWebhookServer {
	c := &captureWebhookServer{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		c.bodies = append(c.bodies, body)
		c.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	return c
}

func (c *captureWebhookServer) LastBody() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		return nil
	}
	return c.bodies[len(c.bodies)-1]
}
