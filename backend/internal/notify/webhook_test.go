package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// webhookCapture records the requests posted to a mock generic-webhook
// endpoint so tests can assert method, path, content type and payload.
type webhookCapture struct {
	mu      sync.Mutex
	method  string
	path    string
	ctype   string
	payload WebhookPayload
	calls   int
	status  int
}

func (c *webhookCapture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.method = r.Method
		c.path = r.URL.Path
		c.ctype = r.Header.Get("Content-Type")
		c.calls++
		json.Unmarshal(body, &c.payload)
		status := c.status
		c.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func (c *webhookCapture) snapshot() (string, string, string, WebhookPayload, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.method, c.path, c.ctype, c.payload, c.calls
}

// TestWebhookJobFinishedPayload: a failed job posts a JSON document carrying
// the event type, series/episode, status, error, node, retry count, duration,
// deep link and a timestamp.
func TestWebhookJobFinishedPayload(t *testing.T) {
	cap := &webhookCapture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	wh := &Webhook{URL: srv.URL + "/hook", ControllerURL: "http://ctrl:8080/", HTTP: srv.Client(), Log: testLog()}
	started := time.Now().UTC().Add(-90 * time.Second).Truncate(time.Second)
	finished := started.Add(90 * time.Second)
	j := &model.Job{
		ID: 7, Series: "SomeShow", Episode: "03", Status: model.JobFailed,
		Step: "encode", Error: "x265 crashed", RetryCount: 2,
		StartedAt: &started, FinishedAt: &finished,
	}
	wh.JobFinished(context.Background(), j, "enc-01")

	method, path, ctype, p, calls := cap.snapshot()
	if calls != 1 {
		t.Fatalf("want 1 POST, got %d", calls)
	}
	if method != http.MethodPost || path != "/hook" {
		t.Fatalf("want POST /hook, got %s %s", method, path)
	}
	if ctype != "application/json" {
		t.Fatalf("content type = %q", ctype)
	}
	if p.Event != "job_finished" {
		t.Errorf("event = %q", p.Event)
	}
	if p.JobID != 7 || p.Series != "SomeShow" || p.Episode != "03" {
		t.Errorf("job identity wrong: %+v", p)
	}
	if p.Status != "failed" || p.Error != "x265 crashed" || p.Step != "encode" {
		t.Errorf("failure facts wrong: %+v", p)
	}
	if p.Node != "enc-01" || p.RetryCount != 2 {
		t.Errorf("node/retries wrong: %+v", p)
	}
	if p.DurationSeconds < 89 || p.DurationSeconds > 91 {
		t.Errorf("duration = %v, want ~90", p.DurationSeconds)
	}
	if p.Link != "http://ctrl:8080/jobs?job=7" {
		t.Errorf("link = %q", p.Link)
	}
	if p.Timestamp.IsZero() {
		t.Error("timestamp must be set")
	}
}

// TestWebhookDonePayload: a done job carries status=done and no error.
func TestWebhookDonePayload(t *testing.T) {
	cap := &webhookCapture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	wh := &Webhook{URL: srv.URL, HTTP: srv.Client(), Log: testLog()}
	wh.JobFinished(context.Background(), &model.Job{ID: 1, Series: "S", Episode: "01", Status: model.JobDone}, "n1")
	_, _, _, p, _ := cap.snapshot()
	if p.Event != "job_finished" || p.Status != "done" || p.Error != "" {
		t.Fatalf("done payload wrong: %+v", p)
	}
	if p.Link != "" {
		t.Errorf("no controller URL → no link, got %q", p.Link)
	}
}

// TestWebhookAlertPayload: free-form operational alerts post event=alert
// with the pre-formatted message.
func TestWebhookAlertPayload(t *testing.T) {
	cap := &webhookCapture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	wh := &Webhook{URL: srv.URL, HTTP: srv.Client(), Log: testLog()}
	wh.Alert(context.Background(), "💾 low disk on enc-01")
	_, _, _, p, calls := cap.snapshot()
	if calls != 1 || p.Event != "alert" || p.Message != "💾 low disk on enc-01" {
		t.Fatalf("alert payload wrong: %+v (calls=%d)", p, calls)
	}
	if p.Timestamp.IsZero() {
		t.Error("timestamp must be set")
	}
}

// TestWebhookFailureIsContained: a rejecting endpoint and an unreachable URL
// must return without panicking (the fan-out loop keeps going).
func TestWebhookFailureIsContained(t *testing.T) {
	cap := &webhookCapture{status: http.StatusInternalServerError}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	wh := &Webhook{URL: srv.URL, HTTP: srv.Client(), Log: testLog()}
	wh.JobFinished(context.Background(), &model.Job{ID: 1, Status: model.JobDone}, "n")
	wh.Alert(context.Background(), "x")

	dead := &Webhook{URL: "http://127.0.0.1:0/unreachable", HTTP: sharedHTTP, Log: testLog()}
	dead.JobFinished(context.Background(), &model.Job{ID: 1, Status: model.JobDone}, "n")
	dead.Alert(context.Background(), "x")
}
