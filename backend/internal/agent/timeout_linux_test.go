//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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

// TestStepDeadlineDisarmedOnTransition is the regression for the stale
// step deadline: step 'encode' has a 1s limit and finishes at ~0.3s; the
// NEXT step 'mux' has NO limit and runs past encode's old deadline. The
// transition must disarm encode's deadline — mux is unlimited and the job
// must succeed (before the fix, mux was killed at ~1.1s with "step encode
// timeout after 1s").
func TestStepDeadlineDisarmedOnTransition(t *testing.T) {
	script, runLog := writeTimeoutScript(t,
		"echo 'ENCODE_STEP encode 0.0'\n"+
			"sleep 0.3\n"+
			"echo 'ENCODE_STEP mux 0.0'\n"+
			"sleep 1.2\n"+ // finishes ~1.5s — past encode's 1s deadline
			"echo 'ENCODE_JOB_DONE'\n")

	a, err := New(Config{ControllerURL: "http://127.0.0.1:0", NodeName: "n", Token: nodeTok(), DataDir: t.TempDir()}, "v", testLog())
	if err != nil {
		t.Fatal(err)
	}
	limits := jobLimits{steps: map[string]time.Duration{"encode": time.Second}}

	start := time.Now()
	exitCode, _, stepErr, _ := a.runBash(script, runLog, nil, nil, context.Background(), limits)
	if time.Since(start) > 10*time.Second {
		t.Fatal("job did not finish promptly")
	}
	if exitCode != 0 || stepErr != "" {
		t.Fatalf("unlimited step after a timed step failed: exit=%d err=%q (stale step deadline not disarmed?)", exitCode, stepErr)
	}
}

// TestJobDeadlineKillsUnlimitedStep proves the job deadline still kills a
// step that has NO per-step limit: with the step deadline disarmed on
// transition, the job deadline remains the outer bound for unlimited steps.
func TestJobDeadlineKillsUnlimitedStep(t *testing.T) {
	script, runLog := writeTimeoutScript(t,
		"echo 'ENCODE_STEP mux 0.0'\nsleep 30\n")

	a, err := New(Config{ControllerURL: "http://127.0.0.1:0", NodeName: "n", Token: nodeTok(), DataDir: t.TempDir()}, "v", testLog())
	if err != nil {
		t.Fatal(err)
	}
	jobCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	limits := jobLimits{job: time.Second} // no step limits: mux is unlimited

	exitCode, _, stepErr, _ := a.runBash(script, runLog, nil, nil, jobCtx, limits)
	if exitCode == 0 {
		t.Fatal("killed job must report nonzero exit")
	}
	if stepErr != "job timeout after 1s" {
		t.Fatalf("stepErr = %q, want the JOB timeout reason for an unlimited step", stepErr)
	}
}

// TestStepTimeoutStillArmsAfterTransition proves the disarm-on-transition
// fix does not break arming: a later step WITH its own limit is still
// killed with ITS OWN name and budget when it overruns.
func TestStepTimeoutStillArmsAfterTransition(t *testing.T) {
	script, runLog := writeTimeoutScript(t,
		"echo 'ENCODE_STEP encode 0.0'\n"+
			"sleep 0.2\n"+
			"echo 'ENCODE_STEP mux 0.0'\n"+
			"sleep 30\n")

	a, err := New(Config{ControllerURL: "http://127.0.0.1:0", NodeName: "n", Token: nodeTok(), DataDir: t.TempDir()}, "v", testLog())
	if err != nil {
		t.Fatal(err)
	}
	limits := jobLimits{steps: map[string]time.Duration{"encode": time.Second, "mux": time.Second}}

	start := time.Now()
	exitCode, _, stepErr, _ := a.runBash(script, runLog, nil, nil, context.Background(), limits)
	if time.Since(start) > 10*time.Second {
		t.Fatal("step timeout did not kill promptly")
	}
	if exitCode == 0 {
		t.Fatal("killed step must report nonzero exit")
	}
	if stepErr != "step mux timeout after 1s" {
		t.Fatalf("stepErr = %q, want the SECOND step's own timeout reason", stepErr)
	}
}

