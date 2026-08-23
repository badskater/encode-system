// Package notify sends operational alerts out of the control plane. The only
// built-in transport is a Discord webhook (simple POST, no bot, no gateway);
// the Notifier interface keeps the API handlers free of transport details.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// Notifier delivers job-outcome alerts. Implementations must be safe to call
// concurrently and must never block the caller for long.
type Notifier interface {
	// JobFinished reports a terminal job outcome (done or failed).
	JobFinished(ctx context.Context, j *model.Job, nodeName string)
}

// Nop is the zero notifier used when nothing is configured.
type Nop struct{}

// JobFinished does nothing.
func (Nop) JobFinished(context.Context, *model.Job, string) {}

// Discord posts alerts to a webhook URL. Messages are plain-text embeds;
// Discord accepts a JSON body of {"content": "..."}.
type Discord struct {
	WebhookURL string
	HTTP       *http.Client
	Log        *slog.Logger
}

// NewDiscord builds a notifier for the given webhook URL. An empty URL
// returns Nop so callers never need to nil-check.
func NewDiscord(webhookURL string, log *slog.Logger) Notifier {
	if strings.TrimSpace(webhookURL) == "" {
		return Nop{}
	}
	return &Discord{
		WebhookURL: webhookURL,
		HTTP:       &http.Client{Timeout: 10 * time.Second},
		Log:        log,
	}
}

// JobFinished formats and posts the outcome. Failures carry the error and a
// short log tail so the alert is actionable without opening the UI.
func (d *Discord) JobFinished(ctx context.Context, j *model.Job, nodeName string) {
	var b strings.Builder
	if j.Status == model.JobDone {
		fmt.Fprintf(&b, "✅ **Encode done** — %s Ep %s\n", j.Series, j.Episode)
		fmt.Fprintf(&b, "node: `%s` · flow: #%d · release: `[Group] %s - Raws`", nodeName, j.FlowID, j.Series)
	} else {
		fmt.Fprintf(&b, "❌ **Encode failed** — %s Ep %s\n", j.Series, j.Episode)
		fmt.Fprintf(&b, "node: `%s` · step: `%s`", nodeName, j.Step)
		if j.Error != "" {
			fmt.Fprintf(&b, "\nerror: `%s`", truncate(j.Error, 300))
		}
	}
	if j.StartedAt != nil && j.FinishedAt != nil && !j.StartedAt.IsZero() && !j.FinishedAt.IsZero() {
		fmt.Fprintf(&b, "\nduration: %s", j.FinishedAt.Sub(*j.StartedAt).Round(time.Second))
	}

	payload := map[string]string{"content": b.String()}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.HTTP.Do(req)
	if err != nil {
		d.Log.Warn("discord notify failed", "err", err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 300 {
		d.Log.Warn("discord notify rejected", "status", resp.StatusCode)
	}
}

// truncate caps a string for alert payloads.
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
