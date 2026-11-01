package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestScanNowTriggerReturns202 verifies POST /api/scan returns 202
// {"triggered":true} and is admin-only.
func TestScanNowTriggerReturns202(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "POST", ts.URL+"/api/scan", adminTok, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Triggered bool `json:"triggered"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("bad response %s: %v", body, err)
	}
	if !out.Triggered {
		t.Errorf("triggered = false, want true")
	}

	// A second request (while the first is queued) still returns 202 — the
	// buffered-1 channel coalesces, the response never changes.
	resp, body = doJSON(t, "POST", ts.URL+"/api/scan", adminTok, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("second request status %d: %s", resp.StatusCode, body)
	}
}

// TestScanNowRejectsNonAdmin verifies the scan trigger is admin-only.
func TestScanNowRejectsNonAdmin(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// Anonymous caller is refused.
	resp, _ := doJSON(t, "POST", ts.URL+"/api/scan", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous: status %d, want 401", resp.StatusCode)
	}

	// Node-token caller is refused (not a management session).
	resp, _ = doJSON(t, "POST", ts.URL+"/api/scan", e.token, nil)
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("node token: status %d, want 401/403", resp.StatusCode)
	}
}

// TestScanNowAudit verifies the scan.trigger audit event is recorded.
func TestScanNowAudit(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	doJSON(t, "POST", ts.URL+"/api/scan", adminTok, nil)

	resp, body := doJSON(t, "GET", ts.URL+"/api/audit", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audit list: %d", resp.StatusCode)
	}
	var events []struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(body, &events); err != nil {
		t.Fatalf("bad audit response: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Action == "scan.trigger" {
			found = true
		}
	}
	if !found {
		t.Errorf("scan.trigger audit event not found in audit log")
	}
}
