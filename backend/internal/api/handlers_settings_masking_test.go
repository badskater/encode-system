package api

// Regression for quality review Q5: GET /api/settings is any-authenticated-
// user, and ntfy_topic_url / generic_webhook_url were returned UNMASKED —
// URL-embedded credentials (?auth=<token>) were readable by viewer sessions
// and read-scope API tokens. Non-admins must see query VALUES masked
// (scheme/host/path stay visible so the endpoint is still identifiable);
// admins see the full URLs. Dedicated secret fields stay write-only for
// everyone (has_* flags, asserted here too).
//
// Consumer investigation: the only non-admin surface rendering these URLs
// is the Settings page's disabled input fields (frontend/src/pages/
// Settings.tsx) — viewers cannot PUT, so masking query values breaks no
// workflow. Least-breaking choice: mask values, keep scheme/host/path.

import (
	"context"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/auth"
	"github.com/badskater/encode-system/backend/internal/model"
)

// seedChannelURLSettings stores settings whose channel URLs carry query
// credentials, returning the two secret values.
func seedChannelURLSettings(t *testing.T, e *testEnv) (string, string) {
	t.Helper()
	ntfySecret := "ntfy-" + "url-s3cret-88"
	hookSecret := "hook-" + "url-s3cret-99"
	st := e.server.defaultsSettings()
	st.NtfyTopicURL = "https://ntfy.example.com/encode?auth=" + ntfySecret
	st.GenericWebhookURL = "https://hooks.example.com/endpoint?token=" + hookSecret
	st.NtfyToken = "dedicated-" + "ntfy-tok"
	st.SMTPHost = "mail.example.com"
	st.SMTPFrom = "a@example.com"
	st.SMTPTo = "b@example.com"
	st.SMTPPassword = "dedicated-" + "smtp-pw"
	if err := e.server.Store.SaveSettings(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	return ntfySecret, hookSecret
}

// mintAPIToken creates an API token with the given scope and returns its
// plaintext bearer value.
func mintAPIToken(t *testing.T, e *testEnv, name, scope string) string {
	t.Helper()
	plaintext, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	tok := &model.APIToken{Name: name, Scope: scope, TokenHash: auth.HashToken(plaintext)}
	if err := e.server.Store.CreateAPIToken(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
	return plaintext
}

// TestGetSettingsMasksURLCredentialsForNonAdmin: viewer sessions and
// read-scope tokens must not see query credential values; admin sessions
// and admin-scope tokens must. Dedicated secrets stay write-only for all.
func TestGetSettingsMasksURLCredentialsForNonAdmin(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ntfySecret, hookSecret := seedChannelURLSettings(t, e)
	viewerTok := mintViewerSession(t, e, "url-viewer", "viewer-pass-url")
	readTok := mintAPIToken(t, e, "read-tok", model.TokenScopeRead)
	adminScopeTok := mintAPIToken(t, e, "admin-tok", model.TokenScopeAdmin)

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
				if !strings.Contains(got, ntfySecret) || !strings.Contains(got, hookSecret) {
					t.Fatalf("admin must see full channel URLs, got: %s", got)
				}
			} else {
				if strings.Contains(got, ntfySecret) || strings.Contains(got, hookSecret) {
					t.Fatalf("non-admin GET leaked URL credentials: %s", got)
				}
				// Scheme/host/path stay visible (least-breaking mask).
				if !strings.Contains(got, "ntfy.example.com/encode") || !strings.Contains(got, "hooks.example.com/endpoint") {
					t.Fatalf("non-admin mask must keep scheme/host/path, got: %s", got)
				}
				if !strings.Contains(got, "REDACTED") {
					t.Fatalf("non-admin mask must mark redacted query values, got: %s", got)
				}
			}
			// Dedicated secret fields are write-only for EVERYONE.
			if strings.Contains(got, "dedicated-") {
				t.Fatalf("dedicated secrets must never be returned: %s", got)
			}
			if !strings.Contains(got, `"has_ntfy_token":true`) || !strings.Contains(got, `"has_smtp_password":true`) {
				t.Fatalf("has_* flags must reflect stored secrets: %s", got)
			}
		})
	}
}

// TestGetSettingsMaskingDoesNotMutateStoredURLs: the viewer mask operates on
// a copy — a subsequent admin GET still sees the full stored URLs.
func TestGetSettingsMaskingDoesNotMutateStoredURLs(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ntfySecret, hookSecret := seedChannelURLSettings(t, e)
	viewerTok := mintViewerSession(t, e, "url-viewer2", "viewer-pass-url2")

	if resp, body := doJSON(t, "GET", ts.URL+"/api/settings", viewerTok, nil); resp.StatusCode != 200 {
		t.Fatalf("viewer GET: %d %s", resp.StatusCode, body)
	}
	resp, body := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("admin GET: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), ntfySecret) || !strings.Contains(string(body), hookSecret) {
		t.Fatalf("viewer GET mutated the stored URLs: %s", body)
	}
}
