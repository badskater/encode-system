package notify

// Regression for P4 quality review Q3: RedactURL failed OPEN on credential
// URLs EMBEDDED in a larger error string. url.Parse of the whole message
// succeeds with an empty Scheme (opaque/path), so the userinfo/query masking
// never ran and the secret survived into log + audit + alert. The probe:
//
//	Put "https://AKIA…cret@host/k?x=1": dial tcp …
//
// Embedded http(s) substrings must now be redacted in place, surrounding
// text preserved. Bare-URL behavior is unchanged (redact_test.go).

import (
	"strings"
	"testing"
)

// TestRedactURLEmbeddedCredentials: the url.Error shape that leaked through
// the offsite failure path — userinfo AND query values must be masked while
// the diagnostic text around the URL survives.
func TestRedactURLEmbeddedCredentials(t *testing.T) {
	user := "AKIA" + "EMBEDDEDKEY"
	pass := "emb" + "edded-s3cret-77"
	query := "tok" + "en-query-88"
	in := `Put "https://` + user + `:` + pass + `@s3.example.com/backups/k?x=` + query + `": dial tcp 192.0.2.1:443: connect: connection refused`
	got := RedactURL(in)
	for _, secret := range []string{user, pass, query} {
		if strings.Contains(got, secret) {
			t.Fatalf("embedded credential leaked: %q", got)
		}
	}
	// Surrounding diagnostic text and the URL skeleton survive.
	for _, keep := range []string{"Put ", "dial tcp 192.0.2.1:443", "s3.example.com/backups/k", "REDACTED"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("redaction lost diagnostic text %q: %q", keep, got)
		}
	}
}

// TestRedactURLEmbeddedMultipleAndBare: several URLs in one message are each
// redacted; a message that IS a bare URL keeps the existing behavior; text
// without any URL passes through untouched.
func TestRedactURLEmbeddedMultipleAndBare(t *testing.T) {
	s1 := "fir" + "st-secret-11"
	s2 := "sec" + "ond-secret-22"
	in := "upload https://ntfy.sh/topic?auth=" + s1 + " failed; retry https://hooks.example.com/h?token=" + s2 + " also failed"
	got := RedactURL(in)
	if strings.Contains(got, s1) || strings.Contains(got, s2) {
		t.Fatalf("embedded secrets leaked: %q", got)
	}
	if !strings.Contains(got, "upload ") || !strings.Contains(got, " failed; retry ") || !strings.Contains(got, " also failed") {
		t.Fatalf("surrounding text lost: %q", got)
	}
	// Bare URL still redacts exactly as before.
	if got := RedactURL("https://ntfy.sh/topic?auth=" + s1); got != "https://ntfy.sh/topic?auth=REDACTED" {
		t.Fatalf("bare URL behavior changed: %q", got)
	}
	// No URL, no query → unchanged.
	if got := RedactURL("plain failure text"); got != "plain failure text" {
		t.Fatalf("plain text changed: %q", got)
	}
	// Embedded Discord path token is masked too.
	disc := "disc" + "path-tok-33"
	got = RedactURL(`Post "https://discord.com/api/webhooks/123/` + disc + `": EOF`)
	if strings.Contains(got, disc) {
		t.Fatalf("embedded Discord path token leaked: %q", got)
	}
}
