package api

import (
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// The watchdog tests drive stallWatchdogTick directly (the loop's test entry
// point, mirroring nodeAlertTick) with backdated last_progress_at stamps so
// no real waiting is needed. The recordingNotifier from retry_test.go
// observes whether the job-failure notification fired.

// stallEnv builds a test env with a recording notifier and a running job
// whose progress stamp is backdated two hours. The flow's retry policy is
// configurable per test.
func stallEnv(t *testing.T, maxRetries int) (*testEnv, *recordingNotifier, *model.Job) {
	t.Helper()
	e := newTestEnv(t)
	rec := &recordingNotifier{}
	e.server.Notifier = rec
	ctx := ctxBg()
	fl, err := e.server.Store.CreateFlow(ctx, &model.Flow{
		Name:                "stall-flow",
		Steps:               []model.Step{{Type: model.StepDGIndex}},
		MaxRetries:          maxRetries,
		RetryBackoffMinutes: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "StallShow", Episode: "01", EpisodeDir: "StallShow/Ep 01",
		ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.UpdateJobStatus(ctx, job.ID, model.JobRunning, "encode", 42, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.SeedJobLastProgress(ctx, job.ID, time.Now().UTC().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	return e, rec, job
}

// enableStallWatchdog saves settings with the given threshold (store-level
// save; the watchdog reads the live row exactly as production does).
func enableStallWatchdog(t *testing.T, e *testEnv, minutes int) {
	t.Helper()
	if err := e.server.Store.SaveSettings(ctxBg(), &model.Settings{StallTimeoutMinutes: minutes}); err != nil {
		t.Fatal(err)
	}
}

// TestStallWatchdogFailsStalledJobWithRetryPolicy: a stalled running job on
// a flow with a retry budget is failed then silently re-queued — pending,
// retry_count incremented, backoff gate stamped, notification skipped.
func TestStallWatchdogFailsStalledJobWithRetryPolicy(t *testing.T) {
	e, rec, job := stallEnv(t, 2)
	enableStallWatchdog(t, e, 30)

	e.server.stallWatchdogTick(ctxBg())

	got, err := e.server.Store.GetJob(ctxBg(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobPending {
		t.Fatalf("status = %s, want pending (auto-retried)", got.Status)
	}
	if got.RetryCount != 1 {
		t.Fatalf("retry_count = %d, want 1", got.RetryCount)
	}
	if got.NextRetryAt == nil || !got.NextRetryAt.After(time.Now()) {
		t.Fatalf("next_retry_at = %v, want a future backoff gate", got.NextRetryAt)
	}
	if rec.count() != 0 {
		t.Fatalf("auto-retried stall must be silent, notifier fired %d times", rec.count())
	}
	// The stall is audited even when the retry is silent.
	events, err := e.server.Store.ListAudit(ctxBg(), 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Action == "job.stalled" && ev.Actor == "controller" && ev.Object == auditObject("job", job.ID) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no job.stalled audit event: %+v", events)
	}
}

// TestStallWatchdogFailsStalledJobWithoutRetryPolicy: with no retry budget
// the job stays failed with the stall error and the standard job-failure
// notification fires.
func TestStallWatchdogFailsStalledJobWithoutRetryPolicy(t *testing.T) {
	e, rec, job := stallEnv(t, 0)
	enableStallWatchdog(t, e, 30)

	e.server.stallWatchdogTick(ctxBg())

	got, err := e.server.Store.GetJob(ctxBg(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
	if !strings.HasPrefix(got.Error, "stalled: no progress for 30 minutes") {
		t.Fatalf("error = %q, want the stall reason", got.Error)
	}
	if rec.count() != 1 {
		t.Fatalf("notifier fired %d times, want 1", rec.count())
	}
	// The node's slot must be freed (no phantom-busy node).
	node, err := e.server.Store.GetNode(ctxBg(), e.node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if node.Status != model.NodeIdle {
		t.Fatalf("node status = %s, want idle after the stall failure", node.Status)
	}
}

// TestStallWatchdogSkipsFreshProgress: a running job with a recent stamp is
// untouched.
func TestStallWatchdogSkipsFreshProgress(t *testing.T) {
	e, rec, job := stallEnv(t, 0)
	enableStallWatchdog(t, e, 30)
	// Refresh the stamp to now — the job is making progress.
	if err := e.server.Store.TouchJobProgress(ctxBg(), job.ID); err != nil {
		t.Fatal(err)
	}

	e.server.stallWatchdogTick(ctxBg())

	got, _ := e.server.Store.GetJob(ctxBg(), job.ID)
	if got.Status != model.JobRunning {
		t.Fatalf("fresh-progress job touched: status = %s", got.Status)
	}
	if rec.count() != 0 {
		t.Fatal("notifier fired for a healthy job")
	}
}

// TestStallWatchdogDisabled: threshold 0 (the default) is a no-op even for
// an ancient stamp.
func TestStallWatchdogDisabled(t *testing.T) {
	e, rec, job := stallEnv(t, 0)
	enableStallWatchdog(t, e, 0)

	e.server.stallWatchdogTick(ctxBg())

	got, _ := e.server.Store.GetJob(ctxBg(), job.ID)
	if got.Status != model.JobRunning {
		t.Fatalf("disabled watchdog touched the job: status = %s", got.Status)
	}
	if rec.count() != 0 {
		t.Fatal("notifier fired while the watchdog is disabled")
	}
}

// TestStallWatchdogNeverTouchesPending: pending jobs are never stalled —
// they have not started executing (queue wait is not a stall).
func TestStallWatchdogNeverTouchesPending(t *testing.T) {
	e, rec, _ := stallEnv(t, 0)
	enableStallWatchdog(t, e, 30)
	ctx := ctxBg()
	fl, _ := e.server.Store.FlowByName(ctx, "stall-flow")
	pending, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "StallShow", Episode: "02", EpisodeDir: "StallShow/Ep 02",
		ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Backdate the pending job's stamp too — it must still be ignored.
	if err := e.server.Store.SeedJobLastProgress(ctx, pending.ID, time.Now().UTC().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	e.server.stallWatchdogTick(ctx)

	got, _ := e.server.Store.GetJob(ctx, pending.ID)
	if got.Status != model.JobPending {
		t.Fatalf("pending job touched by watchdog: status = %s", got.Status)
	}
	if rec.count() != 1 {
		// Exactly one notification: the running job from stallEnv, never
		// the pending one.
		t.Fatalf("notifier fired %d times, want exactly 1 (the running job only)", rec.count())
	}
}

// TestStallWatchdogDoesNotOverwriteConcurrentlyCompletedJob is the F1
// regression: the watchdog snapshot says running+stale, but the agent
// completes the job inside the failure window. The guarded transition must
// leave the done row (and its outputs) untouched — no failed overwrite, no
// auto-retry of a finished encode, no notification.
func TestStallWatchdogDoesNotOverwriteConcurrentlyCompletedJob(t *testing.T) {
	e, rec, job := stallEnv(t, 2) // retry policy ON: an overwrite would re-queue
	enableStallWatchdog(t, e, 30)
	ctx := ctxBg()
	// Snapshot taken while the job is still running and stale…
	snapshot, err := e.server.Store.StalledRunningJobs(ctx, time.Now().UTC().Add(-30*time.Minute))
	if err != nil || len(snapshot) != 1 {
		t.Fatalf("snapshot = %+v err=%v, want the one stalled job", snapshot, err)
	}
	// …then the agent completes the job before the watchdog acts on it.
	if err := e.server.Store.FinishJobWithReport(ctx, job.ID, model.JobDone, 0, "",
		[]string{"StallShow/Ep 01/out.mkv"}, "tail", "", nil, nil); err != nil {
		t.Fatal(err)
	}

	e.server.failStalledJob(ctx, snapshot[0], 30)

	got, err := e.server.Store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobDone {
		t.Fatalf("concurrently completed job overwritten: status = %s, want done", got.Status)
	}
	if len(got.Outputs) != 1 || got.Outputs[0] != "StallShow/Ep 01/out.mkv" {
		t.Fatalf("completed job's outputs wiped: %+v", got.Outputs)
	}
	if got.RetryCount != 0 {
		t.Fatalf("retry_count = %d, want 0 (a finished encode must not re-queue)", got.RetryCount)
	}
	if rec.count() != 0 {
		t.Fatalf("notifier fired %d times for a completed job", rec.count())
	}
}

// TestStallWatchdogStaleSnapshotFailsJobOnce is the F1 double-transition
// regression: two failStalledJob calls on ONE stale snapshot (duplicate
// tick / orphan-recovery race) must burn exactly one retry, not two.
func TestStallWatchdogStaleSnapshotFailsJobOnce(t *testing.T) {
	e, rec, job := stallEnv(t, 2)
	enableStallWatchdog(t, e, 30)
	ctx := ctxBg()
	snapshot, err := e.server.Store.StalledRunningJobs(ctx, time.Now().UTC().Add(-30*time.Minute))
	if err != nil || len(snapshot) != 1 {
		t.Fatalf("snapshot = %+v err=%v, want the one stalled job", snapshot, err)
	}

	e.server.failStalledJob(ctx, snapshot[0], 30)
	e.server.failStalledJob(ctx, snapshot[0], 30)

	got, err := e.server.Store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobPending {
		t.Fatalf("status = %s, want pending (one auto-retry)", got.Status)
	}
	if got.RetryCount != 1 {
		t.Fatalf("retry_count = %d, want 1 (double fail burned two retries)", got.RetryCount)
	}
	if rec.count() != 0 {
		t.Fatalf("notifier fired %d times, want 0 (silent auto-retry)", rec.count())
	}
}

// TestStallWatchdogRetryBudgetExhausted: a stalled job that already burned
// its retry budget stays failed and notifies (carrying the retry count).
func TestStallWatchdogRetryBudgetExhausted(t *testing.T) {
	e, rec, job := stallEnv(t, 1)
	enableStallWatchdog(t, e, 30)
	ctx := ctxBg()
	// Simulate one burned retry: fail + re-queue with retry_count=1, then
	// re-assign/run and backdate again.
	if err := e.server.Store.FinishJob(ctx, job.ID, model.JobFailed, 1, "prior failure", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.ScheduleJobRetry(ctx, job.ID, 1, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.UpdateJobStatus(ctx, job.ID, model.JobRunning, "encode", 42, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.SeedJobLastProgress(ctx, job.ID, time.Now().UTC().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	e.server.stallWatchdogTick(ctx)

	got, _ := e.server.Store.GetJob(ctx, job.ID)
	if got.Status != model.JobFailed {
		t.Fatalf("status = %s, want failed (budget exhausted)", got.Status)
	}
	if got.RetryCount != 1 {
		t.Fatalf("retry_count = %d, want 1 (unchanged)", got.RetryCount)
	}
	if rec.count() != 1 {
		t.Fatalf("notifier fired %d times, want 1", rec.count())
	}
}
