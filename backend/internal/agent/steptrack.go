package agent

import (
	"os"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// maxFullLogBytes caps the full run.log snapshot sent in the job completion
// report. 1 MiB is enough to diagnose any encode failure while keeping the
// POST body bounded — a multi-gigabyte log would stall the controller.
const maxFullLogBytes = 1 << 20 // 1 MiB

// truncationMarker is prepended when a run.log exceeds the cap so the UI can
// show that content was elided (the tail is the most recent, relevant part).
const truncationMarker = "[…truncated…]\n"

// captureRunLog reads the job's run.log and returns at most the last 1 MiB.
// If the file is larger than the cap, it is trimmed to the first newline
// boundary after the cap (never splitting a line mid-way) and prefixed with
// the truncation marker. A missing file yields ("", nil) so a log-read
// failure never aborts the completion report.
func captureRunLog(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		// Missing or unreadable log: report empty rather than failing the
		// completion. The tail (captured separately) may still carry signal.
		return "", nil
	}
	if len(data) <= maxFullLogBytes {
		return string(data), nil
	}
	// Trim to the last 1 MiB, then advance to the first newline so we never
	// split a line mid-way. The newline itself is consumed (not included in
	// the output body start) so the body begins at a full line.
	cut := len(data) - maxFullLogBytes
	nl := indexByteFrom(data, cut, '\n')
	if nl >= 0 {
		data = data[nl+1:]
	} else {
		// No newline in the tail window: take the whole tail verbatim.
		data = data[cut:]
	}
	return truncationMarker + string(data), nil
}

// indexByteFrom returns the index of c in data starting from offset, or -1.
func indexByteFrom(data []byte, offset int, c byte) int {
	for i := offset; i < len(data); i++ {
		if data[i] == c {
			return i
		}
	}
	return -1
}

// stepTimingTracker records the first-seen wall-clock time for each ENCODE_STEP
// marker name in order. Steps re-report progress at higher percentages; the
// first occurrence marks the step's start, so duplicates are ignored.
type stepTimingTracker struct {
	order  []string        // preserves first-seen order for deterministic output
	seen   map[string]bool // dedup by step name
	starts map[string]time.Time
}

func newStepTracker() *stepTimingTracker {
	return &stepTimingTracker{
		seen:   make(map[string]bool),
		starts: make(map[string]time.Time),
	}
}

// observe feeds a raw output line into the tracker. If the line matches the
// ENCODE_STEP marker and the step name has not been seen, the step's start
// time is recorded. The step name is the first token after "ENCODE_STEP"
// (matching the existing stepLine regex which captures \w+). The timestamp is
// injected (not time.Now) so tests can control durations.
func (t *stepTimingTracker) observe(line string, at time.Time) {
	m := stepLine.FindStringSubmatch(line)
	if m == nil {
		return
	}
	name := m[1]
	if t.seen[name] {
		return
	}
	t.seen[name] = true
	t.starts[name] = at
	t.order = append(t.order, name)
}

// finish converts the recorded starts into []model.StepTiming. The duration
// of each step is the gap to the next step's start; the last step's duration
// is finish minus its start. Steps with no successor and no finish get 0.
func (t *stepTimingTracker) finish(finish time.Time) []model.StepTiming {
	if len(t.order) == 0 {
		return []model.StepTiming{}
	}
	out := make([]model.StepTiming, 0, len(t.order))
	for i, name := range t.order {
		start := t.starts[name]
		var dur float64
		if i+1 < len(t.order) {
			next := t.starts[t.order[i+1]]
			dur = next.Sub(start).Seconds()
		} else if !finish.IsZero() {
			dur = finish.Sub(start).Seconds()
		}
		out = append(out, model.StepTiming{
			Step:        name,
			StartedAt:   start,
			DurationSec: dur,
		})
	}
	return out
}
