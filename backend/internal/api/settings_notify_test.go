package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------- Settings: notification channels (webhook / ntfy / email) ----------

// settingsMap GETs the live settings as a generic map so tests can mutate
// individual keys (including the write-only secret keys the model hides).
func settingsMap(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, body := doJSON(t, "GET", url+"/api/settings", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get settings: %d", resp.StatusCode)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestSettingsNotifyChannelValidation: table of accepted/rejected channel
// configurations. Empty = disabled is always accepted; set fields must be
// sane (http(s) URLs, port bounds, valid email addresses).
func TestSettingsNotifyChannelValidation(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	base := settingsMap(t, ts.URL)

	put := func(mutate func(m map[string]any)) int {
		m := make(map[string]any, len(base))
		for k, v := range base {
			m[k] = v
		}
		mutate(m)
		resp, _ := doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, m)
		return resp.StatusCode
	}

	cases := []struct {
		name   string
		mutate func(m map[string]any)
		want   int
	}{
		{"empty channels accepted", func(m map[string]any) {}, 200},
		{"generic webhook loopback", func(m map[string]any) { m["generic_webhook_url"] = "http://127.0.0.1:9/hook" }, 200},
		{"generic webhook https", func(m map[string]any) { m["generic_webhook_url"] = "https://hooks.example.com/abc" }, 200},
		{"generic webhook ftp rejected", func(m map[string]any) { m["generic_webhook_url"] = "ftp://example.com/x" }, 400},
		{"generic webhook not a url", func(m map[string]any) { m["generic_webhook_url"] = "notaurl" }, 400},
		{"generic webhook no host", func(m map[string]any) { m["generic_webhook_url"] = "http://" }, 400},
		{"ntfy topic https", func(m map[string]any) { m["ntfy_topic_url"] = "https://ntfy.sh/encode-alerts" }, 200},
		{"ntfy topic selfhosted http", func(m map[string]any) { m["ntfy_topic_url"] = "http://192.168.1.10:8080/alerts" }, 200},
		{"ntfy topic missing scheme", func(m map[string]any) { m["ntfy_topic_url"] = "ntfy.sh/topic" }, 400},
		{"smtp full config", func(m map[string]any) {
			m["smtp_host"] = "mail.example.com"
			m["smtp_port"] = 587
			m["smtp_from"] = "encode@example.com"
			m["smtp_to"] = "ops@example.com, oncall@example.com"
		}, 200},
		{"smtp port zero defaults", func(m map[string]any) {
			m["smtp_host"] = "mail.example.com"
			m["smtp_port"] = 0
			m["smtp_from"] = "encode@example.com"
			m["smtp_to"] = "ops@example.com"
		}, 200},
		{"smtp port max", func(m map[string]any) {
			m["smtp_host"] = "mail.example.com"
			m["smtp_port"] = 65535
			m["smtp_from"] = "encode@example.com"
			m["smtp_to"] = "ops@example.com"
		}, 200},
		{"smtp port too high", func(m map[string]any) { m["smtp_port"] = 65536 }, 400},
		{"smtp port negative", func(m map[string]any) { m["smtp_port"] = -1 }, 400},
		{"smtp host without from/to", func(m map[string]any) { m["smtp_host"] = "mail.example.com" }, 400},
		{"smtp from without host", func(m map[string]any) { m["smtp_from"] = "encode@example.com" }, 400},
		{"smtp to without host", func(m map[string]any) { m["smtp_to"] = "ops@example.com" }, 400},
		{"smtp bad from", func(m map[string]any) {
			m["smtp_host"] = "mail.example.com"
			m["smtp_from"] = "not-an-email"
			m["smtp_to"] = "ops@example.com"
		}, 400},
		{"smtp bad recipient in list", func(m map[string]any) {
			m["smtp_host"] = "mail.example.com"
			m["smtp_from"] = "encode@example.com"
			m["smtp_to"] = "ops@example.com, bad"
		}, 400},
		{"smtp host is a url", func(m map[string]any) {
			m["smtp_host"] = "http://mail.example.com"
			m["smtp_from"] = "encode@example.com"
			m["smtp_to"] = "ops@example.com"
		}, 400},
		// G3: a port appended to the host breaks net.JoinHostPort at send
		// time — reject ':' unless the host is a bracketed IPv6 literal.
		{"smtp host with port rejected", func(m map[string]any) {
			m["smtp_host"] = "mail.example.com:25"
			m["smtp_from"] = "encode@example.com"
			m["smtp_to"] = "ops@example.com"
		}, 400},
		{"smtp host bracketed ipv6 accepted", func(m map[string]any) {
			m["smtp_host"] = "[::1]"
			m["smtp_from"] = "encode@example.com"
			m["smtp_to"] = "ops@example.com"
		}, 200},
		{"smtp host bracketed ipv6 full accepted", func(m map[string]any) {
			m["smtp_host"] = "[2001:db8::25]"
			m["smtp_from"] = "encode@example.com"
			m["smtp_to"] = "ops@example.com"
		}, 200},
		{"smtp host unbracketed ipv6 rejected", func(m map[string]any) {
			m["smtp_host"] = "::1"
			m["smtp_from"] = "encode@example.com"
			m["smtp_to"] = "ops@example.com"
		}, 400},
		{"smtp host bracketed ipv6 with port rejected", func(m map[string]any) {
			m["smtp_host"] = "[::1]:25"
			m["smtp_from"] = "encode@example.com"
			m["smtp_to"] = "ops@example.com"
		}, 400},
		{"smtp host bracketed non-ip rejected", func(m map[string]any) {
			m["smtp_host"] = "[mail.example.com]"
			m["smtp_from"] = "encode@example.com"
			m["smtp_to"] = "ops@example.com"
		}, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := put(tc.mutate); got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestSettingsSecretsWriteOnly: PUT a token + password, then prove neither
// the PUT response nor any GET ever contains them (keys absent, has_* flags
// present). An empty secret on a later PUT keeps the stored value (the
// shares pattern), and a non-empty one replaces it.
func TestSettingsSecretsWriteOnly(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Assembled at runtime: fake credentials, redaction-safe.
	tok := "nt" + "fy-leak-tok-1"
	pw := "sm" + "tp-leak-pw-2"

	m := settingsMap(t, ts.URL)
	m["ntfy_topic_url"] = "https://ntfy.sh/encode"
	m["ntfy_token"] = tok
	m["smtp_host"] = "mail.example.com"
	m["smtp_from"] = "encode@example.com"
	m["smtp_to"] = "ops@example.com"
	m["smtp_password"] = pw
	resp, body := doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, m)
	if resp.StatusCode != 200 {
		t.Fatalf("put with secrets: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), tok) || strings.Contains(string(body), pw) {
		t.Fatal("PUT response leaked a write-only secret")
	}

	resp, body = doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get: %d", resp.StatusCode)
	}
	if strings.Contains(string(body), tok) || strings.Contains(string(body), pw) {
		t.Fatal("GET response leaked a write-only secret")
	}
	var got map[string]any
	json.Unmarshal(body, &got)
	if _, ok := got["ntfy_token"]; ok {
		t.Error("ntfy_token key must be absent from GET")
	}
	if _, ok := got["smtp_password"]; ok {
		t.Error("smtp_password key must be absent from GET")
	}
	if got["has_ntfy_token"] != true || got["has_smtp_password"] != true {
		t.Errorf("has_* flags must be true: %v", got)
	}

	// The UI round-trips the GET body (minus the flags it strips) with EMPTY
	// secrets — the stored values must survive (shares keep-semantics).
	round := settingsMap(t, ts.URL)
	round["ntfy_token"] = ""
	round["smtp_password"] = ""
	resp, body = doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, round)
	if resp.StatusCode != 200 {
		t.Fatalf("round-trip put: %d %s", resp.StatusCode, body)
	}
	st, err := e.server.Store.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.NtfyToken != tok || st.SMTPPassword != pw {
		t.Fatal("empty secrets on PUT must keep the stored values")
	}

	// A non-empty secret replaces the stored one.
	tok2 := "nt" + "fy-leak-tok-2"
	round["ntfy_token"] = tok2
	resp, _ = doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, round)
	if resp.StatusCode != 200 {
		t.Fatalf("replace put: %d", resp.StatusCode)
	}
	st, _ = e.server.Store.GetSettings(ctx)
	if st.NtfyToken != tok2 {
		t.Fatal("non-empty secret must replace the stored value")
	}
	if st.SMTPPassword != pw {
		t.Fatal("untouched secret must survive a partial update")
	}
}

// TestSettingsHasFlagsEchoTolerated: a client that PUTs the GET body back
// verbatim (including the has_* output flags) must not 400 — the flags are
// accepted and ignored, keeping naive round-trip automation working.
func TestSettingsHasFlagsEchoTolerated(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	m := settingsMap(t, ts.URL)
	m["ntfy_topic_url"] = "https://ntfy.sh/x"
	m["ntfy_token"] = "to" + "k-echo"
	resp, _ := doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, m)
	if resp.StatusCode != 200 {
		t.Fatalf("initial put: %d", resp.StatusCode)
	}
	echo := settingsMap(t, ts.URL) // carries has_ntfy_token/has_smtp_password
	resp, body := doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, echo)
	if resp.StatusCode != 200 {
		t.Fatalf("verbatim GET→PUT round-trip must be accepted: %d %s", resp.StatusCode, body)
	}
}

