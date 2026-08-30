package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestCaptureRunLogSmallFileUntouched verifies that a run.log under the 1 MiB
// cap is returned verbatim, with no truncation marker prepended.
func TestCaptureRunLogSmallFileUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.log")
	content := "line one\nline two\nline three\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := captureRunLog(path)
	if err != nil {
		t.Fatalf("captureRunLog error: %v", err)
	}
	if got != content {
		t.Fatalf("small file must be verbatim; got %d bytes, want %d", len(got), len(content))
	}
	if strings.Contains(got, truncationMarker) {
		t.Fatalf("small file must not carry truncation marker: %q", got)
	}
}

// TestCaptureRunLogLargeFileTruncatesTo1MiB verifies that a file larger than
// 1 MiB is trimmed to at most 1 MiB, starts with the truncation marker, and
// never splits a line mid-way (cuts at the first newline after the byte cap).
func TestCaptureRunLogLargeFileTruncatesTo1MiB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.log")

	// Build a file well over 1 MiB: each line is "Lxxxxxxxxxx\n" (12 bytes),
	// with a unique suffix so we can detect mid-line splits.
	const lineLen = 12
	lineCount := (maxFullLogBytes / lineLen) + 2000
	var sb strings.Builder
	for i := 0; i < lineCount; i++ {
		sb.WriteString("L")
		sb.WriteString(padInt(i, lineLen-2))
		sb.WriteString("\n")
	}
	raw := sb.String()
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := captureRunLog(path)
	if err != nil {
		t.Fatalf("captureRunLog error: %v", err)
	}
	// The content portion (excluding the marker) must be <= 1 MiB. The
	// marker is prepended in ADDITION to the capped content.
	body := strings.TrimPrefix(got, truncationMarker)
	if len(body) > maxFullLogBytes {
		t.Fatalf("captured content = %d bytes, must be <= %d", len(body), maxFullLogBytes)
	}
	if !strings.HasPrefix(got, truncationMarker) {
		t.Fatalf("truncated file must start with marker %q, got prefix %q", truncationMarker, got[:min(len(got), len(truncationMarker)+20)])
	}
	// The content after the marker must end on a newline boundary (no partial line).
	if len(body) > 0 && !strings.HasSuffix(body, "\n") {
		t.Fatalf("captured log must end on newline boundary; last bytes: %q", body[len(body)-min(len(body), 40):])
	}
	// Every line in the body must be a complete line: the line content
	// (sans newline) is "L" + 10 digits = lineLen-1 chars.
	lineContentLen := lineLen - 1
	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		if line == "" {
			continue
		}
		if len(line) != lineContentLen || !strings.HasPrefix(line, "L") {
			t.Fatalf("mid-line split detected: line %q (len %d) is not %d chars starting with L", line, len(line), lineContentLen)
		}
	}
}

// TestCaptureRunLogMissing verifies that a nonexistent run.log yields an empty
// string (and no error that would fail the completion report).
func TestCaptureRunLogMissing(t *testing.T) {
	dir := t.TempDir()
	got, err := captureRunLog(filepath.Join(dir, "does-not-exist.log"))
	if err != nil {
		t.Fatalf("missing file must not error (completion must still succeed): %v", err)
	}
	if got != "" {
		t.Fatalf("missing file must return empty string, got %d bytes", len(got))
	}
}

// TestStepTimingTracker verifies the ordered step tracker: first-seen-wins on
// duplicate names, and durations are computed from consecutive start times
// with the last step's duration measured from a provided finish time.
func TestStepTimingTracker(t *testing.T) {
	tr := newStepTracker()

	t1 := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(30 * time.Second)
	t3 := t1.Add(75 * time.Second)
	finish := t1.Add(120 * time.Second)

	// Feed marker lines: first-seen wins (dgindex appears twice with different pct).
	tr.observe("ENCODE_STEP dgindex 5", t1)
	tr.observe("ENCODE_STEP dgindex 50", t1.Add(5*time.Second)) // duplicate name -> ignored
	tr.observe("ENCODE_STEP encode 50", t2)
	tr.observe("ENCODE_STEP mux 90", t3)

	timings := tr.finish(finish)
	if len(timings) != 3 {
		t.Fatalf("timings count = %d, want 3", len(timings))
	}

	want := []struct {
		step string
		dur  float64
	}{
		{"dgindex", t2.Sub(t1).Seconds()}, // 30s
		{"encode", t3.Sub(t2).Seconds()},  // 45s
		{"mux", finish.Sub(t3).Seconds()}, // 45s
	}
	for i, w := range want {
		if timings[i].Step != w.step {
			t.Errorf("timings[%d].Step = %q, want %q", i, timings[i].Step, w.step)
		}
		if timings[i].DurationSec != w.dur {
			t.Errorf("timings[%d].DurationSec = %v, want %v", i, timings[i].DurationSec, w.dur)
		}
	}
	// StartedAt for dgindex must be t1 (first-seen), not t1+5s.
	if !timings[0].StartedAt.Equal(t1) {
		t.Errorf("dgindex StartedAt = %v, want %v (first-seen wins)", timings[0].StartedAt, t1)
	}
}

// TestStepTimingTrackerZeroMarkers verifies that a job with no ENCODE_STEP
// markers produces an empty (non-nil) slice.
func TestStepTimingTrackerZeroMarkers(t *testing.T) {
	tr := newStepTracker()
	timings := tr.finish(time.Now())
	if len(timings) != 0 {
		t.Fatalf("zero markers must yield empty slice, got %d", len(timings))
	}
}

