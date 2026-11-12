package notify

// Regression for quality review Q2: channel error logs embedded the RAW
// request URL via *url.Error, leaking URL-embedded credentials (?auth=<token>
// on ntfy/webhook URLs, the path token on Discord webhooks) into the log
// stream. The fix is a shared RedactURL helper applied at every channel
// error-log site; these tests assert on the ACTUAL captured log bytes.

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// bufLog returns a logger writing into the returned buffer so tests can
// assert on the exact log output.
func bufLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// deadEndpoint starts and immediately closes an httptest server, returning a
// URL that forces a dial error (connection refused) on any request.
func deadEndpoint(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	u := srv.URL
	srv.Close()
	return u
}

// TestRedactURL: scheme/host/path survive for diagnostics; query VALUES are
// masked (keys kept), userinfo and fragments are dropped, and Discord
// webhook path tokens are masked. Unparseable input never keeps a raw query.
func TestRedactURL(t *testing.T) {
	secret := "s3c" + "ret-query-val"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"query value masked", "https://ntfy.sh/topic?auth=" + secret, "https://ntfy.sh/topic?auth=REDACTED"},
		{"multiple values masked", "https://host/hook?a=1&b=" + secret, "https://host/hook?a=REDACTED&b=REDACTED"},
		{"no query unchanged", "https://ntfy.sh/topic", "https://ntfy.sh/topic"},
		{"userinfo dropped", "https://user:" + secret + "@host/path", "https://host/path"},
		{"fragment dropped", "https://host/path#" + secret, "https://host/path"},
		{"discord path token masked", "https://discord.com/api/webhooks/12345/" + secret, "https://discord.com/api/webhooks/12345/REDACTED"},
		{"unparseable query stripped", "ht tp://host/topic?auth=" + secret, "ht tp://host/topic?<redacted>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactURL(tc.in)
			if got != tc.want {
				t.Errorf("RedactURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, secret) {
				t.Errorf("RedactURL output still contains the secret: %q", got)
			}
		})
	}
}

// TestNtfyErrorLogRedactsURLToken: a dial error on a topic URL carrying
// ?auth=<secret> must not leak the secret into the log bytes.
func TestNtfyErrorLogRedactsURLToken(t *testing.T) {
	log, buf := bufLog()
	secret := "ntfy-" + "auth-s3cret-9x7"
	n := &Ntfy{TopicURL: deadEndpoint(t) + "/topic?auth=" + secret, HTTP: sharedHTTP, Log: log}
	n.JobFinished(context.Background(), &model.Job{ID: 1, Series: "S", Episode: "01", Status: model.JobFailed}, "n1")

	out := buf.String()
	if !strings.Contains(out, "ntfy notify failed") {
		t.Fatalf("expected a failure log line, got: %s", out)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("ntfy error log leaked the URL token: %s", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Errorf("expected the query value to be masked, got: %s", out)
	}
}

// TestWebhookErrorLogRedactsURLToken: same guarantee for the generic webhook.
func TestWebhookErrorLogRedactsURLToken(t *testing.T) {
	log, buf := bufLog()
	secret := "hook-" + "auth-s3cret-4z2"
	w := &Webhook{URL: deadEndpoint(t) + "/hook?token=" + secret, HTTP: sharedHTTP, Log: log}
	w.JobFinished(context.Background(), &model.Job{ID: 1, Series: "S", Episode: "01", Status: model.JobFailed}, "n1")

	out := buf.String()
	if !strings.Contains(out, "webhook notify failed") {
		t.Fatalf("expected a failure log line, got: %s", out)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("webhook error log leaked the URL token: %s", out)
	}
}

// TestDiscordErrorLogRedactsURLToken: same guarantee for the pre-existing
// Discord path (query token AND the webhook path token).
func TestDiscordErrorLogRedactsURLToken(t *testing.T) {
	log, buf := bufLog()
	secret := "disc-" + "auth-s3cret-7q1"
	d := &Discord{WebhookURL: deadEndpoint(t) + "/api/webhooks/123?auth=" + secret, HTTP: sharedHTTP, Log: log}
	d.JobFinished(context.Background(), &model.Job{ID: 1, Series: "S", Episode: "01", Status: model.JobFailed}, "n1")

	out := buf.String()
	if !strings.Contains(out, "discord notify failed") {
		t.Fatalf("expected a failure log line, got: %s", out)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("discord error log leaked the URL token: %s", out)
	}
}

// TestNtfyRequestErrorLogRedactsURLToken: the http.NewRequest failure path
// (malformed URL) also embeds the raw URL in its error — it must be redacted
// too, not just the transport-error path.
func TestNtfyRequestErrorLogRedactsURLToken(t *testing.T) {
	log, buf := bufLog()
	secret := "bad-" + "url-s3cret-3m8"
	// A space makes url.Parse fail inside http.NewRequestWithContext.
	n := &Ntfy{TopicURL: "http://exa mple.com/topic?auth=" + secret, HTTP: sharedHTTP, Log: log}
	n.Alert(context.Background(), "x")

	out := buf.String()
	if !strings.Contains(out, "ntfy notify failed") {
		t.Fatalf("expected a failure log line, got: %s", out)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("ntfy request-error log leaked the URL token: %s", out)
	}
}
