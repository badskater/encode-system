//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// Real-process timeout tests: second-scale deadlines through the
// executeJobWithLimits / runBash injection seams, proving the Linux
// process-group kill reaches the whole tree and the failure flows through
// the normal completion report.

// writeTimeoutScript writes a bash script into a temp dir and returns its
// path plus the run.log path.
func writeTimeoutScript(t *testing.T, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "job.sh")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script, filepath.Join(dir, "run.log")
}

// childDead reports whether the pid is gone OR a zombie. A SIGKILLed orphan
// is reaped by PID 1, which in containers may never reap — the process stays
// a zombie (kill(pid,0) still succeeds) even though it is dead. Reading the
// /proc state distinguishes "killed, awaiting reaping" from "still running".
func childDead(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true // process entry gone
	}
	s := string(data)
	// stat format: pid (comm) state ... — comm may contain spaces/parens,
	// so find the LAST ')' and read the state char after it.
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ')' && i+2 < len(s) {
			return s[i+2] == 'Z'
		}
	}
	return false
}

// TestJobTimeoutKillsProcessTree proves the job deadline kills the WHOLE
// process tree — a background child (the wedged-encoder stand-in) must die
// with the script, not be orphaned.
func TestJobTimeoutKillsProcessTree(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script, runLog := writeTimeoutScript(t, fmt.Sprintf(
		"sleep 30 &\necho $! > %q\nwait\n", pidFile))

	a, err := New(Config{ControllerURL: "http://127.0.0.1:0", NodeName: "n", Token: nodeTok(), DataDir: dir}, "v", testLog())
	if err != nil {
		t.Fatal(err)
	}
	jobCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	start := time.Now()
	exitCode, _, stepErr, _ := a.runBash(script, runLog, nil, nil, jobCtx, jobLimits{job: time.Second})
	elapsed := time.Since(start)

	if elapsed > 10*time.Second {
		t.Fatalf("timeout kill took %s — process was not killed promptly", elapsed)
	}
	if exitCode == 0 {
		t.Fatalf("killed job must report nonzero exit, got 0")
	}
	if stepErr != "job timeout after 1s" {
		t.Fatalf("stepErr = %q, want job timeout reason", stepErr)
	}
	// The background child must be dead too (process-group kill, not just
	// the script pid). Poll briefly for the kill to land.
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("child pid file missing: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("bad child pid %q: %v", b, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !childDead(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !childDead(pid) {
		// Clean up the orphan before failing so the test never leaks.
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("child process %d survived the job timeout — tree kill failed", pid)
	}
}

// TestStepTimeoutKillsRunningStep proves a per-step deadline fires when the
// step's first ENCODE_STEP marker arrived but the step never finishes.
func TestStepTimeoutKillsRunningStep(t *testing.T) {
	script, runLog := writeTimeoutScript(t,
		"echo 'ENCODE_STEP encode 0.0'\nsleep 30\n")

	a, err := New(Config{ControllerURL: "http://127.0.0.1:0", NodeName: "n", Token: nodeTok(), DataDir: t.TempDir()}, "v", testLog())
	if err != nil {
		t.Fatal(err)
	}
	limits := jobLimits{steps: map[string]time.Duration{"encode": time.Second}}

	start := time.Now()
	exitCode, _, stepErr, _ := a.runBash(script, runLog, nil, nil, context.Background(), limits)
	if time.Since(start) > 10*time.Second {
		t.Fatal("step timeout did not kill promptly")
	}
	if exitCode == 0 {
		t.Fatal("killed step must report nonzero exit")
	}
	if stepErr != "step encode timeout after 1s" {
		t.Fatalf("stepErr = %q, want step timeout reason", stepErr)
	}
}

