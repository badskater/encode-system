package agent

import (
	"strings"
	"sync"
)

// logRingLines is how many recent agent log lines the ring keeps for
// heartbeat shipping. 40 lines of JSON slog output stays comfortably
// bounded while covering the last few minutes of agent activity — enough
// to diagnose a wedged node without WinRM.
const logRingLines = 40

// logRingBytes caps the SERIALIZED tail attached to a heartbeat (the wire
// copy). A pathological log line (multi-KB error) must not balloon every
// 5-second heartbeat; the tail is truncated from the FRONT so the most
// recent lines always survive. Note the in-memory ring itself is bounded
// by LINE COUNT, not bytes — a fat line is retained whole until evicted —
// but the wire payload (and thus the controller's stored column) never
// exceeds this cap.
const logRingBytes = 8 * 1024

// LogRing is a mutex-guarded ring buffer of recent agent log lines. It is
// written by the slog handler (via its io.Writer face) and read by the
// heartbeat goroutine. In-memory only and bounded: observability data,
// never durable state.
//
// Write buffers partial trailing data so a line split across two Writes
// lands in the ring as ONE entry — the contract holds even for writers
// that do not emit whole lines (slog always does; subprocess tees may not).
type LogRing struct {
	mu      sync.Mutex
	lines   []string
	pending string // partial line not yet terminated by \n
}

// NewLogRing returns an empty, ready ring.
func NewLogRing() *LogRing { return &LogRing{} }

// Write implements io.Writer so the ring can tee the agent's slog output.
// It appends to a pending buffer and pushes each newline-terminated line as
// one ring entry, keeping the last logRingLines lines. Unterminated tail
// data stays pending until its line completes.
func (r *LogRing) Write(p []byte) (int, error) {
	n := len(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending += string(p)
	for {
		i := strings.IndexByte(r.pending, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(r.pending[:i], "\r")
		r.pending = r.pending[i+1:]
		if line == "" {
			continue
		}
		r.pushLocked(line)
	}
	// A pending buffer with no newline in sight cannot grow without bound:
	// cap it so a writer that never terminates lines is still contained.
	if len(r.pending) > logRingBytes {
		r.pending = r.pending[len(r.pending)-logRingBytes:]
	}
	return n, nil
}

// pushLocked appends one line, evicting the oldest when the ring is full.
// Caller holds r.mu.
func (r *LogRing) pushLocked(line string) {
	r.lines = append(r.lines, line)
	if len(r.lines) > logRingLines {
		r.lines = r.lines[len(r.lines)-logRingLines:]
	}
}

// Tail returns the buffered lines joined with newlines, oldest first,
// truncated to logRingBytes from the front. The unterminated pending
// fragment is included as the newest entry when non-empty (a live error
// mid-line is exactly what an operator wants to see). Empty string when
// nothing has been logged yet (the heartbeat omits the field entirely).
func (r *LogRing) Tail() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	lines := r.lines
	if r.pending != "" {
		lines = append(append([]string{}, r.lines...), r.pending)
	}
	if len(lines) == 0 {
		return ""
	}
	out := strings.Join(lines, "\n")
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
