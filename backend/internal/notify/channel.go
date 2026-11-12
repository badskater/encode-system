package notify

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// Channel is one notification transport. It mirrors the Notifier event
// surface (job outcomes + free-form operational alerts) so every transport
// receives the same events with the same semantics — the skip rules (series
// mute, digest buffering, silent auto-retry) are applied by the API layer
// BEFORE the fan-out, so a skipped event reaches no channel at all.
//
// Implementations must be safe for concurrent use, must never panic, and
// must log their own delivery failures (structured, with a "channel" field)
// instead of returning them — one broken transport must not block the rest.
type Channel interface {
	// Name identifies the transport in logs ("discord", "webhook", ...).
	Name() string
	// JobFinished reports a terminal job outcome (done or failed).
	JobFinished(ctx context.Context, j *model.Job, nodeName string)
	// Alert posts a free-form operational alert (disk space, node status,
	// digest summaries).
	Alert(ctx context.Context, content string)
}

// Fanout delivers every event to every configured channel IN PARALLEL. It
// implements Notifier so it drops into the existing notification seam.
// Isolation is twofold: channels log-and-return on transport errors, and a
// defensive recover contains a panicking channel so the remaining channels
// (and the caller's loop) always continue. Parallel delivery keeps the
// worst case at ≈ the SLOWEST channel's bound (10s) instead of the SUM of
// all channel bounds (~50s) — the fan-out runs inside request handlers.
type Fanout struct {
	Channels []Channel
	Log      *slog.Logger
}

// NewFanout builds a dispatcher over the given channels. A nil/empty slice
// is a silent no-op (nothing configured). A nil logger falls back to the
// slog default so the dispatcher never panics on a missing logger.
func NewFanout(channels []Channel, log *slog.Logger) *Fanout {
	if log == nil {
		log = slog.Default()
	}
	return &Fanout{Channels: channels, Log: log}
}

// JobFinished fans a terminal job outcome out to every channel in parallel
// and waits for all of them (the caller's ctx bounds each channel).
func (f *Fanout) JobFinished(ctx context.Context, j *model.Job, nodeName string) {
	f.fan(func(ch Channel) { ch.JobFinished(ctx, j, nodeName) })
}

// Alert fans a free-form operational alert out to every channel in parallel.
func (f *Fanout) Alert(ctx context.Context, content string) {
	f.fan(func(ch Channel) { ch.Alert(ctx, content) })
}

// fan runs fn on every channel concurrently, one goroutine per channel,
// each panic-contained by deliver. It blocks until every channel finished
// so callers keep the same synchronous semantics (a notification is fully
// delivered — or failed — before the handler continues).
func (f *Fanout) fan(fn func(Channel)) {
	var wg sync.WaitGroup
	for _, ch := range f.Channels {
		wg.Add(1)
		go func(ch Channel) {
			defer wg.Done()
			f.deliver(ch.Name(), func() { fn(ch) })
		}(ch)
	}
	wg.Wait()
}

// deliver runs one channel call, containing a panic so a broken transport
// can never take down the notification loop or starve the other channels.
func (f *Fanout) deliver(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			f.Log.Error("notify channel panicked", "channel", name, "panic", r)
		}
	}()
	fn()
}

// SMTPConfigured reports whether the email channel has the minimum fields
// to send: host, from and to. Partial configurations are rejected by
// settings validation, so this only ever sees all-or-nothing in practice.
func SMTPConfigured(st *model.Settings) bool {
	return st != nil && st.SMTPHost != "" && st.SMTPFrom != "" && st.SMTPTo != ""
}

// AnyConfigured reports whether at least one notification transport is
// configured (Discord included). Used by the node-status alert loop to gate
// alerts: with no transport at all there is nowhere to send them.
func AnyConfigured(st *model.Settings) bool {
	if st == nil {
		return false
	}
	return st.DiscordWebhook != "" || st.GenericWebhookURL != "" || st.NtfyTopicURL != "" || SMTPConfigured(st)
}

