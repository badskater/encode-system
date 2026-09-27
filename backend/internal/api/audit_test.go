package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestAuditRecordsMutations verifies mutating admin actions land in
// GET /api/audit newest-first with actor/action/object/detail, and that
// secrets (webhook URLs) never appear in details.
func TestAuditRecordsMutations(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Settings update (webhook set — must NOT leak into detail). Start
	// from the persisted settings so required fields validate.
	st := e.server.currentSettings(ctx)
	st.DiscordWebhook = "https://discord.com/api/webhooks/SECRET/token"
	resp0, body0 := doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, st)
	if resp0.StatusCode != 200 {
		t.Fatalf("settings put: %d %s", resp0.StatusCode, body0)
	}

	// Node patch.
	if _, body := doJSON(t, "PATCH", ts.URL+"/api/nodes/"+i64s(e.node.ID), adminTok,
		map[string]any{"group": "audit-grp"}); len(body) == 0 {
		t.Fatal("node patch empty")
	}

	// Series update with a webhook override.
	sr, _ := e.server.Store.UpsertSeriesByName(ctx, "Audit Show")
	doJSON(t, "PATCH", ts.URL+"/api/series/"+i64s(sr.ID), adminTok,
		map[string]any{"paused": true, "webhook_url": "https://discord.com/api/webhooks/SECRET2/tok2"})

	resp, body := doJSON(t, "GET", ts.URL+"/api/audit", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("audit list: %d %s", resp.StatusCode, body)
	}
	var events []*model.AuditEvent
	if err := json.Unmarshal(body, &events); err != nil {
		t.Fatal(err)
	}
	if len(events) < 3 {
		t.Fatalf("events = %d, want >= 3", len(events))
	}
	// Newest first.
	for i := 1; i < len(events); i++ {
		if events[i-1].ID <= events[i].ID {
			t.Fatalf("not newest-first at %d", i)
		}
	}
	joined := string(body)
	for _, want := range []string{"settings.update", "node.update", "series.update", `"actor":"admin"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("audit missing %q", want)
		}
	}
	// Secret hygiene: webhook URLs must never be in details.
	if strings.Contains(joined, "SECRET") {
		t.Errorf("webhook URL leaked into audit:\n%s", joined)
	}
	// series.update detail carries webhook_set boolean, not the URL.
	for _, ev := range events {
		if ev.Action == "series.update" && !strings.Contains(ev.Detail, "webhook_set") {
			t.Errorf("series.update detail missing webhook_set: %s", ev.Detail)
		}
	}
}

// TestAuditLimitValidation verifies the limit query param bounds.
func TestAuditLimitValidation(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	if resp, _ := doJSON(t, "GET", ts.URL+"/api/audit?limit=0", adminTok, nil); resp.StatusCode != 400 {
		t.Fatalf("limit=0: %d, want 400", resp.StatusCode)
	}
	if resp, _ := doJSON(t, "GET", ts.URL+"/api/audit?limit=1001", adminTok, nil); resp.StatusCode != 400 {
		t.Fatalf("limit=1001: %d, want 400", resp.StatusCode)
	}
	if resp, _ := doJSON(t, "GET", ts.URL+"/api/audit?limit=10", adminTok, nil); resp.StatusCode != 200 {
		t.Fatalf("limit=10: %d, want 200", resp.StatusCode)
	}
	// Unauthenticated: 401.
	if resp, _ := doJSON(t, "GET", ts.URL+"/api/audit", "", nil); resp.StatusCode != 401 {
		t.Fatalf("no auth: %d, want 401", resp.StatusCode)
	}
}
