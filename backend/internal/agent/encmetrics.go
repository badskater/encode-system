package agent

import (
	"regexp"
	"strconv"
	"sync"
)

// maxMetrics bounds the per-job metric map: a buggy or malicious script
// spewing ENCODE_METRIC lines must not grow agent memory without limit.
// 64 keys is far beyond any realistic encode report (vmaf, sizes, rates,
// durations, frame counts).
const maxMetrics = 64

// metricLine matches "ENCODE_METRIC key=value" anywhere in a line (x265
// prefixes its own tags before script output is echoed). Key is a simple
// identifier; value is a float. Mirrors the stepLine regex's tolerance.
var metricLine = regexp.MustCompile(`ENCODE_METRIC\s+([A-Za-z_][A-Za-z0-9_.]*)\s*=\s*(-?\d+(?:\.\d+)?)`)

// metricTracker accumulates ENCODE_METRIC key=value pairs from live job
// output. Same design as progressTracker: mutex-guarded, fed by the
// lineObserver as lines arrive, snapshotted at completion. Duplicate keys
// take the LAST value so a step can re-report a refined measurement
// (e.g. a second VMAF pass superseding a preview score).
type metricTracker struct {
	mu sync.Mutex
	m  map[string]float64
}

func newMetricTracker() *metricTracker {
	return &metricTracker{m: map[string]float64{}}
}

// observe records every ENCODE_METRIC pair on the line (a line may carry
// one; multi-pair lines are tolerated). Non-matching lines are ignored.
func (t *metricTracker) observe(line string) {
	mm := metricLine.FindAllStringSubmatch(line, -1)
	if mm == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, m := range mm {
		if len(t.m) >= maxMetrics {
			if _, exists := t.m[m[1]]; !exists {
				continue // at cap: only updates to known keys get through
			}
		}
		v, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			continue
		}
		t.m[m[1]] = v
	}
}

// snapshot returns a copy of the accumulated metrics (never nil).
func (t *metricTracker) snapshot() map[string]float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]float64, len(t.m))
	for k, v := range t.m {
		out[k] = v
	}
	return out
}

// reset clears state for a new job (trackers are reused across jobs when
// the agent struct is long-lived).
func (t *metricTracker) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m = map[string]float64{}
}
