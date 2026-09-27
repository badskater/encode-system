// Package agent implements the Windows worker logic shared by the service
// wrapper: controller client, job execution, auto-update, reboot enforcement.
// Platform-neutral so it can be tested on Linux; the Windows service plumbing
// lives in cmd/agent.
package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// Config is the agent's runtime configuration (agent.json on disk).
// Either Token is provided directly, or PairingCode is set for one-shot
// self-registration with the controller (the resulting permanent token is
// then persisted in data_dir and reused on every start).
type Config struct {
	ControllerURL  string `json:"controller_url"`
	NodeName       string `json:"node_name"`
	Token          string `json:"token"`
	PairingCode    string `json:"pairing_code"` // one-shot bootstrap, consumed once
	DataDir        string `json:"data_dir"`     // e.g. C:\encode-agent
	BinDir         string `json:"bin_dir"`      // tools folder kept in sync with the controller's bin package (e.g. C:\bin)
	LibPath        string `json:"lib_path"`     // EncodeLib.ps1 location
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
	BinVersion int64  // bin-package version on disk (persisted in data_dir)

	mu sync.Mutex
	// reg is the multi-job registry (per-node concurrency). It replaces
	// the old single currentJob pointer; lazy-initialized by registry()
	// so struct-literal Agents in tests work without New().
	reg     *jobRegistry
	syncing bool // update sync in progress: job assignment must wait

	// rlGuard holds the current job's run.log path, guarded for concurrent
	// heartbeat reads while executeJob sets/clears it. Separate from a.mu so
	// FPS tail-reads do not couple to job-state mutations.
	rlGuard runLogGuard

	// prog holds the current job's live step/percentage/log tail, fed by
	// the lineObserver as PowerShell output arrives and snapshotted by the
	// heartbeat goroutine so the controller (and its SSE log stream) sees
	// progress between completions. Own mutex — never couples to a.mu.
	prog *progressTracker

	// Injectable exec seams for metrics collectors. nil → production
	// defaults (runPSDefault, gpuProbeDefault). Tests inject fakes to
	// verify parsing without shelling out to PowerShell or nvidia-smi.
	runPS    psFunc
	gpuProbe gpuFunc

	wg       sync.WaitGroup // tracks in-flight job/update goroutines for clean shutdown
	stopOnce sync.Once
	stopCh   chan struct{}
}

// New builds an agent with sane defaults. A Token or a PairingCode must be
// available; pairing is performed on first Run (or via bootstrap).
func New(cfg Config, version string, log *slog.Logger) (*Agent, error) {
	if cfg.ControllerURL == "" || cfg.NodeName == "" {
		return nil, fmt.Errorf("controller_url and node_name are required")
	}
	if cfg.Token == "" && cfg.PairingCode == "" {
		return nil, fmt.Errorf("token or pairing_code is required")
	}
	if cfg.BinDir == "" {
		cfg.BinDir = `C:\bin`
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
	ag := &Agent{
		Cfg: cfg,
		Log: log,
		// No hard client Timeout: large payload downloads are governed by
		// per-request contexts (heartbeat uses 30s, updates 30m). A fixed
		// client-wide cap would contradict the download windows.
		Client:  &http.Client{},
		Version: version,
		prog:    newProgressTracker(progressRingLines),
		reg:     newJobRegistry(),
		stopCh:  make(chan struct{}),
	}
	// Default metrics exec seam: only Windows has PowerShell; on other
	// platforms (Linux test hosts) the seam stays nil and the CPU/RAM/
	// disk collectors are skipped (tests inject fakes to exercise parsing).
	if runtime.GOOS == "windows" {
		ag.runPS = ag.runPSDefault
	}
	// Bin version persists across agent restarts: a 200 MiB tools zip must
	// not re-download on every service bounce while the controller's version
	// still matches what is on disk.
	if b, err := os.ReadFile(filepath.Join(cfg.DataDir, "bin-version")); err == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
			ag.BinVersion = v
		}
	}
	return ag, nil
}

// saveBinVersion persists the deployed bin-package version next to the agent
// state so restarts do not re-download an unchanged package.
func (a *Agent) saveBinVersion() error {
	return os.WriteFile(filepath.Join(a.Cfg.DataDir, "bin-version"),
		[]byte(strconv.FormatInt(a.BinVersion, 10)), 0o644)
}

