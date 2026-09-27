package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// mustCreateNode creates a node and fails the test on error (test helper).
func mustCreateNode(t *testing.T, s *Store, name string) int64 {
	t.Helper()
	n, err := s.CreateNode(context.Background(), name, "hash-"+name)
	if err != nil {
		t.Fatalf("create node %s: %v", name, err)
	}
	return n.ID
}

// ---------- options_json round-trip ----------

// TestFlowOptionsRoundTrip asserts the per-flow retry policy survives a
// create → get → list cycle. The default (no policy set) must read back as
// zero values — which the caller interprets as "retry OFF".
func TestFlowOptionsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// A flow with a retry policy.
	withPolicy := &model.Flow{
		Name:                "retry-flow",
		Steps:               []model.Step{{Type: model.StepDGIndex}},
		MaxRetries:          3,
		RetryBackoffMinutes: 5,
	}
	created, err := s.CreateFlow(ctx, withPolicy)
	if err != nil {
		t.Fatalf("create flow: %v", err)
	}
	got, err := s.GetFlow(ctx, created.ID)
	if err != nil {
		t.Fatalf("get flow: %v", err)
	}
	if got.MaxRetries != 3 || got.RetryBackoffMinutes != 5 {
		t.Fatalf("GetFlow: retry policy lost: max=%d backoff=%d", got.MaxRetries, got.RetryBackoffMinutes)
	}

	// ListFlows must also carry the policy.
	list, err := s.ListFlows(ctx)
	if err != nil {
		t.Fatalf("list flows: %v", err)
	}
	var found *model.Flow
	for _, f := range list {
		if f.ID == created.ID {
			found = f
		}
	}
	if found == nil {
		t.Fatal("created flow not in list")
	}
	if found.MaxRetries != 3 || found.RetryBackoffMinutes != 5 {
		t.Fatalf("ListFlows: retry policy lost: max=%d backoff=%d", found.MaxRetries, found.RetryBackoffMinutes)
	}

	// UpdateFlow must persist a changed policy.
	created.MaxRetries = 1
	created.RetryBackoffMinutes = 2
	if err := s.UpdateFlow(ctx, created); err != nil {
		t.Fatalf("update flow: %v", err)
	}
	updated, _ := s.GetFlow(ctx, created.ID)
	if updated.MaxRetries != 1 || updated.RetryBackoffMinutes != 2 {
		t.Fatalf("UpdateFlow: policy not changed: max=%d backoff=%d", updated.MaxRetries, updated.RetryBackoffMinutes)
	}

	// DefaultFlow reads the same column (DefaultFlow SELECTs flows).
	created.IsDefault = true
	if err := s.SetDefaultFlow(ctx, created.ID); err != nil {
		t.Fatalf("set default flow: %v", err)
	}
	def, err := s.DefaultFlow(ctx)
	if err != nil {
		t.Fatalf("default flow: %v", err)
	}
	if def.ID != created.ID {
		t.Fatalf("default flow id mismatch: %d vs %d", def.ID, created.ID)
	}
	if def.MaxRetries != 1 || def.RetryBackoffMinutes != 2 {
		t.Fatalf("DefaultFlow: policy lost: max=%d backoff=%d", def.MaxRetries, def.RetryBackoffMinutes)
	}
}

// TestFlowOptionsDefaultsZero asserts a flow created without a retry policy
// reads back with zero values (retry OFF).
func TestFlowOptionsDefaultsZero(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	plain := &model.Flow{
		Name:  "no-retry",
		Steps: []model.Step{{Type: model.StepDGIndex}},
	}
	created, err := s.CreateFlow(ctx, plain)
	if err != nil {
		t.Fatalf("create flow: %v", err)
	}
	got, _ := s.GetFlow(ctx, created.ID)
	if got.MaxRetries != 0 || got.RetryBackoffMinutes != 0 {
		t.Fatalf("default policy must be zero: max=%d backoff=%d", got.MaxRetries, got.RetryBackoffMinutes)
	}
}

// TestFlowOptionsMigrationIdempotent asserts the options_json ALTER is safe
// to run twice (the tolerant-ALTER pattern).
func TestFlowOptionsMigrationIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opts.db")

	st1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	st1.Close()

	st2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("second open (idempotency): %v", err)
	}
	t.Cleanup(func() { st2.Close() })
	ctx := context.Background()

	// options_json column must be present and selectable.
	_, err = st2.db.ExecContext(ctx, `SELECT options_json FROM flows WHERE id = 0`)
	if err != nil {
		t.Fatalf("options_json column not selectable after re-open: %v", err)
	}
}

// ---------- ScheduleJobRetry ----------

