package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// seedStatsJob inserts a job row with explicit started_at/finished_at so the
// stats aggregates can assert exact durations. The normal CreateJob path
// stamps finished_at=datetime('now') on FinishJob, which is uncontrolled for
// tests; SeedFinishedJob lets us backdate and set precise spans.
func seedStatsJob(t *testing.T, s *Store, flowID, nodeID int64, status, step, started, finished string) {
	t.Helper()
	ctx := context.Background()
	if err := s.SeedFinishedJob(ctx, flowID, nodeID, status, step, started, finished); err != nil {
		t.Fatalf("seed job: %v", err)
	}
}

// TestJobStatsEmptyDBReturnsZeroedShape confirms a fresh install with no jobs
// returns a zeroed Stats (not an error), so the Stats page renders cleanly.
func TestJobStatsEmptyDBReturnsZeroedShape(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	got, err := s.JobStats(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("empty stats: %v", err)
	}
	if got == nil {
		t.Fatal("want non-nil stats")
	}
	if got.Totals.Done != 0 || got.Totals.Failed != 0 || got.Totals.Cancelled != 0 {
		t.Fatalf("totals should be zeroed: %+v", got.Totals)
	}
	if got.Totals.AvgDurationSec != 0 {
		t.Fatalf("avg duration should be 0 on empty, got %v", got.Totals.AvgDurationSec)
	}
	if len(got.PerNode) != 0 || len(got.PerFlow) != 0 || len(got.FailuresByStep) != 0 || len(got.PerDay) != 0 {
		t.Fatalf("slices should be empty on empty DB: per_node=%d per_flow=%d failures=%d per_day=%d",
			len(got.PerNode), len(got.PerFlow), len(got.FailuresByStep), len(got.PerDay))
	}
	if got.RangeDays != 7 {
		t.Fatalf("range_days: want 7, got %d", got.RangeDays)
	}
}

