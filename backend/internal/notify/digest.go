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

// DigestEntry is one buffered job-outcome event awaiting a digest flush.
// It captures the fields the summary line needs (series/episode/status/node)
// without holding a reference to the whole Job (the jobs table is the source
// of truth; the buffer is transient observability data).
type DigestEntry struct {
	JobID    int64
	Series   string
	Episode  string
	Status   model.JobStatus
	NodeName string
	At       time.Time
}

// DigestBuffer is a mutex-guarded slice of job-outcome events. When digest
// mode is ON, the controller buffers events here instead of alerting per job;
// a ticker goroutine flushes the buffer on an interval, posting ONE summary.
//
// The buffer is in-memory only: a controller restart loses pending events.
// This is acceptable — the buffer is observability data, not durable state;
// the jobs table remains the source of truth for what actually ran. A job
// that completed during the lost window simply never appears in a digest,
// which is strictly better than losing the job itself.
type DigestBuffer struct {
	mu      sync.Mutex
	entries []DigestEntry
}

// NewDigestBuffer returns an empty, ready buffer.
func NewDigestBuffer() *DigestBuffer { return &DigestBuffer{} }

// Push appends one job-outcome event. Safe for concurrent callers (the
// heartbeat/complete path and the orphan-recovery path can both fire).
func (d *DigestBuffer) Push(e DigestEntry) {
	d.mu.Lock()
	d.entries = append(d.entries, e)
	d.mu.Unlock()
}

// Drain returns and clears the buffer in one atomic step. The caller owns
// the returned slice afterward. Returns nil when empty (the flush path uses
// a nil check to stay silent on empty ticks — no empty-message posts).
func (d *DigestBuffer) Drain() []DigestEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]DigestEntry, len(d.entries))
	copy(out, d.entries)
	d.entries = nil
	return out
}

// Len returns the current count (test helper; not used on the hot path).
func (d *DigestBuffer) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries)
}

// digestMaxLines caps the per-job lines in a single digest message so a
// 100-job hour doesn't blow past Discord's 2000-char content limit. The
// summary header + the controller link reserve headroom; the remaining lines
// are capped, with a "…and K more" trailer when truncated.
const digestMaxLines = 20

// FlushDigest posts a single summary of buffered job-outcome events to the
// Discord webhook, then clears the buffer. An empty buffer is a silent
// no-op (no empty-message post). The message shape:
//
//	Encode digest (1h): ✅ N done · ❌ M failed
//	✅ Series Ep 01 · node
//	…
//	<controller_url>/jobs
//
// When more than digestMaxLines jobs are buffered, the lines are capped and a
// "…and K more" trailer is appended. The whole payload is capped at
// discordCap (truncate) so it always fits Discord's limit.
//
// When webhookURL is empty, the flush is a silent no-op (notifications off),
// but the buffer is still drained — events are not retained forever when
// there is nowhere to send them.
func FlushDigest(log *slog.Logger, webhookURL, controllerURL string, buf *DigestBuffer, _ time.Time) {
	entries := buf.Drain()
	if len(entries) == 0 {
		return // silent tick — nothing to report
	}
	webhookURL = strings.TrimSpace(webhookURL)
	if webhookURL == "" {
		return // notifications off; buffer already drained
	}

	// "failed" here means "not done" — the buffer only ever receives done/failed
	// completions (cancelled jobs never reach notifyJobFinished), so the bucket
	// is accurate in practice; the name is kept for the Discord message text.
	done, failed := 0, 0
	for _, e := range entries {
		if e.Status == model.JobDone {
			done++
		} else {
			failed++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Encode digest (1h): ✅ %d done · ❌ %d failed\n", done, failed)
	shown := 0
	for _, e := range entries {
		if shown >= digestMaxLines {
			remaining := len(entries) - shown
			fmt.Fprintf(&b, "…and %d more\n", remaining)
			break
		}
		emoji := "✅"
		if e.Status != model.JobDone {
			emoji = "❌"
		}
		fmt.Fprintf(&b, "%s %s Ep %s · %s\n", emoji, e.Series, e.Episode, e.NodeName)
		shown++
	}
	// Controller link to the Jobs fleet view (a digest has no single job to
	// deep-link to). jobLink with id 0 produces "…/jobs?job=0"; we strip the
	// query so the digest links to the fleet, not job #0.
	if link := jobLink(controllerURL, 0); link != "" {
		fmt.Fprintf(&b, "%s", strings.TrimSuffix(link, "?job=0"))
	}

	content := truncate(b.String(), discordCap)
	d := &Discord{WebhookURL: webhookURL, HTTP: sharedHTTP, Log: log}
	d.post(context.Background(), content)
}
