package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// Cross-platform unit tests for the timeout machinery. The real kill-path
// tests live in timeout_linux_test.go (process-group SIGKILL); the Windows
// mechanism is pinned here via its argument contract and compile-gated by
// the GOOS=windows build.

func TestLimitsFromPayload(t *testing.T) {
	l := limitsFromPayload(&model.JobPayload{
		JobTimeoutMinutes: 90,
		StepTimeouts: []model.StepTimeout{
			{Index: 0, Step: "encode", TimeoutMinutes: 45},
			{Index: 1, Step: "mux", TimeoutMinutes: 0}, // 0 = absent, dropped
			{Index: 2, Step: "", TimeoutMinutes: 5},    // empty name, dropped
		},
	})
	if l.job != 90*time.Minute {
		t.Fatalf("job limit = %s, want 90m", l.job)
	}
	if len(l.steps) != 1 || l.steps["encode"] != 45*time.Minute {
		t.Fatalf("step limits = %v, want only encode=45m", l.steps)
	}
	// Zero payload → no limits (old controllers / flows without timeouts).
	empty := limitsFromPayload(&model.JobPayload{})
	if empty.job != 0 || len(empty.steps) != 0 {
		t.Fatalf("empty payload must yield no limits: %+v", empty)
	}
}

// TestStartStepTransitionDisarmsPreviousDeadline pins the watcher contract
// behind the stale-deadline regression: every first-marker transition
// clears the previous step's deadline; a limit of 0 (unlimited step) leaves
// NO step deadline, and a later limited step arms a fresh one.
func TestStartStepTransitionDisarmsPreviousDeadline(t *testing.T) {
	w := &timeoutWatcher{}
	base := time.Now()
	w.startStep("encode", time.Second, base)
	// Next step has no limit: encode's deadline must be disarmed.
	w.startStep("mux", 0, base.Add(300*time.Millisecond))
	if reason, ok := w.expire(base.Add(5 * time.Second)); ok {
		t.Fatalf("unlimited step killed by a stale deadline: %q", reason)
	}
	// A later step WITH a limit arms a fresh deadline under its own name.
	w.startStep("verify", time.Second, base.Add(5*time.Second))
	if reason, ok := w.expire(base.Add(6 * time.Second)); !ok || reason != "step verify timeout after 1s" {
		t.Fatalf("fresh step deadline not armed: ok=%v reason=%q", ok, reason)
	}
}

// TestLimitsFromPayloadDuplicateStepLastEntryWins pins the duplicate-step
// lookup contract: enforcement keys on the step NAME (markers carry no
// index), so when a flow lists the same step twice with different limits,
// the LAST payload entry wins (encode 10 then 90 → 90m).
func TestLimitsFromPayloadDuplicateStepLastEntryWins(t *testing.T) {
	l := limitsFromPayload(&model.JobPayload{
		StepTimeouts: []model.StepTimeout{
			{Index: 0, Step: "encode", TimeoutMinutes: 10},
			{Index: 3, Step: "encode", TimeoutMinutes: 90},
		},
	})
	if l.steps["encode"] != 90*time.Minute {
		t.Fatalf("duplicate encode limit = %s, want the LAST entry (90m)", l.steps["encode"])
	}
}

func TestTimeoutMessage(t *testing.T) {
	cases := []struct {
		kind string
		d    time.Duration
		want string
	}{
		{"job", 90 * time.Minute, "job timeout after 90 minutes"},
		{"job", time.Minute, "job timeout after 1 minute"},
		{"step encode", 45 * time.Minute, "step encode timeout after 45 minutes"},
		{"job", time.Second, "job timeout after 1s"},
	}
	for _, c := range cases {
		if got := timeoutMessage(c.kind, c.d); got != c.want {
			t.Errorf("timeoutMessage(%q, %s) = %q, want %q", c.kind, c.d, got, c.want)
		}
	}
}

// TestWindowsKillArgs pins the Windows tree-kill mechanism: taskkill with
// /T (tree) and /F (force). The Windows kill file executes exactly these
// args; asserting them here keeps the mechanism reviewable on Linux.
func TestWindowsKillArgs(t *testing.T) {
	got := windowsKillArgs(1234)
	want := []string{"taskkill", "/T", "/F", "/PID", "1234"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("windowsKillArgs = %v, want %v", got, want)
	}
}
