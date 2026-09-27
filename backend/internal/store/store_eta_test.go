package store

import (
	"context"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// seedFinishedJob creates a done job on the flow with explicit start/finish
// timestamps so durations are exact.
func seedFinishedJob(t *testing.T, s *Store, ctx context.Context, flowID int64, ep string, dur time.Duration) {
	t.Helper()
	// AssignJob validates the node exists (capacity check); ensure node 1.
	if _, err := s.GetNode(ctx, 1); err != nil {
		if _, err := s.CreateNode(ctx, "eta-node", "h"); err != nil {
			t.Fatal(err)
		}
	}
	j, err := s.CreateJob(ctx, &model.Job{
		Series: "ETA Show", Episode: ep, EpisodeDir: "ETA Show/Ep " + ep,
		ScriptType: "avs", FlowID: flowID, Status: model.JobPending})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AssignJob(ctx, j.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJobWithReport(ctx, j.ID, model.JobDone, 0, "", nil, "t", "l", nil, nil); err != nil {
		t.Fatal(err)
	}
	// Backdate to an exact duration (FinishJob stamps 'now').
	start := time.Now().UTC().Add(-dur)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET started_at=?, finished_at=? WHERE id=?`,
		start.Format("2006-01-02 15:04:05"), time.Now().UTC().Format("2006-01-02 15:04:05"), j.ID); err != nil {
		t.Fatal(err)
	}
}

// TestAvgFlowDuration verifies the ETA basis query: only done jobs on the
// given flow count, the average is over their wall-clock durations, and the
// sample count is returned so callers can gate on confidence.
func TestAvgFlowDuration(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	fl, _ := s.CreateFlow(ctx, &model.Flow{Name: "eta-flow"})
	other, _ := s.CreateFlow(ctx, &model.Flow{Name: "other-flow"})

	avg, n, err := s.AvgFlowDuration(ctx, fl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || avg != 0 {
		t.Fatalf("empty flow: avg=%v n=%d, want 0/0", avg, n)
	}

	seedFinishedJob(t, s, ctx, fl.ID, "01", 10*time.Minute)
	seedFinishedJob(t, s, ctx, fl.ID, "02", 20*time.Minute)
	seedFinishedJob(t, s, ctx, other.ID, "03", 60*time.Minute) // different flow: excluded

	avg, n, err = s.AvgFlowDuration(ctx, fl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("sample count = %d, want 2 (other flow excluded)", n)
	}
	if avg < 14.5*60 || avg > 15.5*60 {
		t.Fatalf("avg = %.1fs, want ~900s (15m)", avg)
	}
}

// TestAvgFlowDurationIgnoresFailures verifies failed/cancelled jobs never
// pollute the average (they ran partial durations).
func TestAvgFlowDurationIgnoresFailures(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	fl, _ := s.CreateFlow(ctx, &model.Flow{Name: "eta-flow2"})
	seedFinishedJob(t, s, ctx, fl.ID, "01", 10*time.Minute)

	// A failed job with a bogus long duration.
	j, _ := s.CreateJob(ctx, &model.Job{
		Series: "ETA Show", Episode: "02", EpisodeDir: "ETA Show/Ep 02",
		ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending})
	s.AssignJob(ctx, j.ID, 1)
	s.FinishJob(ctx, j.ID, model.JobFailed, 1, "boom", nil, "")
	s.db.ExecContext(ctx,
		`UPDATE jobs SET started_at=datetime('now','-3 hours'), finished_at=datetime('now') WHERE id=?`, j.ID)

	avg, n, _ := s.AvgFlowDuration(ctx, fl.ID)
	if n != 1 {
		t.Fatalf("failed job counted: n=%d, want 1", n)
	}
	if avg < 9*60 || avg > 11*60 {
		t.Fatalf("avg = %.1fs, want ~600s", avg)
	}
}
