package api

import (
	"net/http"
	"testing"
)

// Provision endpoint contract tests (engine nil in the test harness: the
// handler must report 503 unavailable rather than panic; validation errors
// surface as 400 from the request shape checks that happen before the
// engine is touched only for well-formed requests — so here we assert the
// unavailable path and the auth gate).

func TestProvisionRequiresAuth(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, _ := doJSON(t, "POST", ts.URL+"/api/provision", "", map[string]string{"host": "x"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 without session, got %d", resp.StatusCode)
	}
	resp, _ = doJSON(t, "GET", ts.URL+"/api/provision/runs", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 without session, got %d", resp.StatusCode)
	}
}

func TestProvisionUnavailableWithoutEngine(t *testing.T) {
	e := newTestEnv(t) // harness leaves Provision nil
	ts := e.serve(t)

	resp, body := doJSON(t, "POST", ts.URL+"/api/provision", adminTok, map[string]any{
		"host": "172.24.92.250", "winrm_password": "x", "node_name": "enc-03",
	})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503 without engine, got %d (%s)", resp.StatusCode, body)
	}
}

func TestProvisionRunsListEmpty(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "GET", ts.URL+"/api/provision/runs", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("list runs: %d %s", resp.StatusCode, body)
	}
	var runs []map[string]any
	mustUnmarshal(t, body, &runs)
	if len(runs) != 0 {
		t.Fatalf("expected empty list, got %d runs", len(runs))
	}
}

func TestProvisionRunLogNotFound(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, _ := doJSON(t, "GET", ts.URL+"/api/provision/runs/999", adminTok, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

func TestDeleteNode(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// The harness registers one node (e.node). Idle nodes can be deleted.
	resp, _ := doJSON(t, "DELETE", ts.URL+"/api/nodes/999", adminTok, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing node: want 404, got %d", resp.StatusCode)
	}

	resp, body := doJSON(t, "DELETE", ts.URL+"/api/nodes/"+itoa(e.node.ID), adminTok, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete idle node: want 204, got %d (%s)", resp.StatusCode, body)
	}

	// Second delete: gone.
	resp, _ = doJSON(t, "DELETE", ts.URL+"/api/nodes/"+itoa(e.node.ID), adminTok, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted node: want 404, got %d", resp.StatusCode)
	}
}

func TestSettingsControllerURLValidation(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	var st map[string]any
	mustUnmarshal(t, body, &st)

	// Bad URL rejected.
	st["controller_url"] = "not-a-url"
	resp, _ = doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, st)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad controller_url: want 400, got %d", resp.StatusCode)
	}

	// Good URL accepted and persisted.
	st["controller_url"] = "http://172.24.92.232:8080"
	resp, body = doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, st)
	if resp.StatusCode != 200 {
		t.Fatalf("put settings: %d %s", resp.StatusCode, body)
	}
	var saved map[string]any
	mustUnmarshal(t, body, &saved)
	if saved["controller_url"] != "http://172.24.92.232:8080" {
		t.Fatalf("controller_url not persisted: %v", saved["controller_url"])
	}
}
