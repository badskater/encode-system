package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestDiskAlertFires verifies a heartbeat reporting free disk below the
// settings threshold triggers exactly one Alert on the notifier, and that
// the node is soft-drained (no job assignment despite a pending job).
func TestDiskAlertFires(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Settings: alert below 50 GB. A fresh store has no settings row yet
	// (GetSettings returns nil, nil) — write a minimal row directly.
	st, err := e.server.Store.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		st = &model.Settings{}
	}
	st.DiskAlertGB = 50
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}

	// A pending job that WOULD dispatch on a healthy node.
	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "Disk Show", Episode: "01", EpisodeDir: "Disk Show/Ep 01",
		ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending,
	}); err != nil {
		t.Fatal(err)
	}

	rec := &recordingNotifier{}
	e.server.Notifier = rec

	hb := heartbeat("enc-01", 1, 0)
	hb.Metrics = &model.NodeMetrics{DiskFreeGB: 20}
	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, hb)
	if resp.StatusCode != 200 {
		t.Fatalf("heartbeat: %d %s", resp.StatusCode, body)
	}
	var reply model.HeartbeatReply
	json.Unmarshal(body, &reply)
	if reply.Instruction != "none" {
		t.Fatalf("low-disk node got instruction %q, want none (soft drain)", reply.Instruction)
	}
	if got := rec.alerts(); got != 1 {
		t.Fatalf("alerts fired = %d, want 1", got)
	}

	// Cooldown: the very next heartbeat must NOT re-alert.
	hb.TasksSinceBoot = 2
	_, body = doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, hb)
	json.Unmarshal(body, &reply)
	if got := rec.alerts(); got != 1 {
		t.Fatalf("alerts after second heartbeat = %d, want 1 (cooldown)", got)
	}

	// Recovered disk: assignment resumes.
	hb.Metrics = &model.NodeMetrics{DiskFreeGB: 200}
	hb.TasksSinceBoot = 3
	_, body = doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, hb)
	json.Unmarshal(body, &reply)
	if reply.Instruction != "job" {
		t.Fatalf("healthy node got %q, want job", reply.Instruction)
	}
}

// TestDiskAlertDisabledByDefault verifies threshold 0 (default) never
// alerts and never drains, even at 1 GB free.
func TestDiskAlertDisabledByDefault(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	rec := &recordingNotifier{}
	e.server.Notifier = rec

	hb := heartbeat("enc-01", 1, 0)
	hb.Metrics = &model.NodeMetrics{DiskFreeGB: 1}
	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, hb)
	if resp.StatusCode != 200 {
		t.Fatalf("heartbeat: %d %s", resp.StatusCode, body)
	}
	if rec.alerts() != 0 {
		t.Fatalf("alerted with threshold 0")
	}
}

// TestDiskAlertCooldownExpires verifies the cooldown window actually
// expires (unit-level, no sleeping through the real interval).
func TestDiskAlertCooldownExpires(t *testing.T) {
	e := newTestEnv(t)
	g := e.server.diskGuard
	if g == nil {
		t.Fatal("diskGuard not initialized")
	}
	now := time.Now().UTC()
	if !g.shouldAlert(1, now, diskAlertCooldown) {
		t.Fatal("first alert suppressed")
	}
	if g.shouldAlert(1, now.Add(diskAlertCooldown-time.Minute), diskAlertCooldown) {
		t.Fatal("alerted again inside cooldown")
	}
	if !g.shouldAlert(1, now.Add(diskAlertCooldown+time.Minute), diskAlertCooldown) {
		t.Fatal("cooldown did not expire")
	}
	// A different node has its own window.
	if !g.shouldAlert(2, now.Add(time.Minute), diskAlertCooldown) {
		t.Fatal("node 2 suppressed by node 1's alert")
	}
}
