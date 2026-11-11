package notify

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// SMTPSender is the send seam for the email channel: production uses
// net/smtp.SendMail, tests inject a fake that records the envelope and
// message. Keeping it an interface (not a func field) lets a fake carry
// state and assert across calls.
type SMTPSender interface {
	SendMail(addr string, auth smtp.Auth, from string, to []string, msg []byte) error
}

// defaultSMTPSender is the production seam: net/smtp with a bounded
// exchange. smtp.SendMail itself has NO timeout — a black-holed relay would
// block the fan-out (and the heartbeat/complete handler it runs in)
// indefinitely, starving the other channels. This mirrors SendMail's
// negotiation (STARTTLS, then AUTH, then the envelope) over a connection
// with a dial timeout + whole-exchange deadline, matching the 10s bound the
// HTTP channels get from sharedHTTP. PlainAuth still refuses to send
// credentials over an unencrypted non-loopback connection, so authenticated
// relays must offer TLS (documented in Operations.md).
type defaultSMTPSender struct {
	// log, when set, receives a structured Warn if credentials are
	// configured but the relay does not advertise AUTH (the send still
	// proceeds for compatibility — the operator must know it is unauthenticated).
	log *slog.Logger
}

// smtpTimeout bounds dial + the whole SMTP exchange. A var so tests can
// shrink it against a silent fake relay.
var smtpTimeout = 10 * time.Second

func (s defaultSMTPSender) SendMail(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	conn, err := net.DialTimeout("tcp", addr, smtpTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(smtpTimeout)); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return err
		}
	}
	if auth != nil {
		if ok, _ := c.Extension("AUTH"); ok {
			if err := c.Auth(auth); err != nil {
				return err
			}
		} else if s.log != nil {
			// Credentials are configured but the relay offers no AUTH:
			// sending unauthenticated is a compat fallback, not silence —
			// the operator must know the credentials are not being used.
			s.log.Warn("smtp credentials configured but relay does not advertise AUTH; sending unauthenticated",
				"channel", "email", "host", host)
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// defaultSMTPPort is the submission port used when settings.smtp_port is 0.
const defaultSMTPPort = 587

// Email delivers notifications as plain-text mail via net/smtp. All fields
// come from live settings; Sender is the injectable send seam (nil → the
// real net/smtp sender).
type Email struct {
	Host     string
	Port     int
	User     string
	Password string
	From     string
	To       string // comma-separated recipient list
	Sender   SMTPSender
	Log      *slog.Logger
}

// NewEmail builds the email channel from settings. The caller gates on
// SMTPConfigured; a nil sender selects the production net/smtp seam.
func NewEmail(st *model.Settings, sender SMTPSender, log *slog.Logger) *Email {
	if sender == nil {
		sender = defaultSMTPSender{log: log}
	}
	return &Email{
		Host:     st.SMTPHost,
		Port:     st.SMTPPort,
		User:     st.SMTPUser,
		Password: st.SMTPPassword,
		From:     st.SMTPFrom,
		To:       st.SMTPTo,
		Sender:   sender,
		Log:      log,
	}
}

// Name identifies the channel in logs.
func (e *Email) Name() string { return "email" }

// JobFinished sends the outcome with a verdict subject and the plain-text
// fact block (same facts as the webhook JSON and the ntfy body).
func (e *Email) JobFinished(ctx context.Context, j *model.Job, nodeName string) {
	e.send(ctx, jobTitle(j), jobFactsText(j, nodeName, ""))
}

// Alert sends an operational alert with the alert subject and the content
// as the body.
func (e *Email) Alert(ctx context.Context, content string) {
	e.send(ctx, "Encode alert", content)
}

// recipients splits the comma-separated To list into trimmed addresses.
func (e *Email) recipients() []string {
	var out []string
	for _, addr := range strings.Split(e.To, ",") {
		if a := strings.TrimSpace(addr); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// send builds one RFC 5322 plain-text message and hands it to the seam.
// Failures are logged structured and swallowed — never panic the loop.
func (e *Email) send(_ context.Context, subject, body string) {
	to := e.recipients()
	if len(to) == 0 {
		return
	}
	port := e.Port
	if port <= 0 {
		port = defaultSMTPPort
	}
	addr := net.JoinHostPort(e.Host, strconv.Itoa(port))

	var auth smtp.Auth
	if e.User != "" && e.Password != "" {
		auth = smtp.PlainAuth("", e.User, e.Password, e.Host)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", e.From)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	b.WriteString("\r\n")
	// Normalize body line endings to CRLF for the wire.
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))

	if err := e.Sender.SendMail(addr, auth, e.From, to, []byte(b.String())); err != nil {
		e.Log.Warn("email notify failed", "channel", "email", "err", err)
	}
}
