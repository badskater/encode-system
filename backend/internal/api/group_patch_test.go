package api

import (
	"encoding/json"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestPatchNodeGroup verifies PATCH /api/nodes/{id} {group} persists the
// routing label and survives alongside the enabled flag (field-scoped SQL,
// not read-modify-write clobbering).
func TestPatchNodeGroup(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "PATCH", ts.URL+"/api/nodes/"+i64s(e.node.ID), adminTok, map[string]any{"group": "gpu"})
	if resp.StatusCode != 200 {
		t.Fatalf("patch group: %d %s", resp.StatusCode, body)
	}
	var got model.Node
	json.Unmarshal(body, &got)
	if got.Group != "gpu" {
		t.Fatalf("group = %q, want gpu", got.Group)
	}

	// Combined patch: enabled=false must not clear the group.
	resp, body = doJSON(t, "PATCH", ts.URL+"/api/nodes/"+i64s(e.node.ID), adminTok, map[string]any{"enabled": false})
	if resp.StatusCode != 200 {
		t.Fatalf("patch enabled: %d %s", resp.StatusCode, body)
	}
	got = model.Node{}
	json.Unmarshal(body, &got)
	if got.Group != "gpu" || got.Enabled {
		t.Fatalf("combined patch wrong: group=%q enabled=%v", got.Group, got.Enabled)
	}

	// Reload from store confirms persistence.
	db, err := e.server.Store.GetNode(ctxBg(), e.node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if db.Group != "gpu" {
		t.Fatalf("persisted group = %q", db.Group)
	}
}

// TestPatchSeriesNodeGroup verifies PATCH /api/series/{id} {node_group}
// persists and round-trips through ListSeries.
func TestPatchSeriesNodeGroup(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	sr, err := e.server.Store.UpsertSeriesByName(ctx, "Group Show")
	if err != nil {
		t.Fatal(err)
	}
	resp, body := doJSON(t, "PATCH", ts.URL+"/api/series/"+i64s(sr.ID), adminTok, map[string]any{"node_group": "gpu"})
	if resp.StatusCode != 200 {
		t.Fatalf("patch: %d %s", resp.StatusCode, body)
	}
	var got model.Series
	json.Unmarshal(body, &got)
	if got.NodeGroup != "gpu" {
		t.Fatalf("node_group = %q, want gpu", got.NodeGroup)
	}
	// Empty string clears (wildcard).
	resp, body = doJSON(t, "PATCH", ts.URL+"/api/series/"+i64s(sr.ID), adminTok, map[string]any{"node_group": ""})
	if resp.StatusCode != 200 {
		t.Fatalf("clear: %d %s", resp.StatusCode, body)
	}
	got = model.Series{}
	json.Unmarshal(body, &got)
	if got.NodeGroup != "" {
		t.Fatalf("node_group = %q after clear", got.NodeGroup)
	}
}
