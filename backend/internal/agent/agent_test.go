package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// nodeTok builds the shared bearer credential for agent tests programmatically
// so no secret-looking literal appears in source.
func nodeTok() string { return strings.Repeat("ab", 16) }

func TestNewRequiresFields(t *testing.T) {
	if _, err := New(Config{}, "0.1.0", testLog()); err == nil {
		t.Fatal("empty config must be rejected")
	}
	a, err := New(Config{ControllerURL: "http://x", NodeName: "n", Token: nodeTok(), DataDir: t.TempDir()}, "0.1.0", testLog())
	if err != nil {
		t.Fatal(err)
	}
	if a.Cfg.HeartbeatEvery != 15 {
		t.Fatalf("default heartbeat = %d", a.Cfg.HeartbeatEvery)
	}
}

func TestTaskCounterPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{ControllerURL: "http://x", NodeName: "n", Token: nodeTok(), DataDir: dir}
	a, _ := New(cfg, "v", testLog())
	if a.TasksSinceBoot() != 0 {
		t.Fatal("counter must start at 0")
	}
	a.bumpCounter()
	a.bumpCounter()

	// Simulate agent restart (no reboot): counter survives via the file.
	b, _ := New(cfg, "v", testLog())
	if b.TasksSinceBoot() != 2 {
		t.Fatalf("counter after restart = %d, want 2", b.TasksSinceBoot())
	}
	b.resetCounter()
	if b.TasksSinceBoot() != 0 {
		t.Fatal("reset must zero the counter")
	}
}

// TestExecuteJobEndToEnd spins up a fake controller, has the agent run a job
// whose "script" is a tiny PowerShell program (pwsh), and verifies the
// completion report lands with status done.
func TestExecuteJobEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	dir := t.TempDir()

	// A stub EncodeLib (the job script dot-sources it).
	libPath := filepath.Join(dir, "EncodeLib.ps1")
	os.WriteFile(libPath, []byte("# stub lib\n"), 0o644)

	var completed atomic.Int32
	var lastStatus string
	var lastExit int
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/agent/job/") && strings.HasSuffix(r.URL.Path, "/complete"):
			var rep struct {
				Status   string `json:"status"`
				ExitCode int    `json:"exit_code"`
			}
			json.NewDecoder(r.Body).Decode(&rep)
			lastStatus, lastExit = rep.Status, rep.ExitCode
			completed.Add(1)
			w.WriteHeader(200)
			w.Write([]byte(`{}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer controller.Close()

	a, err := New(Config{
		ControllerURL: controller.URL, NodeName: "n", Token: nodeTok(), DataDir: dir, LibPath: libPath,
	}, "v", testLog())
	if err != nil {
		t.Fatal(err)
	}
	ps := a.findPowerShell()
	if ps == "powershell" {
		if _, err := os.Stat("powershell"); err != nil {
			t.Skip("no PowerShell available on this host")
		}
	}

	job := &model.JobPayload{
		ID: 7,
		Script: "param([string]$LibPath)\n" +
			". $LibPath\n" +
			"Write-Output 'ENCODE_STEP encode 50'\n" +
			"Write-Output 'ENCODE_JOB_DONE'\n" +
			"exit 0\n",
		Vars: map[string]string{"episode_dir": "S/Ep 01"},
		Flow: "test",
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
	// run.log must exist in the job dir.
	if _, err := os.Stat(filepath.Join(dir, "jobs", "7", "run.log")); err != nil {
		t.Errorf("run.log missing: %v", err)
	}
}

// TestExecuteJobFailureReportsFailed verifies a non-zero script exit yields a
// failed completion with the exit code.
func TestExecuteJobFailureReportsFailed(t *testing.T) {
	dir := t.TempDir()
	libPath := filepath.Join(dir, "EncodeLib.ps1")
	os.WriteFile(libPath, []byte("# stub\n"), 0o644)

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

	a, _ := New(Config{ControllerURL: controller.URL, NodeName: "n", Token: nodeTok(), DataDir: dir, LibPath: libPath}, "v", testLog())
	if a.findPowerShell() == "powershell" {
		if _, err := os.Stat("powershell"); err != nil {
			t.Skip("no PowerShell available on this host")
		}
	}

	job := &model.JobPayload{
		ID:     8,
		Script: "Write-Output 'ENCODE_STEP_FAILED encode boom'\nexit 3\n",
		Vars:   map[string]string{},
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

// TestHeartbeatLoopTalksToController verifies the agent heartbeats with its
// bearer credential and stops when told.
func TestHeartbeatLoopTalksToController(t *testing.T) {
	want := "Bearer " + nodeTok()
	var beats atomic.Int32
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/agent/heartbeat" {
			if r.Header.Get("Authorization") != want {
				w.WriteHeader(401)
				return
			}
			beats.Add(1)
			w.Write([]byte(`{"instruction":"none"}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer controller.Close()

	a, _ := New(Config{ControllerURL: controller.URL, NodeName: "n", Token: nodeTok(), DataDir: t.TempDir(), HeartbeatEvery: 1}, "v", testLog())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for beats.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
	if beats.Load() < 2 {
		t.Fatalf("heartbeats = %d, want >= 2", beats.Load())
	}
}
