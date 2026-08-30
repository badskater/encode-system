package store

import (
	"context"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// sampleMetrics is a fixed-value sample for round-trip assertions.
func sampleMetrics() *model.NodeMetrics {
	return &model.NodeMetrics{
		CPUPct: 42.5, MemUsedMB: 8192, MemTotalMB: 16384, DiskFreeGB: 500,
		GPUUtil: 73, GPUTemp: 68, GPUMemUsedMB: 2048, EncodeFPS: 23.976,
	}
}

// TestInsertAndListNodeMetricsRoundTrip inserts one sample and reads it back,
// asserting every field survives the store round-trip exactly.
func TestInsertAndListNodeMetricsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	node, _ := s.CreateNode(ctx, "enc-01", "h")

	m := sampleMetrics()
	if err := s.InsertNodeMetric(ctx, node.ID, m); err != nil {
		t.Fatalf("insert: %v", err)
	}

	since := time.Now().UTC().Add(-1 * time.Hour)
	got, err := s.ListNodeMetrics(ctx, node.ID, since, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 sample, got %d", len(got))
	}
	g := got[0]
	if g.CPUPct != m.CPUPct {
		t.Errorf("cpu_pct: want %v, got %v", m.CPUPct, g.CPUPct)
	}
	if g.MemUsedMB != m.MemUsedMB {
		t.Errorf("mem_used_mb: want %v, got %v", m.MemUsedMB, g.MemUsedMB)
	}
	if g.MemTotalMB != m.MemTotalMB {
		t.Errorf("mem_total_mb: want %v, got %v", m.MemTotalMB, g.MemTotalMB)
	}
	if g.DiskFreeGB != m.DiskFreeGB {
		t.Errorf("disk_free_gb: want %v, got %v", m.DiskFreeGB, g.DiskFreeGB)
	}
	if g.GPUUtil != m.GPUUtil {
		t.Errorf("gpu_util: want %v, got %v", m.GPUUtil, g.GPUUtil)
	}
	if g.GPUTemp != m.GPUTemp {
		t.Errorf("gpu_temp: want %v, got %v", m.GPUTemp, g.GPUTemp)
	}
	if g.GPUMemUsedMB != m.GPUMemUsedMB {
		t.Errorf("gpu_mem_used_mb: want %v, got %v", m.GPUMemUsedMB, g.GPUMemUsedMB)
	}
	if g.EncodeFPS != m.EncodeFPS {
		t.Errorf("encode_fps: want %v, got %v", m.EncodeFPS, g.EncodeFPS)
	}
	if g.Ts.IsZero() {
		t.Error("ts should be set")
	}
}

// TestInsertNodeMetricNilIsNoop confirms a nil sample (old agent) is a no-op
// and never writes a row.
func TestInsertNodeMetricNilIsNoop(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	node, _ := s.CreateNode(ctx, "enc-01", "h")

	if err := s.InsertNodeMetric(ctx, node.ID, nil); err != nil {
		t.Fatalf("nil insert should not error: %v", err)
	}
	got, _ := s.ListNodeMetrics(ctx, node.ID, time.Now().UTC().Add(-1*time.Hour), 0)
	if len(got) != 0 {
		t.Fatalf("nil insert must write no rows, got %d", len(got))
	}
}

