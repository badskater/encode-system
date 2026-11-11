package notify

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/badskater/encode-system/backend/internal/model"
)

// ntfy priority mapping (https://docs.ntfy.sh/publish/#message-priority):
// failed jobs and operational alerts are high (4) so they push through
// phone quiet-hours; done jobs and digests stay at default (3).
const (
	ntfyPriorityDefault = "3"
	ntfyPriorityHigh    = "4"
)

// Ntfy publishes messages to an ntfy topic (https://ntfy.sh or self-hosted).
// The topic URL is the full publish endpoint (scheme + host + topic path);
// an optional access token is sent as a Bearer Authorization header.
type Ntfy struct {
	TopicURL string
	Token    string
	HTTP     *http.Client
	Log      *slog.Logger
}

// Name identifies the channel in logs.
func (n *Ntfy) Name() string { return "ntfy" }

// JobFinished publishes the outcome: title carries the verdict + episode,
// the body carries the plain-text fact block, priority maps failed→high.
func (n *Ntfy) JobFinished(ctx context.Context, j *model.Job, nodeName string) {
	prio := ntfyPriorityDefault
	if j.Status != model.JobDone {
		prio = ntfyPriorityHigh
	}
	n.publish(ctx, jobTitle(j), jobFactsText(j, nodeName, ""), prio)
}

// Alert publishes an operational alert with the alert title and high
// priority; the content is the body verbatim.
func (n *Ntfy) Alert(ctx context.Context, content string) {
	n.publish(ctx, "Encode alert", content, ntfyPriorityHigh)
}

// publish POSTs the body to the topic URL with Title/Priority headers and
// the optional Bearer token. Failures are logged structured and swallowed.
func (n *Ntfy) publish(ctx context.Context, title, body, priority string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.TopicURL, strings.NewReader(body))
	if err != nil {
		n.Log.Warn("ntfy notify failed", "channel", "ntfy", "err", redactErr(err, n.TopicURL))
		return
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("Title", title)
	req.Header.Set("Priority", priority)
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	resp, err := n.HTTP.Do(req)
	if err != nil {
		n.Log.Warn("ntfy notify failed", "channel", "ntfy", "err", redactErr(err, n.TopicURL))
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 300 {
		n.Log.Warn("ntfy notify rejected", "channel", "ntfy", "status", resp.StatusCode)
	}
}
