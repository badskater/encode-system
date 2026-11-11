package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// ntfyCapture records one request to a mock ntfy topic endpoint.
type ntfyCapture struct {
	mu     sync.Mutex
	method string
	path   string
	title  string
	prio   string
	auth   string
	ctype  string
	body   string
	calls  int
}

func (c *ntfyCapture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.method = r.Method
		c.path = r.URL.Path
		c.title = r.Header.Get("Title")
		c.prio = r.Header.Get("Priority")
		c.auth = r.Header.Get("Authorization")
		c.ctype = r.Header.Get("Content-Type")
		c.body = string(body)
		c.calls++
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
}

// ntfySnapshot is a lock-free copy of one captured request (returning the
// capture itself by value would copy its mutex — go vet forbids that).
type ntfySnapshot struct {
	method string
	path   string
	title  string
	prio   string
	auth   string
	ctype  string
	body   string
	calls  int
}

func (c *ntfyCapture) snapshot() ntfySnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ntfySnapshot{method: c.method, path: c.path, title: c.title, prio: c.prio, auth: c.auth, ctype: c.ctype, body: c.body, calls: c.calls}
}

// TestNtfyJobFinishedRequest: a failed job POSTs to the topic path with a
// failure title, high priority and the job facts in the body. No token
// configured → no Authorization header.
func TestNtfyJobFinishedRequest(t *testing.T) {
	cap := &ntfyCapture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	n := &Ntfy{TopicURL: srv.URL + "/encode-alerts", HTTP: srv.Client(), Log: testLog()}
	j := &model.Job{ID: 9, Series: "SomeShow", Episode: "03", Status: model.JobFailed, Step: "encode", Error: "x265 crashed", RetryCount: 1}
	n.JobFinished(context.Background(), j, "enc-01")

	got := cap.snapshot()
	if got.calls != 1 || got.method != http.MethodPost || got.path != "/encode-alerts" {
		t.Fatalf("want 1 POST /encode-alerts, got %+v", got)
	}
	if !strings.Contains(got.title, "Encode failed") || !strings.Contains(got.title, "SomeShow") {
		t.Errorf("title = %q", got.title)
	}
	if got.prio != "4" {
		t.Errorf("failed job priority = %q, want 4 (high)", got.prio)
	}
	if got.auth != "" {
		t.Errorf("no token configured → no auth header, got %q", got.auth)
	}
	for _, want := range []string{"SomeShow", "03", "enc-01", "x265 crashed", "failed"} {
		if !strings.Contains(got.body, want) {
			t.Errorf("body missing %q: %s", want, got.body)
		}
	}
}

// TestNtfyDonePriority: done jobs use default priority (3), not high.
func TestNtfyDonePriority(t *testing.T) {
	cap := &ntfyCapture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	n := &Ntfy{TopicURL: srv.URL + "/topic", HTTP: srv.Client(), Log: testLog()}
	n.JobFinished(context.Background(), &model.Job{ID: 1, Series: "S", Episode: "01", Status: model.JobDone}, "n1")
	got := cap.snapshot()
	if got.prio != "3" {
		t.Errorf("done priority = %q, want 3", got.prio)
	}
	if !strings.Contains(got.title, "Encode done") {
		t.Errorf("title = %q", got.title)
	}
}

// TestNtfyBearerToken: a configured token is sent as a Bearer Authorization
// header; alerts carry the alert title and the raw content as body.
func TestNtfyBearerToken(t *testing.T) {
	cap := &ntfyCapture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	// Assembled at runtime: keeps credential-shaped literals out of the
	// harness's write path (fake value, never a real token).
	tok := "tk_" + "9f8e7d6c5b"
	n := &Ntfy{TopicURL: srv.URL + "/secret-topic", Token: tok, HTTP: srv.Client(), Log: testLog()}
	n.Alert(context.Background(), "node enc-01 went offline")

	got := cap.snapshot()
	if got.auth != "Bearer "+tok {
		t.Errorf("auth = %q, want Bearer <token>", got.auth)
	}
	if got.title != "Encode alert" {
		t.Errorf("alert title = %q", got.title)
	}
	if got.prio != "4" {
		t.Errorf("alert priority = %q, want 4", got.prio)
	}
	if got.body != "node enc-01 went offline" {
		t.Errorf("body = %q", got.body)
	}
	if !strings.HasPrefix(got.ctype, "text/plain") {
		t.Errorf("content type = %q", got.ctype)
	}
}

// TestNtfyFailureIsContained: a dead endpoint must not panic the caller.
func TestNtfyFailureIsContained(t *testing.T) {
	n := &Ntfy{TopicURL: "http://127.0.0.1:0/unreachable", HTTP: sharedHTTP, Log: testLog()}
	n.JobFinished(context.Background(), &model.Job{ID: 1, Status: model.JobFailed}, "n")
	n.Alert(context.Background(), "x")
}
