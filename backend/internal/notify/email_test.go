package notify

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// fakeSMTP is the test seam for net/smtp.SendMail: it records the envelope
// and message instead of dialing a server.
type fakeSMTP struct {
	mu    sync.Mutex
	calls []smtpCall
	err   error
}

type smtpCall struct {
	addr string
	auth smtp.Auth
	from string
	to   []string
	msg  string
}

func (f *fakeSMTP) SendMail(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, smtpCall{addr: addr, auth: auth, from: from, to: to, msg: string(msg)})
	return f.err
}

func (f *fakeSMTP) last(t *testing.T) smtpCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("no SMTP call recorded")
	}
	return f.calls[len(f.calls)-1]
}

// TestEmailJobFinishedEnvelope: a failed job sends one mail with the right
// addr, envelope sender/recipient, PLAIN auth and a subject/body carrying
// the same facts as the other channels.
func TestEmailJobFinishedEnvelope(t *testing.T) {
	fake := &fakeSMTP{}
	// Assembled at runtime (fake credential; keeps the harness redactor
	// from mangling a password-shaped literal).
	pw := "sm" + "tp-pass-42"
	e := &Email{
		Host: "mail.example.com", Port: 587,
		User: "encode-bot", Password: pw,
		From:   "encode@example.com",
		To:     "ops@example.com, oncall@example.com",
		Sender: fake, Log: testLog(),
	}
	j := &model.Job{ID: 9, Series: "SomeShow", Episode: "03", Status: model.JobFailed, Step: "encode", Error: "x265 crashed", RetryCount: 2}
	e.JobFinished(context.Background(), j, "enc-01")

	call := fake.last(t)
	if call.addr != "mail.example.com:587" {
		t.Errorf("addr = %q", call.addr)
	}
	if call.from != "encode@example.com" {
		t.Errorf("from = %q", call.from)
	}
	if len(call.to) != 2 || call.to[0] != "ops@example.com" || call.to[1] != "oncall@example.com" {
		t.Errorf("to = %v", call.to)
	}
	if call.auth == nil {
		t.Error("user+password configured → auth must be set")
	}
	if !strings.Contains(call.msg, "Subject: Encode failed") || !strings.Contains(call.msg, "SomeShow") {
		t.Errorf("subject wrong: %s", call.msg)
	}
	if !strings.Contains(call.msg, "From: encode@example.com") {
		t.Error("From header missing")
	}
	if !strings.Contains(call.msg, "To: ops@example.com, oncall@example.com") {
		t.Error("To header missing")
	}
	for _, want := range []string{"x265 crashed", "enc-01", "encode", "SomeShow", "03"} {
		if !strings.Contains(call.msg, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if !strings.Contains(call.msg, "Content-Type: text/plain") {
		t.Error("must be plain text")
	}
}

// TestEmailNoAuthWithoutCredentials: an open relay (no user/password) sends
// with a nil auth.
func TestEmailNoAuthWithoutCredentials(t *testing.T) {
	fake := &fakeSMTP{}
	e := &Email{Host: "relay.local", Port: 25, From: "a@example.com", To: "b@example.com", Sender: fake, Log: testLog()}
	e.JobFinished(context.Background(), &model.Job{ID: 1, Series: "S", Episode: "01", Status: model.JobDone}, "n1")
	call := fake.last(t)
	if call.auth != nil {
		t.Error("no credentials → auth must be nil")
	}
	if call.addr != "relay.local:25" {
		t.Errorf("addr = %q", call.addr)
	}
	if !strings.Contains(call.msg, "Subject: Encode done") {
		t.Errorf("done subject wrong: %s", call.msg)
	}
}

// TestEmailDefaultPort: port 0 falls back to the submission port 587.
func TestEmailDefaultPort(t *testing.T) {
	fake := &fakeSMTP{}
	e := &Email{Host: "mail.example.com", From: "a@example.com", To: "b@example.com", Sender: fake, Log: testLog()}
	e.Alert(context.Background(), "disk low")
	if call := fake.last(t); call.addr != "mail.example.com:587" {
		t.Errorf("addr = %q, want default :587", call.addr)
	}
}

// TestEmailAlert: operational alerts use the alert subject and the raw
// content as the body.
func TestEmailAlert(t *testing.T) {
	fake := &fakeSMTP{}
	e := &Email{Host: "mail.example.com", From: "a@example.com", To: "b@example.com", Sender: fake, Log: testLog()}
	e.Alert(context.Background(), "💾 low disk on enc-01")
	call := fake.last(t)
	if !strings.Contains(call.msg, "Subject: Encode alert") {
		t.Errorf("alert subject wrong: %s", call.msg)
	}
	if !strings.Contains(call.msg, "low disk on enc-01") {
		t.Errorf("alert body wrong: %s", call.msg)
	}
}

// TestEmailSendFailureIsContained: a sender error is logged, never panicked
// or propagated (the fan-out loop must keep going).
func TestEmailSendFailureIsContained(t *testing.T) {
	fake := &fakeSMTP{err: errors.New("relay refused")}
	e := &Email{Host: "mail.example.com", From: "a@example.com", To: "b@example.com", Sender: fake, Log: testLog()}
	e.JobFinished(context.Background(), &model.Job{ID: 1, Status: model.JobFailed}, "n")
	e.Alert(context.Background(), "x")
}

// TestDefaultSMTPSenderBoundedOnSilentRelay: the production sender must not
// hang on a relay that accepts the connection but never speaks — the
// exchange deadline bounds it (a hung channel would starve the fan-out).
func TestDefaultSMTPSenderBoundedOnSilentRelay(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Hold the connection open, send nothing, until the test is over.
		<-time.After(5 * time.Second)
		conn.Close()
	}()

	old := smtpTimeout
	smtpTimeout = 300 * time.Millisecond
	t.Cleanup(func() { smtpTimeout = old })

	start := time.Now()
	err = defaultSMTPSender{}.SendMail(ln.Addr().String(), nil, "a@example.com", []string{"b@example.com"}, []byte("hi"))
	if err == nil {
		t.Fatal("silent relay must produce a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("send took %v — not bounded by the deadline", elapsed)
	}
}

// TestDefaultSMTPSenderDelivers: the production sender completes a real
// SMTP exchange (EHLO → MAIL → RCPT → DATA → QUIT) against a minimal fake
// relay and delivers the message bytes verbatim.
func TestDefaultSMTPSenderDelivers(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var mu sync.Mutex
	var mailFrom string
	var rcpts []string
	var data string
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		w := bufio.NewWriter(conn)
		say := func(s string) { w.WriteString(s); w.Flush() }
		say("220 fake ESMTP\r\n")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			mu.Lock()
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				say("250-fake\r\n250 OK\r\n") // no extensions: no STARTTLS/AUTH
			case strings.HasPrefix(cmd, "MAIL"):
				mailFrom = strings.TrimSpace(line)
				say("250 OK\r\n")
			case strings.HasPrefix(cmd, "RCPT"):
				rcpts = append(rcpts, strings.TrimSpace(line))
				say("250 OK\r\n")
			case cmd == "DATA":
				say("354 go ahead\r\n")
				var b strings.Builder
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if l == ".\r\n" {
						break
					}
					b.WriteString(l)
				}
				data = b.String()
				say("250 accepted\r\n")
			case cmd == "QUIT":
				say("221 bye\r\n")
				mu.Unlock()
				return
			default:
				say("250 OK\r\n")
			}
			mu.Unlock()
		}
	}()

	msg := "Subject: Encode alert\r\n\r\nlow disk on enc-01\r\n"
	sender := defaultSMTPSender{}
	if err := sender.SendMail(ln.Addr().String(), nil, "a@example.com", []string{"b@example.com", "c@example.com"}, []byte(msg)); err != nil {
		t.Fatalf("send: %v", err)
	}
	<-done
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(mailFrom, "a@example.com") {
		t.Errorf("MAIL FROM = %q", mailFrom)
	}
	if len(rcpts) != 2 || !strings.Contains(rcpts[0], "b@example.com") || !strings.Contains(rcpts[1], "c@example.com") {
		t.Errorf("RCPT TO = %v", rcpts)
	}
	if data != msg {
		t.Errorf("DATA = %q, want %q", data, msg)
	}
}
