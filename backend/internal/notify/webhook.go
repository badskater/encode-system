package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// WebhookPayload is the JSON document posted by the generic webhook channel.
// Shape (stable contract, documented in Docs/md/Operations.md):
//
//	{
//	  "event": "job_finished" | "alert",
//	  "timestamp": "<RFC3339 UTC>",
//	  "job_id": 7, "series": "...", "episode": "03",
//	  "status": "done|failed", "step": "...", "error": "...",
//	  "node": "...", "retry_count": 2, "duration_seconds": 90,
//	  "message": "...",            // alert events only (incl. digest summaries)
//	  "link": "http://ctrl/jobs?job=7"
//	}
//
// Job fields are omitted on alert events; message is omitted on job events.
// Digest summaries arrive as alert events whose message is the summary text.
type WebhookPayload struct {
	Event           string    `json:"event"`
	Timestamp       time.Time `json:"timestamp"`
	JobID           int64     `json:"job_id,omitempty"`
	Series          string    `json:"series,omitempty"`
	Episode         string    `json:"episode,omitempty"`
	Status          string    `json:"status,omitempty"`
	Step            string    `json:"step,omitempty"`
	Error           string    `json:"error,omitempty"`
	Node            string    `json:"node,omitempty"`
	RetryCount      int       `json:"retry_count,omitempty"`
	DurationSeconds float64   `json:"duration_seconds,omitempty"`
	Message         string    `json:"message,omitempty"`
	Link            string    `json:"link,omitempty"`
}

// Webhook posts structured JSON events to any HTTP endpoint. Unlike the
// Discord transport (fixed {"content": ...} shape) this is a machine-readable
// feed for home-grown automation, Node-RED, n8n, etc.
type Webhook struct {
	URL           string
	ControllerURL string
	HTTP          *http.Client
	Log           *slog.Logger
}

// Name identifies the channel in logs.
func (w *Webhook) Name() string { return "webhook" }

// JobFinished posts the terminal outcome as a job_finished event carrying
// the full fact set (series/episode/status/error/node/retries/duration/link).
func (w *Webhook) JobFinished(ctx context.Context, j *model.Job, nodeName string) {
	p := WebhookPayload{
		Event:     "job_finished",
		Timestamp: time.Now().UTC(),
		JobID:     j.ID,
		Series:    j.Series,
		Episode:   j.Episode,
		Status:    string(j.Status),
		Node:      nodeName,
		Link:      jobLink(w.ControllerURL, j.ID),
	}
	if j.Status != model.JobDone {
		p.Step = j.Step
		p.Error = j.Error
		p.RetryCount = j.RetryCount
	}
	if j.StartedAt != nil && j.FinishedAt != nil && !j.StartedAt.IsZero() && !j.FinishedAt.IsZero() {
		p.DurationSeconds = j.FinishedAt.Sub(*j.StartedAt).Seconds()
	}
	w.post(ctx, p)
}

// Alert posts a free-form operational alert (disk space, node status, digest
// summary) as an alert event with the pre-formatted message.
func (w *Webhook) Alert(ctx context.Context, content string) {
	w.post(ctx, WebhookPayload{Event: "alert", Timestamp: time.Now().UTC(), Message: content})
}

// post sends one payload. Failures are logged structured and swallowed —
// the fan-out must continue with the remaining channels.
func (w *Webhook) post(ctx context.Context, p WebhookPayload) {
	body, err := json.Marshal(p)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		w.Log.Warn("webhook notify failed", "channel", "webhook", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.HTTP.Do(req)
	if err != nil {
		w.Log.Warn("webhook notify failed", "channel", "webhook", "err", err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 300 {
		w.Log.Warn("webhook notify rejected", "channel", "webhook", "status", resp.StatusCode)
	}
}
