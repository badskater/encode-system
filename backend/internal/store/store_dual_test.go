package store

// This file dual-runs the highest-value existing store tests against Postgres
// (env-gated) by extracting their logic into shared functions called by both
// the SQLite test and a PG wrapper. The shared functions take a *Store so the
// same assertions run on both backends.
//
// Tests dual-ran:
//   - store_steering_test.go: TestFinishFailureRecordsNode,
//     TestNextAssignableJobForNodeSteersFromFailedNode,
//     TestPriorityBeatsSteering (dispatch semantics — most PG-sensitive)
//   - store_test.go: TestJobLifecycleAndRetry (lifecycle transitions)
//
// These tests use newTestStore(t) (SQLite tmpfile). The PG wrappers below
// open a PG store (env-gated) and call the same shared logic. The PG store
// is TRUNCATEd in cleanup.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// openTestStorePG opens a Postgres store for dual-backend tests. It is
// env-gated: when ENCODE_TEST_PG_DSN is unset, the calling test is skipped.
// The store is TRUNCATEd in t.Cleanup so each test starts from an empty
// database (the schema persists from migratePG; only data is cleared).
func openTestStorePG(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("ENCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("ENCODE_TEST_PG_DSN not set; skipping PG test")
	}
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		tables := []string{
			"node_metrics", "audit_events", "pairing_codes", "provision_runs",
			"sessions", "users", "api_tokens", "shares", "settings",
			"step_templates", "series", "jobs", "flows", "nodes",
		}
		for _, tbl := range tables {
			s.db.ExecContext(ctx, fmt.Sprintf("TRUNCATE TABLE %s RESTART IDENTITY CASCADE", tbl))
		}
		s.Close()
	})
	return s
}

// seedSteerJobPG is the PG variant of seedSteerJob (same logic, different
// store). Kept as a separate function so the original SQLite tests don't
// need any changes.
func seedSteerJobPG(t *testing.T, s *Store, dir string) *model.Job {
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

// TestPGFinishFailureRecordsNode is the Postgres dual of
// TestFinishFailureRecordsNode.
func TestPGFinishFailureRecordsNode(t *testing.T) {
	s := openTestStorePG(t)
	ctx := context.Background()
	n1, err := s.CreateNode(ctx, "n1", "h1")
	if err != nil {
		t.Fatal(err)
	}
	j := seedSteerJobPG(t, s, "S/Ep 01")
	if err := s.AssignJob(ctx, j.ID, n1.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJobWithReport(ctx, j.ID, model.JobFailed, 1, "boom", nil, "tail", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastFailedNodeID != n1.ID {
		t.Errorf("last_failed_node_id = %d, want %d", got.LastFailedNodeID, n1.ID)
	}
	// A done finish clears the steering stamp.
	if _, err := s.RetryJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignJob(ctx, j.ID, n1.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJobWithReport(ctx, j.ID, model.JobDone, 0, "", []string{"o.mkv"}, "tail", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetJob(ctx, j.ID)
	if got.LastFailedNodeID != 0 {
		t.Errorf("after done: last_failed_node_id = %d, want 0", got.LastFailedNodeID)
	}
}

// TestPGNextAssignableJobForNodeSteersFromFailedNode is the Postgres dual of
// TestNextAssignableJobForNodeSteersFromFailedNode.
func TestPGNextAssignableJobForNodeSteersFromFailedNode(t *testing.T) {
	s := openTestStorePG(t)
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
	j1 := seedSteerJobPG(t, s, "S/Ep 01")
	if err := s.AssignJob(ctx, j1.ID, nA.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJobWithReport(ctx, j1.ID, model.JobFailed, 1, "boom", nil, "", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetryJob(ctx, j1.ID); err != nil {
		t.Fatal(err)
	}

	// Node A asks: with no other candidate, the fallback must still return j1.
	got, err := s.NextAssignableJobForNode(ctx, nA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != j1.ID {
		t.Fatalf("fallback: got %v, want job %d", got, j1.ID)
	}

	// Add a fresh job (never failed anywhere). Node A must now get the
	// FRESH job, steering away from j1; node B gets j1.
	j2 := seedSteerJobPG(t, s, "S/Ep 02")
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

// TestPGPriorityBeatsSteering is the Postgres dual of TestPriorityBeatsSteering.
func TestPGPriorityBeatsSteering(t *testing.T) {
	s := openTestStorePG(t)
	ctx := context.Background()
	nA, _ := s.CreateNode(ctx, "nA", "hA")

	// j1: failed on A, re-queued, then bumped to high priority.
	j1 := seedSteerJobPG(t, s, "S/Ep 01")
	if err := s.AssignJob(ctx, j1.ID, nA.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJobWithReport(ctx, j1.ID, model.JobFailed, 1, "boom", nil, "", "", nil, nil); err != nil {
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
	seedSteerJobPG(t, s, "S/Ep 02")

	got, err := s.NextAssignableJobForNode(ctx, nA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != j1.ID {
		t.Fatalf("got %v, want high-priority j1=%d even though it failed on this node", got, j1.ID)
	}
}

// TestPGJobLifecycleAndRetry is the Postgres dual of TestJobLifecycleAndRetry.
func TestPGJobLifecycleAndRetry(t *testing.T) {
	s := openTestStorePG(t)
	ctx := context.Background()
	flow, err := s.CreateFlow(ctx, &model.Flow{
		Name:  "default-1080",
		Steps: []model.Step{{Type: model.StepDGIndex, Params: nil}},
	})
	if err != nil {
		t.Fatalf("create flow: %v", err)
	}

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

// TestPGBackoffRespected is a PG variant of the backoff steering test.
func TestPGBackoffRespected(t *testing.T) {
	s := openTestStorePG(t)
	ctx := context.Background()
	nA, _ := s.CreateNode(ctx, "nA", "hA")
	nB, _ := s.CreateNode(ctx, "nB", "hB")

	j := seedSteerJobPG(t, s, "S/Ep 01")
	if err := s.AssignJob(ctx, j.ID, nA.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJobWithReport(ctx, j.ID, model.JobFailed, 1, "boom", nil, "", "", nil, nil); err != nil {
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
