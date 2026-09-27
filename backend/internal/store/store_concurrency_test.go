package store

import (
	"context"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestAssignJobConcurrency verifies the per-node slot guard: with
// max_concurrent_jobs = N, up to N jobs assign; the N+1th is refused.
// Default (1) preserves the historical one-job-per-node rule.
func TestAssignJobConcurrency(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	fl, err := s.CreateFlow(ctx, &model.Flow{Name: "f1", Steps: []model.Step{{Type: model.StepEncode}}})
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.CreateNode(ctx, "n1", "h1")
	if err != nil {
		t.Fatal(err)
	}
	mkJob := func(ep string) *model.Job {
		j, err := s.CreateJob(ctx, &model.Job{
			Series: "Conc Show", Episode: ep, EpisodeDir: "Conc Show/Ep " + ep,
			ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending,
		})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}

	// Default: one slot.
	j1 := mkJob("01")
	if err := s.AssignJob(ctx, j1.ID, n.ID); err != nil {
		t.Fatalf("first assign: %v", err)
	}
	j2 := mkJob("02")
	if err := s.AssignJob(ctx, j2.ID, n.ID); err == nil {
		t.Fatal("second assign succeeded at default concurrency 1")
	}

	// Raise the cap: the second job now assigns, a third still refuses.
	if err := s.SetNodeMaxConcurrent(ctx, n.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignJob(ctx, j2.ID, n.ID); err != nil {
		t.Fatalf("second assign at max=2: %v", err)
	}
	j3 := mkJob("03")
	if err := s.AssignJob(ctx, j3.ID, n.ID); err == nil {
		t.Fatal("third assign succeeded at max=2")
	}

	// Node status is busy only at capacity.
	got, _ := s.GetNode(ctx, n.ID)
	if got.Status != model.NodeBusy {
		t.Fatalf("status = %s at capacity, want busy", got.Status)
	}

	// Finishing one job frees a slot and un-busies the node.
	if err := s.FinishJob(ctx, j1.ID, model.JobDone, 0, "", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseNode(ctx, n.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetNode(ctx, n.ID)
	if got.Status != model.NodeIdle {
		t.Fatalf("status = %s with a free slot, want idle", got.Status)
	}
	if err := s.AssignJob(ctx, j3.ID, n.ID); err != nil {
		t.Fatalf("third assign after finish: %v", err)
	}
}

// TestActiveJobsForNode verifies the set-based orphan-recovery query: all
// assigned/running jobs for a node, terminal jobs of other nodes excluded.
func TestActiveJobsForNode(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	fl, _ := s.CreateFlow(ctx, &model.Flow{Name: "f1", Steps: []model.Step{{Type: model.StepEncode}}})
	n1, _ := s.CreateNode(ctx, "n1", "h1")
	n2, _ := s.CreateNode(ctx, "n2", "h2")

	j1, _ := s.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending})
	j2, _ := s.CreateJob(ctx, &model.Job{Series: "S", Episode: "02", EpisodeDir: "S/Ep 02", ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending})
	j3, _ := s.CreateJob(ctx, &model.Job{Series: "S", Episode: "03", EpisodeDir: "S/Ep 03", ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending})
	if err := s.SetNodeMaxConcurrent(ctx, n1.ID, 2); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{j1.ID, j2.ID} {
		if err := s.AssignJob(ctx, id, n1.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AssignJob(ctx, j3.ID, n2.ID); err != nil {
		t.Fatal(err)
	}

	active, err := s.ActiveJobsForNode(ctx, n1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 {
		t.Fatalf("active jobs = %d, want 2", len(active))
	}
	ids := map[int64]bool{active[0].ID: true, active[1].ID: true}
	if !ids[j1.ID] || !ids[j2.ID] {
		t.Fatalf("wrong jobs: %v", ids)
	}
	// Finish one: only the survivor is active.
	if err := s.FinishJob(ctx, j1.ID, model.JobDone, 0, "", nil, ""); err != nil {
		t.Fatal(err)
	}
	active, _ = s.ActiveJobsForNode(ctx, n1.ID)
	if len(active) != 1 || active[0].ID != j2.ID {
		t.Fatalf("after finish: %+v", active)
	}
}

// TestSetNodeMaxConcurrentValidation verifies the setter clamps invalid
// values: <1 becomes 1, >8 becomes 8 (the same bounds the API enforces,
// defended at the storage layer so direct callers can't corrupt the row).
func TestSetNodeMaxConcurrentValidation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	n, _ := s.CreateNode(ctx, "n1", "h1")

	if err := s.SetNodeMaxConcurrent(ctx, n.ID, 0); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetNode(ctx, n.ID)
	if got.MaxConcurrentJobs != 1 {
		t.Fatalf("0 clamped to %d, want 1", got.MaxConcurrentJobs)
	}
	if err := s.SetNodeMaxConcurrent(ctx, n.ID, 99); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetNode(ctx, n.ID)
	if got.MaxConcurrentJobs != 8 {
		t.Fatalf("99 clamped to %d, want 8", got.MaxConcurrentJobs)
	}
}