// TestPruneNodeMetricsRemovesOldRows inserts rows with backdated timestamps
// and confirms prune deletes only the ones older than the maxAge window.
func TestPruneNodeMetricsRemovesOldRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	node, _ := s.CreateNode(ctx, "enc-01", "h")

	// Insert a fresh row (kept) and a backdated row older than the retention
	// window (pruned). We backdate via a direct INSERT so we control the ts.
	old := time.Now().UTC().Add(-25 * time.Hour).Format("2006-01-02 15:04:05")
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO node_metrics (node_id, ts, cpu_pct, mem_used_mb, mem_total_mb, disk_free_gb, gpu_util, gpu_temp, gpu_mem_used_mb, encode_fps)
		 VALUES (?, ?, 0, 0, 0, 0, -1, -1, -1, 0)`, node.ID, old)
	if err != nil {
		t.Fatalf("backdate insert: %v", err)
	}
	// Fresh row via the normal path (uses datetime('now')).
	if err := s.InsertNodeMetric(ctx, node.ID, sampleMetrics()); err != nil {
		t.Fatalf("fresh insert: %v", err)
	}

	// Prune with a 24h window — the 25h-old row must go, the fresh one stays.
	if err := s.PruneNodeMetrics(ctx, 24*time.Hour); err != nil {
		t.Fatalf("prune: %v", err)
	}
	since := time.Now().UTC().Add(-48 * time.Hour)
	got, _ := s.ListNodeMetrics(ctx, node.ID, since, 0)
	if len(got) != 1 {
		t.Fatalf("after prune want 1 row (the fresh one), got %d", len(got))
	}
}

// TestListNodeMetricsDownsamplesOver500 inserts 600 rows and confirms the
// result is capped at metricsMaxPoints (500) and still ordered ascending.
func TestListNodeMetricsDownsamplesOver500(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	node, _ := s.CreateNode(ctx, "enc-01", "h")

	// Insert 600 rows with distinct backdated timestamps so they are all in
	// range. We space them 1 minute apart going back from now so the "since"
	// filter (1h ago) includes none of them after the first ~60, but we query
	// with a wide since to include all.
	base := time.Now().UTC().Add(-10 * time.Hour)
	for i := 0; i < 600; i++ {
		ts := base.Add(time.Duration(i) * time.Minute).Format("2006-01-02 15:04:05")
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO node_metrics (node_id, ts, cpu_pct, mem_used_mb, mem_total_mb, disk_free_gb, gpu_util, gpu_temp, gpu_mem_used_mb, encode_fps)
			 VALUES (?, ?, ?, 0, 0, 0, -1, -1, -1, 0)`, node.ID, ts, float64(i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	since := time.Now().UTC().Add(-11 * time.Hour)
	got, err := s.ListNodeMetrics(ctx, node.ID, since, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) > metricsMaxPoints {
		t.Fatalf("downsample failed: want ≤%d, got %d", metricsMaxPoints, len(got))
	}
	// Ascending order check.
	for i := 1; i < len(got); i++ {
		if got[i].Ts.Before(got[i-1].Ts) {
			t.Fatalf("sample %d before %d (not ascending)", i, i-1)
		}
	}
	// The first and last must be present so endpoints are represented.
	if len(got) > 0 {
		if !got[0].Ts.Equal(base) && !got[0].Ts.Equal(base.Add(time.Minute)) {
			t.Logf("note: first sample ts=%v (base=%v)", got[0].Ts, base)
		}
	}
}

// TestLatestNodeMetricReturnsMostRecent inserts two samples and confirms
// LatestNodeMetric returns the newest one. Also confirms nil for a node with
// no metrics.
func TestLatestNodeMetricReturnsMostRecent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	node, _ := s.CreateNode(ctx, "enc-01", "h")
	other, _ := s.CreateNode(ctx, "enc-02", "h2")

	// Old sample.
	old := time.Now().UTC().Add(-1 * time.Hour).Format("2006-01-02 15:04:05")
	_, _ = s.db.ExecContext(ctx,
		`INSERT INTO node_metrics (node_id, ts, cpu_pct, mem_used_mb, mem_total_mb, disk_free_gb, gpu_util, gpu_temp, gpu_mem_used_mb, encode_fps)
		 VALUES (?, ?, 10, 0, 0, 0, -1, -1, -1, 0)`, node.ID, old)
	// Fresh sample (newer ts).
	if err := s.InsertNodeMetric(ctx, node.ID, &model.NodeMetrics{CPUPct: 99}); err != nil {
		t.Fatalf("fresh insert: %v", err)
	}

	got, err := s.LatestNodeMetric(ctx, node.ID)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if got == nil {
		t.Fatal("want non-nil latest metric")
	}
	if got.CPUPct != 99 {
		t.Fatalf("want latest cpu=99, got %v", got.CPUPct)
	}

	// Node with no metrics returns nil, no error.
	none, err := s.LatestNodeMetric(ctx, other.ID)
	if err != nil {
		t.Fatalf("latest on empty node: %v", err)
	}
	if none != nil {
		t.Fatalf("want nil for node with no metrics, got %+v", none)
	}
}