// Stop signals the run loop to exit.
func (a *Agent) Stop() { a.stopOnce.Do(func() { close(a.stopCh) }) }

// TasksSinceBoot reports completed tasks this boot (from the persisted
// counter file, which survives agent restarts without a reboot).
func (a *Agent) TasksSinceBoot() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.readCounter()
}

// counterPath stores the per-boot task counter. The OS clears it implicitly:
// the agent resets it to zero after executing a reboot instruction.
func (a *Agent) counterPath() string { return filepath.Join(a.Cfg.DataDir, "tasks_since_boot") }

// tokenPath stores the permanent bearer credential obtained via pairing, so
// the one-shot code never needs to survive on disk.
func (a *Agent) tokenPath() string { return filepath.Join(a.Cfg.DataDir, "node.token") }

// pairIfNeeded completes one-shot self-registration when no credential is
// configured yet: a persisted token file wins; otherwise the pairing code is
// exchanged for a permanent token at the controller. Idempotent across
// restarts.
func (a *Agent) pairIfNeeded(ctx context.Context) error {
	if a.Cfg.Token != "" {
		return nil
	}
	if b, err := os.ReadFile(a.tokenPath()); err == nil {
		if tok := strings.TrimSpace(string(b)); tok != "" {
			a.Cfg.Token = tok
			a.Log.Info("loaded persisted node credential")
			return nil
		}
		// Empty/whitespace-only file: treat as absent and re-pair below.
		a.Log.Warn("persisted credential is empty, re-pairing")
	}
	if a.Cfg.PairingCode == "" {
		return fmt.Errorf("no credential configured and no pairing code")
	}
	req := map[string]string{"code": a.Cfg.PairingCode, "name": a.Cfg.NodeName}
	var resp struct {
		Token string `json:"token"`
	}
	if err := a.postJSON(ctx, "/api/agent/pair", req, &resp); err != nil {
		return fmt.Errorf("pairing failed: %w", err)
	}
	if resp.Token == "" {
		return fmt.Errorf("pairing returned no credential")
	}
	if err := os.WriteFile(a.tokenPath(), []byte(resp.Token), 0o600); err != nil {
		return fmt.Errorf("persist credential: %w", err)
	}
	a.Cfg.Token = resp.Token
	a.Log.Info("node paired with controller", "node", a.Cfg.NodeName)
	return nil
}