// BuildChannels assembles the enabled channels from live settings. Order is
// stable: discord, webhook, ntfy, email. discordHook is the EFFECTIVE Discord
// webhook (the per-series override when one is set); when empty, the global
// settings webhook applies. The other transports always use the global
// settings — a per-series webhook is a Discord-channel concept. mail is the
// SMTP sender seam (nil → the real net/smtp sender). An empty configuration
// yields an empty slice.
func BuildChannels(st *model.Settings, discordHook string, mail SMTPSender, log *slog.Logger) []Channel {
	if st == nil {
		return nil
	}
	if strings.TrimSpace(discordHook) == "" {
		discordHook = st.DiscordWebhook
	}
	var chs []Channel
	if strings.TrimSpace(discordHook) != "" {
		chs = append(chs, &Discord{
			WebhookURL:    strings.TrimSpace(discordHook),
			ControllerURL: strings.TrimSpace(st.ControllerURL),
			HTTP:          sharedHTTP,
			Log:           log,
		})
	}
	if u := strings.TrimSpace(st.GenericWebhookURL); u != "" {
		chs = append(chs, &Webhook{URL: u, ControllerURL: strings.TrimSpace(st.ControllerURL), HTTP: sharedHTTP, Log: log})
	}
	if u := strings.TrimSpace(st.NtfyTopicURL); u != "" {
		chs = append(chs, &Ntfy{TopicURL: u, Token: st.NtfyToken, HTTP: sharedHTTP, Log: log})
	}
	if SMTPConfigured(st) {
		chs = append(chs, NewEmail(st, mail, log))
	}
	return chs
}

// NewFanoutFromSettings is the one-call wiring used by the API notification
// paths: build the enabled channels from live settings and wrap them in the
// isolating dispatcher.
func NewFanoutFromSettings(st *model.Settings, discordHook string, mail SMTPSender, log *slog.Logger) *Fanout {
	return NewFanout(BuildChannels(st, discordHook, mail, log), log)
}

// jobFactsText renders the plain-text fact block shared by the ntfy and
// email channels (the generic webhook carries the same facts as JSON).
// One line per fact; empty facts are omitted so alerts stay compact.
func jobFactsText(j *model.Job, nodeName, controllerURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Series: %s\n", j.Series)
	fmt.Fprintf(&b, "Episode: %s\n", j.Episode)
	fmt.Fprintf(&b, "Status: %s\n", j.Status)
	if nodeName != "" {
		fmt.Fprintf(&b, "Node: %s\n", nodeName)
	}
	if j.Status != model.JobDone && j.Step != "" {
		fmt.Fprintf(&b, "Step: %s\n", j.Step)
	}
	if j.Error != "" {
		fmt.Fprintf(&b, "Error: %s\n", j.Error)
	}
	if j.RetryCount > 0 {
		fmt.Fprintf(&b, "Retries: %d\n", j.RetryCount)
	}
	if j.StartedAt != nil && j.FinishedAt != nil && !j.StartedAt.IsZero() && !j.FinishedAt.IsZero() {
		fmt.Fprintf(&b, "Duration: %s\n", j.FinishedAt.Sub(*j.StartedAt).Round(time.Second))
	}
	if link := jobLink(controllerURL, j.ID); link != "" {
		fmt.Fprintf(&b, "Link: %s\n", link)
	}
	fmt.Fprintf(&b, "Timestamp: %s\n", time.Now().UTC().Format(time.RFC3339))
	return b.String()
}

// jobTitle builds the shared subject/title for a job outcome ("Encode done
// — Series Ep NN" / "Encode failed — Series Ep NN"). The result is sanitized
// for header safety: series names can carry CRLF/control chars (the scanner
// ingests folder names directly, bypassing API validation), which would
// inject headers into the email Subject (Bcc exploit) or make Go reject the
// ntfy Title header (per-series delivery loss).
func jobTitle(j *model.Job) string {
	if j.Status == model.JobDone {
		return sanitizeHeader(fmt.Sprintf("Encode done — %s Ep %s", j.Series, j.Episode))
	}
	return sanitizeHeader(fmt.Sprintf("Encode failed — %s Ep %s", j.Series, j.Episode))
}

// sanitizeHeader replaces CR/LF and every other C0 control char (plus DEL)
// with a space so a value is safe to put in a single-line message header
// (email Subject, ntfy Title). Spaces keep words separated; printable
// Unicode (e.g. the em dash, CJK titles) passes through untouched.
func sanitizeHeader(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
