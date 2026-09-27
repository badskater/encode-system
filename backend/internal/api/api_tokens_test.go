package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestAPITokenLifecycle: create → use (GET ok) → list → delete → use fails.
func TestAPITokenLifecycle(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// Create a read-scope token via the admin session.
	resp, body := doJSON(t, "POST", ts.URL+"/api/tokens", adminTok, map[string]any{"name": "grafana", "scope": "read"})
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var created struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Scope string `json:"scope"`
		Token string `json:"token"`
	}
	json.Unmarshal(body, &created)
	if created.Token == "" || created.ID == 0 {
		t.Fatalf("bad create response: %s", body)
	}

	// Duplicate name → 409.
	if r2, _ := doJSON(t, "POST", ts.URL+"/api/tokens", adminTok, map[string]any{"name": "grafana"}); r2.StatusCode != 409 {
		t.Fatalf("dup name: %d, want 409", r2.StatusCode)
	}

	// Token authenticates a GET.
	r3, b3 := doJSON(t, "GET", ts.URL+"/api/jobs", created.Token, nil)
	if r3.StatusCode != 200 {
		t.Fatalf("token GET jobs: %d %s", r3.StatusCode, b3)
	}

	// List shows the token but never its hash.
	r4, b4 := doJSON(t, "GET", ts.URL+"/api/tokens", adminTok, nil)
	if r4.StatusCode != 200 {
		t.Fatalf("list: %d", r4.StatusCode)
	}
	var list []*model.APIToken
	json.Unmarshal(b4, &list)
	if len(list) != 1 || list[0].Name != "grafana" || list[0].Scope != "read" {
		t.Fatalf("list = %s", b4)
	}
	if strings.Contains(string(b4), "token_hash") || strings.Contains(string(b4), created.Token) {
		t.Fatalf("hash/plaintext leaked in list: %s", b4)
	}
	if list[0].LastUsedAt == nil {
		t.Fatal("last_used_at not stamped by the GET above")
	}

	// Delete → token stops working.
	if r5, _ := doJSON(t, "DELETE", ts.URL+"/api/tokens/"+i64s(created.ID), adminTok, nil); r5.StatusCode != 204 {
		t.Fatalf("delete: %d", r5.StatusCode)
	}
	if r6, _ := doJSON(t, "GET", ts.URL+"/api/jobs", created.Token, nil); r6.StatusCode != 401 {
		t.Fatalf("deleted token GET: %d, want 401", r6.StatusCode)
	}
	// Delete unknown → 404.
	if r7, _ := doJSON(t, "DELETE", ts.URL+"/api/tokens/999", adminTok, nil); r7.StatusCode != 404 {
		t.Fatalf("delete unknown: %d, want 404", r7.StatusCode)
	}
}

// TestAPITokenScopes: read tokens can't mutate; admin tokens can; tokens
// can't manage tokens (no privilege escalation from a leaked token).
func TestAPITokenScopes(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	_, rb := doJSON(t, "POST", ts.URL+"/api/tokens", adminTok, map[string]any{"name": "reader", "scope": "read"})
	var reader struct {
		Token string `json:"token"`
	}
	json.Unmarshal(rb, &reader)

	// Read token: POST /api/jobs/bulk → 403.
	if r1, _ := doJSON(t, "POST", ts.URL+"/api/jobs/bulk", reader.Token, map[string]any{"action": "retry", "ids": []int64{1}}); r1.StatusCode != 403 {
		t.Fatalf("read POST: %d, want 403", r1.StatusCode)
	}
	// Read token: PUT settings → 403.
	st := e.server.currentSettings(ctxBg())
	if r2, _ := doJSON(t, "PUT", ts.URL+"/api/settings", reader.Token, st); r2.StatusCode != 403 {
		t.Fatalf("read PUT: %d, want 403", r2.StatusCode)
	}
	// Read token: create token → 403 (method gate hits first — either is fine, both deny).
	if r3, _ := doJSON(t, "POST", ts.URL+"/api/tokens", reader.Token, map[string]any{"name": "x"}); r3.StatusCode != 403 {
		t.Fatalf("read create token: %d, want 403", r3.StatusCode)
	}
	// Read token: list tokens (GET) → 403 by the explicit handler gate.
	if r4, _ := doJSON(t, "GET", ts.URL+"/api/tokens", reader.Token, nil); r4.StatusCode != 403 {
		t.Fatalf("read list tokens: %d, want 403", r4.StatusCode)
	}

	// Admin-scope token can mutate: retry a nonexistent job list is 200 with skips.
	_, ab := doJSON(t, "POST", ts.URL+"/api/tokens", adminTok, map[string]any{"name": "sonarr", "scope": "admin"})
	var adminTok2 struct {
		Token string `json:"token"`
	}
	json.Unmarshal(ab, &adminTok2)
	r5, b5 := doJSON(t, "POST", ts.URL+"/api/jobs/bulk", adminTok2.Token, map[string]any{"action": "retry", "ids": []int64{424242}})
	if r5.StatusCode != 200 {
		t.Fatalf("admin POST bulk: %d %s", r5.StatusCode, b5)
	}
	// Admin token still cannot mint tokens (escalation guard).
	if r6, _ := doJSON(t, "POST", ts.URL+"/api/tokens", adminTok2.Token, map[string]any{"name": "evil"}); r6.StatusCode != 403 {
		t.Fatalf("admin-token create token: %d, want 403", r6.StatusCode)
	}
}

// TestAPITokenAuditActor: actions taken with a token are audited under
// "api-token:<name>".
func TestAPITokenAuditActor(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	_, ab := doJSON(t, "POST", ts.URL+"/api/tokens", adminTok, map[string]any{"name": "sonarr", "scope": "admin"})
	var tok struct {
		Token string `json:"token"`
	}
	json.Unmarshal(ab, &tok)

	// Mutate something with the token: retry an unknown job → audited skip path
	// still writes bulk audit. Use prune instead: always audited.
	if r1, b1 := doJSON(t, "POST", ts.URL+"/api/jobs/prune", tok.Token, map[string]any{"days": 3650}); r1.StatusCode != 200 {
		t.Fatalf("prune: %d %s", r1.StatusCode, b1)
	}

	_, b2 := doJSON(t, "GET", ts.URL+"/api/audit", adminTok, nil)
	var events []*model.AuditEvent
	json.Unmarshal(b2, &events)
	found := false
	for _, ev := range events {
		if ev.Action == "jobs.prune" && ev.Actor == "api-token:sonarr" {
			found = true
		}
	}
	if !found {
		t.Fatalf("prune not audited under api-token actor:\n%s", b2)
	}
}
