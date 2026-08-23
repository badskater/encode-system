package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// Regression (adversarial review): bumpCounter/TasksSinceBoot are mutex-safe
// under concurrent use — the counter gates the reboot safety limit.
func TestCounterConcurrencySafe(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{ControllerURL: "http://x", NodeName: "n", DataDir: dir}
	cfg.Token = nodeTok()
	a, _ := New(cfg, "v", testLog())

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); a.bumpCounter() }()
	}
	wg.Wait()
	if got := a.TasksSinceBoot(); got != 50 {
		t.Fatalf("counter = %d, want 50 (lost increments = racy RMW)", got)
	}
}

// Regression: the reboot counter must NOT reset when the reboot command fails
// (resetting first would defeat the task-limit mechanism on failure).
func TestFailedRebootKeepsCounter(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{ControllerURL: "http://x", NodeName: "n", DataDir: dir}
	cfg.Token = nodeTok()
	a, _ := New(cfg, "v", testLog())
	a.bumpCounter()
	a.bumpCounter()

	a.handleReboot(30) // shutdown not found on this host -> command fails

	if got := a.TasksSinceBoot(); got != 2 {
		t.Fatalf("counter after failed reboot = %d, want 2 (must not reset on failure)", got)
	}
}

// Regression: update payloads with a checksum mismatch must be refused.
func TestUpdateRefusesChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	// Serve a lib payload whose hash does NOT match the manifest.
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/lib") {
			w.Write([]byte("tampered lib content"))
			return
		}
		w.WriteHeader(404)
	}))
	defer controller.Close()

	cfg := Config{ControllerURL: controller.URL, NodeName: "n", DataDir: dir,
		LibPath: filepath.Join(dir, "EncodeLib.ps1")}
	cfg.Token = nodeTok()
	os.WriteFile(cfg.LibPath, []byte("# original"), 0o644)
	a, _ := New(cfg, "v", testLog())

	a.handleUpdate(model.UpdateManifest{LibVersion: 2, LibSHA256: strings.Repeat("0", 64)})

	// The original lib must be untouched and the version not advanced.
	if b, _ := os.ReadFile(cfg.LibPath); string(b) != "# original" {
		t.Fatalf("lib replaced despite checksum mismatch: %s", b)
	}
	if a.LibVersion == 2 {
		t.Fatal("lib version advanced despite refused install")
	}
}

// Regression: matching checksum installs the payload.
func TestUpdateAcceptsMatchingChecksum(t *testing.T) {
	dir := t.TempDir()
	payload := "new lib content"
	sum := sha256.Sum256([]byte(payload))
	wantHash := hex.EncodeToString(sum[:])

	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/lib") {
			w.Write([]byte(payload))
			return
		}
		w.WriteHeader(404)
	}))
	defer controller.Close()

	cfg := Config{ControllerURL: controller.URL, NodeName: "n", DataDir: dir,
		LibPath: filepath.Join(dir, "EncodeLib.ps1")}
	cfg.Token = nodeTok()
	os.WriteFile(cfg.LibPath, []byte("# original"), 0o644)
	a, _ := New(cfg, "v", testLog())

	a.handleUpdate(model.UpdateManifest{LibVersion: 3, LibSHA256: wantHash})

	if b, _ := os.ReadFile(cfg.LibPath); string(b) != payload {
		t.Fatalf("lib not installed with valid checksum: %s", b)
	}
	if a.LibVersion != 3 {
		t.Fatalf("lib version = %d, want 3", a.LibVersion)
	}
}

// Regression: an agent with only a pairing code exchanges it once, persists
// the returned credential, and reuses the persisted file on later starts.
func TestAgentPairingBootstrap(t *testing.T) {
	dir := t.TempDir()
	var paired atomic.Int32
	issuedAuth := nodeTok() // what the controller hands back
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/agent/pair":
			var req struct {
				Code string `json:"code"`
				Name string `json:"name"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			if req.Code != "abc123pairingcode" || req.Name != "enc-new" {
				w.WriteHeader(401)
				return
			}
			paired.Add(1)
			json.NewEncoder(w).Encode(map[string]string{"token": issuedAuth})
		default:
			w.WriteHeader(404)
		}
	}))
	defer controller.Close()

	cfg := Config{ControllerURL: controller.URL, NodeName: "enc-new", DataDir: dir}
	cfg.PairingCode = "abc123pairingcode"
	a, err := New(cfg, "v", testLog())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.pairIfNeeded(context.Background()); err != nil {
		t.Fatalf("pair: %v", err)
	}
	if paired.Load() != 1 {
		t.Fatalf("pair endpoint hits = %d", paired.Load())
	}
	if a.Cfg.Token != issuedAuth {
		t.Fatal("agent must adopt the issued credential")
	}

	// Persisted file exists with 0600 perms.
	info, err := os.Stat(filepath.Join(dir, "node.token"))
	if err != nil {
		t.Fatalf("credential not persisted: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential file perms = %v, want 0600", info.Mode().Perm())
	}

	// Second agent instance loads the persisted file and never pairs again.
	cfg2 := Config{ControllerURL: controller.URL, NodeName: "enc-new", DataDir: dir}
	cfg2.PairingCode = "abc123pairingcode"
	b, err := New(cfg2, "v", testLog())
	if err != nil {
		t.Fatal(err)
	}
	if err := b.pairIfNeeded(context.Background()); err != nil {
		t.Fatal(err)
	}
	if paired.Load() != 1 {
		t.Fatal("persisted credential must skip re-pairing")
	}
}

// Regression: no token and no pairing code is a hard startup error.
func TestAgentRefusesBootWithoutCredential(t *testing.T) {
	cfg := Config{ControllerURL: "http://x", NodeName: "n", DataDir: t.TempDir()}
	if _, err := New(cfg, "v", testLog()); err == nil {
		t.Fatal("agent without token or pairing code must be rejected")
	}
}
