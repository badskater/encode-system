//go:build linux

package agent

import (
	"context"
	"errors"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

// Kill-retry regressions (F4/F6): a deadline kill is retried, and when every
// attempt fails the timeout reason MUST win even if the script later exits
// 0 — "survived because the kill failed" is a timeout failure, while
// "exited naturally before the kill landed" stays a success.

// TestKillProcessTreeAlreadyGoneIsSuccess proves the kill mechanism treats
// an already-reaped process as "nothing to kill" (nil), not a failure — on
// Linux kill(-pgid) returns ESRCH once the group is gone, and counting that
// as a failed kill would turn natural exits into false timeout failures.
func TestKillProcessTreeAlreadyGoneIsSuccess(t *testing.T) {
	cmd := exec.Command("sleep", "0.1")
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := killProcessTree(cmd); err != nil {
		t.Fatalf("kill of an already-reaped process = %v, want nil (nothing to kill)", err)
	}
}

// TestKillSkippedWhenProcessAlreadyExited is the F6 mechanism test: once
// the child is reaped (ProcessState recorded under the guard), the kill
// routine must not attempt a kill at all — closing the kill-after-reap
// window and keeping natural exits out of the kill-failure path.
func TestKillSkippedWhenProcessAlreadyExited(t *testing.T) {
	cmd := exec.Command("sleep", "0.1")
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	guard := &procGuard{}
	guard.record(cmd)

	var calls atomic.Int32
	oldFn := killTreeFn
	killTreeFn = func(*exec.Cmd) error { calls.Add(1); return nil }
	t.Cleanup(func() { killTreeFn = oldFn })

	if ok := killTreeWithRetry(cmd, testLog(), guard); !ok {
		t.Fatal("kill of an already-reaped process must count as success (nothing to kill)")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("kill attempted %d times after the process was reaped, want 0", got)
	}
}

// TestTimeoutWinsWhenAllKillsFail is the F4 regression: the deadline fires,
// every kill attempt fails (simulated transient taskkill failure), and the
// script runs to completion with exit 0. The job must STILL be reported
// failed with the timeout reason — before the fix the single discarded kill
// error let exit 0 suppress the timeout and report false success.
func TestTimeoutWinsWhenAllKillsFail(t *testing.T) {
	script, runLog := writeTimeoutScript(t, "sleep 1.5\nexit 0\n")
	a, err := New(Config{ControllerURL: "http://127.0.0.1:0", NodeName: "n", Token: nodeTok(), DataDir: t.TempDir()}, "v", testLog())
	if err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	oldFn, oldDelay := killTreeFn, killRetryDelay
	killTreeFn = func(*exec.Cmd) error {
		attempts.Add(1)
		return errors.New("simulated transient taskkill failure")
	}
	killRetryDelay = 20 * time.Millisecond
	t.Cleanup(func() { killTreeFn, killRetryDelay = oldFn, oldDelay })

	jobCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	exitCode, _, stepErr, _ := a.runBash(script, runLog, nil, nil, jobCtx, jobLimits{job: 300 * time.Millisecond})

	if got := attempts.Load(); got != 3 {
		t.Fatalf("kill attempts = %d, want 3 (retry on failure)", got)
	}
	if exitCode == 0 {
		t.Fatal("all kills failed but the job reported success (exit 0) — the timeout reason must win")
	}
	if stepErr != "job timeout after 300ms" {
		t.Fatalf("stepErr = %q, want the timeout reason despite exit 0", stepErr)
	}
}

// TestKillRetriedUntilSuccess proves a transient kill failure is retried:
// the first two attempts fail, the third lands, and the job is reported
// failed with the timeout reason (the successful-kill outcome is unchanged).
func TestKillRetriedUntilSuccess(t *testing.T) {
	script, runLog := writeTimeoutScript(t, "sleep 2\nexit 0\n")
	a, err := New(Config{ControllerURL: "http://127.0.0.1:0", NodeName: "n", Token: nodeTok(), DataDir: t.TempDir()}, "v", testLog())
	if err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	oldFn, oldDelay := killTreeFn, killRetryDelay
	killTreeFn = func(cmd *exec.Cmd) error {
		if attempts.Add(1) < 3 {
			return errors.New("transient kill failure")
		}
		return killProcessTree(cmd) // third attempt lands for real
	}
	killRetryDelay = 20 * time.Millisecond
	t.Cleanup(func() { killTreeFn, killRetryDelay = oldFn, oldDelay })

	jobCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	exitCode, _, stepErr, _ := a.runBash(script, runLog, nil, nil, jobCtx, jobLimits{job: 300 * time.Millisecond})
	if time.Since(start) > 10*time.Second {
		t.Fatal("retried kill did not land promptly")
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("kill attempts = %d, want 3 (stop retrying once one lands)", got)
	}
	if exitCode == 0 {
		t.Fatal("killed job must report nonzero exit")
	}
	if stepErr != "job timeout after 300ms" {
		t.Fatalf("stepErr = %q, want the timeout reason", stepErr)
	}
}
