package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// newTestStore opens a throwaway SQLite DB in the test's temp dir.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedFlow creates the default test flow and returns it.
func seedFlow(t *testing.T, s *Store) *model.Flow {
	t.Helper()
	f, err := s.CreateFlow(context.Background(), &model.Flow{
		Name:  "default-1080",
		Steps: []model.Step{{Type: model.StepDGIndex, Params: nil}},
	})
	if err != nil {
		t.Fatalf("create flow: %v", err)
	}
	return f
}

func TestNodeCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	n, err := s.CreateNode(ctx, "enc-01", "hash-a")
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if n.Name != "enc-01" || n.TokenHash != "hash-a" {
		t.Fatalf("unexpected node: %+v", n)
	}
	if !n.Enabled {
		t.Fatal("new node should default to enabled")
	}

	got, err := s.NodeByTokenHash(ctx, "hash-a")
	if err != nil {
		t.Fatalf("lookup by token hash: %v", err)
	}
	if got.ID != n.ID {
		t.Fatalf("token lookup returned wrong node: %+v", got)
	}

	n.Status = model.NodeBusy
	n.TasksSinceBoot = 3
	if err := s.UpdateNode(ctx, n); err != nil {
		t.Fatalf("update node: %v", err)
	}
	got, err = s.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Status != model.NodeBusy || got.TasksSinceBoot != 3 {
		t.Fatalf("update not persisted: %+v", got)
	}

	nodes, err := s.ListNodes(ctx)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("list nodes: got %d, err %v", len(nodes), err)
	}
}

func TestAssignJobEnforcesOneJobPerNode(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)

	node, err := s.CreateNode(ctx, "enc-01", "h")
	if err != nil {
		t.Fatal(err)
	}
	j1, err := s.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: flow.ID})
	if err != nil {
		t.Fatal(err)
	}
	j2, err := s.CreateJob(ctx, &model.Job{Series: "S", Episode: "02", EpisodeDir: "S/Ep 02", ScriptType: "vpy", FlowID: flow.ID})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.AssignJob(ctx, j1.ID, node.ID); err != nil {
		t.Fatalf("first assign should succeed: %v", err)
	}
	// Second assignment to the same node must be refused.
	if err := s.AssignJob(ctx, j2.ID, node.ID); err == nil {
		t.Fatal("second concurrent assign to same node must fail")
	}

	// The active job must be retrievable.
	active, err := s.ActiveJobForNode(ctx, node.ID)
	if err != nil || active == nil || active.ID != j1.ID {
		t.Fatalf("active job mismatch: %+v, err %v", active, err)
	}

	// Finishing j1 frees the node for j2.
	if err := s.FinishJob(ctx, j1.ID, model.JobDone, 0, "", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseNode(ctx, node.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignJob(ctx, j2.ID, node.ID); err != nil {
		t.Fatalf("assign after finish should succeed: %v", err)
	}
}

func TestJobLifecycleAndRetry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)

	node, err := s.CreateNode(ctx, "lifecycle-node", "h")
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "avs", FlowID: flow.ID})
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != model.JobPending {
		t.Fatalf("new job status = %s, want pending", j.Status)
	}

	exists, err := s.JobExistsForEpisode(ctx, "S/Ep 01")
	if err != nil || !exists {
		t.Fatalf("episode dedupe failed: exists=%v err=%v", exists, err)
	}

	if err := s.AssignJob(ctx, j.ID, node.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateJobStatus(ctx, j.ID, model.JobRunning, "encode", 42.5, "tail"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobRunning || got.Step != "encode" || got.Progress != 42.5 {
		t.Fatalf("status update not persisted: %+v", got)
	}

	if err := s.FinishJob(ctx, j.ID, model.JobFailed, 2, "x265 crashed", []string{"a.mkv"}, "err tail"); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobFailed || got.ExitCode != 2 || len(got.Outputs) != 1 {
		t.Fatalf("finish not persisted: %+v", got)
	}

	// Retry re-queues the job as pending.
	if n, err := s.RetryJob(ctx, j.ID); err != nil || n != 1 {
		t.Fatalf("retry: n=%d err=%v", n, err)
	}
	got, err = s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobPending || got.NodeID != 0 {
		t.Fatalf("retry not persisted: %+v", got)
	}
}

func TestFlowCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	f := seedFlow(t, s)
	got, err := s.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != f.ID || len(got.Steps) != 1 || got.Steps[0].Type != model.StepDGIndex {
		t.Fatalf("flow round-trip failed: %+v", got)
	}

	got.Steps = append(got.Steps, model.Step{Type: model.StepAudio, Params: map[string]string{"bitrate": "320"}})
	if err := s.UpdateFlow(ctx, got); err != nil {
		t.Fatal(err)
	}
	got2, err := s.GetFlow(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2.Steps) != 2 || got2.Steps[1].Params["bitrate"] != "320" {
		t.Fatalf("flow update not persisted: %+v", got2)
	}

	flows, err := s.ListFlows(ctx)
	if err != nil || len(flows) != 1 {
		t.Fatalf("list flows: %d, err %v", len(flows), err)
	}

	if err := s.DeleteFlow(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetFlow(ctx, f.ID); err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestListJobsFilterByStatus(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)

	j1, _ := s.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: flow.ID})
	s.CreateJob(ctx, &model.Job{Series: "S", Episode: "02", EpisodeDir: "S/Ep 02", ScriptType: "vpy", FlowID: flow.ID})
	s.FinishJob(ctx, j1.ID, model.JobDone, 0, "", nil, "")

	pending, err := s.ListJobs(ctx, model.JobPending, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Episode != "02" {
		t.Fatalf("pending filter wrong: %+v", pending)
	}
	all, err := s.ListJobs(ctx, "", 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("list all: %d err %v", len(all), err)
	}
	// Newest first.
	if all[0].Episode != "02" {
		t.Fatalf("ordering wrong: %+v", all[0])
	}
}

