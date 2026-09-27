package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

func testTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// metricsWebhook spins up an httptest server that records posted Discord
// payloads and returns the collected content strings.
func metricsWebhook(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var p struct {
			Content string `json:"content"`
		}
		json.Unmarshal(b, &p)
		got = append(got, p.Content)
		w.WriteHeader(204)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func doneJob() *model.Job {
	start := testTime("2026-09-27T10:00:00Z")
	fin := testTime("2026-09-27T10:35:12Z")
	return &model.Job{
		ID: 42, Series: "Frieren", Episode: "07", Status: model.JobDone,
		StartedAt: &start, FinishedAt: &fin,
		Metrics: map[string]float64{
			"vmaf":                94.21,
			"output_bitrate_kbps": 8123,
			"source_bitrate_kbps": 25000,
			"output_size_mb":      3450.5,
			"source_size_mb":      10240,
			"duration_sec":        1440,
		},
	}
}

// TestJobFinishedIncludesMetrics verifies the done message carries the
// quality/output stats so a bad encode is visible from the phone alert.
func TestJobFinishedIncludesMetrics(t *testing.T) {
	srv, got := metricsWebhook(t)
	n := NewDiscordWithLink(srv.URL, "", testLog())
	n.JobFinished(context.Background(), doneJob(), "enc-01")

	if len(*got) != 1 {
		t.Fatalf("posts = %d, want 1", len(*got))
	}
	c := (*got)[0]
	for _, want := range []string{"vmaf: **94.21**", "8,123 kb/s", "(src 25,000)", "3,450.5 MB", "(src 10,240 MB)", "24m 0s"} {
		if !strings.Contains(c, want) {
			t.Errorf("message missing %q:\n%s", want, c)
		}
	}
}

// TestJobFinishedNoMetricsUnchanged verifies a job without metrics renders
// exactly as before (no empty "metrics:" line).
func TestJobFinishedNoMetricsUnchanged(t *testing.T) {
	srv, got := metricsWebhook(t)
	n := NewDiscordWithLink(srv.URL, "", testLog())
	j := doneJob()
	j.Metrics = nil
	n.JobFinished(context.Background(), j, "enc-01")
	c := (*got)[0]
	if strings.Contains(c, "vmaf") || strings.Contains(strings.ToLower(c), "metrics") {
		t.Fatalf("metrics line rendered for a metrics-less job:\n%s", c)
	}
}

// TestJobFinishedFailedNoMetrics verifies failures don't render a metrics
// block (a failed encode has no output stats worth showing).
func TestJobFinishedFailedNoMetrics(t *testing.T) {
	srv, got := metricsWebhook(t)
	n := NewDiscordWithLink(srv.URL, "", testLog())
	j := doneJob()
	j.Status = model.JobFailed
	j.Error = "x265 crashed"
	n.JobFinished(context.Background(), j, "enc-01")
	c := (*got)[0]
	if strings.Contains(c, "vmaf") {
		t.Fatalf("failed job rendered VMAF:\n%s", c)
	}
	if !strings.Contains(c, "x265 crashed") {
		t.Fatalf("error missing:\n%s", c)
	}
}
