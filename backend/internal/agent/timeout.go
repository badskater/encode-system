package agent

import (
	"fmt"
	"sync"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// This file implements the agent-side job/step timeout enforcement
// (resilience batch). The controller sends limits on the job payload
// (job_timeout_minutes + step_timeouts); the agent runs the job script
// under those deadlines and kills the whole process tree when one expires,
// then reports the failure through the normal completion path so the
// controller's retry/audit/notify machinery applies unchanged.

// jobLimits carries the resolved execution deadlines for one job. Durations
// (not minutes) so tests can exercise the enforcement with second-scale
// limits; limitsFromPayload converts the wire minutes.
type jobLimits struct {
	job   time.Duration            // 0 = no job-level limit
	steps map[string]time.Duration // step marker name → limit (only >0 entries)
}

// limitsFromPayload converts the payload's minute fields into durations.
// Non-positive values mean "no limit" and are dropped; unknown/empty step
// names are ignored defensively (the controller validates at the API).
func limitsFromPayload(p *model.JobPayload) jobLimits {
	l := jobLimits{steps: map[string]time.Duration{}}
	if p == nil {
		return l
	}
	if p.JobTimeoutMinutes > 0 {
		l.job = time.Duration(p.JobTimeoutMinutes) * time.Minute
	}
	for _, st := range p.StepTimeouts {
		if st.TimeoutMinutes > 0 && st.Step != "" {
			l.steps[st.Step] = time.Duration(st.TimeoutMinutes) * time.Minute
		}
	}
	return l
}

// timeoutWatcher tracks the active job/step deadlines and records the first
// expiry reason. Written by the watchdog goroutine and the step-marker hook
// (executeJob goroutine), read after cmd.Wait — every access takes the mutex.
type timeoutWatcher struct {
	mu           sync.Mutex
	jobDeadline  time.Time     // zero = no job limit
	jobLimit     time.Duration // configured job limit (for the message)
	stepName     string
	stepDeadline time.Time     // zero = no active step limit
	stepLimit    time.Duration // configured step limit (for the message)
	reason       string
}

// setJobDeadline arms the job-level deadline (from the job context).
func (w *timeoutWatcher) setJobDeadline(deadline time.Time, limit time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.jobDeadline, w.jobLimit = deadline, limit
}

// startStep arms the deadline for a step that just emitted its first
// ENCODE_STEP marker. The effective deadline is min(now+step limit, job
// deadline) — a step can never outlive the job. The CONFIGURED limit is
// kept for the error message so operators see the value they set.
func (w *timeoutWatcher) startStep(name string, limit time.Duration, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	deadline := now.Add(limit)
	if !w.jobDeadline.IsZero() && deadline.After(w.jobDeadline) {
		deadline = w.jobDeadline
	}
	w.stepName, w.stepLimit, w.stepDeadline = name, limit, deadline
}

// expire checks both deadlines at now and, when one has passed, records and
// returns the failure reason. The job deadline wins when both have passed
// (the job limit is the outer bound). Returns ("", false) while inside both
// limits. The caller kills the process tree when ok is true.
func (w *timeoutWatcher) expire(now time.Time) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.reason != "" {
		return w.reason, true
	}
	if !w.jobDeadline.IsZero() && !now.Before(w.jobDeadline) {
		w.reason = timeoutMessage("job", w.jobLimit)
		return w.reason, true
	}
	if !w.stepDeadline.IsZero() && !now.Before(w.stepDeadline) {
		w.reason = timeoutMessage("step "+w.stepName, w.stepLimit)
		return w.reason, true
	}
	return "", false
}

// takeReason returns the recorded expiry reason ("" when no timeout fired).
func (w *timeoutWatcher) takeReason() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reason
}

// timeoutMessage renders the failure reason: "job timeout after N minutes"
// / "step encode timeout after N minutes" for whole-minute limits (the wire
// unit), falling back to the duration string for sub-minute test limits.
func timeoutMessage(kind string, d time.Duration) string {
	if d > 0 && d%time.Minute == 0 {
		m := int(d / time.Minute)
		unit := "minutes"
		if m == 1 {
			unit = "minute"
		}
		return fmt.Sprintf("%s timeout after %d %s", kind, m, unit)
	}
	return fmt.Sprintf("%s timeout after %s", kind, d)
}

// windowsKillArgs builds the taskkill argument list that kills a process
// AND its children (/T = tree, /F = force). Kept platform-neutral so the
// Windows kill mechanism is unit-testable on any host; kill_windows.go
// executes it.
func windowsKillArgs(pid int) []string {
	return []string{"taskkill", "/T", "/F", "/PID", fmt.Sprintf("%d", pid)}
}
