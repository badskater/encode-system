package store

import (
	"context"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestPruneOldJobs verifies the retention delete: terminal jobs finished
// before the cutoff are deleted, terminal jobs inside the window and ALL
// non-terminal jobs survive (a pending job with no finished_at must never
// be pruned, however old its created_at is).
func TestPruneOldJobs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)

	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	recent := now.Add(-2 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	started := now.Add(-40*24*time.Hour - time.Hour).Format("2006-01-02 15:04:05")

	// Old terminal jobs (done + failed) — prunable at 30d.
	if err := s.SeedFinishedJob(ctx, flow.ID, 0, "done", "mux", started, old); err != nil {
		t.Fatal(err)
	}
	if err := s.SeedFinishedJob(ctx, flow.ID, 0, "failed", "encode", started, old); err != nil {
		t.Fatal(err)
	}
	// Recent terminal job — must survive.
	if err := s.SeedFinishedJob(ctx, flow.ID, 0, "done", "mux", started, recent); err != nil {
		t.Fatal(err)
	}
	// Old-but-pending job (created 40d ago, never finished) — must survive.
	// Created via CreateJob then backdated created_at (SeedFinishedJob is
	// for terminal rows only).
	j, err := s.CreateJob(ctx, &model.Job{
		Series: "S", Episode: "01", EpisodeDir: "S/Ep 99",
		ScriptType: "avs", FlowID: flow.ID, Status: model.JobPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET created_at=? WHERE id=?`, old, j.ID); err != nil {
		t.Fatal(err)
	}

	n, err := s.PruneOldJobs(ctx, 30)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("pruned = %d, want 2 (old done + old failed)", n)
	}

	// Survivors: the recent terminal job and the old pending job.
	jobs, err := s.ListJobs(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("remaining jobs = %d, want 2", len(jobs))
	}
	for _, got := range jobs {
		if got.Status != "done" && got.Status != "pending" {
			t.Errorf("unexpected survivor status %s (job %d)", got.Status, got.ID)
		}
	}
}

// TestPruneOldJobsZeroDaysIsNoop verifies the guard: days <= 0 prunes
// nothing (0 = retention disabled) and reports 0 deleted, so a misconfigured
// settings row can never wipe history.
func TestPruneOldJobsZeroDaysIsNoop(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)

	now := time.Now().UTC()
	old := now.Add(-400 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	started := old
	if err := s.SeedFinishedJob(ctx, flow.ID, 0, "done", "mux", started, old); err != nil {
		t.Fatal(err)
	}
	for _, days := range []int{0, -5} {
		n, err := s.PruneOldJobs(ctx, days)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("days=%d pruned %d, want 0 (retention disabled)", days, n)
		}
	}
	jobs, _ := s.ListJobs(ctx, "", 10)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1 (nothing deleted)", len(jobs))
	}
}
