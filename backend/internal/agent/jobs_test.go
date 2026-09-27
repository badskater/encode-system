package agent

import (
	"sort"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestHeartbeatJobReportsNormalization pins the controller-side helper:
// the Jobs array wins when present; the legacy single-job fields are used
// for old agents; an idle heartbeat reports nothing.
func TestHeartbeatJobReportsNormalization(t *testing.T) {
	// Legacy single-job agent.
	old := model.Heartbeat{JobID: 7, JobStatus: "running", Step: "encode", StepProgress: 42, LogTail: "x"}
	reps := (&old).JobReports()
	if len(reps) != 1 || reps[0].JobID != 7 || reps[0].Step != "encode" {
		t.Fatalf("legacy normalization = %+v", reps)
	}

	// New multi-job agent: Jobs wins even if legacy fields also set.
	nw := model.Heartbeat{
		JobID: 7, // legacy mirror of the first job
		Jobs: []model.HeartbeatJobReport{
			{JobID: 7, JobStatus: "running", Step: "encode", StepProgress: 42},
			{JobID: 9, JobStatus: "running", Step: "mux", StepProgress: 80},
		},
	}
	reps = (&nw).JobReports()
	if len(reps) != 2 || reps[1].JobID != 9 {
		t.Fatalf("multi normalization = %+v", reps)
	}

	// Idle.
	empty := model.Heartbeat{}
	if reps := (&empty).JobReports(); len(reps) != 0 {
		t.Fatalf("idle normalization = %+v", reps)
	}
}

// TestJobRegistry verifies the agent-side multi-job bookkeeping: register
// up to the hard cap, reject duplicates/overflow, remove on completion,
// and report per-job snapshots sorted by id with legacy mirroring.
func TestJobRegistry(t *testing.T) {
	a := &Agent{Log: testLog()}

	p1 := &model.JobPayload{ID: 1, Script: "s1"}
	p2 := &model.JobPayload{ID: 2, Script: "s2"}

	if !a.acceptJobs([]*model.JobPayload{p1, p2}) {
		t.Fatal("acceptJobs returned false for capacity 2")
	}
	if a.activeCount() != 2 {
		t.Fatalf("active = %d, want 2", a.activeCount())
	}
	if a.busy() != true {
		t.Fatal("busy() false with active jobs")
	}

	// Duplicate id is ignored (no second goroutine for the same job).
	if a.acceptJobs([]*model.JobPayload{p1}) {
		t.Fatal("duplicate accepted")
	}

	// Snapshot: two reports, sorted by id; progress tracker per job.
	a.jobProg(1).observe("ENCODE_STEP encode 55.0")
	a.jobProg(2).observe("ENCODE_STEP mux 90.0")
	reps := a.jobReports()
	if len(reps) != 2 {
		t.Fatalf("reports = %+v", reps)
	}
	sort.Slice(reps, func(i, j int) bool { return reps[i].JobID < reps[j].JobID })
	if reps[0].JobID != 1 || reps[0].Step != "encode" || reps[0].StepProgress != 55 {
		t.Fatalf("report[0] = %+v", reps[0])
	}
	if reps[1].JobID != 2 || reps[1].Step != "mux" {
		t.Fatalf("report[1] = %+v", reps[1])
	}

	// Finish job 1: slot frees, busy stays (job 2 running).
	a.finishJob(1)
	if a.activeCount() != 1 || !a.busy() {
		t.Fatalf("after finish: active=%d busy=%v", a.activeCount(), a.busy())
	}
	a.finishJob(2)
	if a.activeCount() != 0 || a.busy() {
		t.Fatal("registry not empty after finishing all jobs")
	}
	// Finishing an unknown job is a no-op, not a panic.
	a.finishJob(99)
}

// TestJobRegistryHardCap verifies the agent refuses beyond its local hard
// cap even if a buggy controller over-assigns (defense in depth).
func TestJobRegistryHardCap(t *testing.T) {
	a := &Agent{Log: testLog()}
	var payloads []*model.JobPayload
	for i := int64(1); i <= maxLocalJobs+2; i++ {
		payloads = append(payloads, &model.JobPayload{ID: i})
	}
	a.acceptJobs(payloads)
	if a.activeCount() != maxLocalJobs {
		t.Fatalf("active = %d, want hard cap %d", a.activeCount(), maxLocalJobs)
	}
}