// TestSettingsSecretsNotInAudit: the settings.update audit detail carries
// only booleans about secrets — never their values.
func TestSettingsSecretsNotInAudit(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	tok := "nt" + "fy-audit-tok"
	pw := "sm" + "tp-audit-pw"
	m := settingsMap(t, ts.URL)
	m["ntfy_topic_url"] = "https://ntfy.sh/audit"
	m["ntfy_token"] = tok
	m["smtp_host"] = "mail.example.com"
	m["smtp_from"] = "a@example.com"
	m["smtp_to"] = "b@example.com"
	m["smtp_password"] = pw
	if resp, _ := doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, m); resp.StatusCode != 200 {
		t.Fatalf("put: %d", resp.StatusCode)
	}

	events, err := e.server.Store.ListAudit(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Action != "settings.update" {
			continue
		}
		found = true
		if strings.Contains(ev.Detail, tok) || strings.Contains(ev.Detail, pw) {
			t.Fatalf("audit detail leaked a secret: %s", ev.Detail)
		}
		if !strings.Contains(ev.Detail, "ntfy_token_changed") || !strings.Contains(ev.Detail, "smtp_password_changed") {
			t.Errorf("audit detail must record secret-change booleans: %s", ev.Detail)
		}
	}
	if !found {
		t.Fatal("no settings.update audit event found")
	}
}
