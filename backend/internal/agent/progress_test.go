package agent

import (
	"strings"
	"testing"
)

// TestProgressTrackerRecordsStepAndTail verifies the tracker keeps the latest
// ENCODE_STEP marker (name + pct) and a bounded tail of recent output lines,
// all under its mutex so the heartbeat goroutine can snapshot concurrently.
func TestProgressTrackerRecordsStepAndTail(t *testing.T) {
	tr := newProgressTracker(5) // small ring for the test

	tr.observe("ENCODE_STEP dgindex 12.5")
	tr.observe("some noise line")
	tr.observe("ENCODE_STEP encode 40")

	step, pct, tail := tr.snapshot()
	if step != "encode" {
		t.Errorf("step = %q, want encode", step)
	}
	if pct != 40 {
		t.Errorf("pct = %v, want 40", pct)
	}
	// Tail holds the last N lines in order, newest last.
	lines := strings.Split(strings.TrimSpace(tail), "\n")
	if len(lines) != 3 {
		t.Fatalf("tail lines = %d, want 3: %q", len(lines), tail)
	}
	if lines[2] != "ENCODE_STEP encode 40" {
		t.Errorf("last tail line = %q", lines[2])
	}
}

// TestProgressTrackerRingBounded verifies the tail never grows past the ring
// size: after more observations than the cap, only the newest lines survive.
func TestProgressTrackerRingBounded(t *testing.T) {
	tr := newProgressTracker(3)
	for _, l := range []string{"one", "two", "three", "four", "five"} {
		tr.observe(l)
	}
	_, _, tail := tr.snapshot()
	lines := strings.Split(tail, "\n")
	if len(lines) != 3 {
		t.Fatalf("ring lines = %d, want 3: %q", len(lines), tail)
	}
	if lines[0] != "three" || lines[2] != "five" {
		t.Errorf("ring contents wrong: %q", tail)
	}
}

// TestProgressTrackerReset verifies reset clears step/progress/tail so a new
// job never inherits the previous job's progress state.
func TestProgressTrackerReset(t *testing.T) {
	tr := newProgressTracker(10)
	tr.observe("ENCODE_STEP encode 90")
	tr.reset()
	step, pct, tail := tr.snapshot()
	if step != "" || pct != 0 || tail != "" {
		t.Errorf("after reset: step=%q pct=%v tail=%q, want empty", step, pct, tail)
	}
}