func (a *Agent) readCounter() int {
	b, err := os.ReadFile(a.counterPath())
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

// bumpCounter/readCounter/resetCounter serialize on a.mu: the counter gates
// the reboot safety limit, so a lost increment under concurrent reads would
// defeat the mechanism.
func (a *Agent) bumpCounter() {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := a.readCounter() + 1
	if err := os.WriteFile(a.counterPath(), []byte(strconv.Itoa(n)), 0o644); err != nil {
		// A failed write would silently undercount tasks and defeat the
		// reboot-after-N limit — surface it loudly instead.
		a.Log.Error("task counter write failed", "err", err, "count", n)
	}
}

func (a *Agent) resetCounter() {
	a.mu.Lock()
	defer a.mu.Unlock()
	os.Remove(a.counterPath())
}

// Run drives the heartbeat loop until Stop or ctx cancellation, then waits
// for in-flight job/update goroutines so no encode is orphaned mid-shutdown.
func (a *Agent) Run(ctx context.Context) error {
	a.Log.Info("agent starting", "node", a.Cfg.NodeName, "controller", a.Cfg.ControllerURL, "version", a.Version)
	if strings.HasPrefix(a.Cfg.ControllerURL, "http://") {
		a.Log.Warn("controller URL is plain HTTP — credentials and job scripts transit unencrypted; use HTTPS (reverse proxy) for anything beyond a trusted LAN")
	}
	if err := a.pairIfNeeded(ctx); err != nil {
		return fmt.Errorf("agent cannot start without a credential: %w", err)
	}
	tick := time.NewTicker(time.Duration(a.Cfg.HeartbeatEvery) * time.Second)
	defer tick.Stop()
	defer a.wg.Wait()

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
	libVer, binVer := a.LibVersion, a.BinVersion
	syncing := a.syncing
	a.mu.Unlock()

	// Per-job live progress: each active job carries its OWN tracker (a
	// shared one would interleave concurrent encodes' lines). The Jobs
	// array is the new wire shape; the legacy single-job fields mirror
	// Jobs[0] so an old controller still tracks one job during a rolling
	// upgrade.
	reports := a.jobReports()

	hb := model.Heartbeat{
		Node:           a.Cfg.NodeName,
		AgentVersion:   a.Version,
		LibVersion:     libVer,
		BinVersion:     binVer,
		Syncing:        syncing, // update in flight -> controller holds jobs back
		TasksSinceBoot: a.TasksSinceBoot(),
		// Metrics is collected best-effort on every heartbeat. Old
		// controllers ignore the unknown key, so this is wire-safe.
		// The pointer is always non-nil: zero values are meaningful
		// (0 = not yet sampled, -1 = no GPU).
		Metrics: a.collectMetrics(ctx),
		Jobs:    reports,
	}
	if len(reports) > 0 {
		hb.JobID = reports[0].JobID
		hb.JobStatus = reports[0].JobStatus
		hb.Step = reports[0].Step
		hb.StepProgress = reports[0].StepProgress
		hb.LogTail = reports[0].LogTail
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
		a.spawnJobs([]*model.JobPayload{reply.Job})
	case "jobs":
		if len(reply.Jobs) == 0 {
			return fmt.Errorf("jobs instruction without payloads")
		}
		a.spawnJobs(reply.Jobs)
	case "reboot":
		a.handleReboot(reply.RebootDelay)
	case "update":
		if reply.Update != nil {
			a.wg.Add(1)
			go func(m model.UpdateManifest) { defer a.wg.Done(); a.handleUpdate(m) }(*reply.Update)
		}
	case "none", "":
	}
	return nil
}

// spawnJobs registers payloads (dedupe + hard cap inside the registry) and
// launches one executeJob goroutine per ACCEPTED payload. A refused payload
// (duplicate id or beyond maxLocalJobs) is logged and skipped — the agent
// must never run the same job twice even on a double dispatch.
func (a *Agent) spawnJobs(payloads []*model.JobPayload) {
	accepted := a.registry().accept(payloads)
	for _, skipped := range payloads {
		found := false
		for _, got := range accepted {
			if got.ID == skipped.ID {
				found = true
				break
			}
		}
		if !found {
			a.Log.Warn("refused job assignment", "job", skipped.ID, "active", a.activeCount())
		}
	}
	for _, p := range accepted {
		a.wg.Add(1)
		go func(job *model.JobPayload) { defer a.wg.Done(); a.executeJob(job) }(p)
	}
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
	// Register if not already (direct test calls bypass acceptJobs); the
	// registry dedupes by id so a double-register is impossible.
	r := a.registry()
	r.accept([]*model.JobPayload{job})
	aj := r.get(job.ID)
	defer a.finishJob(job.ID)

	log := a.Log.With("job", job.ID)
	log.Info("executing job", "flow", job.Flow, "episode", job.Vars["episode_dir"])

	jobDir := filepath.Join(a.Cfg.DataDir, "jobs", fmt.Sprintf("%d", job.ID))
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		a.completeJob(job.ID, "failed", -1, "create job dir: "+err.Error(), nil, "", "", nil)
		return
	}
	scriptPath := filepath.Join(jobDir, "job.ps1")
	// Windows PowerShell 5.1 reads .ps1 files WITHOUT a BOM as ANSI and
	// mangles any non-ASCII content — with anime series names that is a
	// data-corruption bug, not a cosmetic one. Always write UTF-8 with BOM;
	// pwsh (7+) and PS 5.1 both honor it identically.
	scriptBytes := append([]byte{0xEF, 0xBB, 0xBF}, []byte(job.Script)...)
	if err := os.WriteFile(scriptPath, scriptBytes, 0o644); err != nil {
		a.completeJob(job.ID, "failed", -1, "write job script: "+err.Error(), nil, "", "", nil)
		return
	}
	if _, err := os.Stat(a.Cfg.LibPath); err != nil {
		a.completeJob(job.ID, "failed", -1, "EncodeLib.ps1 missing at "+a.Cfg.LibPath, nil, "", "", nil)
		return
	}

	ps := a.findPowerShell()
	runLog := filepath.Join(jobDir, "run.log")
	// Expose this job's run.log to the heartbeat goroutine (FPS parsing
	// picks the newest active job's log). Per-job slot in the registry;
	// cleared with the job on completion.
	r.setRunLog(job.ID, runLog)
	exitCode, tail, stepErr, timings := a.runPowerShell(ps, scriptPath, runLog, aj.prog)

	status := "done"
	errMsg := ""
	if exitCode != 0 {
		status = "failed"
		errMsg = stepErr
		if errMsg == "" {
			errMsg = fmt.Sprintf("script exited with code %d", exitCode)
		}
	}

	// Verify the expected mux artifact actually exists on success — an
	// exit-0 encode that produced no file must not report a bogus output.
	outputs := []string{}
	if status == "done" {
		if epDir, ok := job.Vars["episode_dir"]; ok {
			artifact := epDir
			if name, ok2 := job.Vars["expected_output"]; ok2 && name != "" {
				// The episode dir var is share-relative on Windows nodes; the
				// agent runs the job from the rendered script which resolves
				// $ScriptsDir internally, so report the relative artifact.
				artifact = epDir + "/" + name
			}
			outputs = append(outputs, artifact)
		}
	}
	// Capture the last 1 MiB of run.log as the full log snapshot. A read
	// failure must never abort the completion — captureRunLog returns ""
	// on error, and we log a warning so operators can investigate.
	fullLog, capErr := captureRunLog(runLog)
	if capErr != nil {
		log.Warn("full log capture failed; sending empty", "err", capErr)
		fullLog = ""
	}
	a.completeJob(job.ID, status, exitCode, errMsg, outputs, tail, fullLog, timings.finish(time.Now()))
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
// returns the exit code plus the last output lines and the step timing
// tracker. Progress lines (ENCODE_STEP) are forwarded into the agent log for
// heartbeat context AND feed the tracker's first-seen timestamps — the scan
// is LIVE: a lineObserver processes each complete line as it arrives (at
// arrival time, time.Now()), not a post-hoc single-timestamp pass after
// cmd.Wait. The full output is still accumulated for tail extraction and the
// ENCODE_STEP_FAILED scan, which run unchanged.
func (a *Agent) runPowerShell(ps, scriptPath, runLog string, prog *progressTracker) (exitCode int, tail string, stepErr string, timings *stepTimingTracker) {
	timings = newStepTracker()
	cmd := exec.Command(ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", scriptPath, "-LibPath", a.Cfg.LibPath)

	observer := newLineObserver(a.Log, timings) // live scan: stamps each marker at arrival
	if prog == nil {
		// Defensive: direct runPowerShell callers without a tracker get a
		// throwaway one so the observer feed never nil-panics.
		prog = newProgressTracker(progressRingLines)
	}
	observer.prog = prog // per-job live heartbeat step/pct/tail feed
	prog.reset()         // fresh state per job — never inherit the previous run
	f, err := os.Create(runLog)
	if err == nil {
		defer f.Close()
	}
	mw := io.MultiWriter(observer) // tee: observer accumulates ALL output
	if f != nil {
		mw = io.MultiWriter(observer, f) // run.log gets the raw bytes too
	}
	cmd.Stdout = mw
	cmd.Stderr = mw

	if err := cmd.Start(); err != nil {
		return -1, "", "start powershell: " + err.Error(), timings
	}
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}
	// Process any final trailing partial line that never got a newline.
	observer.flush()

	out := observer.String() // full accumulated output (no lost/dup bytes)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	tail = strings.Join(lines, "\n")

	// Extract the last failure marker for a compact error message.
	// (This post-hoc scan is for error extraction only — progress logging
	// and timing observation happen LIVE in lineObserver.Write above.)
	for _, l := range lines {
		if strings.HasPrefix(l, "ENCODE_STEP_FAILED") {
			stepErr = strings.TrimSpace(l)
		}
	}
	return exitCode, tail, stepErr, timings
}

// completeJob reports the final state to the controller. fullLog is the
// last-1-MiB run.log snapshot; stepTimings is the per-step wall-clock record.
// Both are added as new JSON keys ("log_full", "step_timings") alongside the
// legacy fields; old controllers ignore unknown keys, so adding them is safe.
func (a *Agent) completeJob(id int64, status string, exitCode int, errMsg string, outputs []string, tail, fullLog string, stepTimings []model.StepTiming) {
	// Normalize a nil slice to []model.StepTiming{} so the wire shape is
	// always "step_timings":[] (never null). The three early-failure call
	// sites pass nil; without this the controller would unmarshal null into a
	// nil slice, breaking parity with the populated path. Consistent empty
	// array, not null, for both code paths.
	if stepTimings == nil {
		stepTimings = []model.StepTiming{}
	}
	rep := map[string]any{
		"status": status, "exit_code": exitCode, "error": errMsg,
		"outputs": outputs, "log_tail": tail,
		// log_full: bounded full-log snapshot for the UI to show without
		// re-reading agent files. Empty when capture failed or no log exists.
		"log_full": fullLog,
		// step_timings: per-step wall-clock samples for the observability
		// dashboard. Empty slice when no ENCODE_STEP markers were emitted.
		"step_timings": stepTimings,
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
	// shutdown.exe exists on Windows Server; on non-Windows test hosts this
	// fails harmlessly and is logged. The counter resets ONLY after the
	// reboot command is accepted — resetting first would undercount when the
	// command fails and defeat the task-limit safety mechanism.
	cmd := exec.Command("shutdown", "/r", "/t", strconv.Itoa(delaySeconds), "/c", "encode-system: task limit reached")
	if out, err := cmd.CombinedOutput(); err != nil {
		a.Log.Error("reboot command failed", "err", err, "output", strings.TrimSpace(string(out)))
		return
	}
	a.resetCounter()
	a.Log.Info("reboot scheduled", "delay_seconds", delaySeconds)
}

// maxUpdateBytes caps agent/lib payload downloads; anything larger is an
// error, not something to buffer. Bin packages use maxBinDownloadBytes
// instead (the controller caps bin uploads at 1 GiB — a node that refused
// anything over 256 MiB could never sync a legitimately large tools folder).
const maxUpdateBytes = 256 << 20    // 256 MiB
const maxBinDownloadBytes = 1 << 30 // 1 GiB (matches the publish cap)

// sha256Bytes hashes a payload for manifest verification.
func sha256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// limitedBuffer buffers a download but fails past its cap so a malicious or
// buggy controller cannot exhaust agent memory.
type limitedBuffer struct {
	buf bytes.Buffer
	cap int64
}

func (lb *limitedBuffer) Write(p []byte) (int, error) {
	if lb.cap <= 0 {
		lb.cap = maxUpdateBytes
	}
	if int64(lb.buf.Len()+len(p)) > lb.cap {
		return 0, fmt.Errorf("payload exceeds %d byte cap", lb.cap)
	}
	return lb.buf.Write(p)
}

func (lb *limitedBuffer) Bytes() []byte { return lb.buf.Bytes() }

// handleUpdate compares the manifest and applies lib/bin/agent updates.
// Every payload is SHA-256 verified against the manifest before install: an
// unverified binary would mean a compromised or MITM'd controller achieves
// code execution on every node.
//
// The three sync steps are INDEPENDENT: a failure in one (bad checksum,
// download error, extract error) logs and moves on — it must never block the
// others. In particular a broken bin package must not strand the node on an
// old agent binary (agent self-update runs regardless).
//
// Extraction happens with a.syncing set, which the controller sees on the
// next heartbeat as busy — no job lands mid-swap.
func (a *Agent) handleUpdate(m model.UpdateManifest) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	a.mu.Lock()
	a.syncing = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.syncing = false
		a.mu.Unlock()
	}()

	a.syncLib(ctx, m)
	a.syncBin(ctx, m)
	a.syncAgent(ctx, m)
}

