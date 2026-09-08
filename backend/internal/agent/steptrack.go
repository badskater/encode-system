package agent

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"strings"
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
// the truncation marker. A read failure returns the error so the caller
// (executeJob) can log a warning and degrade to an empty string — the
// completion still succeeds, but operators see that the log was not captured.
func captureRunLog(path string) (string, error) {
	// Bounded read: run.log for a multi-hour 4K encode can reach gigabytes,
	// so stat first and read only the tail window when the file exceeds the
	// cap (os.ReadFile of the whole log would spike the agent's RSS and can
	// OOM the node — the cap exists precisely because these logs are big).
	f, err := os.Open(path)
	if err != nil {
		// Surface the read failure so executeJob's warning branch fires and
		// operators can investigate; the caller degrades to "" + warn.
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	var data []byte
	if st.Size() <= int64(maxFullLogBytes) {
		data, err = io.ReadAll(f)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	// Large file: seek to end-(cap+slack) so the rune-align + newline scan
	// below can still land within the cap. The extra slack (64 bytes) covers
	// a worst-case rune boundary + newline skip eating into the window.
	const alignSlack = 64
	off := st.Size() - int64(maxFullLogBytes) - alignSlack
	if off < 0 {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "", err
	}
	data, err = io.ReadAll(f)
	if err != nil {
		return "", err
	}
	// Trim to the last 1 MiB, then advance to the first newline so we never
	// split a line mid-way. The newline itself is consumed (not included in
	// the output body start) so the body begins at a full line.
	cut := len(data) - maxFullLogBytes
	if cut < 0 {
		cut = 0
	}
	// Align the byte cut to a rune boundary: if it landed inside a multibyte
	// UTF-8 sequence, skip forward over continuation bytes (0x80-0xBF) so the
	// truncated body never starts mid-rune. Encode logs carry CJK series
	// names, so a raw byte offset routinely splits a 3-byte rune and would
	// persist/serve invalid UTF-8. Advancing at most 3 bytes (a continuation
	// byte can never start a valid sequence); kept size only shrinks (≤ 1 MiB).
	for cut < len(data) && data[cut]&0xC0 == 0x80 {
		cut++
	}
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

// lineObserver is an io.Writer that tees child-process output live: it
// accumulates ALL bytes into an internal buffer (runPowerShell still needs the
// full output afterwards for tail extraction and the ENCODE_STEP_FAILED scan)
// AND, for each complete line as it arrives, forwards ENCODE_STEP markers to
// the agent log and the step-timing tracker at time.Now() — the arrival
// instant, NOT a single post-hoc timestamp after cmd.Wait.
//
// Lines are split on '\n'. A Write may end mid-line; the trailing partial is
// held in remainder and completed by the next Write. flush() must be called
// after cmd.Wait to process any final partial line that never got a newline.
type lineObserver struct {
	log       *slog.Logger // agent logger for progress (may be nil in tests)
	timings   *stepTimingTracker
	buf       bytes.Buffer // accumulates ALL output verbatim
	remainder string       // trailing partial line from the last Write
}

// newLineObserver creates a live-scanning writer. log may be nil (unit tests
// that only care about the tracker); timings must be non-nil.
func newLineObserver(log *slog.Logger, timings *stepTimingTracker) *lineObserver {
	return &lineObserver{log: log, timings: timings}
}

// Write implements io.Writer. It appends p to the internal buffer (so
// String() returns the full output) and processes every complete line now —
// at arrival time — stamping the tracker with time.Now().
func (o *lineObserver) Write(p []byte) (int, error) {
	o.buf.Write(p) // accumulate ALL bytes verbatim

	data := o.remainder + string(p)
	o.remainder = ""
	for {
		nl := strings.IndexByte(data, '\n')
		if nl < 0 {
			// No more complete lines: keep the rest as the partial remainder.
			o.remainder = data
			break
		}
		line := data[:nl]
		o.processLine(line)
		data = data[nl+1:]
	}
	return len(p), nil
}

// flush processes any trailing partial line that never received a newline
// (the final Write ended mid-line). Must be called once after cmd.Wait.
func (o *lineObserver) flush() {
	if o.remainder != "" {
		o.processLine(o.remainder)
		o.remainder = ""
	}
}

// processLine checks a single complete line against the ENCODE_STEP regex and,
// on match, logs progress and feeds the timing tracker at time.Now() (the
// line's arrival instant).
func (o *lineObserver) processLine(line string) {
	m := stepLine.FindStringSubmatch(line)
	if m == nil {
		return
	}
	if o.log != nil {
		o.log.Info("job progress", "step", m[1], "pct", m[2])
	}
	o.timings.observe(line, time.Now())
}

// String returns the full accumulated output, reassembled exactly — no lost
// or duplicated bytes — for the post-Wait tail extraction and failure scan.
func (o *lineObserver) String() string {
	return o.buf.String()
}
