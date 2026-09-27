package store

import (
	"context"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// seedSteerJob creates one pending job on its own uniquely-named flow and
// returns it. (seedFlow uses a fixed name; these tests seed several jobs per
// store, so each gets its own flow to avoid the UNIQUE(name) collision.)
func seedSteerJob(t *testing.T, s *Store, dir string) *model.Job {
	t.Helper()
	ctx := context.Background()
	fl, err := s.CreateFlow(ctx, &model.Flow{
		Name:  "steer-flow-" + dir,
		Steps: []model.Step{{Type: model.StepEncode}},
	})
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.CreateJob(ctx, &model.Job{
		Series: "S", Episode: "01", EpisodeDir: dir,
		ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// TestFinishFailureRecordsNode verifies a failed finish stamps
// last_failed_node_id with the node that owned the job, so the dispatcher
// can steer the retry elsewhere. A done finish clears it instead (a success
// ends the steering; the job is terminal anyway).
func TestFinishFailureRecordsNode(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	n1, err := s.CreateNode(ctx, "n1", "h1")
	if err != nil {
		t.Fatal(err)
	}
	j := seedSteerJob(t, s, "S/Ep 01")
	if err := s.AssignJob(ctx, j.ID, n1.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJobWithReport(ctx, j.ID, model.JobFailed, 1, "boom", nil, "tail", "", nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastFailedNodeID != n1.ID {
		t.Errorf("last_failed_node_id = %d, want %d", got.LastFailedNodeID, n1.ID)
	}

	// A done finish on a retried job clears the steering stamp.
	if _, err := s.RetryJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignJob(ctx, j.ID, n1.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJobWithReport(ctx, j.ID, model.JobDone, 0, "", []string{"o.mkv"}, "tail", "", nil); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetJob(ctx, j.ID)
	if got.LastFailedNodeID != 0 {
		t.Errorf("after done: last_failed_node_id = %d, want 0", got.LastFailedNodeID)
	}
}

// TestFinishJobOrphanRecordsNode verifies the v1 FinishJob path (orphan
// recovery) also stamps last_failed_node_id — the controller-detected
// failure must steer exactly like an agent-reported one.
func TestFinishJobOrphanRecordsNode(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	n1, err := s.CreateNode(ctx, "n1", "h1")
	if err != nil {
		t.Fatal(err)
	}
	j := seedSteerJob(t, s, "S/Ep 01")
	if err := s.AssignJob(ctx, j.ID, n1.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJob(ctx, j.ID, model.JobFailed, -1, "orphaned", nil, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetJob(ctx, j.ID)
	if got.LastFailedNodeID != n1.ID {
		t.Errorf("orphan: last_failed_node_id = %d, want %d", got.LastFailedNodeID, n1.ID)
	}
}

// TestNextAssignableJobForNodeSteersFromFailedNode verifies dispatch
// preference: a job that failed on node A is NOT handed to node A while any
// other assignable job exists, but node B gets it first when it is the
// top-priority candidate for B.
func TestNextAssignableJobForNodeSteersFromFailedNode(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	nA, err := s.CreateNode(ctx, "nA", "hA")
	if err != nil {
		t.Fatal(err)
	}
	nB, err := s.CreateNode(ctx, "nB", "hB")
	if err != nil {
		t.Fatal(err)
	}

	// Job 1 fails on A, then is re-queued (pending, stamped last_failed=A).
	j1 := seedSteerJob(t, s, "S/Ep 01")
	if err := s.AssignJob(ctx, j1.ID, nA.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJobWithReport(ctx, j1.ID, model.JobFailed, 1, "boom", nil, "", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetryJob(ctx, j1.ID); err != nil {
		t.Fatal(err)
	}

	// Node A asks: with no other candidate, the fallback must still return
	// j1 (a single-job queue must not starve on a single-node farm).
	got, err := s.NextAssignableJobForNode(ctx, nA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != j1.ID {
		t.Fatalf("fallback: got %v, want job %d", got, j1.ID)
	}

	// Add a fresh job (never failed anywhere). Node A must now get the
	// FRESH job, steering away from j1; node B gets j1 (oldest by FIFO,
	// and B has no failure history with it).
	j2 := seedSteerJob(t, s, "S/Ep 02")
	gotA, err := s.NextAssignableJobForNode(ctx, nA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotA == nil || gotA.ID != j2.ID {
		t.Fatalf("node A: got %v, want fresh job %d (steered away from %d)", gotA, j2.ID, j1.ID)
	}
	gotB, err := s.NextAssignableJobForNode(ctx, nB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotB == nil || gotB.ID != j1.ID {
		t.Fatalf("node B: got %v, want %d (FIFO, no history)", gotB, j1.ID)
	}
}

// TestNextAssignableJobForNodeRespectsBackoff verifies steering does not
// bypass the next_retry_at gate: a backoff-pending job is invisible to
// every node until the gate elapses.
func TestNextAssignableJobForNodeRespectsBackoff(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	nA, _ := s.CreateNode(ctx, "nA", "hA")
	nB, _ := s.CreateNode(ctx, "nB", "hB")

	j := seedSteerJob(t, s, "S/Ep 01")
	if err := s.AssignJob(ctx, j.ID, nA.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJobWithReport(ctx, j.ID, model.JobFailed, 1, "boom", nil, "", "", nil); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := s.ScheduleJobRetry(ctx, j.ID, 1, future); err != nil {
		t.Fatal(err)
	}
	for _, n := range []*model.Node{nA, nB} {
		got, err := s.NextAssignableJobForNode(ctx, n.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Errorf("node %s: gated job was dispatched (id %d)", n.Name, got.ID)
		}
	}
}

// TestPriorityBeatsSteering verifies the ordering contract: a high-priority
// job that failed on this node outranks a normal-priority fresh job.
// Steering is a tie-break WITHIN the priority ordering, not above it.
func TestPriorityBeatsSteering(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	nA, _ := s.CreateNode(ctx, "nA", "hA")

	// j1: failed on A, re-queued, then bumped to high priority.
	j1 := seedSteerJob(t, s, "S/Ep 01")
	if err := s.AssignJob(ctx, j1.ID, nA.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJobWithReport(ctx, j1.ID, model.JobFailed, 1, "boom", nil, "", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetryJob(ctx, j1.ID); err != nil {
		t.Fatal(err)
	}
	hi := 1
	if ok, err := s.PatchPendingJob(ctx, j1.ID, nil, &hi); err != nil || !ok {
		t.Fatalf("patch priority: ok=%v err=%v", ok, err)
	}
	// j2: fresh, normal priority.
	seedSteerJob(t, s, "S/Ep 02")

	got, err := s.NextAssignableJobForNode(ctx, nA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != j1.ID {
		t.Fatalf("got %v, want high-priority j1=%d even though it failed on this node", got, j1.ID)
	}
}
