package store

import (
	"context"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// seedGroupJob creates a series with a node_group and one pending job for it.
func seedGroupJob(t *testing.T, s *Store, ctx context.Context, seriesName, group string, flowID int64) *model.Job {
	t.Helper()
	sr, err := s.UpsertSeriesByName(ctx, seriesName)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSeriesNodeGroup(ctx, sr.ID, group); err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateJob(ctx, &model.Job{
		Series: seriesName, Episode: "01", EpisodeDir: seriesName + "/Ep 01",
		ScriptType: "avs", FlowID: flowID, Status: model.JobPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// TestGroupRouting verifies the wildcard matching rule: a job dispatches to a
// node when series.node_group == nodes.group, or when EITHER side is empty
// (empty = "any"). A mismatched pair never dispatches.
func TestGroupRouting(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	fl, err := s.CreateFlow(ctx, &model.Flow{Name: "f1", Steps: []model.Step{{Type: model.StepEncode}}})
	if err != nil {
		t.Fatal(err)
	}
	gpuNode, err := s.CreateNode(ctx, "gpu-1", "h1")
	if err != nil {
		t.Fatal(err)
	}
	cpuNode, err := s.CreateNode(ctx, "cpu-1", "h2")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodeGroup(ctx, gpuNode.ID, "gpu"); err != nil {
		t.Fatal(err)
	}
	// cpuNode stays in the default "" group.

	gpuJob := seedGroupJob(t, s, ctx, "GPU Show", "gpu", fl.ID)
	cpuJob := seedGroupJob(t, s, ctx, "CPU Show", "cpu", fl.ID)
	anyJob := seedGroupJob(t, s, ctx, "Any Show", "", fl.ID)

	// gpu-1 (group "gpu"): sees gpuJob and anyJob, never cpuJob.
	// Lower id wins within the same priority tier.
	got, err := s.NextAssignableJobForNode(ctx, gpuNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != gpuJob.ID {
		t.Fatalf("gpu node got %+v, want gpuJob %d", got, gpuJob.ID)
	}
	// Consume gpuJob so the next pick advances. FinishJob (not ReleaseNode)
	// is what clears the AssignJob active-job guard, which counts jobs by
	// status not by node status.
	if err := s.FinishJob(ctx, gpuJob.ID, model.JobDone, 0, "", nil, ""); err != nil {
		t.Fatal(err)
	}
	got, err = s.NextAssignableJobForNode(ctx, gpuNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != anyJob.ID {
		t.Fatalf("gpu node second pick = %+v, want anyJob %d (empty series group = wildcard)", got, anyJob.ID)
	}
	if err := s.FinishJob(ctx, anyJob.ID, model.JobDone, 0, "", nil, ""); err != nil {
		t.Fatal(err)
	}
	got, err = s.NextAssignableJobForNode(ctx, gpuNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("gpu node sees mismatched job %d (series group cpu vs node group gpu)", got.ID)
	}

	// cpu-1 (group ""): wildcard node, sees everything — cpuJob remains.
	got, err = s.NextAssignableJobForNode(ctx, cpuNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != cpuJob.ID {
		t.Fatalf("cpu node got %+v, want cpuJob %d", got, cpuJob.ID)
	}

	// The plain NextAssignableJob (no node context) must NOT break: it stays
	// group-agnostic and simply returns the lowest-id pending job.
	got, err = s.NextAssignableJob(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("NextAssignableJob returned nil with pending jobs")
	}
}

// TestSetNodeGroupFieldScoped verifies the setter only touches the group
// column (concurrent enabled/status changes are not clobbered).
func TestSetNodeGroupFieldScoped(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	n, err := s.CreateNode(ctx, "n1", "h1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE nodes SET status='busy' WHERE id=?`, n.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodeGroup(ctx, n.ID, "gpu"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Group != "gpu" {
		t.Fatalf("group = %q, want gpu", got.Group)
	}
	if got.Status != model.NodeBusy {
		t.Fatalf("status clobbered: %s", got.Status)
	}
}
