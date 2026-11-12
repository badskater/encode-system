package api

// Final integration-review regression (W9): Discord webhook masking
// consistency for non-admin readers.
//
// GET /api/settings already masks ntfy/webhook/ingest URL query values and
// the offsite S3 access_key for non-admins, but discord_webhook — whose
// secret is a PATH token — was returned in full to viewer sessions and
// read-scope API tokens. notify.RedactURL already masks the Discord path
// token (redactParsedURL); the settings mask just never applied it. The
// per-series webhook override (GET /api/series → webhook_url) carried the
// same exposure and gets the same mask. Admins see everything, unchanged.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestGetSettingsMasksDiscordWebhookForNonAdmin: viewer sessions and
// read-scope tokens must not see the Discord path token; admin sessions and
// admin-scope tokens must. The masked URL stays identifiable (scheme/host/
// path prefix + REDACTED marker).
func TestGetSettingsMasksDiscordWebhookForNonAdmin(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	token := "disc-" + "webhook-s3cret-77"
	hook := "https://discord.com/api/webhooks/1234567890/" + token
	st := e.server.defaultsSettings()
	st.DiscordWebhook = hook
	if err := e.server.Store.SaveSettings(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	viewerTok := mintViewerSession(t, e, "disc-viewer", "viewer-pass-disc")
	readTok := mintAPIToken(t, e, "disc-read-tok", model.TokenScopeRead)
	adminScopeTok := mintAPIToken(t, e, "disc-admin-tok", model.TokenScopeAdmin)

	cases := []struct {
		name     string
		bearer   string
		wantFull bool
	}{
		{"viewer session", viewerTok, false},
		{"read-scope api token", readTok, false},
		{"admin session", adminTok, true},
		{"admin-scope api token", adminScopeTok, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doJSON(t, "GET", ts.URL+"/api/settings", tc.bearer, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("GET settings: %d %s", resp.StatusCode, body)
			}
			got := string(body)
			if tc.wantFull {
				if !strings.Contains(got, hook) {
					t.Fatalf("admin must see the full discord webhook, got: %s", got)
				}
				return
			}
			if strings.Contains(got, token) {
				t.Fatalf("non-admin GET leaked the discord path token: %s", got)
			}
			if !strings.Contains(got, "discord.com/api/webhooks") {
				t.Fatalf("non-admin mask must keep the webhook identifiable, got: %s", got)
			}
			if !strings.Contains(got, "REDACTED") {
				t.Fatalf("non-admin mask must mark the redacted token, got: %s", got)
			}
		})
	}
}

// TestGetSettingsDiscordMaskDoesNotMutateStored: the viewer mask operates on
// a copy — a subsequent admin GET still sees the full stored webhook.
func TestGetSettingsDiscordMaskDoesNotMutateStored(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	token := "disc-" + "webhook-s3cret-88"
	hook := "https://discord.com/api/webhooks/1111111111/" + token
	st := e.server.defaultsSettings()
	st.DiscordWebhook = hook
	if err := e.server.Store.SaveSettings(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	viewerTok := mintViewerSession(t, e, "disc-viewer2", "viewer-pass-disc2")
	if resp, body := doJSON(t, "GET", ts.URL+"/api/settings", viewerTok, nil); resp.StatusCode != 200 {
		t.Fatalf("viewer GET: %d %s", resp.StatusCode, body)
	}
	resp, body := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("admin GET: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), hook) {
		t.Fatalf("viewer GET mutated the stored webhook: %s", body)
	}
}

// TestSeriesListMasksWebhookForNonAdmin: the per-series Discord override
// (webhook_url) must get the same non-admin mask as the global webhook.
func TestSeriesListMasksWebhookForNonAdmin(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	token := "series-" + "webhook-s3cret-99"
	hook := "https://discord.com/api/webhooks/9876543210/" + token

	resp, body := doJSON(t, "POST", ts.URL+"/api/series", adminTok, map[string]any{
		"name": "Mask Show", "episodes": 1,
	})
	if resp.StatusCode != 201 {
		t.Fatalf("create series: %d %s", resp.StatusCode, body)
	}
	var created struct {
		Series struct {
			ID int64 `json:"id"`
		} `json:"series"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	patchURL := fmt.Sprintf("%s/api/series/%d", ts.URL, created.Series.ID)
	if resp, body := doJSON(t, "PATCH", patchURL, adminTok, map[string]any{"webhook_url": hook}); resp.StatusCode != 200 {
		t.Fatalf("patch series webhook: %d %s", resp.StatusCode, body)
	}

	viewerTok := mintViewerSession(t, e, "series-viewer", "viewer-pass-series")
	resp, body = doJSON(t, "GET", ts.URL+"/api/series", viewerTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("viewer GET series: %d %s", resp.StatusCode, body)
	}
	got := string(body)
	if strings.Contains(got, token) {
		t.Fatalf("viewer GET /api/series leaked the series webhook token: %s", got)
	}
	if !strings.Contains(got, "Mask Show") {
		t.Fatalf("series list must still carry the series rows: %s", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Fatalf("masked series webhook must carry the REDACTED marker: %s", got)
	}

	resp, body = doJSON(t, "GET", ts.URL+"/api/series", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("admin GET series: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), hook) {
		t.Fatalf("admin must see the full series webhook: %s", body)
	}
}
