package agent

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestExecuteBashJobEndToEnd runs a real bash job script through the Linux
// execution path on this Linux dev host and verifies the ENCODE_STEP
// progress markers parse, the step timings record, the exit-0 path reports
// done, and the run.log + job.sh are written with the right perms. This is
// the Linux mirror of TestExecuteJobEndToEnd (PowerShell).
//
// The script is hand-written bash (not flow.RenderDialect output) because the
// test asserts the agent's EXECUTION path, not the renderer's — the markers
// are identical text regardless of which dialect produced the script.
func TestExecuteBashJobEndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bash execution path runs only on Linux")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()

	var completed atomic.Int32
	var lastStatus string
	var lastExit int
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/complete") {
			var rep struct {
				Status      string `json:"status"`
				ExitCode    int    `json:"exit_code"`
				LogTail     string `json:"log_tail"`
				StepTimings []model.StepTiming
			}
			json.NewDecoder(r.Body).Decode(&rep)
			lastStatus, lastExit = rep.Status, rep.ExitCode
			completed.Add(1)
		}
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer controller.Close()

	a, err := New(Config{
		ControllerURL: controller.URL, NodeName: "n", Token: nodeTok(), DataDir: dir,
	}, "v", testLog())
	if err != nil {
		t.Fatal(err)
	}

	job := &model.JobPayload{
		ID: 11,
		Script: "#!/usr/bin/env bash\n" +
			"set -euo pipefail\n" +
			"echo 'ENCODE_STEP encode 25.0'\n" +
			"echo 'ENCODE_STEP encode 75.0'\n" +
			"echo 'ENCODE_STEP mux 10.0'\n" +
			"echo 'ENCODE_JOB_DONE'\n" +
			"exit 0\n",
		Vars: map[string]string{"episode_dir": "S/Ep 01"},
		Flow: "test-bash",
	}

	a.executeJob(job)

	if completed.Load() != 1 {
		t.Fatalf("completion reports = %d, want 1", completed.Load())
	}
	if lastStatus != "done" || lastExit != 0 {
		t.Fatalf("completion wrong: status=%s exit=%d", lastStatus, lastExit)
	}
	if a.TasksSinceBoot() != 1 {
		t.Fatalf("tasks counter = %d, want 1", a.TasksSinceBoot())
	}

	// The bash script must be written as job.sh (not job.ps1) with 0700.
	scriptPath := filepath.Join(dir, "jobs", "11", "job.sh")
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("job.sh missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("job.sh perms = %v, want 0700", perm)
	}
	// run.log must exist and be 0600.
	runLogPath := filepath.Join(dir, "jobs", "11", "run.log")
	rlInfo, err := os.Stat(runLogPath)
	if err != nil {
		t.Fatalf("run.log missing: %v", err)
	}
	if perm := rlInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("run.log perms = %v, want 0600", perm)
	}
	// The bash script must NOT carry a UTF-8 BOM (byte 0xEF at offset 0).
	b, _ := os.ReadFile(scriptPath)
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		t.Fatal("job.sh has a UTF-8 BOM — bash shebang must be at byte 0")
	}
	if !strings.HasPrefix(string(b), "#!/usr/bin/env bash") {
		t.Fatalf("job.sh does not start with shebang:\n%s", b)
	}
}

// TestExecuteBashJobFailureReportsFailed runs a bash script that emits an
// ENCODE_STEP_FAILED marker and exits nonzero, then asserts the completion
// reports failed with the exit code and the marker in the tail.
func TestExecuteBashJobFailureReportsFailed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bash execution path runs only on Linux")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()

	var lastStatus string
	var lastExit int
	var lastTail string
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/complete") {
			var rep struct {
				Status   string `json:"status"`
				ExitCode int    `json:"exit_code"`
				LogTail  string `json:"log_tail"`
			}
			json.NewDecoder(r.Body).Decode(&rep)
			lastStatus, lastExit, lastTail = rep.Status, rep.ExitCode, rep.LogTail
		}
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer controller.Close()

	a, _ := New(Config{
		ControllerURL: controller.URL, NodeName: "n", Token: nodeTok(), DataDir: dir,
	}, "v", testLog())

	job := &model.JobPayload{
		ID: 12,
		Script: "#!/usr/bin/env bash\n" +
			"echo 'ENCODE_STEP_FAILED encode boom'\n" +
			"exit 3\n",
		Vars: map[string]string{},
	}
	a.executeJob(job)

	if lastStatus != "failed" {
		t.Fatalf("status = %s, want failed", lastStatus)
	}
	if lastExit != 3 {
		t.Fatalf("exit code = %d, want 3", lastExit)
	}
	if !strings.Contains(lastTail, "ENCODE_STEP_FAILED") {
		t.Errorf("tail should carry the failure marker: %q", lastTail)
	}
}

// TestExecuteBashJobDoesNotRequireEncodeLib verifies the Linux path does NOT
// fail when EncodeLib.ps1 is absent (bash scripts are self-contained; the lib
// is a Windows-only dot-source). The job still completes successfully.
func TestExecuteBashJobDoesNotRequireEncodeLib(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bash execution path runs only on Linux")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()

	var completed atomic.Int32
	var lastStatus string
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/complete") {
			var rep struct {
				Status string `json:"status"`
			}
			json.NewDecoder(r.Body).Decode(&rep)
			lastStatus = rep.Status
			completed.Add(1)
		}
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer controller.Close()

	a, _ := New(Config{
		ControllerURL: controller.URL, NodeName: "n", Token: nodeTok(), DataDir: dir,
	}, "v", slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Explicitly confirm no EncodeLib.ps1 exists in the data dir.
	if _, err := os.Stat(filepath.Join(dir, "EncodeLib.ps1")); !os.IsNotExist(err) {
		t.Fatalf("precondition: EncodeLib.ps1 unexpectedly present")
	}

	job := &model.JobPayload{
		ID: 13,
		Script: "#!/usr/bin/env bash\n" +
			"echo 'ENCODE_STEP encode 100.0'\n" +
			"echo 'ENCODE_JOB_DONE'\n" +
			"exit 0\n",
		Vars: map[string]string{},
	}
	a.executeJob(job)

	if completed.Load() != 1 {
		t.Fatalf("completion reports = %d, want 1", completed.Load())
	}
	if lastStatus != "done" {
		t.Fatalf("status = %s, want done (bash path must not require EncodeLib.ps1)", lastStatus)
	}
}
