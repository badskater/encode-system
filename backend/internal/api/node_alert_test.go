package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestDiffNodeStatesNoTransitions verifies a stable state (no changes between
// prev and cur) produces zero transitions.
func TestDiffNodeStatesNoTransitions(t *testing.T) {
	prev := map[int64]bool{1: true, 2: false}
	cur := map[int64]nodeState{
		1: {online: true, name: "node-1"},
		2: {online: false, name: "node-2"},
	}
	transitions := diffNodeStates(prev, cur)
	if len(transitions) != 0 {
		t.Fatalf("expected 0 transitions, got %d: %+v", len(transitions), transitions)
	}
}

// TestDiffNodeStatesOnlineToOffline verifies a node that was online and is now
// offline produces a single "offline" transition.
func TestDiffNodeStatesOnlineToOffline(t *testing.T) {
	prev := map[int64]bool{1: true}
	lastSeen := time.Now().UTC().Add(-2 * time.Minute)
	cur := map[int64]nodeState{
		1: {online: false, name: "node-1", lastSeen: &lastSeen},
	}
	transitions := diffNodeStates(prev, cur)
	if len(transitions) != 1 {
		t.Fatalf("expected 1 transition, got %d", len(transitions))
	}
	tr := transitions[0]
	if tr.nodeID != 1 || tr.name != "node-1" {
		t.Errorf("transition node: %+v", tr)
	}
	if tr.to != "offline" {
		t.Errorf("transition to = %q, want offline", tr.to)
	}
	if tr.lastSeen == nil || !tr.lastSeen.Equal(lastSeen) {
		t.Errorf("transition lastSeen mismatch: got %v, want %v", tr.lastSeen, lastSeen)
	}
}

// TestDiffNodeStatesOfflineToOnline verifies a node that was offline and is now
// online produces a single "online" transition.
func TestDiffNodeStatesOfflineToOnline(t *testing.T) {
	prev := map[int64]bool{1: false}
	cur := map[int64]nodeState{
		1: {online: true, name: "node-1"},
	}
	transitions := diffNodeStates(prev, cur)
	if len(transitions) != 1 {
		t.Fatalf("expected 1 transition, got %d", len(transitions))
	}
	if transitions[0].to != "online" {
		t.Errorf("transition to = %q, want online", transitions[0].to)
	}
}

// TestDiffNodeStatesNewNodeNoTransition verifies a NEW node (not in prev) does
// not produce a transition on the first tick — the seed tick sets the baseline
// silently.
func TestDiffNodeStatesNewNodeNoTransition(t *testing.T) {
	prev := map[int64]bool{} // empty — first tick
	cur := map[int64]nodeState{
		1: {online: true, name: "node-1"},
		2: {online: false, name: "node-2"},
	}
	transitions := diffNodeStates(prev, cur)
	if len(transitions) != 0 {
		t.Fatalf("new nodes should not produce transitions on seed tick, got %d: %+v", len(transitions), transitions)
	}
}

// TestDiffNodeStatesMultipleTransitions verifies multiple nodes transitioning
// in the same tick all produce transitions.
func TestDiffNodeStatesMultipleTransitions(t *testing.T) {
	prev := map[int64]bool{1: true, 2: false, 3: true}
	lastSeen := time.Now().UTC().Add(-2 * time.Minute)
	cur := map[int64]nodeState{
		1: {online: false, name: "node-1", lastSeen: &lastSeen}, // online→offline
		2: {online: true, name: "node-2"},                       // offline→online
		3: {online: true, name: "node-3"},                       // unchanged
	}
	transitions := diffNodeStates(prev, cur)
	if len(transitions) != 2 {
		t.Fatalf("expected 2 transitions, got %d: %+v", len(transitions), transitions)
	}
}

// TestNodeAlertLoopSeedsSilently verifies the first tick seeds the baseline
// without sending notifications, even when nodes are offline.
func TestNodeAlertLoopSeedsSilently(t *testing.T) {
	e := newTestEnv(t)
	ctx := ctxBg()

	// Node is stale (offline) from the start.
	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	_ = fl

	// Enable node-status alerts in settings.
	st := e.server.currentSettings(ctx)
	st.NotifyNodeStatus = true
	st.DiscordWebhook = "https://discord.com/api/webhooks/test/node-alert"
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}

	rec := &recordingNotifier{}
	e.server.Notifier = rec

	// Run one tick — the node is offline but this is the seed tick.
	e.server.nodeAlertTick(ctx)

	if rec.alerts() != 0 {
		t.Errorf("seed tick fired %d alerts, want 0 (silent seed)", rec.alerts())
	}
}

