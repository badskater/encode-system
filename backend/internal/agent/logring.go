package agent

import (
	"strings"
	"sync"
)

// logRingLines is how many recent agent log lines the ring keeps for
// heartbeat shipping. 40 lines of JSON slog output (~200 chars each) stays
// comfortably under the 8 KiB wire cap while covering the last few minutes
// of agent activity — enough to diagnose a wedged node without WinRM.
const logRingLines = 40

// logRingBytes caps the serialized tail attached to a heartbeat. A
// pathological log line (multi-KB PowerShell error) must not balloon every
// 5-second heartbeat; the tail is truncated from the FRONT so the most
// recent lines always survive.
const logRingBytes = 8 * 1024

// LogRing is a mutex-guarded ring buffer of recent agent log lines. It is
// written by the slog handler (via its io.Writer face) and read by the
// heartbeat goroutine. In-memory only and bounded: observability data,
// never durable state.
type LogRing struct {
	mu    sync.Mutex
	lines []string
}

// NewLogRing returns an empty, ready ring.
func NewLogRing() *LogRing { return &LogRing{} }

// Write implements io.Writer so the ring can tee the agent's slog output.
// It splits on newlines and keeps the last logRingLines non-empty lines.
// Partial trailing writes (no newline yet) are buffered into the last slot
// and replaced when the line completes — slog always emits whole lines, so
// in practice each Write is one record.
func (r *LogRing) Write(p []byte) (int, error) {
	n := len(p)
	for _, line := range strings.Split(string(p), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		r.push(line)
	}
	return n, nil
}

// push appends one line, evicting the oldest when the ring is full.
func (r *LogRing) push(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if len(r.lines) > logRingLines {
		r.lines = r.lines[len(r.lines)-logRingLines:]
	}
}

// Tail returns the buffered lines joined with newlines, oldest first,
// truncated to logRingBytes from the front. Empty string when nothing has
// been logged yet (the heartbeat omits the field entirely in that case).
func (r *LogRing) Tail() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.lines) == 0 {
		return ""
	}
	out := strings.Join(r.lines, "\n")
	if len(out) > logRingBytes {
		// Cut from the front on a line boundary so the tail never starts
		// mid-line (garbage first line in the UI).
		out = out[len(out)-logRingBytes:]
		if i := strings.IndexByte(out, '\n'); i >= 0 {
			out = out[i+1:]
		}
	}
	return out
}
