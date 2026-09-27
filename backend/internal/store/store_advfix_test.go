package store

import (
	"context"
	"errors"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestFinishJobWithReportGuardedAgainstConcurrentCancel asserts the status
// guard on FinishJobWithReport: an UPDATE against a job that is no longer
// 'assigned'/'running' (e.g. concurrently cancelled) matches zero rows and
// returns ErrJobNotFinishable so the caller can answer idempotently instead
// of erroring.
func TestFinishJobWithReportGuardedAgainstConcurrentCancel(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)
	node, err := s.CreateNode(ctx, "guard-n", "h")
	if err != nil {
		t.Fatal(err)
	}

	j, err := s.CreateJob(ctx, &model.Job{
		Series: "G", Episode: "01", EpisodeDir: "G/Ep 01", ScriptType: "vpy", FlowID: flow.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AssignJob(ctx, j.ID, node.ID); err != nil {
		t.Fatal(err)
	}

	// Simulate a concurrent cancel: move the job to 'cancelled' directly.
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET status='cancelled' WHERE id=?`, j.ID); err != nil {
		t.Fatal(err)
	}

	// Now FinishJobWithReport must hit zero rows and return the sentinel.
	err = s.FinishJobWithReport(ctx, j.ID, model.JobDone, 0, "", nil, "tail", "log", nil, nil)
	if !errors.Is(err, ErrJobNotFinishable) {
		t.Fatalf("expected ErrJobNotFinishable for concurrently-cancelled job, got %v", err)
	}

	// The job must remain cancelled (not overwritten to done).
	got, _ := s.GetJob(ctx, j.ID)
	if got.Status != model.JobCancelled {
		t.Fatalf("job status = %q, want cancelled (guard prevented overwrite)", got.Status)
	}
}

// TestRetryJobResetsRetryCount asserts the manual-retry budget refresh:
// RetryJob sets retry_count=0 in the same UPDATE so an exhausted job gets
// a fresh automatic retry budget after operator intervention.
func TestRetryJobResetsRetryCount(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)

	j, err := s.CreateJob(ctx, &model.Job{
		Series: "RC", Episode: "01", EpisodeDir: "RC/Ep 01", ScriptType: "vpy", FlowID: flow.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Stamp a retry_count of 3 (exhausted) on a failed job, mirroring what
	// ScheduleJobRetry would write.
	if err := s.FinishJob(ctx, j.ID, model.JobFailed, 1, "exhausted", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT retry_count FROM jobs WHERE id=?`, j.ID).Scan(new(int)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET retry_count=3 WHERE id=?`, j.ID); err != nil {
		t.Fatal(err)
	}

	// Manual retry.
	n, err := s.RetryJob(ctx, j.ID)
	if err != nil || n != 1 {
		t.Fatalf("RetryJob: n=%d err=%v (want 1, nil)", n, err)
	}

	got, err := s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RetryCount != 0 {
		t.Fatalf("retry_count = %d, want 0 (manual retry refreshes the budget)", got.RetryCount)
	}
}

// TestPatchPendingJobAtomicBothFields asserts PatchPendingJob updates both
// flow_id and priority in a single guarded UPDATE when both pointers are set.
func TestPatchPendingJobAtomicBothFields(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)
	flow2, err := s.CreateFlow(ctx, &model.Flow{
		Name:  "alt-flow",
		Steps: []model.Step{{Type: model.StepDGIndex}},
	})
	if err != nil {
		t.Fatal(err)
	}

	j, err := s.CreateJob(ctx, &model.Job{
		Series: "PA", Episode: "01", EpisodeDir: "PA/Ep 01", ScriptType: "vpy", FlowID: flow.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	changed, err := s.PatchPendingJob(ctx, j.ID, &flow2.ID, intPtr(42))
	if err != nil {
		t.Fatalf("PatchPendingJob: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true for pending job")
	}

	got, _ := s.GetJob(ctx, j.ID)
	if got.FlowID != flow2.ID {
		t.Fatalf("flow_id = %d, want %d", got.FlowID, flow2.ID)
	}
	if got.Priority != 42 {
		t.Fatalf("priority = %d, want 42", got.Priority)
	}
}

// TestPatchPendingJobFlowOnly asserts patching only flow_id leaves priority
// untouched (conditional SET).
func TestPatchPendingJobFlowOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)
	flow2, err := s.CreateFlow(ctx, &model.Flow{
		Name:  "flow-only",
		Steps: []model.Step{{Type: model.StepDGIndex}},
	})
	if err != nil {
		t.Fatal(err)
	}

	j, err := s.CreateJob(ctx, &model.Job{
		Series: "PF", Episode: "01", EpisodeDir: "PF/Ep 01", ScriptType: "vpy", FlowID: flow.ID, Priority: 7,
	})
	if err != nil {
		t.Fatal(err)
	}

	changed, err := s.PatchPendingJob(ctx, j.ID, &flow2.ID, nil)
	if err != nil {
		t.Fatalf("PatchPendingJob flow-only: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true for pending job")
	}

	got, _ := s.GetJob(ctx, j.ID)
	if got.FlowID != flow2.ID {
		t.Fatalf("flow_id = %d, want %d", got.FlowID, flow2.ID)
	}
	if got.Priority != 7 {
		t.Fatalf("priority = %d, want 7 (untouched)", got.Priority)
	}
}

// TestPatchPendingJobPriorityOnly asserts patching only priority leaves flow_id
// untouched (conditional SET).
func TestPatchPendingJobPriorityOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)

	j, err := s.CreateJob(ctx, &model.Job{
		Series: "PP", Episode: "01", EpisodeDir: "PP/Ep 01", ScriptType: "vpy", FlowID: flow.ID, Priority: 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	changed, err := s.PatchPendingJob(ctx, j.ID, nil, intPtr(99))
	if err != nil {
		t.Fatalf("PatchPendingJob priority-only: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true for pending job")
	}

	got, _ := s.GetJob(ctx, j.ID)
	if got.Priority != 99 {
		t.Fatalf("priority = %d, want 99", got.Priority)
	}
	if got.FlowID != flow.ID {
		t.Fatalf("flow_id = %d, want %d (untouched)", got.FlowID, flow.ID)
	}
}

// TestPatchPendingJobRunningJobReturns409Shape asserts a non-pending job
// returns changed=false (caller answers 409).
func TestPatchPendingJobRunningJobReturns409Shape(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)
	node, err := s.CreateNode(ctx, "patch-n", "h")
	if err != nil {
		t.Fatal(err)
	}

	j, err := s.CreateJob(ctx, &model.Job{
		Series: "PR", Episode: "01", EpisodeDir: "PR/Ep 01", ScriptType: "vpy", FlowID: flow.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Assign → status becomes 'assigned' (not pending).
	if err := s.AssignJob(ctx, j.ID, node.ID); err != nil {
		t.Fatal(err)
	}

	changed, err := s.PatchPendingJob(ctx, j.ID, nil, intPtr(5))
	if err != nil {
		t.Fatalf("PatchPendingJob on assigned: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false for assigned job (caller should 409)")
	}

	// The assigned job's priority must be unchanged (guard prevented the write).
	got, _ := s.GetJob(ctx, j.ID)
	if got.Priority != 0 {
		t.Fatalf("priority = %d, want 0 (guard prevented write on assigned job)", got.Priority)
	}
}

func intPtr(v int) *int { return &v }
