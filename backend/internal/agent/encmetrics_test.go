package agent

import (
	"testing"
)

// TestMetricTrackerParsing pins the ENCODE_METRIC protocol: key=value
// lines accumulate into a float map; unknown/malformed lines are ignored;
// duplicate keys take the LAST value (a step may re-report a refined
// measurement, e.g. vmaf after a second pass).
func TestMetricTrackerParsing(t *testing.T) {
	mt := newMetricTracker()
	mt.observe("ENCODE_METRIC vmaf=94.21")
	mt.observe("ENCODE_METRIC output_bitrate_kbps=8123")
	mt.observe("ENCODE_METRIC duration_sec=1440.5")
	mt.observe("ENCODE_STEP encode 50")           // not a metric
	mt.observe("ENCODE_METRIC bogus")             // no '='
	mt.observe("ENCODE_METRIC nan=notanumber")    // bad float
	mt.observe("[x265] ENCODE_METRIC vmaf=95.00") // embedded prefix ok, last wins
	mt.observe("ENCODE_METRIC  spaced = 1.5")     // spaces around '='

	got := mt.snapshot()
	if got["vmaf"] != 95.0 {
		t.Fatalf("vmaf = %v, want 95 (last wins)", got["vmaf"])
	}
	if got["output_bitrate_kbps"] != 8123 {
		t.Fatalf("bitrate = %v", got["output_bitrate_kbps"])
	}
	if got["duration_sec"] != 1440.5 {
		t.Fatalf("duration = %v", got["duration_sec"])
	}
	if got["spaced"] != 1.5 {
		t.Fatalf("spaced = %v, want 1.5 (trimmed)", got["spaced"])
	}
	if _, ok := got["bogus"]; ok {
		t.Fatal("bogus key recorded")
	}
	if _, ok := got["nan"]; ok {
		t.Fatal("non-numeric value recorded")
	}
	if len(got) != 4 {
		t.Fatalf("metrics = %v, want exactly 4 keys", got)
	}
}

// TestMetricTrackerCap bounds the map so a pathological script spewing
// ENCODE_METRIC lines cannot grow agent memory without limit.
func TestMetricTrackerCap(t *testing.T) {
	mt := newMetricTracker()
	for i := 0; i < maxMetrics+50; i++ {
		mt.observe("ENCODE_METRIC k" + metricKeyItoa(i) + "=1")
	}
	if got := len(mt.snapshot()); got != maxMetrics {
		t.Fatalf("tracker holds %d metrics, want cap %d", got, maxMetrics)
	}
}

func metricKeyItoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestObserverFeedsMetrics verifies the lineObserver wires metric lines
// into the tracker alongside step timings (same feed path).
func TestObserverFeedsMetrics(t *testing.T) {
	timings := newStepTracker()
	mt := newMetricTracker()
	o := newLineObserver(testLog(), timings)
	o.metrics = mt
	o.Write([]byte("ENCODE_STEP encode 10\nENCODE_METRIC vmaf=93.5\n"))
	o.flush()
	if got := mt.snapshot()["vmaf"]; got != 93.5 {
		t.Fatalf("observer did not feed metrics: %v", mt.snapshot())
	}
	if len(timings.order) != 1 || timings.order[0] != "encode" {
		t.Fatalf("step timing feed broken: %v", timings.order)
	}
}