// TestScheduleJobRetryOnFailed asserts the guarded UPDATE succeeds when the
// job is in the 'failed' state and stamps retry_count + next_retry_at.
func TestScheduleJobRetryOnFailed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)

	j, err := s.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: flow.ID})
	if err != nil {
		t.Fatal(err)
	}
	// Move to failed state (the completion path does this via FinishJob).
	if err := s.FinishJob(ctx, j.ID, model.JobFailed, 1, "boom", nil, "tail"); err != nil {
		t.Fatalf("finish job: %v", err)
	}

	next := time.Now().UTC().Add(5 * time.Minute)
	if err := s.ScheduleJobRetry(ctx, j.ID, 1, next); err != nil {
		t.Fatalf("schedule retry: %v", err)
	}

	got, _ := s.GetJob(ctx, j.ID)
	if got.Status != model.JobPending {
		t.Fatalf("job status = %q, want pending", got.Status)
	}
	if got.RetryCount != 1 {
		t.Fatalf("retry_count = %d, want 1", got.RetryCount)
	}
	if got.NextRetryAt == nil {
		t.Fatal("next_retry_at not set")
	}
	// Within a 2-second tolerance for the stamp write/read round-trip.
	if got.NextRetryAt.Sub(next).Abs() > 2*time.Second {
		t.Fatalf("next_retry_at = %v, want ~%v", got.NextRetryAt, next)
	}
}

// TestScheduleJobRetryClearsStaleRunState asserts ScheduleJobRetry wipes the
// failed run's observability fields so a re-queued pending job never briefly
// shows the old run's error/log to the UI. The clear set must mirror
// RetryJob: started_at=NULL, finished_at=NULL, error=”, exit_code=0,
// outputs_json='[]', log_tail=”, full_log=”, step_timings_json='[]'.
func TestScheduleJobRetryClearsStaleRunState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)

	j, err := s.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: flow.ID})
	if err != nil {
		t.Fatal(err)
	}
	// Assign + run + fail with a full report so every stale field is
	// populated (error, exit_code, outputs, log_tail, full_log,
	// step_timings_json, started_at, finished_at).
	if err := s.AssignJob(ctx, j.ID, mustCreateNode(t, s, "enc-clear")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateJobStatus(ctx, j.ID, model.JobRunning, "encode", 50, "run tail"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJobWithReport(ctx, j.ID, model.JobFailed, 9, "encode crashed",
		[]string{"stale.mkv"}, "final tail", "full log body",
		[]model.StepTiming{{Step: "encode", DurationSec: 120}}, nil); err != nil {
		t.Fatalf("finish with report: %v", err)
	}
	// Confirm the stale state is present before retry.
	pre, _ := s.GetJob(ctx, j.ID)
	if pre.Error != "encode crashed" || pre.ExitCode != 9 || pre.LogTail != "final tail" ||
		pre.FullLog != "full log body" || len(pre.Outputs) != 1 || len(pre.StepTimings) != 1 ||
		pre.StartedAt == nil || pre.FinishedAt == nil {
		t.Fatalf("pre-retry stale state not populated: %+v", pre)
	}

	// ScheduleJobRetry must clear ALL of those fields.
	next := time.Now().UTC().Add(1 * time.Minute)
	if err := s.ScheduleJobRetry(ctx, j.ID, 1, next); err != nil {
		t.Fatalf("schedule retry: %v", err)
	}

	got, _ := s.GetJob(ctx, j.ID)
	if got.Status != model.JobPending {
		t.Fatalf("status = %q, want pending", got.Status)
	}
	if got.Error != "" {
		t.Errorf("error = %q, want empty", got.Error)
	}
	if got.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0", got.ExitCode)
	}
	if got.LogTail != "" {
		t.Errorf("log_tail = %q, want empty", got.LogTail)
	}
	if got.FullLog != "" {
		t.Errorf("full_log = %q, want empty", got.FullLog)
	}
	if len(got.Outputs) != 0 {
		t.Errorf("outputs = %v, want empty", got.Outputs)
	}
	if len(got.StepTimings) != 0 {
		t.Errorf("step_timings = %v, want empty", got.StepTimings)
	}
	if got.StartedAt != nil {
		t.Errorf("started_at = %v, want nil", got.StartedAt)
	}
	if got.FinishedAt != nil {
		t.Errorf("finished_at = %v, want nil", got.FinishedAt)
	}
}

// TestScheduleJobRetryRefusesNonFailed asserts the guarded UPDATE refuses a
// job that is NOT failed (e.g. running), so a retry can never resurrect a
// live job.
func TestScheduleJobRetryRefusesNonFailed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)

	j, err := s.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: flow.ID})
	if err != nil {
		t.Fatal(err)
	}
	// Job is pending (not failed). The guarded UPDATE must be a no-op.
	next := time.Now().UTC().Add(5 * time.Minute)
	if err := s.ScheduleJobRetry(ctx, j.ID, 1, next); err != nil {
		t.Fatalf("schedule retry on non-failed should return nil error (no-op), got: %v", err)
	}
	got, _ := s.GetJob(ctx, j.ID)
	if got.Status != model.JobPending {
		t.Fatalf("job status changed to %q, want pending", got.Status)
	}
	if got.RetryCount != 0 {
		t.Fatalf("retry_count = %d, want 0 (not retried)", got.RetryCount)
	}
	if got.NextRetryAt != nil {
		t.Fatalf("next_retry_at = %v, want nil (not retried)", got.NextRetryAt)
	}
}