// TestStepTimingTrackerSingleStep verifies the single-step case: its duration
// is finish - start.
func TestStepTimingTrackerSingleStep(t *testing.T) {
	tr := newStepTracker()
	start := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	finish := start.Add(60 * time.Second)
	tr.observe("ENCODE_STEP encode 0", start)
	timings := tr.finish(finish)
	if len(timings) != 1 {
		t.Fatalf("timings count = %d, want 1", len(timings))
	}
	if timings[0].Step != "encode" {
		t.Fatalf("step = %q, want encode", timings[0].Step)
	}
	if timings[0].DurationSec != 60 {
		t.Fatalf("duration = %v, want 60", timings[0].DurationSec)
	}
}

// TestStepTimingTrackerStepNameFirstTokenOnly verifies that a marker line
// "ENCODE_STEP <name> <pct>" takes only the first token after ENCODE_STEP as
// the step name (matching the existing progress regex), so step names never
// contain spaces.
func TestStepTimingTrackerStepNameFirstTokenOnly(t *testing.T) {
	tr := newStepTracker()
	now := time.Now()
	// The existing regex `ENCODE_STEP (\w+) (\d+...)` captures \w+ — a name
	// with a space would not match at all. Verify our tracker uses the same
	// rule: a bare word token.
	tr.observe("ENCODE_STEP dgindex 5", now)
	timings := tr.finish(now.Add(time.Second))
	if len(timings) != 1 || timings[0].Step != "dgindex" {
		t.Fatalf("step name = %q, want dgindex", timings[0].Step)
	}
}

// TestCompleteReportIncludesFullLogAndStepTimings is the wire-level test: the
// mock controller captures the POSTed JSON and asserts that "log_full" and
// "step_timings" are present with expected values for a tiny run. We drive the
// completion path directly (not executeJob, which needs real PowerShell) by
// invoking completeJob with a captured fullLog and stepTimings.
func TestCompleteReportIncludesFullLogAndStepTimings(t *testing.T) {
	dir := t.TempDir()
	// Write a small run.log that captureRunLog will read.
	runLogPath := filepath.Join(dir, "jobs", "42", "run.log")
	os.MkdirAll(filepath.Dir(runLogPath), 0o755)
	logContent := "ENCODE_STEP encode 50\nENCODE_JOB_DONE\n"
	os.WriteFile(runLogPath, []byte(logContent), 0o644)

	fullLog, err := captureRunLog(runLogPath)
	if err != nil {
		t.Fatal(err)
	}

	timings := []model.StepTiming{
		{Step: "encode", StartedAt: time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC), DurationSec: 30.0},
	}

	var capturedBody map[string]any
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/complete") {
			raw, _ := io.ReadAll(r.Body)
			json.Unmarshal(raw, &capturedBody)
		}
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer controller.Close()

	a, _ := New(Config{
		ControllerURL: controller.URL, NodeName: "n", Token: nodeTok(), DataDir: dir,
		LibPath: filepath.Join(dir, "EncodeLib.ps1"),
	}, "v", testLog())

	a.completeJob(42, "done", 0, "", []string{"out.mkv"}, "tail-line", fullLog, timings)

	if capturedBody == nil {
		t.Fatal("no completion body captured")
	}
	if lf, ok := capturedBody["log_full"].(string); !ok || lf != logContent {
		t.Fatalf("log_full = %v, want %q", capturedBody["log_full"], logContent)
	}
	rawTimings, ok := capturedBody["step_timings"].([]any)
	if !ok {
		t.Fatalf("step_timings missing or wrong type: %T", capturedBody["step_timings"])
	}
	if len(rawTimings) != 1 {
		t.Fatalf("step_timings len = %d, want 1", len(rawTimings))
	}
	st := rawTimings[0].(map[string]any)
	if st["step"] != "encode" {
		t.Fatalf("step_timings[0].step = %v, want encode", st["step"])
	}
	// Also verify the legacy fields still present (backward compat).
	if capturedBody["log_tail"] == nil {
		t.Fatal("log_tail must still be present for backward compat")
	}
	if capturedBody["outputs"] == nil {
		t.Fatal("outputs must still be present for backward compat")
	}
}

func padInt(n, width int) string {
	s := []byte{}
	for i := 0; i < width-len(itoa(n)); i++ {
		s = append(s, '0')
	}
	return string(s) + itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestStepTrackerObservesFromScanLine verifies that a raw output line matching
// the ENCODE_STEP regex feeds the tracker with the correct step name, using
// the same regex the existing scan uses (stepLine). This guards the wiring
// contract: the tracker must be fed by the SAME line scan, not a second pass.
func TestStepTrackerObservesFromScanLine(t *testing.T) {
	tr := newStepTracker()
	now := time.Now()
	lines := []string{
		"some preamble",
		"ENCODE_STEP dgindex 5",
		"ENCODE_STEP dgindex 50",
		"ENCODE_STEP encode 50",
		"ENCODE_STEP mux 90",
		"ENCODE_STEP_FAILED encode boom",
	}
	for _, l := range lines {
		if m := stepLine.FindStringSubmatch(l); m != nil {
			tr.observe(l, now)
		}
	}
	timings := tr.finish(now.Add(100 * time.Second))
	if len(timings) != 3 {
		t.Fatalf("timings = %d, want 3", len(timings))
	}
	wantSteps := []string{"dgindex", "encode", "mux"}
	for i, s := range wantSteps {
		if timings[i].Step != s {
			t.Errorf("timings[%d].Step = %q, want %q", i, timings[i].Step, s)
		}
	}
}