// TestJobStatsCountsAndDurations seeds 2 nodes + 2 flows with known done/
// failed/cancelled jobs at known durations and asserts the exact counts, the
// avg duration within epsilon, and the per-node/per-flow breakdowns.
func TestJobStatsCountsAndDurations(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow1, _ := s.CreateFlow(ctx, &model.Flow{Name: "f1", Steps: []model.Step{{Type: "encode"}}})
	flow2, _ := s.CreateFlow(ctx, &model.Flow{Name: "f2", Steps: []model.Step{{Type: "encode"}}})
	node1, _ := s.CreateNode(ctx, "enc-01", "h1")
	node2, _ := s.CreateNode(ctx, "enc-02", "h2")

	// All jobs finished ~1 hour ago so they're well within the 7d range.
	// Durations: done jobs = 120s, so avg over two = 120. A failed job
	// has a 60s span; a cancelled job has no started_at (null) so it's
	// excluded from the AVG (it never started).
	fin := time.Now().UTC().Add(-1 * time.Hour)
	started := fin.Add(-120 * time.Second)
	finStr := fin.Format("2006-01-02 15:04:05")
	startStr := started.Format("2006-01-02 15:04:05")
	failedFin := fin.Add(-60 * time.Second)
	failedStart := failedFin.Add(-60 * time.Second)
	failedFinStr := failedFin.Format("2006-01-02 15:04:05")
	failedStartStr := failedStart.Format("2006-01-02 15:04:05")

	// node1/flow1: 2 done (120s each) + 1 failed (60s)
	seedStatsJob(t, s, flow1.ID, node1.ID, "done", "encode", startStr, finStr)
	seedStatsJob(t, s, flow1.ID, node1.ID, "done", "encode", startStr, finStr)
	seedStatsJob(t, s, flow1.ID, node1.ID, "failed", "encode", failedStartStr, failedFinStr)
	// node2/flow2: 1 done (120s) + 1 cancelled (null started_at)
	seedStatsJob(t, s, flow2.ID, node2.ID, "done", "mux", startStr, finStr)
	seedStatsJob(t, s, flow2.ID, node2.ID, "cancelled", "", "", finStr)

	got, err := s.JobStats(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	// Totals: 3 done, 1 failed, 1 cancelled. Avg duration over the 3 done
	// + 1 failed that have both timestamps = (120+120+120+60)/4 = 105s.
	// The cancelled job has null started_at → excluded from AVG.
	if got.Totals.Done != 3 || got.Totals.Failed != 1 || got.Totals.Cancelled != 1 {
		t.Fatalf("totals: want done=3 failed=1 cancelled=1, got %+v", got.Totals)
	}
	wantAvg := 105.0
	if got.Totals.AvgDurationSec < wantAvg-1 || got.Totals.AvgDurationSec > wantAvg+1 {
		t.Fatalf("avg duration: want ~%.0f, got %v", wantAvg, got.Totals.AvgDurationSec)
	}

	// Per-node: node1 = 2 done / 1 failed, node2 = 1 done / 0 failed.
	if len(got.PerNode) != 2 {
		t.Fatalf("per_node: want 2 rows, got %d", len(got.PerNode))
	}
	byNode := map[int64]model.StatsNodeRow{}
	for _, r := range got.PerNode {
		byNode[r.NodeID] = r
	}
	n1 := byNode[node1.ID]
	if n1.Name != "enc-01" || n1.Done != 2 || n1.Failed != 1 {
		t.Fatalf("node1 row: %+v", n1)
	}
	// node1 avg over 2 done(120) + 1 failed(60) = 100s.
	if n1.AvgDurationSec < 99 || n1.AvgDurationSec > 101 {
		t.Fatalf("node1 avg: want ~100, got %v", n1.AvgDurationSec)
	}
	n2 := byNode[node2.ID]
	if n2.Name != "enc-02" || n2.Done != 1 || n2.Failed != 0 {
		t.Fatalf("node2 row: %+v", n2)
	}
	// node2 avg over 1 done(120) — cancelled excluded (null started).
	if n2.AvgDurationSec < 119 || n2.AvgDurationSec > 121 {
		t.Fatalf("node2 avg: want ~120, got %v", n2.AvgDurationSec)
	}

	// Per-flow: flow1 = 2 done / 1 failed, flow2 = 1 done / 0 failed.
	if len(got.PerFlow) != 2 {
		t.Fatalf("per_flow: want 2 rows, got %d", len(got.PerFlow))
	}
	byFlow := map[int64]model.StatsFlowRow{}
	for _, r := range got.PerFlow {
		byFlow[r.FlowID] = r
	}
	f1 := byFlow[flow1.ID]
	if f1.Name != "f1" || f1.Done != 2 || f1.Failed != 1 {
		t.Fatalf("flow1 row: %+v", f1)
	}
	f2 := byFlow[flow2.ID]
	if f2.Name != "f2" || f2.Done != 1 || f2.Failed != 0 {
		t.Fatalf("flow2 row: %+v", f2)
	}
}

// TestJobStatsFailuresByStep asserts the failures-by-step attribution: the
// step column names where the job died, grouped + ordered by count DESC.
func TestJobStatsFailuresByStep(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow, _ := s.CreateFlow(ctx, &model.Flow{Name: "f", Steps: []model.Step{{Type: "encode"}}})
	node, _ := s.CreateNode(ctx, "enc-01", "h")

	fin := time.Now().UTC().Add(-1 * time.Hour)
	start := fin.Add(-60 * time.Second)
	finStr := fin.Format("2006-01-02 15:04:05")
	startStr := start.Format("2006-01-02 15:04:05")

	// 3 failures at "encode", 1 at "mux".
	for i := 0; i < 3; i++ {
		seedStatsJob(t, s, flow.ID, node.ID, "failed", "encode", startStr, finStr)
	}
	seedStatsJob(t, s, flow.ID, node.ID, "failed", "mux", startStr, finStr)

	got, err := s.JobStats(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(got.FailuresByStep) != 2 {
		t.Fatalf("failures_by_step: want 2 rows, got %d", len(got.FailuresByStep))
	}
	// Ordered by count DESC: encode(3) first, then mux(1).
	if got.FailuresByStep[0].Step != "encode" || got.FailuresByStep[0].Count != 3 {
		t.Fatalf("top failure: %+v", got.FailuresByStep[0])
	}
	if got.FailuresByStep[1].Step != "mux" || got.FailuresByStep[1].Count != 1 {
		t.Fatalf("second failure: %+v", got.FailuresByStep[1])
	}
}

// TestJobStatsPerDayBuckets asserts the per-day throughput trend: done jobs
// are bucketed by date(finished_at) and ordered ascending.
func TestJobStatsPerDayBuckets(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow, _ := s.CreateFlow(ctx, &model.Flow{Name: "f", Steps: []model.Step{{Type: "encode"}}})
	node, _ := s.CreateNode(ctx, "enc-01", "h")

	// Two done jobs finishing "today" (now), one done job finishing 2 days
	// ago. The range is 7d so both days are in range.
	now := time.Now().UTC()
	todayStr := now.Format("2006-01-02")
	twoDaysAgo := now.Add(-48 * time.Hour)
	twoDaysAgoDate := twoDaysAgo.Format("2006-01-02")
	// started 60s before finished.
	seedStatsJob(t, s, flow.ID, node.ID, "done", "encode",
		now.Add(-60*time.Second).Format("2006-01-02 15:04:05"), now.Format("2006-01-02 15:04:05"))
	seedStatsJob(t, s, flow.ID, node.ID, "done", "encode",
		now.Add(-60*time.Second).Format("2006-01-02 15:04:05"), now.Format("2006-01-02 15:04:05"))
	seedStatsJob(t, s, flow.ID, node.ID, "done", "encode",
		twoDaysAgo.Add(-60*time.Second).Format("2006-01-02 15:04:05"), twoDaysAgo.Format("2006-01-02 15:04:05"))

	got, err := s.JobStats(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(got.PerDay) != 2 {
		t.Fatalf("per_day: want 2 buckets, got %d (%+v)", len(got.PerDay), got.PerDay)
	}
	// Ascending: twoDaysAgo first (count 1), then today (count 2).
	if got.PerDay[0].Date != twoDaysAgoDate || got.PerDay[0].Count != 1 {
		t.Fatalf("first day: %+v", got.PerDay[0])
	}
	if got.PerDay[1].Date != todayStr || got.PerDay[1].Count != 2 {
		t.Fatalf("second day: %+v", got.PerDay[1])
	}
}

// TestJobStatsRangeBoundaryExcludesOldJobs asserts the range window excludes
// jobs that finished before the cutoff. A job finished 8 days ago must NOT
// appear in a 7d-range query (it would in "all").
func TestJobStatsRangeBoundaryExcludesOldJobs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow, _ := s.CreateFlow(ctx, &model.Flow{Name: "f", Steps: []model.Step{{Type: "encode"}}})
	node, _ := s.CreateNode(ctx, "enc-01", "h")

	// A done job finished 8 days ago (outside 7d) and one finished 1 hour ago.
	old := time.Now().UTC().Add(-8 * 24 * time.Hour)
	seedStatsJob(t, s, flow.ID, node.ID, "done", "encode",
		old.Add(-60*time.Second).Format("2006-01-02 15:04:05"), old.Format("2006-01-02 15:04:05"))
	recent := time.Now().UTC().Add(-1 * time.Hour)
	seedStatsJob(t, s, flow.ID, node.ID, "done", "encode",
		recent.Add(-60*time.Second).Format("2006-01-02 15:04:05"), recent.Format("2006-01-02 15:04:05"))

	// 7d range: only the recent job counts.
	got, err := s.JobStats(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("stats 7d: %v", err)
	}
	if got.Totals.Done != 1 {
		t.Fatalf("7d range: want 1 done, got %d", got.Totals.Done)
	}
	if len(got.PerDay) != 1 {
		t.Fatalf("7d range: want 1 day bucket, got %d", len(got.PerDay))
	}

	// "all" range: both jobs count.
	gotAll, err := s.JobStats(ctx, 0)
	if err != nil {
		t.Fatalf("stats all: %v", err)
	}
	if gotAll.Totals.Done != 2 {
		t.Fatalf("all range: want 2 done, got %d", gotAll.Totals.Done)
	}
	if gotAll.RangeDays != 0 {
		t.Fatalf("all range_days: want 0, got %d", gotAll.RangeDays)
	}
}

// TestJobStatsFailuresByStepCapAt10 seeds 11 distinct failing steps and
// asserts the result is capped at 10 rows (LIMIT 10).
func TestJobStatsFailuresByStepCapAt10(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow, _ := s.CreateFlow(ctx, &model.Flow{Name: "f", Steps: []model.Step{{Type: "encode"}}})
	node, _ := s.CreateNode(ctx, "enc-01", "h")
	fin := time.Now().UTC().Add(-1 * time.Hour).Format("2006-01-02 15:04:05")
	start := time.Now().UTC().Add(-1*time.Hour - 60*time.Second).Format("2006-01-02 15:04:05")
	for i := 0; i < 11; i++ {
		seedStatsJob(t, s, flow.ID, node.ID, "failed",
			"step-"+string(rune('a'+i)), start, fin)
	}
	got, err := s.JobStats(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(got.FailuresByStep) != 10 {
		t.Fatalf("failures_by_step cap: want 10, got %d", len(got.FailuresByStep))
	}
}

// seedStatsJobMetrics inserts a terminal job with an explicit metrics_json
// payload plus started/finished timestamps, so speedup-ratio tests can
// assert exact media-duration/wall-time divisions.
func seedStatsJobMetrics(t *testing.T, s *Store, flowID, nodeID int64, status, metricsJSON, started, finished string) {
	t.Helper()
	ctx := context.Background()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (series, episode, episode_dir, script_type, flow_id, status, node_id, metrics_json, started_at, finished_at)
		 VALUES ('S', '01', 'S/Ep 01', 'vpy', ?, ?, ?, ?, ?, ?)`,
		flowID, status, nodeID, metricsJSON, started, finished)
	if err != nil {
		t.Fatalf("seed metrics job: %v", err)
	}
}

// TestJobStatsSpeedupRatio asserts the encode-speedup aggregate:
// media duration_sec / wall-clock encode seconds, averaged over done jobs
// that carry the metric. Non-qualifying rows (failed jobs, missing
// duration_sec, zero wall time) must be excluded from the average — not
// counted as zero — and a fleet with no qualifying jobs reports 0.
func TestJobStatsSpeedupRatio(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow, _ := s.CreateFlow(ctx, &model.Flow{Name: "f1", Steps: []model.Step{{Type: "encode"}}})
	node, _ := s.CreateNode(ctx, "enc-01", "h1")

	fin := time.Now().UTC().Add(-1 * time.Hour)
	finStr := fin.Format("2006-01-02 15:04:05")
	// 2x: 30 min media encoded in 15 min wall = 2.0x speedup each.
	start2x := fin.Add(-15 * time.Minute).Format("2006-01-02 15:04:05")
	// 1x: 10 min media in 10 min wall = 1.0x.
	start1x := fin.Add(-10 * time.Minute).Format("2006-01-02 15:04:05")
	// 4x: 20 min media in 5 min wall = 4.0x.
	start4x := fin.Add(-5 * time.Minute).Format("2006-01-02 15:04:05")

	// Qualifying: two 2.0x jobs and one 1.0x job → avg (2+2+1)/3 = 1.666…
	seedStatsJobMetrics(t, s, flow.ID, node.ID, "done", `{"duration_sec":1800}`, start2x, finStr)
	seedStatsJobMetrics(t, s, flow.ID, node.ID, "done", `{"duration_sec":1800}`, start2x, finStr)
	seedStatsJobMetrics(t, s, flow.ID, node.ID, "done", `{"duration_sec":600}`, start1x, finStr)
	// Non-qualifying: failed job with a duration metric (excluded — speedup
	// only measures completed encodes).
	seedStatsJobMetrics(t, s, flow.ID, node.ID, "failed", `{"duration_sec":9999}`, start4x, finStr)
	// Non-qualifying: done job with NO duration_sec (old agents/scripts).
	seedStatsJobMetrics(t, s, flow.ID, node.ID, "done", `{}`, start4x, finStr)
	// Non-qualifying: done job with duration_sec=0.
	seedStatsJobMetrics(t, s, flow.ID, node.ID, "done", `{"duration_sec":0}`, start4x, finStr)

	got, err := s.JobStats(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	want := 5.0 / 3.0 // 1.666…
	if got.Totals.AvgSpeedup < want-0.01 || got.Totals.AvgSpeedup > want+0.01 {
		t.Fatalf("totals avg_speedup: want ~%.3f, got %v", want, got.Totals.AvgSpeedup)
	}
	if len(got.PerNode) != 1 {
		t.Fatalf("per_node: want 1 row, got %d", len(got.PerNode))
	}
	if n := got.PerNode[0]; n.AvgSpeedup < want-0.01 || n.AvgSpeedup > want+0.01 {
		t.Fatalf("per_node avg_speedup: want ~%.3f, got %v", want, n.AvgSpeedup)
	}
	if len(got.PerFlow) != 1 {
		t.Fatalf("per_flow: want 1 row, got %d", len(got.PerFlow))
	}
	if f := got.PerFlow[0]; f.AvgSpeedup < want-0.01 || f.AvgSpeedup > want+0.01 {
		t.Fatalf("per_flow avg_speedup: want ~%.3f, got %v", want, f.AvgSpeedup)
	}
}

// TestJobStatsSpeedupNoQualifyingJobsIsZero asserts the empty-data contract:
// done jobs without duration_sec metrics average to 0 (rendered as
// "no data" in the UI), never NULL/NaN.
func TestJobStatsSpeedupNoQualifyingJobsIsZero(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow, _ := s.CreateFlow(ctx, &model.Flow{Name: "f1", Steps: []model.Step{{Type: "encode"}}})
	node, _ := s.CreateNode(ctx, "enc-01", "h1")

	fin := time.Now().UTC().Add(-1 * time.Hour)
	finStr := fin.Format("2006-01-02 15:04:05")
	start := fin.Add(-10 * time.Minute).Format("2006-01-02 15:04:05")
	seedStatsJobMetrics(t, s, flow.ID, node.ID, "done", `{}`, start, finStr)

	got, err := s.JobStats(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if got.Totals.AvgSpeedup != 0 {
		t.Fatalf("totals avg_speedup: want 0, got %v", got.Totals.AvgSpeedup)
	}
	if got.PerNode[0].AvgSpeedup != 0 || got.PerFlow[0].AvgSpeedup != 0 {
		t.Fatalf("row speedups: want 0, got node=%v flow=%v",
			got.PerNode[0].AvgSpeedup, got.PerFlow[0].AvgSpeedup)
	}
}

// TestJobStatsSpeedupRangeBoundaryExcludesOldJobs asserts the speedup
// average respects the same finished_at range window as every other
// aggregate: an ancient 100x job must not skew the 7d figure.
func TestJobStatsSpeedupRangeBoundaryExcludesOldJobs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow, _ := s.CreateFlow(ctx, &model.Flow{Name: "f1", Steps: []model.Step{{Type: "encode"}}})
	node, _ := s.CreateNode(ctx, "enc-01", "h1")

	now := time.Now().UTC()
	// In-range: 10 min media / 10 min wall = 1.0x.
	fin := now.Add(-1 * time.Hour)
	seedStatsJobMetrics(t, s, flow.ID, node.ID, "done", `{"duration_sec":600}`,
		fin.Add(-10*time.Minute).Format("2006-01-02 15:04:05"), fin.Format("2006-01-02 15:04:05"))
	// Out-of-range (30 days old): 100 min media / 1 min wall = 100x.
	old := now.Add(-30 * 24 * time.Hour)
	seedStatsJobMetrics(t, s, flow.ID, node.ID, "done", `{"duration_sec":6000}`,
		old.Add(-1*time.Minute).Format("2006-01-02 15:04:05"), old.Format("2006-01-02 15:04:05"))

	got, err := s.JobStats(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if got.Totals.AvgSpeedup < 0.99 || got.Totals.AvgSpeedup > 1.01 {
		t.Fatalf("avg_speedup: want ~1.0 (old job excluded), got %v", got.Totals.AvgSpeedup)
	}
}

// seedStatsRetryJob inserts a failed job with an explicit retry_count and
// error text, for the stuck-episode (repeat_failures) triage tests.
func seedStatsRetryJob(t *testing.T, s *Store, flowID, nodeID int64, retryCount int, errMsg, started, finished string) {
	t.Helper()
	ctx := context.Background()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (series, episode, episode_dir, script_type, flow_id, status, node_id, retry_count, error, step, started_at, finished_at)
		 VALUES ('S', ?, 'S/Ep 01', 'vpy', ?, 'failed', ?, ?, ?, 'encode', ?, ?)`,
		fmt.Sprintf("%02d", retryCount), flowID, nodeID, retryCount, errMsg, started, finished)
	if err != nil {
		t.Fatalf("seed retry job: %v", err)
	}
}

// TestJobStatsRepeatFailures asserts the stuck-episode triage list: failed
// jobs with retry_count >= 1 appear with attempts = retry_count+1, ordered
// worst-first; a first-try failure (retry_count=0) never appears.
func TestJobStatsRepeatFailures(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow, _ := s.CreateFlow(ctx, &model.Flow{Name: "f1", Steps: []model.Step{{Type: "encode"}}})
	node, _ := s.CreateNode(ctx, "enc-01", "h1")

	fin := time.Now().UTC().Add(-1 * time.Hour)
	finStr := fin.Format("2006-01-02 15:04:05")
	start := fin.Add(-10 * time.Minute).Format("2006-01-02 15:04:05")

	// One 3-retry failure, one 1-retry failure, one first-try failure.
	seedStatsRetryJob(t, s, flow.ID, node.ID, 3, "boom x3", start, finStr)
	seedStatsRetryJob(t, s, flow.ID, node.ID, 1, "boom x1", start, finStr)
	seedStatsRetryJob(t, s, flow.ID, node.ID, 0, "first try", start, finStr)

	got, err := s.JobStats(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(got.RepeatFailures) != 2 {
		t.Fatalf("repeat_failures: want 2 rows, got %d (%+v)", len(got.RepeatFailures), got.RepeatFailures)
	}
	worst := got.RepeatFailures[0]
	if worst.Attempts != 4 || worst.Error != "boom x3" {
		t.Fatalf("worst row: want attempts=4 error=boom x3, got %+v", worst)
	}
	if worst.NodeName != "enc-01" {
		t.Fatalf("node name not joined: %+v", worst)
	}
	if got.RepeatFailures[1].Attempts != 2 {
		t.Fatalf("second row: want attempts=2, got %+v", got.RepeatFailures[1])
	}
}

// TestJobStatsRepeatFailuresEmptyShape asserts a clean fleet marshals the
// section as [] (never null) so the frontend needs no null guard.
func TestJobStatsRepeatFailuresEmptyShape(t *testing.T) {
	s := newTestStore(t)
	got, err := s.JobStats(context.Background(), 7*24*time.Hour)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if got.RepeatFailures == nil {
		t.Fatal("repeat_failures must be non-nil empty slice")
	}
}

// TestJobStatsRepeatFailuresRangeExcluded asserts an ancient retried failure
// outside the window is not listed.
func TestJobStatsRepeatFailuresRangeExcluded(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow, _ := s.CreateFlow(ctx, &model.Flow{Name: "f1", Steps: []model.Step{{Type: "encode"}}})
	node, _ := s.CreateNode(ctx, "enc-01", "h1")

	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	seedStatsRetryJob(t, s, flow.ID, node.ID, 5, "ancient",
		old.Add(-10*time.Minute).Format("2006-01-02 15:04:05"), old.Format("2006-01-02 15:04:05"))

	got, err := s.JobStats(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(got.RepeatFailures) != 0 {
		t.Fatalf("want 0 rows in range, got %+v", got.RepeatFailures)
	}
	// "all" must see it.
	all, err := s.JobStats(ctx, 0)
	if err != nil {
		t.Fatalf("stats all: %v", err)
	}
	if len(all.RepeatFailures) != 1 {
		t.Fatalf("want 1 row in all-time, got %d", len(all.RepeatFailures))
	}
}
