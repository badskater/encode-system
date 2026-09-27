package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// logStreamChanCap bounds each SSE subscriber's pending-event channel. A
// slow consumer (paused browser tab, stalled network) fills its channel;
// once full, publishes drop events for THAT subscriber instead of blocking
// the heartbeat/completion path that feeds the hub. Dropped events are only
// live-tail cosmetics — the client's next poll/snapshot resynchronizes, and
// the persisted job row remains the source of truth.
const logStreamChanCap = 64

// logStreamKeepalive is the SSE comment ping interval. Proxies and the
// browser's fetch reader treat a silent connection as dead after ~30-60s;
// a ping every 15s keeps the stream alive through idle encode stretches
// (a 4K encode can emit no output for minutes).
const logStreamKeepalive = 15 * time.Second

// logHub fans out live job-progress events to SSE subscribers. Publishers
// (heartbeat handler, completion handler, job-status updates) call Publish
// with a pre-serialized event payload; each open stream subscribes on the
// job id and relays until the job goes terminal. In-memory only: a
// controller restart drops live subscribers (they reconnect and re-snapshot
// from the persisted job row), which is acceptable for tail-only data.
type logHub struct {
	mu   sync.Mutex
	subs map[int64]map[chan string]struct{}
}

func newLogHub() *logHub {
	return &logHub{subs: map[int64]map[chan string]struct{}{}}
}

// subscribe registers a channel for one job and returns it plus the
// unsubscribe closure. The closure is idempotent-safe for the handler's
// defer.
func (h *logHub) subscribe(jobID int64) (<-chan string, func()) {
	ch := make(chan string, logStreamChanCap)
	h.mu.Lock()
	if h.subs[jobID] == nil {
		h.subs[jobID] = map[chan string]struct{}{}
	}
	h.subs[jobID][ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			if set := h.subs[jobID]; set != nil {
				delete(set, ch)
				if len(set) == 0 {
					delete(h.subs, jobID)
				}
			}
			h.mu.Unlock()
			close(ch)
		})
	}
}

// publish sends an event to every subscriber of a job. A full channel drops
// the event (slow-consumer protection — see logStreamChanCap); publishing
// must never block the heartbeat path.
func (h *logHub) publish(jobID int64, event string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[jobID] {
		select {
		case ch <- event:
		default: // subscriber stalled — drop; snapshot on reconnect resyncs
		}
	}
}

// sseEvent is the JSON payload of one live log-stream event. It mirrors the
// fields the Jobs UI already renders from GET /api/jobs/{id}, so the client
// can reuse its formatting: step name, overall progress, the bounded log
// tail, and terminal bookkeeping (status/error/exit_code when final).
type sseEvent struct {
	Type     string  `json:"type"`                // "progress" | "final"
	Step     string  `json:"step,omitempty"`      // current step name
	Progress float64 `json:"progress,omitempty"`  // step percentage 0-100
	LogTail  string  `json:"log_tail,omitempty"`  // recent raw output lines
	Status   string  `json:"status,omitempty"`    // job status (final events)
	Error    string  `json:"error,omitempty"`     // failure message (final events)
	ExitCode *int    `json:"exit_code,omitempty"` // process exit code (final events)
	FullLog  bool    `json:"full_log,omitempty"`  // a captured full log is available via GET /api/jobs/{id}/log
}

// publishProgress pushes a live progress snapshot for a job. Called from the
// heartbeat handler (every 5s while a job runs) after the DB update lands.
func (s *Server) publishProgress(jobID int64, step string, progress float64, logTail string) {
	ev, err := json.Marshal(sseEvent{Type: "progress", Step: step, Progress: progress, LogTail: logTail})
	if err != nil {
		return // sseEvent is fully static JSON — marshal cannot fail in practice
	}
	s.logHub.publish(jobID, string(ev))
}

// publishFinal pushes the terminal event for a job and (via the subscriber
// loop's status re-check) closes its stream. Called from the completion
// handler and the orphan-recovery path.
func (s *Server) publishFinal(jobID int64, status, errMsg string, exitCode int, hasFullLog bool) {
	ev, err := json.Marshal(sseEvent{Type: "final", Status: status, Error: errMsg, ExitCode: &exitCode, FullLog: hasFullLog})
	if err != nil {
		return
	}
	s.logHub.publish(jobID, string(ev))
}

// handleJobLogStream serves a Server-Sent Events stream of live progress for
// one job. Flow: snapshot the persisted state immediately (so a client that
// attaches mid-encode sees current step/tail without waiting for the next
// heartbeat), then relay hub events until the job is terminal or the client
// disconnects. Admin-only, matching handleGetJobLog (log tails can hold node
// paths and errors). Kept on text/event-stream + the standard fetch/ReadableStream
// client pattern: EventSource cannot send the Authorization header.
func (s *Server) handleJobLogStream(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad job id")
		return
	}
	job, err := s.Store.GetJob(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Without flush support the response would buffer until handler
		// return — useless for a live stream. Fail loudly instead.
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Initial snapshot: current step/progress/tail from the persisted row.
	// A terminal job sends one final event and closes immediately — the
	// stream is a live tail, not a log archive (GET /log serves that).
	if job.Status.Terminal() {
		writeSSE(w, flusher, sseEvent{
			Type: "final", Status: string(job.Status), Error: job.Error,
			ExitCode: &job.ExitCode, FullLog: job.FullLog != "",
		})
		return
	}
	writeSSE(w, flusher, sseEvent{
		Type: "progress", Step: job.Step, Progress: job.Progress, LogTail: job.LogTail,
	})
	flusher.Flush()

	ch, unsub := s.logHub.subscribe(id)
	defer unsub()

	keepalive := time.NewTicker(logStreamKeepalive)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done(): // client disconnected
			return
		case ev, ok := <-ch:
			if !ok {
				return // channel closed by unsub race — defensive
			}
			fmt.Fprintf(w, "data: %s\n\n", ev)
			flusher.Flush()
			// A final event means the job is terminal: close the stream.
			// The publisher already stamped status in the payload; re-check
			// the DB rather than parsing the event so a missed final publish
			// (e.g. controller-side error) still terminates the stream via
			// the periodic check below.
			var decoded sseEvent
			if json.Unmarshal([]byte(ev), &decoded) == nil && decoded.Type == "final" {
				return
			}
		case <-keepalive.C:
			// Periodic terminal re-check: guards against a lost final event
			// (publish is best-effort) leaving a stream open forever.
			if j, err := s.Store.GetJob(ctx, id); err == nil && j.Status.Terminal() {
				writeSSE(w, flusher, sseEvent{
					Type: "final", Status: string(j.Status), Error: j.Error,
					ExitCode: &j.ExitCode, FullLog: j.FullLog != "",
				})
				return
			}
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// writeSSE serializes one event frame. Marshal failures are impossible for
// the static sseEvent shape; the error is swallowed to keep the frame loop
// allocation-free of error plumbing.
func writeSSE(w http.ResponseWriter, flusher http.Flusher, ev sseEvent) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", b)
	flusher.Flush()
}
