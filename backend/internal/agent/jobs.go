package agent

import (
	"sort"
	"sync"

	"github.com/badskater/encode-system/backend/internal/model"
)

// maxLocalJobs is the agent's hard cap on simultaneous jobs, regardless of
// what a (buggy or racing) controller assigns. It mirrors the store-side
// clamp so a mis-dispatch can never fork-bomb a node: at most 8 encodes.
const maxLocalJobs = 8

// activeJob is one in-flight job's agent-side state. The payload drives
// executeJob; prog is this job's OWN live step/progress/tail tracker (a
// shared tracker would interleave lines from concurrent encodes and report
// nonsense percentages).
type activeJob struct {
	payload *model.JobPayload
	prog    *progressTracker
	runLog  string // this job's run.log path (FPS parsing picks the newest)
}

// jobRegistry is the concurrency-safe multi-job bookkeeping that replaces
// the single currentJob pointer. Every mutation takes the mutex; snapshots
// are returned as copies so the heartbeat goroutine never holds the lock
// while building the wire report.
type jobRegistry struct {
	mu   sync.Mutex
	jobs map[int64]*activeJob
}

func newJobRegistry() *jobRegistry {
	return &jobRegistry{jobs: map[int64]*activeJob{}}
}

// accept registers payloads up to the free capacity (maxLocalJobs minus
// what is already running). Duplicate ids are ignored — a double dispatch
// must never spawn a second goroutine for the same job. Returns the
// payloads actually accepted so the caller spawns exactly those.
func (r *jobRegistry) accept(payloads []*model.JobPayload) []*model.JobPayload {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ok []*model.JobPayload
	for _, p := range payloads {
		if p == nil || len(r.jobs) >= maxLocalJobs {
			continue
		}
		if _, dup := r.jobs[p.ID]; dup {
			continue
		}
		r.jobs[p.ID] = &activeJob{
			payload: p,
			prog:    newProgressTracker(progressRingLines),
		}
		ok = append(ok, p)
	}
	return ok
}

// remove drops a finished job. Unknown ids are a no-op.
func (r *jobRegistry) remove(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.jobs, id)
}

// count returns the number of in-flight jobs.
func (r *jobRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.jobs)
}

// get returns a job's state (nil when absent). The returned pointer's prog
// is itself mutex-guarded, so the heartbeat may snapshot it safely.
func (r *jobRegistry) get(id int64) *activeJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jobs[id]
}

// reports builds one HeartbeatJobReport per active job, sorted by id so
// wire output is deterministic (tests and log diffs rely on it).
func (r *jobRegistry) reports() []model.HeartbeatJobReport {
	r.mu.Lock()
	ids := make([]int64, 0, len(r.jobs))
	jobs := make([]*activeJob, 0, len(r.jobs))
	for id, j := range r.jobs {
		ids = append(ids, id)
		jobs = append(jobs, j)
	}
	r.mu.Unlock()

	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	byID := map[int64]*activeJob{}
	for i, id := range ids {
		byID[id] = jobs[i]
	}
	out := make([]model.HeartbeatJobReport, 0, len(ids))
	for _, id := range ids {
		j := byID[id]
		step, pct, tail := j.prog.snapshot()
		out = append(out, model.HeartbeatJobReport{
			JobID: id, JobStatus: "running",
			Step: step, StepProgress: pct, LogTail: tail,
		})
	}
	return out
}

// setRunLog records a job's run.log path under the registry lock (the
// heartbeat's newestRunLog reads it under the same lock).
func (r *jobRegistry) setRunLog(id int64, path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if aj, ok := r.jobs[id]; ok {
		aj.runLog = path
	}
}

// newestRunLog returns the run.log path of the highest-id active job —
// the FPS metric parses one live log, and with concurrent encodes the
// newest job is the most useful single sample. "" when idle.
func (r *jobRegistry) newestRunLog() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best int64 = -1
	var path string
	for id, j := range r.jobs {
		if id > best {
			best, path = id, j.runLog
		}
	}
	return path
}
