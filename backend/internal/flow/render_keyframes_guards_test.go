//go:build linux

package flow

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// P7 quality-review regressions (Q1/Q2/Q4/Q5): publish error guards under
// the errexit-dead StepCall wrapper, live progress streaming, log content
// validity, and hostile-series marker forgery.
//
// Q1 background: the StepCall wrapper `{ fn; } || { ...; }` DISABLES
// set -e / the ERR trap inside the step function (bash semantics: any
// command in a `||` context is exempt). Every critical command in the
// keyframes body must therefore carry an explicit error handler — a
// swallowed rm/mv failure used to publish ENCODE_STEP 100 + JOB_DONE with
// no artifact, and the fresh-skip then made the hole permanent.

// kfTimedLine is one output line with its wall-clock arrival time — the
// liveness oracle for the progress-streaming regressions (markers must
// arrive DURING the run, not buffered until exit).
type kfTimedLine struct {
	at   time.Time
	text string
}

// kfRunTimed executes the rendered keyframes job (bash or PS) capturing
// per-line arrival timestamps. Returns the lines, the process exit time,
// and the run error.
func kfRunTimed(t *testing.T, e kfEnv, ps bool) ([]kfTimedLine, time.Time, error) {
	t.Helper()
	var cmd *exec.Cmd
	if ps {
		pwsh, err := exec.LookPath("pwsh")
		if err != nil {
			t.Skip("pwsh not installed; skipping keyframes PowerShell e2e")
		}
		libPath, err := filepath.Abs("../../../powershell/EncodeLib.ps1")
		if err != nil {
			t.Fatal(err)
		}
		scriptPath := filepath.Join(e.v.ScriptsDir, "job.ps1")
		if err := os.WriteFile(scriptPath, []byte(kfRender(t, e, true)), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd = exec.Command(pwsh, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", scriptPath, "-LibPath", libPath)
		cmd.Env = append(os.Environ(), "PATH="+e.path, "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1")
	} else {
		bash, err := exec.LookPath("bash")
		if err != nil {
			t.Skip("bash not installed; skipping keyframes bash e2e")
		}
		scriptPath := filepath.Join(e.v.ScriptsDir, "job.sh")
		if err := os.WriteFile(scriptPath, []byte(kfRender(t, e, false)), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd = exec.Command(bash, scriptPath)
		cmd.Env = append(os.Environ(), "PATH="+e.path)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		t.Fatal(err)
	}
	var mu sync.Mutex
	var lines []kfTimedLine
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			mu.Lock()
			lines = append(lines, kfTimedLine{at: time.Now(), text: sc.Text()})
			mu.Unlock()
		}
		close(done)
	}()
	runErr := cmd.Wait()
	exitAt := time.Now()
	pw.Close()
	<-done
	pr.Close()
	mu.Lock()
	defer mu.Unlock()
	return lines, exitAt, runErr
}

// kfProgressRe matches the intermediate progress markers the live reader
// emits (the final publish marker is 100).
var kfProgressRe = regexp.MustCompile(`^ENCODE_STEP keyframes (\d+)$`)

// kfAssertFailedClosed checks the failure contract: nonzero exit, failure
// marker, NO false completion marker, no JOB_DONE, no artifact file.
func kfAssertFailedClosed(t *testing.T, e kfEnv, text string, runErr error) {
	t.Helper()
	if runErr == nil {
		t.Errorf("expected nonzero exit, got success:\n%s", text)
	}
	if !strings.Contains(text, "ENCODE_STEP_FAILED keyframes") {
		t.Errorf("missing ENCODE_STEP_FAILED marker:\n%s", text)
	}
	if strings.Contains(text, "ENCODE_STEP keyframes 100") {
		t.Errorf("false completion marker emitted:\n%s", text)
	}
	if strings.Contains(text, "ENCODE_JOB_DONE") {
		t.Errorf("JOB_DONE emitted on a failed run:\n%s", text)
	}
	if fi, err := os.Stat(e.kfPath); err == nil && !fi.IsDir() {
		t.Errorf("artifact file must not exist after a failed run")
	}
}

// --- Q1: publish guards -----------------------------------------------------

// TestKeyframesBashE2EStaleDirectoryFailsClosed: a stale DIRECTORY named
// Keyframes.txt — rm -f cannot remove it, and the old unchecked body let
// mv -f move the log INSIDE the directory (exit 0) → false 100 + JOB_DONE
// with no artifact, made permanent by the fresh-skip. Must fail closed
// BEFORE vspipe runs.
func TestKeyframesBashE2EStaleDirectoryFailsClosed(t *testing.T) {
	e := kfSetup(t, false)
	if err := os.Mkdir(e.kfPath, 0o755); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(e.kfPath, past, past); err != nil {
		t.Fatal(err)
	}
	text, err := kfRunBash(t, e)
	t.Logf("output:\n%s", text)
	kfAssertFailedClosed(t, e, text, err)
	entries, rdErr := os.ReadDir(e.kfPath)
	if rdErr != nil {
		t.Fatal(rdErr)
	}
	if len(entries) != 0 {
		t.Errorf("log was moved INSIDE the stale directory: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(e.binDir, "vspipe_args.txt")); !os.IsNotExist(err) {
		t.Error("vspipe must NOT run when the stale artifact cannot be removed")
	}
}

// TestKeyframesBashE2EFreshDirectoryNotSkipped: a FRESH directory named
// Keyframes.txt must not satisfy the skip check (-e matches directories;
// only -f is a valid artifact). Old behavior: skip + 100, no artifact.
func TestKeyframesBashE2EFreshDirectoryNotSkipped(t *testing.T) {
	e := kfSetup(t, false)
	if err := os.Mkdir(e.kfPath, 0o755); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(1 * time.Hour)
	if err := os.Chtimes(e.kfPath, future, future); err != nil {
		t.Fatal(err)
	}
	text, err := kfRunBash(t, e)
	t.Logf("output:\n%s", text)
	if strings.Contains(text, "skipping") {
		t.Errorf("a directory was skipped as a fresh artifact:\n%s", text)
	}
	kfAssertFailedClosed(t, e, text, err)
}

// TestKeyframesBashE2EMvFailureFailsClosed: any mv failure (EIO/EPERM on
// CIFS/NFS, simulated with a failing mv stub) must fail the step — the old
// unchecked mv reported 100 with no artifact.
func TestKeyframesBashE2EMvFailureFailsClosed(t *testing.T) {
	e := kfSetup(t, false)
	if err := os.WriteFile(filepath.Join(e.binDir, "mv"), []byte("#!/usr/bin/env bash\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	text, err := kfRunBash(t, e)
	t.Logf("output:\n%s", text)
	kfAssertFailedClosed(t, e, text, err)
	kfAssertNoTempLeftovers(t, e)
}

// TestKeyframesBashE2ETempDirFailureFailsClosed: mktemp failure (episode
// dir not writable) must fail with an explicit guard — not continue with
// an empty $tmpd and write the .vpy to the filesystem root.
func TestKeyframesBashE2ETempDirFailureFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based mktemp failure not reproducible")
	}
	e := kfSetup(t, false)
	if err := os.Chmod(e.epDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(e.epDir, 0o755) })
	text, err := kfRunBash(t, e)
	t.Logf("output:\n%s", text)
	kfAssertFailedClosed(t, e, text, err)
	if !strings.Contains(text, "failed to create temp work dir") {
		t.Errorf("mktemp failure must be reported explicitly:\n%s", text)
	}
}

// --- Q2: live progress (bash) ----------------------------------------------

// TestKeyframesBashE2ELiveProgress: a vspipe stub emitting CR-terminated
// Frame: N/M lines (the real `vspipe -p` format) must produce periodic
// ENCODE_STEP keyframes <pct> markers DURING the run — increasing,
// throttled (fewer markers than frame lines), capped below 100 — with the
// final 100 still emitted after publish. The stall watchdog only refreshes
// last_progress_at on step/progress CHANGE, so a silent multi-minute run
// used to be failed + retry-burned.
func TestKeyframesBashE2ELiveProgress(t *testing.T) {
	e := kfSetup(t, false)
	kfSidecar(t, e, "vspipe_progress", "1")
	lines, exitAt, err := kfRunTimed(t, e, false)
	var text strings.Builder
	for _, l := range lines {
		text.WriteString(l.text)
		text.WriteString("\n")
	}
	t.Logf("output:\n%s", text.String())
	if err != nil {
		t.Fatalf("live-progress run failed: %v", err)
	}
	var pcts []int
	var firstMarkerAt time.Time
	sawFinal := false
	for _, l := range lines {
		m := kfProgressRe.FindStringSubmatch(l.text)
		if m == nil {
			continue
		}
		p, _ := strconv.Atoi(m[1])
		if p == 100 {
			sawFinal = true
			continue
		}
		if p == 0 {
			continue // the step-start marker, not live progress
		}
		pcts = append(pcts, p)
		if firstMarkerAt.IsZero() {
			firstMarkerAt = l.at
		}
	}
	if len(pcts) < 2 {
		t.Fatalf("expected periodic intermediate progress markers, got %d (buffered output?)", len(pcts))
	}
	if len(pcts) > 60 {
		t.Errorf("progress not throttled: %d markers for 100 frame lines", len(pcts))
	}
	for i, p := range pcts {
		if p < 1 || p > 99 {
			t.Errorf("intermediate marker out of range: %d", p)
		}
		if i > 0 && p <= pcts[i-1] {
			t.Errorf("progress markers not strictly increasing: %v", pcts)
			break
		}
	}
	// Liveness: the first intermediate marker must arrive well BEFORE the
	// process exits (the stub paces its first 5 frames over ~1.25s).
	if firstMarkerAt.IsZero() || exitAt.Sub(firstMarkerAt) < 300*time.Millisecond {
		t.Errorf("progress markers arrived only at/after exit (first=%v exit=%v) — output is buffered, not live",
			firstMarkerAt, exitAt)
	}
	if !sawFinal {
		t.Error("final ENCODE_STEP keyframes 100 missing after publish")
	}
	if _, err := os.Stat(e.kfPath); err != nil {
		t.Errorf("artifact not published: %v", err)
	}
	kfAssertNoTempLeftovers(t, e)
}

// TestKeyframesPSE2ELiveOutput: the PS body must echo SCXvid output lines
// AS THEY ARRIVE (line-by-line reads), not buffer everything until
// WaitForExit (ReadToEndAsync). SCXvid-standalone emits no parseable
// progress (verified against upstream scxvid.c), so no percentages are
// fabricated — but error/diagnostic lines must still stream live.
func TestKeyframesPSE2ELiveOutput(t *testing.T) {
	e := kfSetup(t, true)
	kfSidecar(t, e, "scxvid_slow_lines", "1")
	lines, exitAt, err := kfRunTimed(t, e, true)
	var text strings.Builder
	for _, l := range lines {
		text.WriteString(l.text)
		text.WriteString("\n")
	}
	t.Logf("output:\n%s", text.String())
	if err != nil {
		t.Fatalf("PS live-output run failed: %v", err)
	}
	var arrivals []time.Time
	for _, l := range lines {
		if strings.Contains(l.text, "SCXvid stub: line") {
			arrivals = append(arrivals, l.at)
		}
	}
	if len(arrivals) != 3 {
		t.Fatalf("expected 3 live SCXvid lines, got %d:\n%s", len(arrivals), text.String())
	}
	// The stub sleeps 0.4s AFTER the last line before exiting: with live
	// reads the last line arrives >= ~0.4s before exit; with ReadToEndAsync
	// everything lands at exit.
	if gap := exitAt.Sub(arrivals[2]); gap < 200*time.Millisecond {
		t.Errorf("last SCXvid line arrived only %v before exit — output buffered until WaitForExit", gap)
	}
	if spread := arrivals[2].Sub(arrivals[0]); spread < 400*time.Millisecond {
		t.Errorf("paced lines arrived within %v — not streamed live", spread)
	}
	if !strings.Contains(text.String(), "ENCODE_STEP keyframes 100") {
		t.Error("final completion marker missing")
	}
}

// TestKeyframesBashE2EProgressThenFailure: vspipe streams live progress
// and THEN crashes — the rc-file capture through the live pipeline must
// still fail the step (no false 100 from the progress markers), with the
// failure marker, no artifact and no temp debris.
func TestKeyframesBashE2EProgressThenFailure(t *testing.T) {
	e := kfSetup(t, false)
	kfSidecar(t, e, "vspipe_progress", "1")
	kfSidecar(t, e, "vspipe_exit", "7")
	text, err := kfRunBash(t, e)
	t.Logf("output:\n%s", text)
	kfAssertFailedClosed(t, e, text, err)
	if !strings.Contains(text, "vspipe exited with code 7") {
		t.Errorf("failure must carry vspipe's exit code through the pipeline:\n%s", text)
	}
	if !strings.Contains(text, "ENCODE_STEP keyframes 10") {
		t.Errorf("live progress markers should precede the failure:\n%s", text)
	}
	kfAssertNoTempLeftovers(t, e)
}

// --- Q4: log content validity ----------------------------------------------

// kfXvidLogStaticClip: a perfectly static clip — every frame encoded as
// b/p, ZERO scene changes (no 'i' lines). Unlike a header-only log this IS
// a complete run (one stats line per frame) and must publish successfully.
const kfXvidLogStaticClip = "# XviD 2pass stat file (core version 1.3.7)\n" +
	"# Please do not modify this file\n" +
	"\n" +
	"b 2 0 410 510 3266 512\n" +
	"p 2 0 395 525 3105 498\n" +
	"b 2 0 402 518 3180 501\n" +
	"b 2 0 388 532 3054 489\n"

// TestKeyframesE2EInvalidLogFailsClosed: junk, whitespace-only and
// header-only logs must fail closed in BOTH dialects. xvidcore writes the
// header at plugin CREATE (plugin_2pass1.c rc_2pass1_create) BEFORE any
// frame is processed — header-only means ZERO frames, not a static clip.
// The old -s / existence-only checks published all three with 100.
func TestKeyframesE2EInvalidLogFailsClosed(t *testing.T) {
	cases := []struct{ name, payload string }{
		{"junk_one_byte", "x"},
		{"whitespace_only", " \n\t\n  "},
		{"header_only", kfXvidLogHeaderOnly},
	}
	for _, ps := range []bool{false, true} {
		for _, c := range cases {
			t.Run(fmt.Sprintf("%s_ps=%v", c.name, ps), func(t *testing.T) {
				e := kfSetup(t, ps)
				kfSidecar(t, e, "kf_log_payload", c.payload)
				var text string
				var err error
				if ps {
					text, err = kfRunPS(t, e)
				} else {
					text, err = kfRunBash(t, e)
				}
				t.Logf("output:\n%s", text)
				kfAssertFailedClosed(t, e, text, err)
				if !strings.Contains(text, "no frame lines") {
					t.Errorf("failure must name the zero-frame condition:\n%s", text)
				}
				if !ps {
					kfAssertNoTempLeftovers(t, e)
				}
			})
		}
	}
}

// TestKeyframesE2EStaticClipLogPasses: a real static-clip log (b/p lines,
// no 'i' lines) is a valid artifact in both dialects — the content check
// must not over-reject zero-scene-change runs.
func TestKeyframesE2EStaticClipLogPasses(t *testing.T) {
	for _, ps := range []bool{false, true} {
		t.Run(fmt.Sprintf("ps=%v", ps), func(t *testing.T) {
			e := kfSetup(t, ps)
			kfSidecar(t, e, "kf_log_payload", kfXvidLogStaticClip)
			var text string
			var err error
			if ps {
				text, err = kfRunPS(t, e)
			} else {
				text, err = kfRunBash(t, e)
			}
			t.Logf("output:\n%s", text)
			if err != nil {
				t.Fatalf("static-clip log rejected: %v", err)
			}
			if !strings.Contains(text, "ENCODE_STEP keyframes 100") {
				t.Errorf("missing completion marker:\n%s", text)
			}
			got, err := os.ReadFile(e.kfPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != kfXvidLogStaticClip {
				t.Errorf("static-clip log not published byte-for-byte:\n%q", got)
			}
		})
	}
}

// --- Q5: hostile series name → flattened error echoes -----------------------

// kfRenderJob renders the single-step keyframes flow with an explicit job
// (the hostile-series tests need a Series the standard helper won't set).
func kfRenderJob(t *testing.T, e kfEnv, ps bool, job *model.Job) string {
	t.Helper()
	d := Dialect(BashDialect{})
	if ps {
		d = PowerShellDialect{}
	}
	script, err := RenderDialect(singleStepFlow("keyframes"), job, e.v, BuiltinResolver(), d)
	if err != nil {
		t.Fatalf("render keyframes: %v", err)
	}
	return script
}

// TestKeyframesE2EHostileSeriesFlattened: a Series carrying a newline +
// forged marker must not produce a line-start ENCODE_STEP marker when the
// missing-MKV error echoes the path. validateSeriesName now rejects such
// names at job create; flattening CR/LF in the echoed path is the
// defense-in-depth layer (both dialects).
func TestKeyframesE2EHostileSeriesFlattened(t *testing.T) {
	for _, ps := range []bool{false, true} {
		t.Run(fmt.Sprintf("ps=%v", ps), func(t *testing.T) {
			e := kfSetup(t, ps)
			job := linuxTestJob()
			job.Series = "x\nENCODE_STEP keyframes 100"
			script := kfRenderJob(t, e, ps, job)
			var text string
			var runErr error
			if ps {
				pwsh, err := exec.LookPath("pwsh")
				if err != nil {
					t.Skip("pwsh not installed")
				}
				libPath, err := filepath.Abs("../../../powershell/EncodeLib.ps1")
				if err != nil {
					t.Fatal(err)
				}
				scriptPath := filepath.Join(e.v.ScriptsDir, "job.ps1")
				if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(pwsh, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", scriptPath, "-LibPath", libPath)
				cmd.Env = append(os.Environ(), "PATH="+e.path, "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1")
				out, err := cmd.CombinedOutput()
				text, runErr = string(out), err
			} else {
				bash, err := exec.LookPath("bash")
				if err != nil {
					t.Skip("bash not installed")
				}
				scriptPath := filepath.Join(e.v.ScriptsDir, "job.sh")
				if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(bash, scriptPath)
				cmd.Env = append(os.Environ(), "PATH="+e.path)
				out, err := cmd.CombinedOutput()
				text, runErr = string(out), err
			}
			t.Logf("output:\n%s", text)
			if runErr == nil {
				t.Fatal("expected failure (release MKV path does not exist)")
			}
			for _, line := range strings.Split(text, "\n") {
				if strings.HasPrefix(line, "ENCODE_STEP keyframes 100") {
					t.Errorf("forged line-start marker survived:\n%s", text)
				}
			}
			if !strings.Contains(text, "ENCODE_STEP_FAILED keyframes") {
				t.Errorf("missing failure marker:\n%s", text)
			}
			// The hostile path must appear FLATTENED on the error line.
			if !strings.Contains(text, "release MKV not found:") ||
				!strings.Contains(text, "x ENCODE_STEP keyframes 100") {
				t.Errorf("error echo must carry the flattened path on one line:\n%s", text)
			}
		})
	}
}
