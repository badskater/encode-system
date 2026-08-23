// Package agent implements the Windows worker logic shared by the service
// wrapper: controller client, job execution, auto-update, reboot enforcement.
// Platform-neutral so it can be tested on Linux; the Windows service plumbing
// lives in cmd/agent.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// Config is the agent's runtime configuration (agent.json on disk).
type Config struct {
	ControllerURL  string `json:"controller_url"`
	NodeName       string `json:"node_name"`
	Token          string `json:"token"`
	DataDir        string `json:"data_dir"` // e.g. C:\encode-agent
	LibPath        string `json:"lib_path"` // EncodeLib.ps1 location
	HeartbeatEvery int    `json:"heartbeat_seconds"`
	PowerShell     string `json:"powershell"` // optional override; default finds powershell.exe
}

// Agent is one worker node's runtime state.
type Agent struct {
	Cfg    Config
	Log    *slog.Logger
	Client *http.Client

	Version    string // agent build version (set via ldflags)
	LibVersion int64  // EncodeLib version on disk

	mu         sync.Mutex
	currentJob *model.JobPayload
	tasksDone  int // completed tasks since process start (~boot)

	stopOnce sync.Once
	stopCh   chan struct{}
}

// New builds an agent with sane defaults.
func New(cfg Config, version string, log *slog.Logger) (*Agent, error) {
	if cfg.ControllerURL == "" || cfg.Token == "" || cfg.NodeName == "" {
		return nil, fmt.Errorf("controller_url, node_name and token are required")
	}
	if cfg.HeartbeatEvery <= 0 {
		cfg.HeartbeatEvery = 15
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "."
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	if cfg.LibPath == "" {
		cfg.LibPath = filepath.Join(cfg.DataDir, "EncodeLib.ps1")
	}
	return &Agent{
		Cfg:     cfg,
		Log:     log,
		Client:  &http.Client{Timeout: 5 * time.Minute},
		Version: version,
		stopCh:  make(chan struct{}),
	}, nil
}

// Stop signals the run loop to exit.
func (a *Agent) Stop() { a.stopOnce.Do(func() { close(a.stopCh) }) }

// TasksSinceBoot reports completed tasks this boot (from the persisted
// counter file, which survives agent restarts without a reboot).
func (a *Agent) TasksSinceBoot() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tasksDone + a.readCounter()
}

// counterPath stores the per-boot task counter. The OS clears it implicitly:
// the agent resets it to zero after executing a reboot instruction.
func (a *Agent) counterPath() string { return filepath.Join(a.Cfg.DataDir, "tasks_since_boot") }

