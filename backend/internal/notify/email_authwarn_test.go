package notify

// Regression for quality review Q4: with smtp_user + smtp_password set but
// the relay NOT advertising AUTH, the production sender silently skipped
// authentication — the operator believes mail is authenticated when it is
// not. The fix logs a structured Warn (and still sends, for compatibility
// with relays that accept unauthenticated submission).

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// noAuthRelay is a minimal fake SMTP relay that advertises NO extensions
// (no STARTTLS, no AUTH). It records whether the client attempted AUTH and
// the delivered DATA bytes.
type noAuthRelay struct {
	ln       net.Listener
	mu       sync.Mutex
	authSeen bool
	data     string
	done     chan struct{}
}

func startNoAuthRelay(t *testing.T) *noAuthRelay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	r := &noAuthRelay{ln: ln, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		rd := bufio.NewReader(conn)
		wr := bufio.NewWriter(conn)
		say := func(s string) { wr.WriteString(s); wr.Flush() }
		say("220 fake ESMTP\r\n")
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			r.mu.Lock()
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				say("250-fake\r\n250 OK\r\n") // no extensions at all
			case strings.HasPrefix(cmd, "AUTH"):
				r.authSeen = true
				say("502 not implemented\r\n")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				say("250 OK\r\n")
			case cmd == "DATA":
				say("354 go ahead\r\n")
				var b strings.Builder
				for {
					l, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					if l == ".\r\n" {
						break
					}
					b.WriteString(l)
				}
				r.data = b.String()
				say("250 accepted\r\n")
			case cmd == "QUIT":
				say("221 bye\r\n")
				r.mu.Unlock()
				return
			default:
				say("250 OK\r\n")
			}
			r.mu.Unlock()
		}
	}()
	return r
}

// TestSMTPAuthUnavailableWarns: credentials configured + relay without AUTH
// → a structured Warn naming AUTH, and the mail is STILL delivered (compat).
func TestSMTPAuthUnavailableWarns(t *testing.T) {
	relay := startNoAuthRelay(t)
	log, buf := bufLog()

	host, portStr, err := net.SplitHostPort(relay.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	pw := "sm" + "tp-authwarn-pw"
	st := &model.Settings{
		SMTPHost: host, SMTPPort: port,
		SMTPUser: "encode-bot", SMTPPassword: pw,
		SMTPFrom: "encode@example.com", SMTPTo: "ops@example.com",
	}
	// nil sender → the PRODUCTION net/smtp seam (the path that skipped AUTH).
	e := NewEmail(st, nil, log)
	e.JobFinished(context.Background(), &model.Job{ID: 1, Series: "S", Episode: "01", Status: model.JobDone}, "n1")
	<-relay.done

	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "AUTH") {
		t.Fatalf("expected a structured Warn about unavailable AUTH, got: %s", out)
	}
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if relay.authSeen {
		t.Error("relay does not advertise AUTH — the client must not attempt it")
	}
	if !strings.Contains(relay.data, "Subject: Encode done") {
		t.Errorf("mail must still be delivered (compat), got DATA: %q", relay.data)
	}
}

// TestSMTPNoCredentialsNoAuthWarn: an open relay with NO credentials
// configured must not warn (nothing was skipped).
func TestSMTPNoCredentialsNoAuthWarn(t *testing.T) {
	relay := startNoAuthRelay(t)
	log, buf := bufLog()

	host, portStr, err := net.SplitHostPort(relay.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	st := &model.Settings{
		SMTPHost: host, SMTPPort: port,
		SMTPFrom: "encode@example.com", SMTPTo: "ops@example.com",
	}
	e := NewEmail(st, nil, log)
	e.Alert(context.Background(), "disk low")
	<-relay.done

	if strings.Contains(buf.String(), "AUTH") {
		t.Fatalf("no credentials configured → no AUTH warning expected, got: %s", buf.String())
	}
}
