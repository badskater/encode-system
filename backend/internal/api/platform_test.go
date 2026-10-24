package api

import (
	"encoding/json"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// platformHeartbeat builds a heartbeat carrying a platform field.
func platformHeartbeat(node string, tasks int, platform string) model.Heartbeat {
	hb := heartbeat(node, tasks, 0)
	hb.Platform = platform
	return hb
}

// TestHeartbeatPlatformPersists verifies a heartbeat reporting platform=linux
// persists onto the node row and is served by GET /api/nodes.
func TestHeartbeatPlatformPersists(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	hb := platformHeartbeat("enc-01", 1, "linux")
	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, hb)
	if resp.StatusCode != 200 {
		t.Fatalf("heartbeat: %d %s", resp.StatusCode, body)
	}

	// GET /api/nodes must surface the platform field.
	resp2, body2 := doJSON(t, "GET", ts.URL+"/api/nodes", adminTok, nil)
	if resp2.StatusCode != 200 {
		t.Fatalf("nodes: %d %s", resp2.StatusCode, body2)
	}
	var nodes []model.Node
	if err := json.Unmarshal(body2, &nodes); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body2)
	}
	if len(nodes) != 1 || nodes[0].Platform != "linux" {
		t.Fatalf("platform not persisted: %+v", nodes)
	}
}

// TestHeartbeatWithoutPlatformKeepsPriorValue verifies that an old agent
// (heartbeat WITHOUT a platform field) does NOT clobber a stored platform.
// This is the rolling-upgrade invariant: absent/empty field => keep prior.
func TestHeartbeatWithoutPlatformKeepsPriorValue(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// First, set the node's platform to linux via a heartbeat.
	hb1 := platformHeartbeat("enc-01", 1, "linux")
	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, hb1)
	if resp.StatusCode != 200 {
		t.Fatalf("first heartbeat: %d %s", resp.StatusCode, body)
	}

	// Second heartbeat: old-agent shape (no Platform field set). Must keep linux.
	hb2 := heartbeat("enc-01", 2, 0) // Platform is zero value ""
	resp2, body2 := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, hb2)
	if resp2.StatusCode != 200 {
		t.Fatalf("second heartbeat: %d %s", resp2.StatusCode, body2)
	}

	resp3, body3 := doJSON(t, "GET", ts.URL+"/api/nodes", adminTok, nil)
	if resp3.StatusCode != 200 {
		t.Fatalf("nodes: %d %s", resp3.StatusCode, body3)
	}
	var nodes []model.Node
	if err := json.Unmarshal(body3, &nodes); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body3)
	}
	if len(nodes) != 1 || nodes[0].Platform != "linux" {
		t.Fatalf("absent platform clobbered prior value: %+v", nodes)
	}
}