// Regression (adversarial review): AssignJob must error when the job is not
// pending and must NOT mark the node busy — the old code committed a phantom
// busy node with no job, breaking the one-job-per-node invariant.
func TestAssignNonPendingJobLeavesNodeIdle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)
	node, _ := s.CreateNode(ctx, "n", "h")
	j, _ := s.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: flow.ID})
	s.FinishJob(ctx, j.ID, model.JobDone, 0, "", nil, "")

	if err := s.AssignJob(ctx, j.ID, node.ID); err == nil {
		t.Fatal("assigning a done job must error")
	}
	got, _ := s.GetNode(ctx, node.ID)
	if got.Status != model.NodeIdle {
		t.Fatalf("node must stay idle after refused assign, got %s", got.Status)
	}
	active, _ := s.ActiveJobForNode(ctx, node.ID)
	if active != nil {
		t.Fatalf("no active job expected: %+v", active)
	}
}

// Regression: assigning to a disabled node must fail.
func TestAssignToDisabledNodeFails(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)
	node, _ := s.CreateNode(ctx, "n", "h")
	node.Enabled = false
	s.UpdateNode(ctx, node)
	j, _ := s.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: flow.ID})

	if err := s.AssignJob(ctx, j.ID, node.ID); err == nil {
		t.Fatal("assign to disabled node must error")
	}
	got, _ := s.GetJob(ctx, j.ID)
	if got.Status != model.JobPending {
		t.Fatalf("job must remain pending: %+v", got)
	}
}

// Regression: deleting a flow with job history must be refused (FK).
func TestDeleteFlowWithJobsRefused(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)
	s.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: flow.ID})

	if err := s.DeleteFlow(ctx, flow.ID); err == nil {
		t.Fatal("delete of referenced flow must error")
	}
	// Unreferenced flows still delete fine.
	f2, _ := s.CreateFlow(ctx, &model.Flow{Name: "free", Steps: []model.Step{{Type: model.StepDGIndex}}})
	if err := s.DeleteFlow(ctx, f2.ID); err != nil {
		t.Fatalf("unreferenced flow delete failed: %v", err)
	}
}

// Regression: UpdateJobStatus must not resurrect terminal jobs.
func TestUpdateJobStatusIgnoresTerminalJobs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)
	j, _ := s.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: flow.ID})
	s.FinishJob(ctx, j.ID, model.JobDone, 0, "", nil, "")

	s.UpdateJobStatus(ctx, j.ID, model.JobRunning, "encode", 50, "stale")
	got, _ := s.GetJob(ctx, j.ID)
	if got.Status != model.JobDone {
		t.Fatalf("terminal job resurrected: %+v", got)
	}
}

// Regression: failed jobs must not report 100% progress.
func TestFinishFailedJobKeepsProgress(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)
	node, _ := s.CreateNode(ctx, "n", "h")
	j, _ := s.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: flow.ID})
	if err := s.AssignJob(ctx, j.ID, node.ID); err != nil {
		t.Fatal(err)
	}
	s.UpdateJobStatus(ctx, j.ID, model.JobRunning, "encode", 37, "")
	s.FinishJob(ctx, j.ID, model.JobFailed, 1, "crash", nil, "tail")

	got, _ := s.GetJob(ctx, j.ID)
	if got.Progress != 37 {
		t.Fatalf("failed job progress = %v, want 37 (last known)", got.Progress)
	}
}

// Regression: retry clears stale log/output and reports no-ops honestly.
func TestRetryJobClearsStaleStateAndReportsNoop(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)
	j, _ := s.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: flow.ID})
	s.FinishJob(ctx, j.ID, model.JobFailed, 2, "boom", []string{"x.mkv"}, "old tail")

	n, err := s.RetryJob(ctx, j.ID)
	if err != nil || n != 1 {
		t.Fatalf("retry: n=%d err=%v", n, err)
	}
	got, _ := s.GetJob(ctx, j.ID)
	if got.LogTail != "" || len(got.Outputs) != 0 {
		t.Fatalf("stale state survived retry: %+v", got)
	}
	// Retrying the now-pending job is a no-op and must say so.
	n2, err := s.RetryJob(ctx, j.ID)
	if err != nil || n2 != 0 {
		t.Fatalf("no-op retry: n=%d err=%v", n2, err)
	}
}

func TestAssignRefusesNonPendingJob(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)
	node, _ := s.CreateNode(ctx, "n", "h")
	j, _ := s.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: flow.ID})
	s.FinishJob(ctx, j.ID, model.JobDone, 0, "", nil, "")

	// Assigning a done job is a no-op UPDATE (0 rows); the node stays idle
	// and no active job appears. Verify that invariant.
	if err := s.AssignJob(ctx, j.ID, node.ID); err != nil {
		// Acceptable: explicit refusal.
		if !errors.Is(err, context.Canceled) {
			t.Logf("assign of done job refused: %v", err)
		}
	}
	active, err := s.ActiveJobForNode(ctx, node.ID)
	if err != nil || active != nil {
		t.Fatalf("done job must not become active: %+v err=%v", active, err)
	}
}