// ---------- NextAssignableJob ----------

// TestNextAssignableJobSkipsFutureRetry asserts a pending job whose
// next_retry_at is in the future is NOT assignable.
func TestNextAssignableJobSkipsFutureRetry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)

	j, err := s.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: flow.ID})
	if err != nil {
		t.Fatal(err)
	}
	// Stamp a future retry gate directly.
	future := time.Now().UTC().Add(10 * time.Minute)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET next_retry_at=? WHERE id=?`, fmtTime(future), j.ID); err != nil {
		t.Fatal(err)
	}

	got, err := s.NextAssignableJob(ctx)
	if err != nil {
		t.Fatalf("next assignable: %v", err)
	}
	if got != nil {
		t.Fatalf("future-retry job must not be assignable, got job %d", got.ID)
	}
}

// TestNextAssignableJobIncludesPastAndNull asserts a pending job with a
// past next_retry_at (backoff elapsed) and one with NULL (no retry) are
// both assignable, and that within a priority tier the OLDEST job (lowest
// id) wins — true FIFO, so episodes encode in submission order on a farm.
func TestNextAssignableJobIncludesPastAndNull(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)

	// Job A: past next_retry_at (backoff elapsed). Created first → lowest id.
	a, err := s.CreateJob(ctx, &model.Job{Series: "A", Episode: "01", EpisodeDir: "A/Ep 01", ScriptType: "vpy", FlowID: flow.ID})
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-1 * time.Minute)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET next_retry_at=? WHERE id=?`, fmtTime(past), a.ID); err != nil {
		t.Fatal(err)
	}

	// Job B: NULL next_retry_at (no retry). Created second → higher id.
	if _, err := s.CreateJob(ctx, &model.Job{Series: "B", Episode: "01", EpisodeDir: "B/Ep 01", ScriptType: "vpy", FlowID: flow.ID}); err != nil {
		t.Fatal(err)
	}

	got, err := s.NextAssignableJob(ctx)
	if err != nil {
		t.Fatalf("next assignable: %v", err)
	}
	if got == nil {
		t.Fatal("expected an assignable job, got nil")
	}
	// Both are priority 0; id ASC (FIFO) means A (created first) wins.
	if got.ID != a.ID {
		t.Fatalf("expected job %d (oldest, FIFO within tier), got %d", a.ID, got.ID)
	}
}

// TestNextAssignableJobOrdersPriorityThenID asserts the ORDER BY
// priority DESC, id ASC picks the highest-priority job first, breaking
// ties by OLDEST id (true FIFO within a priority tier).
func TestNextAssignableJobOrdersPriorityThenID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)

	// Three pending jobs: low priority (id 1, created first), high priority
	// (id 2), medium priority (id 3).
	low, err := s.CreateJob(ctx, &model.Job{Series: "L", Episode: "01", EpisodeDir: "L/Ep 01", ScriptType: "vpy", FlowID: flow.ID, Priority: 0})
	if err != nil {
		t.Fatal(err)
	}
	high, err := s.CreateJob(ctx, &model.Job{Series: "H", Episode: "01", EpisodeDir: "H/Ep 01", ScriptType: "vpy", FlowID: flow.ID, Priority: 10})
	if err != nil {
		t.Fatal(err)
	}
	med, err := s.CreateJob(ctx, &model.Job{Series: "M", Episode: "01", EpisodeDir: "M/Ep 01", ScriptType: "vpy", FlowID: flow.ID, Priority: 5})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.NextAssignableJob(ctx)
	if err != nil {
		t.Fatalf("next assignable: %v", err)
	}
	if got == nil {
		t.Fatal("expected an assignable job")
	}
	if got.ID != high.ID {
		t.Fatalf("expected highest-priority job %d, got %d", high.ID, got.ID)
	}
	// Remove it (assign), then the medium-priority job must come next.
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET status='assigned' WHERE id=?`, high.ID); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.NextAssignableJob(ctx)
	if got2 == nil || got2.ID != med.ID {
		t.Fatalf("expected job %d next, got %+v", med.ID, got2)
	}
	// Tie-break: two jobs at the same priority → OLDEST id wins (FIFO).
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET status='pending', priority=5 WHERE id=?`, low.ID); err != nil {
		t.Fatal(err)
	}
	// Now low (id=low.ID, priority=5) and med (id=med.ID, priority=5) are
	// both pending at priority 5. low has the lower id (created first).
	got3, _ := s.NextAssignableJob(ctx)
	if got3 == nil || got3.ID != low.ID {
		t.Fatalf("expected tie-break to pick oldest id %d (FIFO), got %+v", low.ID, got3)
	}
}

// TestNextAssignableJobReturnsNilWhenNone asserts the method returns nil
// when there are no assignable pending jobs.
func TestNextAssignableJobReturnsNilWhenNone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	got, err := s.NextAssignableJob(ctx)
	if err != nil {
		t.Fatalf("next assignable: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got job %d", got.ID)
	}
}