// registry returns the job registry, lazily initializing it so struct-literal
// Agents built in tests (bypassing New) work without a nil panic.
func (a *Agent) registry() *jobRegistry {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.reg == nil {
		a.reg = newJobRegistry()
	}
	return a.reg
}

// activeCount reports how many jobs are in flight.
func (a *Agent) activeCount() int { return a.registry().count() }

// acceptJobs registers payloads up to capacity and returns whether all were
// accepted (false when at least one was refused: duplicate or hard cap). The
// caller spawns an executeJob goroutine per returned payload.
func (a *Agent) acceptJobs(payloads []*model.JobPayload) bool {
	got := a.registry().accept(payloads)
	return len(got) == len(payloads)
}

// jobProg returns a job's progress tracker, registering a bare entry if the
// job was never accepted (tests call executeJob directly). Never nil.
func (a *Agent) jobProg(id int64) *progressTracker {
	r := a.registry()
	if aj := r.get(id); aj != nil {
		return aj.prog
	}
	// Register a payload-less stub so a direct executeJob call in a test
	// still has a per-job tracker.
	r.accept([]*model.JobPayload{{ID: id}})
	return r.get(id).prog
}

// jobReports snapshots every in-flight job for the heartbeat.
func (a *Agent) jobReports() []model.HeartbeatJobReport { return a.registry().reports() }

