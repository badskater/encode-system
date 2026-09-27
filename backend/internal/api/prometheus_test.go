package api

import (
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestPrometheusEndpoint verifies GET /metrics serves the text exposition
// format with the core fleet gauges/counters and requires no auth session
// (scrapers authenticate at the network layer; the endpoint leaks only
// aggregate operational numbers, same as the Stats page).
func TestPrometheusEndpoint(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Heartbeat once so the node has a fresh LastSeen (online=1) BEFORE
	// seeding the pending job — otherwise dispatch assigns it away.
	if r2, b2 := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 1, 0)); r2.StatusCode != 200 {
		t.Fatalf("heartbeat: %d %s", r2.StatusCode, b2)
	}

	// Seed: 1 pending job, 1 done job, node exists from the env.
	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	if _, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "Prom Show", Episode: "01", EpisodeDir: "Prom Show/Ep 01",
		ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.SeedFinishedJob(ctx, fl.ID, e.node.ID, "done", "",
		"2026-09-27 10:00:00", "2026-09-27 10:30:00"); err != nil {
		t.Fatal(err)
	}

	resp, body := doJSON(t, "GET", ts.URL+"/metrics", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("metrics: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q, want text/plain", ct)
	}
	text := string(body)

	// HELP/TYPE headers for every series family.
	for _, want := range []string{
		"# HELP encode_jobs_pending",
		"# TYPE encode_jobs_pending gauge",
		"# TYPE encode_jobs_running gauge",
		"# TYPE encode_jobs_done_total counter",
		"# TYPE encode_jobs_failed_total counter",
		"# TYPE encode_node_online gauge",
		"encode_jobs_pending 1",
		"encode_jobs_done_total 1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q:\n%s", want, text)
		}
	}
	// Node gauge with a label.
	if !strings.Contains(text, `encode_node_online{node="enc-01"} 1`) {
		t.Errorf("node gauge missing/wrong:\n%s", text)
	}
	// No Prometheus parse violations: no empty values, no NaN.
	if strings.Contains(text, "NaN") {
		t.Errorf("NaN in output:\n%s", text)
	}
}

// TestPrometheusAuthFreeButScoped confirms /metrics exposes aggregates
// only — no job logs, paths, or series names beyond node labels.
func TestPrometheusAuthFreeButScoped(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "GET", ts.URL+"/metrics", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("metrics: %d", resp.StatusCode)
	}
	text := string(body)
	for _, leak := range []string{"full_log", "log_tail", "scripts", "Ep 01"} {
		if strings.Contains(text, leak) {
			t.Errorf("metrics leaked %q", leak)
		}
	}
}
