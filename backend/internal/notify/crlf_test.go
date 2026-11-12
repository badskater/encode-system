package notify

// Regression for quality review Q6 + spec gap: CRLF injection via series
// names. The scanner ingests folder names directly (bypassing API name
// validation), so a series named "Show\r\nBcc: attacker@evil" reached the
// email Subject → header injection (EXPLOIT-PROVEN: an extra Bcc header on
// the wire), and the ntfy Title → Go rejects the invalid header value →
// per-series delivery loss. jobTitle must sanitize CR/LF/control chars for
// ALL channels.

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// crlfJob builds a failed job whose series name carries a CRLF header
// injection payload (plus a bare control char for the general case).
func crlfJob() *model.Job {
	return &model.Job{
		ID: 7, Series: "SomeShow\r\nBcc: attacker@evil.example", Episode: "03",
		Status: model.JobFailed, Step: "encode", Error: "x265 crashed",
	}
}

// TestEmailSubjectCRLFInjection: the crafted series name must NOT produce an
// injected header on the wire. The header block (everything before the first
// blank line) must contain no Bcc line, and the Subject must stay one line.
func TestEmailSubjectCRLFInjection(t *testing.T) {
	fake := &fakeSMTP{}
	e := &Email{
		Host: "mail.example.com", Port: 587,
		From: "encode@example.com", To: "ops@example.com",
		Sender: fake, Log: testLog(),
	}
	e.JobFinished(context.Background(), crlfJob(), "enc-01")

	call := fake.last(t)
	headers, _, found := strings.Cut(call.msg, "\r\n\r\n")
	if !found {
		t.Fatalf("message has no header/body separator: %q", call.msg)
	}
	for _, line := range strings.Split(headers, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "bcc:") {
			t.Fatalf("CRLF injection produced a Bcc header on the wire: %q", headers)
		}
	}
	// The envelope recipients must be untouched — the injected address may
	// survive as inert Subject TEXT (control chars replaced by spaces), but
	// it must never become a recipient or a header of its own.
	if len(call.to) != 1 || call.to[0] != "ops@example.com" {
		t.Fatalf("injected recipient reached the envelope: %v", call.to)
	}
	// The Subject line itself must be a single line carrying the sanitized
	// series name (words kept, control chars gone).
	var subject string
	for _, line := range strings.Split(headers, "\r\n") {
		if strings.HasPrefix(line, "Subject:") {
			subject = line
		}
	}
	if subject == "" {
		t.Fatalf("no Subject header: %q", headers)
	}
	if strings.ContainsAny(subject, "\r\n") {
		t.Fatalf("Subject spans multiple lines: %q", subject)
	}
	if !strings.Contains(subject, "SomeShow") || !strings.Contains(subject, "Encode failed") {
		t.Fatalf("Subject lost its content: %q", subject)
	}
}

// TestNtfyTitleCRLFDelivery: the crafted series name must not break ntfy
// delivery — the request succeeds with a sanitized single-line Title.
func TestNtfyTitleCRLFDelivery(t *testing.T) {
	cap := &ntfyCapture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	n := &Ntfy{TopicURL: srv.URL + "/encode", HTTP: srv.Client(), Log: testLog()}
	n.JobFinished(context.Background(), crlfJob(), "enc-01")

	got := cap.snapshot()
	if got.calls != 1 {
		t.Fatalf("ntfy delivery lost (calls=%d) — invalid Title header rejected by the transport", got.calls)
	}
	if strings.ContainsAny(got.title, "\r\n") {
		t.Fatalf("Title carries CRLF: %q", got.title)
	}
	if !strings.Contains(got.title, "SomeShow") || !strings.Contains(got.title, "Encode failed") {
		t.Fatalf("Title lost its content: %q", got.title)
	}
}

// TestSanitizeHeaderControlChars: every C0 control char and DEL is removed
// (replaced with a space so words stay separated); printable text survives.
func TestSanitizeHeaderControlChars(t *testing.T) {
	in := "A\x00B\rC\nD\tE\x1bF\x7fG—H"
	got := sanitizeHeader(in)
	if strings.ContainsAny(got, "\x00\r\n\t\x1b\x7f") {
		t.Fatalf("control chars survived: %q", got)
	}
	for _, want := range []string{"A", "B", "C", "D", "E", "F", "G", "—", "H"} {
		if !strings.Contains(got, want) {
			t.Fatalf("printable content %q lost: %q", want, got)
		}
	}
}
