package agent

import (
	"strconv"
	"strings"
	"sync"
)

// progressRingLines is the number of recent output lines the tracker keeps
// for the heartbeat log tail. 40 matches the completion report's tail size
// so the live tail and the final tail look consistent in the UI.
const progressRingLines = 40

// progressTracker holds the latest ENCODE_STEP marker and a bounded ring of
// recent output lines for the live heartbeat report. It is written by the
// lineObserver (encode goroutine) and read by the heartbeat goroutine, so
// every method takes the mutex. Separate from a.mu so live progress reads
// never contend with job-state mutations (same rationale as rlGuard).
type progressTracker struct {
	mu   sync.Mutex
	step string   // latest step name ("" before the first marker)
	pct  float64  // latest reported percentage for that step
	ring []string // last N output lines, oldest first
	capN int      // ring capacity — tracked separately because append(ring[1:], x) can grow the backing array's cap, which would silently break a len==cap check
}

func newProgressTracker(capLines int) *progressTracker {
	if capLines <= 0 {
		capLines = progressRingLines
	}
	return &progressTracker{ring: make([]string, 0, capLines), capN: capLines}
}

// observe records one complete output line: it refreshes the ring and, when
// the line is an ENCODE_STEP marker, updates the current step/percentage.
// The marker regex is the shared stepLine (steptrack.go) so live progress and
// step timings can never disagree about the format.
func (p *progressTracker) observe(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.ring) >= p.capN {
		// Shift in place: copy drops the oldest line and keeps the same
		// backing array, so capacity can never grow past capN.
		copy(p.ring, p.ring[1:])
		p.ring[len(p.ring)-1] = line
	} else {
		p.ring = append(p.ring, line)
	}
	if m := stepLine.FindStringSubmatch(line); m != nil {
		p.step = m[1]
		if v, err := strconv.ParseFloat(m[2], 64); err == nil {
			p.pct = v
		}
	}
}

// snapshot returns the current step name, percentage, and the ring joined
// with newlines (oldest first). Safe to call from the heartbeat goroutine.
func (p *progressTracker) snapshot() (step string, pct float64, tail string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.step, p.pct, strings.Join(p.ring, "\n")
}

// reset clears all state. Called when a new job starts so a heartbeat during
// the next encode never reports the previous job's step/progress/tail.
func (p *progressTracker) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.step = ""
	p.pct = 0
	p.ring = p.ring[:0]
}
