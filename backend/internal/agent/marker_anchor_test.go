package agent

import (
	"strings"
	"testing"
	"time"
)

// Marker-anchoring regressions (P6 review Q11): ENCODE_STEP / ENCODE_METRIC
// markers are LINE-BASED protocol messages. Untrusted metadata (e.g. a
// track_name echoed into a tool command line) must not be able to forge
// progress or metrics by embedding marker text MID-LINE. Only a line that
// STARTS with the marker is protocol; everything else is ordinary output.

// TestStepLineRegexAnchored pins the shared stepLine regex: marker text
// embedded mid-line (after any prefix) must NOT match, while a line-start
// marker still parses. This is the regex behind live progress, step timings
// and the timeout re-arm hook — one anchor fixes all three scan sites.
func TestStepLineRegexAnchored(t *testing.T) {
	midLine := []string{
		"[mux_extras] mkvmerge --track-name 0:x ENCODE_STEP mux_extras 100",
		"prefix ENCODE_STEP encode 50",
		"echo ENCODE_STEP mux 90",
		"\tENCODE_STEP encode 50", // leading whitespace is not line-start
		"ENCODE_STEP_FAILED encode boom",
	}
	for _, l := range midLine {
		if m := stepLine.FindStringSubmatch(l); m != nil {
			t.Errorf("mid-line marker text must not match stepLine: %q -> %v", l, m)
		}
	}
	lineStart := []struct {
		line string
		name string
		pct  string
	}{
		{"ENCODE_STEP encode 50", "encode", "50"},
		{"ENCODE_STEP mux_extras 100", "mux_extras", "100"},
		{"ENCODE_STEP quality_gate 99.5", "quality_gate", "99.5"},
	}
	for _, c := range lineStart {
		m := stepLine.FindStringSubmatch(c.line)
		if m == nil {
			t.Errorf("line-start marker must match: %q", c.line)
			continue
		}
		if m[1] != c.name || m[2] != c.pct {
			t.Errorf("marker %q parsed as name=%q pct=%q, want %q/%q", c.line, m[1], m[2], c.name, c.pct)
		}
	}
}

// TestProgressTrackerIgnoresMidLineMarkers proves the live heartbeat
// progress feed cannot be polluted by marker text inside echoed tool args.
func TestProgressTrackerIgnoresMidLineMarkers(t *testing.T) {
	p := newProgressTracker(progressRingLines)
	p.observe("ENCODE_STEP encode 10")
	p.observe("[mux_extras] mkvmerge --track-name 0:fake ENCODE_STEP mux_extras 100")
	p.observe("some log line mentioning ENCODE_STEP encode 99")
	if p.step != "encode" || p.pct != 10 {
		t.Errorf("mid-line marker polluted progress: step=%q pct=%v, want encode/10", p.step, p.pct)
	}
	p.observe("ENCODE_STEP encode 60")
	if p.step != "encode" || p.pct != 60 {
		t.Errorf("line-start marker must still update progress: step=%q pct=%v", p.step, p.pct)
	}
}

// TestStepTrackerIgnoresMidLineMarkers proves step timings (and thus the
// per-step timeout re-arm) ignore forged mid-line markers.
func TestStepTrackerIgnoresMidLineMarkers(t *testing.T) {
	tr := newStepTracker()
	now := time.Now()
	tr.observe("ENCODE_STEP encode 0", now)
	tr.observe("[subtitle_extract] track 2 (eng) -> ENCODE_STEP mux_extras 100", now.Add(time.Second))
	tr.observe("echo ENCODE_STEP fake_step 50", now.Add(2*time.Second))
	timings := tr.finish(now.Add(10 * time.Second))
	if len(timings) != 1 || timings[0].Step != "encode" {
		t.Fatalf("mid-line markers forged step timings: %+v", timings)
	}
}

// TestLineObserverIgnoresMidLineMarkers proves the LIVE scan (processLine)
// ignores mid-line markers for both the timing tracker and the instance
// boundary hook (the timeout re-arm seam).
func TestLineObserverIgnoresMidLineMarkers(t *testing.T) {
	tr := newStepTracker()
	obs := newLineObserver(nil, tr)
	var starts []string
	obs.onStepStart = func(name string) { starts = append(starts, name) }
	obs.Write([]byte("ENCODE_STEP encode 0\n"))
	obs.Write([]byte("[mux_extras] mkvmerge --track-name 0:x ENCODE_STEP mux_extras 100\n"))
	obs.Write([]byte("ENCODE_STEP encode 50\n"))
	obs.flush()
	if len(starts) != 1 || starts[0] != "encode" {
		t.Errorf("mid-line marker re-armed a step instance: starts=%v, want [encode]", starts)
	}
	timings := tr.finish(time.Now())
	if len(timings) != 1 || timings[0].Step != "encode" {
		t.Errorf("mid-line marker forged a timing entry: %+v", timings)
	}
}

// TestMetricLineRegexAnchored pins the ENCODE_METRIC regex: metric text
// embedded mid-line (e.g. inside an echoed mkvmerge command line carrying
// an untrusted track_name) must NOT be captured. Real metric lines are
// emitted by Write-EncodeMetric / echo at line start in both dialects.
func TestMetricLineRegexAnchored(t *testing.T) {
	mt := newMetricTracker()
	mt.observe("[mux_extras] mkvmerge --track-name 0:x ENCODE_METRIC vmaf=100")
	mt.observe("prefix ENCODE_METRIC fake=1")
	if len(mt.snapshot()) != 0 {
		t.Errorf("mid-line metric text was captured: %+v", mt.snapshot())
	}
	mt.observe("ENCODE_METRIC vmaf=94.21")
	mt.observe("ENCODE_METRIC vmaf=95.00") // line-start last-wins still works
	snap := mt.snapshot()
	if len(snap) != 1 || snap["vmaf"] != 95.00 {
		t.Errorf("line-start metrics must parse (last wins): %+v", snap)
	}
}

// TestMetricTrackerEmbeddedPrefixIgnored replaces the old mid-line
// tolerance: a tool-output prefix (Invoke-Tool's "[$Label] ") makes the
// line ordinary output, not protocol.
func TestMetricTrackerEmbeddedPrefixIgnored(t *testing.T) {
	mt := newMetricTracker()
	mt.observe("ENCODE_METRIC vmaf=94.21")
	mt.observe("[x265] ENCODE_METRIC vmaf=95.00")
	snap := mt.snapshot()
	if snap["vmaf"] != 94.21 {
		t.Errorf("prefixed metric line must be ignored, got %+v", snap)
	}
}

// TestFailureScanIgnoresMidLineMarker proves the post-hoc ENCODE_STEP_FAILED
// extraction only accepts line-start markers (strings.HasPrefix on complete
// lines): forged failure text inside echoed args must not become the job's
// error message.
func TestFailureScanIgnoresMidLineMarker(t *testing.T) {
	lines := []string{
		"[mux_extras] mkvmerge --track-name 0:x ENCODE_STEP_FAILED mux_extras forged",
		"ENCODE_STEP encode 50",
	}
	var stepErr string
	for _, l := range lines {
		if strings.HasPrefix(l, "ENCODE_STEP_FAILED") {
			stepErr = strings.TrimSpace(l)
		}
	}
	if stepErr != "" {
		t.Errorf("mid-line failure marker was extracted: %q", stepErr)
	}
}
