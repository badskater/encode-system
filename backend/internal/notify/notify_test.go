package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(devNull{}, nil))
}

type devNull struct{}

func (devNull) Write(p []byte) (int, error) { return len(p), nil }

// captureWebhook records every payload posted to it.
type captureWebhook struct {
	mu     sync.Mutex
	bodies []map[string]string
	status int
}

func (c *captureWebhook) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		c.bodies = append(c.bodies, body)
		c.mu.Unlock()
		if c.status != 0 {
			w.WriteHeader(c.status)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func (c *captureWebhook) last() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		return nil
	}
	return c.bodies[len(c.bodies)-1]
}

// Empty webhook URL must yield the no-op notifier.
func TestNewDiscordEmptyIsNop(t *testing.T) {
	if _, ok := NewDiscord("", testLog()).(Nop); !ok {
		t.Fatal("empty URL must produce the Nop notifier")
	}
	if _, ok := NewDiscord("   ", testLog()).(Nop); !ok {
		t.Fatal("whitespace URL must produce the Nop notifier")
	}
}

// Done jobs post a success alert with series/episode/release context.
func TestJobFinishedDone(t *testing.T) {
	cap := &captureWebhook{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	n := NewDiscord(srv.URL, testLog())
	start := time.Now().Add(-42 * time.Minute)
	fin := time.Now()
	n.JobFinished(context.Background(), &model.Job{
		ID: 7, Series: "Ookami-san", Episode: "05", FlowID: 2,
		Status: model.JobDone, StartedAt: &start, FinishedAt: &fin,
	}, "enc-01")

	body := cap.last()
	if body == nil {
		t.Fatal("no webhook call received")
	}
	content := body["content"]
	for _, want := range []string{"✅", "Ookami-san", "Ep 05", "enc-01", "42m"} {
		if !strings.Contains(content, want) {
			t.Errorf("alert missing %q: %s", want, content)
		}
	}
}

// Failed jobs carry the error and the step that died.
func TestJobFinishedFailed(t *testing.T) {
	cap := &captureWebhook{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	n := NewDiscord(srv.URL, testLog())
	n.JobFinished(context.Background(), &model.Job{
		ID: 8, Series: "SeriesB", Episode: "01", FlowID: 1,
		Status: model.JobFailed, Step: "mux", Error: "mux input missing: audio.opus",
	}, "enc-02")

	content := cap.last()["content"]
	for _, want := range []string{"❌", "SeriesB", "mux", "audio.opus"} {
		if !strings.Contains(content, want) {
			t.Errorf("alert missing %q: %s", want, content)
		}
	}
}

// Webhook rejections must not panic or block the caller.
func TestJobFinishedWebhookError(t *testing.T) {
	cap := &captureWebhook{status: http.StatusBadGateway}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	n := NewDiscord(srv.URL, testLog())
	done := make(chan struct{})
	go func() {
		n.JobFinished(context.Background(), &model.Job{ID: 1, Series: "S", Episode: "01", Status: model.JobDone}, "n")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("notifier blocked on a failing webhook")
	}
}

// Very long errors are truncated for the payload.
func TestTruncate(t *testing.T) {
	long := strings.Repeat("x", 500)
	// 100 kept chars + a 3-byte ellipsis.
	if got := truncate(long, 100); len(got) > 103 {
		t.Errorf("truncate left %d chars", len(got))
	}
	if got := truncate("a\nb", 100); strings.Contains(got, "\n") {
		t.Error("truncate must flatten newlines")
	}
}