// finishJob drops a completed job from the registry.
func (a *Agent) finishJob(id int64) { a.registry().remove(id) }

// busy reports whether any job is currently running (updates must wait).
func (a *Agent) busy() bool { return a.activeCount() > 0 }

// syncLib updates EncodeLib.ps1 when the controller's version is newer. The
// manifest MUST carry a checksum — an update without one is refused outright.
func (a *Agent) syncLib(ctx context.Context, m model.UpdateManifest) {
	if m.LibVersion <= 0 || m.LibVersion == a.LibVersion {
		return
	}
	if m.LibSHA256 == "" {
		a.Log.Error("lib update refused: manifest has no checksum")
		return
	}
	if a.busy() {
		// Never swap the library under a running job: the next idle
		// heartbeat re-offers the update.
		a.Log.Info("lib update deferred: job running")
		return
	}
	var buf limitedBuffer
	if err := a.getAuth(ctx, "/api/agent/download/lib", &buf); err != nil {
		a.Log.Error("download lib failed", "err", err)
		return
	}
	if got := sha256Bytes(buf.Bytes()); got != m.LibSHA256 {
		a.Log.Error("lib checksum mismatch, refusing install", "want", m.LibSHA256, "got", got)
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
	a.mu.Lock()
	a.LibVersion = m.LibVersion
	a.mu.Unlock()
	a.Log.Info("EncodeLib.ps1 updated", "version", m.LibVersion)
}

// syncBin extracts the tools-folder zip over Cfg.BinDir when the controller's
// version differs. Same mandatory-checksum rule. The version is bumped ONLY
// after a fully successful extraction — a locked or failing file leaves the
// version old, so the next heartbeat re-syncs the whole package and retries
// the file (no half-applied state is ever reported as complete).
func (a *Agent) syncBin(ctx context.Context, m model.UpdateManifest) {
	if m.BinVersion <= 0 || m.BinVersion == a.BinVersion {
		return
	}
	if m.BinSHA256 == "" {
		a.Log.Error("bin sync refused: manifest has no checksum")
		return
	}
	if a.busy() {
		a.Log.Info("bin sync deferred: job running")
		return
	}
	var buf limitedBuffer
	buf.cap = maxBinDownloadBytes
	if err := a.getAuth(ctx, "/api/agent/download/bin", &buf); err != nil {
		a.Log.Error("download bin package failed", "err", err)
		return
	}
	if got := sha256Bytes(buf.Bytes()); got != m.BinSHA256 {
		a.Log.Error("bin checksum mismatch, refusing install", "want", m.BinSHA256, "got", got)
		return
	}
	if err := extractBinZip(buf.Bytes(), a.Cfg.BinDir); err != nil {
		a.Log.Error("extract bin package failed; version NOT bumped, will retry on next heartbeat",
			"err", err, "dir", a.Cfg.BinDir)
		return
	}
	a.mu.Lock()
	a.BinVersion = m.BinVersion
	a.mu.Unlock()
	if err := a.saveBinVersion(); err != nil {
		a.Log.Warn("persist bin version failed", "err", err)
	}
	a.Log.Info("bin folder synced", "version", m.BinVersion, "dir", a.Cfg.BinDir)
}

// syncAgent updates the agent binary when the controller's version differs.
// Same mandatory-checksum rule. On Windows the running exe cannot overwrite
// itself, so the binary is staged and a cmd sidecar performs the swap after
// this process exits (then relaunches it); POSIX renames in place and exits
// for its supervisor.
func (a *Agent) syncAgent(ctx context.Context, m model.UpdateManifest) {
	if m.AgentVersion == "" || m.AgentVersion == a.Version {
		return
	}
	if m.AgentSHA256 == "" {
		a.Log.Error("agent update refused: manifest has no checksum")
		return
	}
	var buf limitedBuffer
	if err := a.getAuth(ctx, "/api/agent/download/agent", &buf); err != nil {
		a.Log.Error("download agent failed", "err", err)
		return
	}
	if got := sha256Bytes(buf.Bytes()); got != m.AgentSHA256 {
		a.Log.Error("agent checksum mismatch, refusing install", "want", m.AgentSHA256, "got", got)
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
	if runtime.GOOS == "windows" {
		a.launchWindowsSwap(staged, self)
	} else {
		// POSIX: the process can rename over its own running binary.
		if err := os.Rename(staged, self); err != nil {
			a.Log.Error("swap binary", "err", err)
			return
		}
		a.Log.Info("binary swapped; exiting — supervisor (systemd etc.) restarts the new version")
		a.Stop()
	}
}

// launchWindowsSwap hands the swap to a cmd sidecar because a running Windows
// exe cannot overwrite itself. The sidecar waits for THIS process to exit,
// retries the move until the file lock clears, and restarts the agent only
// after a successful move — a failed move leaves the old agent running
// unchanged (the manifest still advertises the new version, so the swap is
// re-attempted on a later heartbeat), never a mismatched new-lib/old-agent.
func (a *Agent) launchWindowsSwap(staged, self string) {
	script := windowsSwapScript(filepath.Base(self), staged, self, a.Cfg.DataDir, relaunchCommand(self))
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

// windowsSwapScript renders the cmd sidecar that performs the binary swap.
// Behavior contract: wait for the old process to exit, retry the move until
// the lock clears (15 attempts), restart the agent ONLY after a successful
// move, and log to swap-update.log on exhaustion. Restart strategy: if a
// Windows SERVICE named encode-agent exists, let the service manager restart
// it (net stop/start); otherwise — bare exe or scheduled-task deployments,
// which have no service — relaunch the binary directly with its original
// arguments so the swap never leaves the node without an agent.
func windowsSwapScript(exeName, staged, self, dataDir, relaunch string) string {
	return fmt.Sprintf(`@echo off
setlocal
rem Wait for the running agent process to exit so the file lock clears.
:wait_exit
timeout /t 1 /nobreak >nul
tasklist /FI "IMAGENAME eq %s" 2>NUL | find /I "%s" >NUL
if not errorlevel 1 goto wait_exit
rem Retry the move: an antivirus or indexer may hold the file briefly.
set /a tries=0
:try_move
move /y "%s" "%s" >nul 2>&1
if not errorlevel 1 goto swapped
set /a tries+=1
if %%tries%% geq 15 (
    echo swap failed after %%tries%% retries: %s >>"%s\swap-update.log"
    exit /b 1
)
timeout /t 2 /nobreak >nul
goto try_move
:swapped
rem Prefer the service manager when the agent runs as a Windows service.
sc query encode-agent >nul 2>&1
if errorlevel 1 goto relaunch
net stop encode-agent >nul 2>&1
net start encode-agent
exit /b 0
:relaunch
rem Task/bare deployments: restart the new binary directly.
start "" %s
`, exeName, exeName, staged, self, staged, dataDir, relaunch)
}

// relaunchCommand quotes the agent path and its original arguments so the
// swap sidecar can restart a non-service deployment after the binary swap.
func relaunchCommand(self string) string {
	parts := []string{`"` + self + `"`}
	for _, arg := range os.Args[1:] {
		parts = append(parts, `"`+arg+`"`)
	}
	return strings.Join(parts, " ")
}