// TestStepTimeoutReArmsForRepeatedStepInstance is the F3 regression for
// encode→mux→encode: the SECOND encode instance must be killed at its OWN
// limit. Before the fix, onStepStart fired only on the first marker per
// name, so the second encode ran with no deadline at all (and the mux
// transition had disarmed the first encode's).
func TestStepTimeoutReArmsForRepeatedStepInstance(t *testing.T) {
	script, runLog := writeTimeoutScript(t,
		"echo 'ENCODE_STEP encode 0'\n"+
			"sleep 0.2\n"+
			"echo 'ENCODE_STEP encode 100'\n"+
			"echo 'ENCODE_STEP mux 0'\n"+
			"sleep 0.2\n"+
			"echo 'ENCODE_STEP mux 100'\n"+
			"echo 'ENCODE_STEP encode 0'\n"+ // second encode instance…
			"sleep 30\n") // …wedges past its own 1s limit

	a, err := New(Config{ControllerURL: "http://127.0.0.1:0", NodeName: "n", Token: nodeTok(), DataDir: t.TempDir()}, "v", testLog())
	if err != nil {
		t.Fatal(err)
	}
	limits := jobLimits{steps: map[string]time.Duration{"encode": time.Second}}

	start := time.Now()
	exitCode, _, stepErr, _ := a.runBash(script, runLog, nil, nil, context.Background(), limits)
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Fatalf("second encode instance was never killed (ran %s) — its deadline did not re-arm", elapsed)
	}
	if exitCode == 0 {
		t.Fatal("wedged second encode instance must report nonzero exit")
	}
	if stepErr != "step encode timeout after 1s" {
		t.Fatalf("stepErr = %q, want the second instance's own timeout reason", stepErr)
	}
	// The kill must land AFTER the second instance starts (~0.4s), not at
	// the first instance's old deadline (~1s from job start is fine, but it
	// must not be the mux transition killing early).
	if elapsed < 400*time.Millisecond {
		t.Fatalf("killed too early (%s) — wrong instance's deadline fired", elapsed)
	}
}

// TestAdjacentStepRepeatNotKilledByStaleBudget is the F3 regression for
// adjacent encode→encode: the first instance (1s limit) finishes at ~0.3s;
// the second instance re-arms with a FRESH budget and runs past the first
// instance's old deadline without being killed by it.
func TestAdjacentStepRepeatNotKilledByStaleBudget(t *testing.T) {
	script, runLog := writeTimeoutScript(t,
		"echo 'ENCODE_STEP encode 0'\n"+
			"sleep 0.3\n"+
			"echo 'ENCODE_STEP encode 100'\n"+
			"echo 'ENCODE_STEP encode 0'\n"+ // adjacent repeat → re-arm
			"sleep 0.8\n"+ // finishes ~1.1s: past the stale 1s deadline, inside the fresh one
			"echo 'ENCODE_STEP encode 100'\n"+
			"echo 'ENCODE_JOB_DONE'\n")

	a, err := New(Config{ControllerURL: "http://127.0.0.1:0", NodeName: "n", Token: nodeTok(), DataDir: t.TempDir()}, "v", testLog())
	if err != nil {
		t.Fatal(err)
	}
	limits := jobLimits{steps: map[string]time.Duration{"encode": time.Second}}

	start := time.Now()
	exitCode, _, stepErr, _ := a.runBash(script, runLog, nil, nil, context.Background(), limits)
	if time.Since(start) > 10*time.Second {
		t.Fatal("job did not finish promptly")
	}
	if exitCode != 0 || stepErr != "" {
		t.Fatalf("second instance killed by the first's stale budget: exit=%d err=%q", exitCode, stepErr)
	}
}