// TestStepTimeoutClampedByJobDeadline proves the step deadline can never
// outlive the job: with a 1s job limit and a 10-minute step limit, the JOB
// timeout fires and its message wins.
func TestStepTimeoutClampedByJobDeadline(t *testing.T) {
	script, runLog := writeTimeoutScript(t,
		"echo 'ENCODE_STEP encode 0.0'\nsleep 30\n")

	a, err := New(Config{ControllerURL: "http://127.0.0.1:0", NodeName: "n", Token: nodeTok(), DataDir: t.TempDir()}, "v", testLog())
	if err != nil {
		t.Fatal(err)
	}
	jobCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	limits := jobLimits{job: time.Second, steps: map[string]time.Duration{"encode": 10 * time.Minute}}

	exitCode, _, stepErr, _ := a.runBash(script, runLog, nil, nil, jobCtx, limits)
	if exitCode == 0 {
		t.Fatal("killed job must report nonzero exit")
	}
	if stepErr != "job timeout after 1s" {
		t.Fatalf("stepErr = %q, want the JOB timeout reason (outer bound wins)", stepErr)
	}
}

// TestFastJobUnaffectedByTimeouts proves generous limits never disturb a
// job that completes normally.
func TestFastJobUnaffectedByTimeouts(t *testing.T) {
	script, runLog := writeTimeoutScript(t,
		"echo 'ENCODE_STEP encode 100.0'\necho 'ENCODE_JOB_DONE'\nexit 0\n")

	a, err := New(Config{ControllerURL: "http://127.0.0.1:0", NodeName: "n", Token: nodeTok(), DataDir: t.TempDir()}, "v", testLog())
	if err != nil {
		t.Fatal(err)
	}
	jobCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	limits := jobLimits{job: time.Minute, steps: map[string]time.Duration{"encode": time.Minute}}

	exitCode, _, stepErr, _ := a.runBash(script, runLog, nil, nil, jobCtx, limits)
	if exitCode != 0 || stepErr != "" {
		t.Fatalf("fast job disturbed by limits: exit=%d err=%q", exitCode, stepErr)
	}
}

// TestExecuteJobTimeoutReportsFailure runs the FULL executeJob path under a
// second-scale job deadline and asserts the completion report carries the
// timeout failure — the same wire shape any agent-reported failure uses, so
// the controller's retry/audit/notify machinery applies unchanged.
func TestExecuteJobTimeoutReportsFailure(t *testing.T) {
	dir := t.TempDir()

	var gotStatus, gotError string
	var gotExit int
	reports := make(chan struct{}, 1)
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/complete") {
			var rep struct {
				Status   string `json:"status"`
				ExitCode int    `json:"exit_code"`
				Error    string `json:"error"`
			}
			json.NewDecoder(r.Body).Decode(&rep)
			gotStatus, gotExit, gotError = rep.Status, rep.ExitCode, rep.Error
			reports <- struct{}{}
		}
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer controller.Close()

	a, err := New(Config{ControllerURL: controller.URL, NodeName: "n", Token: nodeTok(), DataDir: dir}, "v", testLog())
	if err != nil {
		t.Fatal(err)
	}
	job := &model.JobPayload{
		ID:     77,
		Script: "#!/usr/bin/env bash\nsleep 30\n",
		Vars:   map[string]string{"episode_dir": "S/Ep 01"},
		Flow:   "timeout-test",
	}

	start := time.Now()
	a.executeJobWithLimits(job, jobLimits{job: time.Second})
	if time.Since(start) > 10*time.Second {
		t.Fatal("executeJob did not return promptly after the timeout")
	}
	select {
	case <-reports:
	case <-time.After(5 * time.Second):
		t.Fatal("no completion report after timeout")
	}
	if gotStatus != "failed" {
		t.Fatalf("completion status = %q, want failed", gotStatus)
	}
	if gotExit == 0 {
		t.Fatal("completion exit_code must be nonzero for a killed job")
	}
	if gotError != "job timeout after 1s" {
		t.Fatalf("completion error = %q, want the timeout reason", gotError)
	}
}
