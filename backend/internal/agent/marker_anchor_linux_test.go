//go:build linux

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunBashMidLineMarkersIgnored drives a REAL bash job through runBash
// (the full lineObserver + post-hoc scan pipeline) whose output embeds
// ENCODE_STEP / ENCODE_STEP_FAILED / ENCODE_METRIC text MID-LINE — exactly
// what an echoed mkvmerge command line with a hostile track_name produces.
// None of it may affect progress, timings, metrics or the failure reason.
func TestRunBashMidLineMarkersIgnored(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "job.sh")
	body := "#!/usr/bin/env bash\n" +
		"echo 'ENCODE_STEP encode 0'\n" +
		"echo '[mux_extras] mkvmerge --track-name 0:x ENCODE_STEP mux_extras 100'\n" +
		"echo '[mux_extras] mkvmerge --track-name 0:x ENCODE_STEP_FAILED mux_extras forged'\n" +
		"echo '[mux_extras] mkvmerge --track-name 0:x ENCODE_METRIC vmaf=100'\n" +
		"echo 'ENCODE_STEP encode 100'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	runLog := filepath.Join(dir, "run.log")
	a, err := New(Config{ControllerURL: "http://127.0.0.1:0", NodeName: "n", Token: nodeTok(), DataDir: dir}, "v", testLog())
	if err != nil {
		t.Fatal(err)
	}
	prog := newProgressTracker(progressRingLines)
	metrics := newMetricTracker()
	exitCode, _, stepErr, timings := a.runBash(script, runLog, prog, metrics, context.Background(), jobLimits{})
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}
	if stepErr != "" {
		t.Errorf("mid-line ENCODE_STEP_FAILED became the error: %q", stepErr)
	}
	if prog.step != "encode" || prog.pct != 100 {
		t.Errorf("progress polluted: step=%q pct=%v, want encode/100", prog.step, prog.pct)
	}
	if snap := metrics.snapshot(); len(snap) != 0 {
		t.Errorf("mid-line ENCODE_METRIC captured: %+v", snap)
	}
	fin := timings.finish(time.Now())
	if len(fin) != 1 || fin[0].Step != "encode" {
		t.Errorf("mid-line markers forged timings: %+v", fin)
	}
}

// TestRunBashLineStartFailureMarkerCaptured is the positive control: a
// genuine line-start ENCODE_STEP_FAILED still becomes the job error.
func TestRunBashLineStartFailureMarkerCaptured(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "job.sh")
	body := "#!/usr/bin/env bash\n" +
		"echo 'ENCODE_STEP encode 0'\n" +
		"echo 'ENCODE_STEP_FAILED encode real failure'\n" +
		"exit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	runLog := filepath.Join(dir, "run.log")
	a, err := New(Config{ControllerURL: "http://127.0.0.1:0", NodeName: "n", Token: nodeTok(), DataDir: dir}, "v", testLog())
	if err != nil {
		t.Fatal(err)
	}
	exitCode, _, stepErr, _ := a.runBash(script, runLog, nil, nil, context.Background(), jobLimits{})
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	if !strings.HasPrefix(stepErr, "ENCODE_STEP_FAILED encode real failure") {
		t.Errorf("line-start failure marker not captured: %q", stepErr)
	}
}