// TestProgressWithinInstanceDoesNotReArm guards F3's other side with a real
// process: a step reporting 0→50 mid-run keeps its ORIGINAL deadline — the
// 50% marker must not extend it (the script is killed at ~1s even though it
// would finish at ~1.2s).
func TestProgressWithinInstanceDoesNotReArm(t *testing.T) {
	script, runLog := writeTimeoutScript(t,
		"echo 'ENCODE_STEP encode 0'\n"+
			"sleep 0.6\n"+
			"echo 'ENCODE_STEP encode 50'\n"+
			"sleep 0.6\n"+
			"echo 'ENCODE_STEP encode 100'\n")

	a, err := New(Config{ControllerURL: "http://127.0.0.1:0", NodeName: "n", Token: nodeTok(), DataDir: t.TempDir()}, "v", testLog())
	if err != nil {
		t.Fatal(err)
	}
	limits := jobLimits{steps: map[string]time.Duration{"encode": time.Second}}

	exitCode, _, stepErr, _ := a.runBash(script, runLog, nil, nil, context.Background(), limits)
	if exitCode == 0 {
		t.Fatal("mid-instance progress re-armed the deadline — the step escaped its timeout")
	}
	if stepErr != "step encode timeout after 1s" {
		t.Fatalf("stepErr = %q, want the original step timeout reason", stepErr)
	}
}

// TestJobDeadlineCoversS3Uploads is the regression for uploads escaping the
// job deadline: the script finishes fast, then the S3 PUT hangs past the
// job limit. The upload must run under the job context, so the job fails
// with an s3 upload error at the deadline — and the failure report still
// reaches the controller (completeJob uses its own context, not jobCtx).
// Before the fix, uploads ran under context.Background(): the PUT completed
// after the deadline and the job wrongly reported done.
func TestJobDeadlineCoversS3Uploads(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "release.mkv"), []byte("mkv"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Minimal S3 endpoint: accept the PUT but hold the response well past
	// the 1s job deadline. When the client's job ctx expires it aborts the
	// request, which cancels r.Context() and releases the handler.
	var putSeen atomic.Int32
	s3srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			putSeen.Add(1)
			io.Copy(io.Discard, r.Body)
			select {
			case <-time.After(3 * time.Second):
			case <-r.Context().Done():
			}
			w.Header().Set("ETag", `"0123456789abcdef0123456789abcdef"`)
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(200)
	}))
	defer s3srv.Close()

	var gotStatus, gotError string
	reports := make(chan struct{}, 1)
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/complete") {
			var rep struct {
				Status string `json:"status"`
				Error  string `json:"error"`
			}
			json.NewDecoder(r.Body).Decode(&rep)
			gotStatus, gotError = rep.Status, rep.Error
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
		ID:     78,
		Script: "#!/usr/bin/env bash\necho 'ENCODE_STEP mux 100.0'\necho 'ENCODE_JOB_DONE'\nexit 0\n",
		Vars:   map[string]string{},
		Flow:   "s3-deadline-test",
		S3: &model.S3Transfer{
			Endpoint:  strings.TrimPrefix(s3srv.URL, "http://"),
			AccessKey: "ak", SecretKey: "sk", Region: "us-east-1",
			Uploads: []model.S3Upload{{Bucket: "releases", Prefix: "out", LocalDir: outDir}},
		},
	}

	start := time.Now()
	a.executeJobWithLimits(job, jobLimits{job: time.Second})
	if time.Since(start) > 10*time.Second {
		t.Fatal("executeJob did not return promptly at the job deadline")
	}
	select {
	case <-reports:
	case <-time.After(5 * time.Second):
		t.Fatal("no completion report after the upload deadline — failure report must survive job ctx expiry")
	}
	if putSeen.Load() == 0 {
		t.Fatal("upload never attempted — test did not exercise the upload path")
	}
	if gotStatus != "failed" {
		t.Fatalf("completion status = %q, want failed (upload ran past the job deadline)", gotStatus)
	}
	if !strings.HasPrefix(gotError, "s3 upload:") {
		t.Fatalf("completion error = %q, want the s3 upload failure reason", gotError)
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