// TestNodeAlertLoopFiresOnTransition verifies a real online→offline transition
// fires a Discord alert, and the reverse transition fires a recovery alert.
func TestNodeAlertLoopFiresOnTransition(t *testing.T) {
	e := newTestEnv(t)
	ctx := ctxBg()

	// Enable node-status alerts.
	st := e.server.currentSettings(ctx)
	st.NotifyNodeStatus = true
	st.DiscordWebhook = "https://discord.com/api/webhooks/test/node-alert"
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}

	rec := &recordingNotifier{}
	e.server.Notifier = rec

	// Seed tick: node starts online (freshly seen via a heartbeat).
	// The test env's node has LastSeen=nil initially, so we set it.
	now := time.Now().UTC()
	e.node.LastSeen = &now
	if err := e.server.Store.UpdateNode(ctx, e.node); err != nil {
		t.Fatal(err)
	}
	e.server.nodeAlertTick(ctx) // seed — silent
	if rec.alerts() != 0 {
		t.Fatalf("seed tick fired %d alerts, want 0", rec.alerts())
	}

	// Make the node stale (offline): set LastSeen far in the past.
	stale := time.Now().UTC().Add(-10 * time.Minute)
	e.node.LastSeen = &stale
	if err := e.server.Store.UpdateNode(ctx, e.node); err != nil {
		t.Fatal(err)
	}
	e.server.nodeAlertTick(ctx) // online→offline transition
	if rec.alerts() != 1 {
		t.Fatalf("online→offline: alerts = %d, want 1", rec.alerts())
	}

	// Node recovers (online again).
	e.node.LastSeen = &now
	if err := e.server.Store.UpdateNode(ctx, e.node); err != nil {
		t.Fatal(err)
	}
	e.server.nodeAlertTick(ctx) // offline→online transition
	if rec.alerts() != 2 {
		t.Fatalf("offline→online: alerts = %d, want 2 (total)", rec.alerts())
	}
}

// TestNodeAlertLoopDisabledByDefault verifies no alerts fire when
// NotifyNodeStatus is false (the default), even on a real transition.
func TestNodeAlertLoopDisabledByDefault(t *testing.T) {
	e := newTestEnv(t)
	ctx := ctxBg()

	// NotifyNodeStatus defaults to false — don't enable it.
	rec := &recordingNotifier{}
	e.server.Notifier = rec

	// Seed tick with an online node.
	now := time.Now().UTC()
	e.node.LastSeen = &now
	if err := e.server.Store.UpdateNode(ctx, e.node); err != nil {
		t.Fatal(err)
	}
	e.server.nodeAlertTick(ctx)

	// Make node stale.
	stale := time.Now().UTC().Add(-10 * time.Minute)
	e.node.LastSeen = &stale
	if err := e.server.Store.UpdateNode(ctx, e.node); err != nil {
		t.Fatal(err)
	}
	e.server.nodeAlertTick(ctx)

	if rec.alerts() != 0 {
		t.Errorf("alerts fired with NotifyNodeStatus=false: %d", rec.alerts())
	}
}

// TestNodeAlertLoopNoWebhookNoAlert verifies no alert fires when
// NotifyNodeStatus is on but the webhook is empty.
func TestNodeAlertLoopNoWebhookNoAlert(t *testing.T) {
	e := newTestEnv(t)
	ctx := ctxBg()

	st := e.server.currentSettings(ctx)
	st.NotifyNodeStatus = true
	st.DiscordWebhook = "" // no webhook
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}

	rec := &recordingNotifier{}
	e.server.Notifier = rec

	// Seed + transition.
	now := time.Now().UTC()
	e.node.LastSeen = &now
	if err := e.server.Store.UpdateNode(ctx, e.node); err != nil {
		t.Fatal(err)
	}
	e.server.nodeAlertTick(ctx)

	stale := time.Now().UTC().Add(-10 * time.Minute)
	e.node.LastSeen = &stale
	if err := e.server.Store.UpdateNode(ctx, e.node); err != nil {
		t.Fatal(err)
	}
	e.server.nodeAlertTick(ctx)

	if rec.alerts() != 0 {
		t.Errorf("alerts fired with empty webhook: %d", rec.alerts())
	}
}

// TestSettingsNotifyNodeStatusRoundTrip verifies the notify_node_status field
// survives a settings PUT/GET cycle.
func TestSettingsNotifyNodeStatusRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// GET current settings.
	resp, body := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get settings: %d", resp.StatusCode)
	}
	var st model.Settings
	decodeJSONBody(t, body, &st)
	if st.NotifyNodeStatus {
		t.Errorf("default NotifyNodeStatus = true, want false")
	}

	// Enable it.
	st.NotifyNodeStatus = true
	resp, body = doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, st)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put settings: %d %s", resp.StatusCode, body)
	}
	resp, body = doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get settings after put: %d", resp.StatusCode)
	}
	decodeJSONBody(t, body, &st)
	if !st.NotifyNodeStatus {
		t.Errorf("after round-trip: NotifyNodeStatus = false, want true")
	}
}

func decodeJSONBody(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatal(err)
	}
}
