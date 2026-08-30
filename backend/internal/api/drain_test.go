package api

import (
	"encoding/json"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// ---------- Phase D3: drain mode ----------

// Drain mode is a live settings flag that pauses job assignment fleet-wide:
// an idle node's heartbeat gets instruction "none" even when a pending job
// is ready, so running jobs finish and nothing new is dispatched. It is read
// on the live settings path (no restart) so toggling it OFF in the UI
// immediately resumes assignment on the next heartbeat.

// TestDrainModeReturnsNoneForIdleNode: with drain_mode=true, an idle node
// with a pending job receives instruction "none" instead of an assignment.
func TestDrainModeReturnsNoneForIdleNode(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Seed a pending job that would normally be assigned immediately.
	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID,
	}); err != nil {
		t.Fatal(err)
	}

	// Enable drain mode via the live settings row (same path the UI's PUT
	// /api/settings writes).
	st := e.server.defaultsSettings()
	st.DrainMode = true
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}

	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 1, 0))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var reply model.HeartbeatReply
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Instruction != "none" {
		t.Fatalf("drain mode must yield none, got %q (%s)", reply.Instruction, body)
	}
	if reply.Job != nil {
		t.Fatalf("drain mode must not assign a job, got %+v", reply.Job)
	}

	// The pending job must still be pending — drain assigns nothing.
	got, _ := e.server.Store.NextAssignableJob(ctx)
	if got == nil {
		t.Fatal("pending job vanished during drain — should still be assignable")
	}
}

// TestDrainModeOffAssignsJob: with drain_mode=false (default), the same idle
// node receives the pending job. This confirms drain is what gated the
// assignment, not a broken queue.
func TestDrainModeOffAssignsJob(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID,
	}); err != nil {
		t.Fatal(err)
	}

	// drain_mode defaults to false — no settings row needed.
	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 1, 0))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var reply model.HeartbeatReply
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Instruction != "job" {
		t.Fatalf("want job instruction when drain is off, got %q (%s)", reply.Instruction, body)
	}
}

// TestDrainModeToggleResumesAssignment: the settings flag is live — flip it
// OFF after it was ON and the next heartbeat assigns the queued job. This
// proves no restart is needed and the operator can resume instantly.
func TestDrainModeToggleResumesAssignment(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID,
	}); err != nil {
		t.Fatal(err)
	}

	// Drain ON -> idle heartbeat gets none.
	st := e.server.defaultsSettings()
	st.DrainMode = true
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	_, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 1, 0))
	var r1 model.HeartbeatReply
	json.Unmarshal(body, &r1)
	if r1.Instruction != "none" {
		t.Fatalf("drain ON must give none, got %q", r1.Instruction)
	}

	// Drain OFF -> same idle node now gets the job (live, no restart).
	st.DrainMode = false
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	_, body2 := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 1, 0))
	var r2 model.HeartbeatReply
	if err := json.Unmarshal(body2, &r2); err != nil {
		t.Fatal(err)
	}
	if r2.Instruction != "job" {
		t.Fatalf("drain OFF must resume assignment, got %q (%s)", r2.Instruction, body2)
	}
}

// TestDrainModeRoundTripViaPutSettings: the drain_mode flag survives a
// PUT /api/settings round-trip (it is serialized into the JSON blob, not a
// SQL column), so the UI toggle persists.
func TestDrainModeRoundTripViaPutSettings(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Fetch baseline (env defaults) so required fields are populated.
	resp, body := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get settings: %d", resp.StatusCode)
	}
	var st model.Settings
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if st.DrainMode {
		t.Fatal("drain_mode must default to false")
	}

	// Toggle on and PUT.
	st.DrainMode = true
	resp2, body2 := doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, st)
	if resp2.StatusCode != 200 {
		t.Fatalf("put settings with drain_mode=true: %d %s", resp2.StatusCode, body2)
	}
	var saved model.Settings
	if err := json.Unmarshal(body2, &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.DrainMode {
		t.Fatal("PUT response did not echo drain_mode=true")
	}

	// Verify it persisted in the store.
	got, _ := e.server.Store.GetSettings(ctx)
	if got == nil || !got.DrainMode {
		t.Fatalf("drain_mode not persisted in store: %+v", got)
	}
}