func (a *Agent) readCounter() int {
	b, err := os.ReadFile(a.counterPath())
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func (a *Agent) bumpCounter() {
	n := a.readCounter() + 1
	os.WriteFile(a.counterPath(), []byte(strconv.Itoa(n)), 0o644)
}

func (a *Agent) resetCounter() {
	os.Remove(a.counterPath())
}

// Run drives the heartbeat loop until Stop or ctx cancellation.
func (a *Agent) Run(ctx context.Context) error {
	a.Log.Info("agent starting", "node", a.Cfg.NodeName, "controller", a.Cfg.ControllerURL, "version", a.Version)
	tick := time.NewTicker(time.Duration(a.Cfg.HeartbeatEvery) * time.Second)
	defer tick.Stop()

	// Heartbeat immediately on start, then on each tick.
	for {
		if err := a.heartbeat(ctx); err != nil {
			a.Log.Warn("heartbeat failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-a.stopCh:
			return nil
		case <-tick.C:
		}
	}
}

// heartbeat sends the status report and acts on the controller's instruction.
func (a *Agent) heartbeat(ctx context.Context) error {
	a.mu.Lock()
	job := a.currentJob
	a.mu.Unlock()

	hb := model.Heartbeat{
		Node:           a.Cfg.NodeName,
		AgentVersion:   a.Version,
		LibVersion:     a.LibVersion,
		TasksSinceBoot: a.TasksSinceBoot(),
	}
	if job != nil {
		hb.JobID = job.ID
		hb.JobStatus = "running"
	}

	var reply model.HeartbeatReply
	if err := a.postJSON(ctx, "/api/agent/heartbeat", hb, &reply); err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}

	switch reply.Instruction {
	case "job":
		if reply.Job == nil {
			return fmt.Errorf("job instruction without payload")
		}
		go a.executeJob(reply.Job)
	case "reboot":
		a.handleReboot(reply.RebootDelay)
	case "update":
		if reply.Update != nil {
			go a.handleUpdate(*reply.Update)
		}
	case "none", "":
	}
	return nil
}

// postJSON sends body to the controller API and decodes the reply.
func (a *Agent) postJSON(ctx context.Context, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := strings.TrimRight(a.Cfg.ControllerURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("controller %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// getAuth performs a GET with the node token, streaming the body to w.
func (a *Agent) getAuth(ctx context.Context, path string, w io.Writer) error {
	url := strings.TrimRight(a.Cfg.ControllerURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Cfg.Token)
	resp, err := a.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

var stepLine = regexp.MustCompile(`ENCODE_STEP (\w+) (\d+(?:\.\d+)?)`)

// executeJob runs one job: write the rendered script, invoke PowerShell,
// parse progress lines, and report completion.
func (a *Agent) executeJob(job *model.JobPayload) {
	a.mu.Lock()
	a.currentJob = job
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.currentJob = nil
		a.mu.Unlock()
	}()

	log := a.Log.With("job", job.ID)
	log.Info("executing job", "flow", job.Flow, "episode", job.Vars["episode_dir"])

	jobDir := filepath.Join(a.Cfg.DataDir, "jobs", fmt.Sprintf("%d", job.ID))
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		a.completeJob(job.ID, "failed", -1, "create job dir: "+err.Error(), nil, "")
		return
	}
	scriptPath := filepath.Join(jobDir, "job.ps1")
	if err := os.WriteFile(scriptPath, []byte(job.Script), 0o644); err != nil {
		a.completeJob(job.ID, "failed", -1, "write job script: "+err.Error(), nil, "")
		return
	}
	if _, err := os.Stat(a.Cfg.LibPath); err != nil {
		a.completeJob(job.ID, "failed", -1, "EncodeLib.ps1 missing at "+a.Cfg.LibPath, nil, "")
		return
	}

	ps := a.findPowerShell()
	runLog := filepath.Join(jobDir, "run.log")
	exitCode, tail, stepErr := a.runPowerShell(ps, scriptPath, runLog)

	status := "done"
	errMsg := ""
	if exitCode != 0 {
		status = "failed"
		errMsg = stepErr
		if errMsg == "" {
			errMsg = fmt.Sprintf("script exited with code %d", exitCode)
		}
	}

	// Collect output artifacts (best effort): the muxed MKV in the episode dir.
	outputs := []string{}
	if epDir, ok := job.Vars["episode_dir"]; ok && status == "done" {
		outputs = append(outputs, epDir)
	}
	a.completeJob(job.ID, status, exitCode, errMsg, outputs, tail)
	a.bumpCounter()
	log.Info("job finished", "status", status, "exit_code", exitCode, "tasks_since_boot", a.TasksSinceBoot())
}

// findPowerShell resolves the interpreter: config override, then the
// standard Windows locations, then PATH.
func (a *Agent) findPowerShell() string {
	if a.Cfg.PowerShell != "" {
		return a.Cfg.PowerShell
	}
	candidates := []string{
		`C:\Program Files\PowerShell\7\pwsh.exe`,
		`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	if p, err := exec.LookPath("pwsh"); err == nil {
		return p
	}
	return "powershell"
}

// runPowerShell executes the job script, teeing output to run.log, and
// returns the exit code plus the last output lines. Progress lines
// (ENCODE_STEP) are forwarded into the agent log for heartbeat context.
func (a *Agent) runPowerShell(ps, scriptPath, runLog string) (exitCode int, tail string, stepErr string) {
	cmd := exec.Command(ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", scriptPath, "-LibPath", a.Cfg.LibPath)

	var buf bytes.Buffer
	f, err := os.Create(runLog)
	if err == nil {
		defer f.Close()
	}
	mw := io.MultiWriter(&buf)
	if f != nil {
		mw = io.MultiWriter(&buf, f)
	}
	cmd.Stdout = mw
	cmd.Stderr = mw

	if err := cmd.Start(); err != nil {
		return -1, "", "start powershell: " + err.Error()
	}
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}

	out := buf.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	tail = strings.Join(lines, "\n")

	// Extract the last failure marker for a compact error message.
	for _, l := range lines {
		if strings.HasPrefix(l, "ENCODE_STEP_FAILED") {
			stepErr = strings.TrimSpace(l)
		}
	}
	// Log progress transitions.
	for _, l := range strings.Split(out, "\n") {
		if m := stepLine.FindStringSubmatch(l); m != nil {
			a.Log.Info("job progress", "step", m[1], "pct", m[2])
		}
	}
	return exitCode, tail, stepErr
}

// completeJob reports the final state to the controller.
func (a *Agent) completeJob(id int64, status string, exitCode int, errMsg string, outputs []string, tail string) {
	rep := map[string]any{
		"status": status, "exit_code": exitCode, "error": errMsg,
		"outputs": outputs, "log_tail": tail,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.postJSON(ctx, fmt.Sprintf("/api/agent/job/%d/complete", id), rep, nil); err != nil {
		a.Log.Error("job completion report failed", "job", id, "err", err)
	}
}

// handleReboot schedules a system reboot after the given delay. Deferred by
// the controller until the node is idle; executed here via shutdown.exe.
func (a *Agent) handleReboot(delaySeconds int) {
	if delaySeconds <= 0 {
		delaySeconds = 30
	}
	a.Log.Warn("reboot instruction received", "delay_seconds", delaySeconds)
	a.resetCounter()
	// shutdown.exe exists on Windows Server; on non-Windows test hosts this
	// fails harmlessly and is logged.
	cmd := exec.Command("shutdown", "/r", "/t", strconv.Itoa(delaySeconds), "/c", "encode-system: task limit reached")
	if out, err := cmd.CombinedOutput(); err != nil {
		a.Log.Error("reboot command failed", "err", err, "output", strings.TrimSpace(string(out)))
		return
	}
	a.Log.Info("reboot scheduled", "delay_seconds", delaySeconds)
}

// handleUpdate compares the manifest and applies agent/lib updates.
// The PowerShell lib swaps immediately (next job uses it); the binary stages
// itself and restarts via a sidecar command.
func (a *Agent) handleUpdate(m model.UpdateManifest) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Update EncodeLib.ps1 when the controller's version is newer.
	if m.LibVersion > 0 && m.LibVersion != a.LibVersion {
		var buf bytes.Buffer
		if err := a.getAuth(ctx, "/api/agent/download/lib", &buf); err != nil {
			a.Log.Error("download lib failed", "err", err)
			return
		}
		tmp := a.Cfg.LibPath + ".new"
		if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
			a.Log.Error("write lib failed", "err", err)
			return
		}
		if err := os.Rename(tmp, a.Cfg.LibPath); err != nil {
			a.Log.Error("swap lib failed", "err", err)
			return
		}
		a.LibVersion = m.LibVersion
		a.Log.Info("EncodeLib.ps1 updated", "version", m.LibVersion)
	}

	// Update the agent binary when the controller's version differs.
	if m.AgentVersion != "" && m.AgentVersion != a.Version {
		var buf bytes.Buffer
		if err := a.getAuth(ctx, "/api/agent/download/agent", &buf); err != nil {
			a.Log.Error("download agent failed", "err", err)
			return
		}
		self, err := os.Executable()
		if err != nil {
			a.Log.Error("resolve self path", "err", err)
			return
		}
		staged := self + ".new"
		if err := os.WriteFile(staged, buf.Bytes(), 0o755); err != nil {
			a.Log.Error("stage agent binary", "err", err)
			return
		}
		a.Log.Info("agent binary staged; will swap on restart", "staged", staged, "version", m.AgentVersion)
		// Swap-and-restart: on Windows a running exe can't overwrite itself,
		// so a cmd sidecar performs the move after we exit. The service manager
		// then restarts us with the new binary.
		script := fmt.Sprintf(`@echo off
timeout /t 2 /nobreak >nul
move /y "%s" "%s"
net stop encode-agent && net start encode-agent
`, staged, self)
		batPath := filepath.Join(a.Cfg.DataDir, "swap-update.bat")
		if err := os.WriteFile(batPath, []byte(script), 0o755); err != nil {
			a.Log.Error("write swap script", "err", err)
			return
		}
		if err := exec.Command("cmd", "/c", "start", "/min", batPath).Start(); err != nil {
			a.Log.Error("launch swap script", "err", err)
			return
		}
		a.Log.Info("exiting for binary swap; service manager will restart")
		a.Stop()
	}
}
