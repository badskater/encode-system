package notify

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// NewDiscordWithLink with an empty webhook must still return Nop (deep links
// are meaningless without a transport).
func TestNewDiscordWithLinkEmptyIsNop(t *testing.T) {
	if _, ok := NewDiscordWithLink("", "http://host:8080", testLog()).(Nop); !ok {
		t.Fatal("empty webhook URL must produce Nop even with a controller URL")
	}
}

// jobLink returns "" when the controller URL is unset (deep links are opt-in).
func TestJobLinkEmptyController(t *testing.T) {
	if got := jobLink("", 5); got != "" {
		t.Fatalf("empty controller URL must yield empty link, got %q", got)
	}
	if got := jobLink("   ", 5); got != "" {
		t.Fatalf("whitespace controller URL must yield empty link, got %q", got)
	}
}

// jobLink trims a trailing slash so the join is always clean.
func TestJobLinkTrimsTrailingSlash(t *testing.T) {
	cases := []string{
		"http://host:8080",
		"http://host:8080/",
		"http://host:8080//",
	}
	for _, cu := range cases {
		got := jobLink(cu, 42)
		want := "http://host:8080/jobs?job=42"
		if got != want {
			t.Errorf("jobLink(%q) = %q, want %q", cu, got, want)
		}
	}
}

// A Discord notifier with a ControllerURL appends the job deep link to the
// posted message.
func TestJobFinishedDeepLinkPresent(t *testing.T) {
	cap := &captureWebhook{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	n := NewDiscordWithLink(srv.URL, "http://ctrl:8080", testLog())
	n.JobFinished(context.Background(), &model.Job{
		ID: 55, Series: "Linked", Episode: "01", FlowID: 2,
		Status: model.JobDone,
	}, "enc-01")

	body := cap.last()
	if body == nil {
		t.Fatal("no webhook call received")
	}
	content := body["content"]
	want := "http://ctrl:8080/jobs?job=55"
	if !strings.Contains(content, want) {
		t.Fatalf("alert missing deep link %q: %s", want, content)
	}
}

// Without a ControllerURL, the message has no deep link (back-compat).
func TestJobFinishedDeepLinkAbsentWhenControllerEmpty(t *testing.T) {
	cap := &captureWebhook{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	n := NewDiscord(srv.URL, testLog())
	n.JobFinished(context.Background(), &model.Job{
		ID: 56, Series: "Plain", Episode: "01", FlowID: 2,
		Status: model.JobDone,
	}, "enc-01")

	body := cap.last()
	if body == nil {
		t.Fatal("no webhook call received")
	}
	content := body["content"]
	if strings.Contains(content, "/jobs?job=") {
		t.Fatalf("deep link must be absent without a controller URL: %s", content)
	}
}

// The 2000-char Discord cap is honored even with a deep link appended and a
// very long error: the whole payload (link included) fits under the limit.
func TestJobFinishedCapHonoredWithDeepLink(t *testing.T) {
	cap := &captureWebhook{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	longErr := strings.Repeat("x", 5000)
	n := NewDiscordWithLink(srv.URL, "http://ctrl.example:8080", testLog())
	n.JobFinished(context.Background(), &model.Job{
		ID: 57, Series: "LongErr", Episode: "01", FlowID: 1,
		Status: model.JobFailed, Step: "encode", Error: longErr,
	}, "enc-09")

	body := cap.last()
	if body == nil {
		t.Fatal("no webhook call received")
	}
	content := body["content"]
	if len(content) > discordCap {
		t.Fatalf("payload exceeds Discord cap: %d chars (cap %d)", len(content), discordCap)
	}
	// The deep link must survive the cap (it is appended before truncation).
	if !strings.Contains(content, "/jobs?job=57") {
		t.Fatalf("deep link lost under cap: %s...[truncated]", content[:min(120, len(content))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
