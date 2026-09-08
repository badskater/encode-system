package notify

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestJobFinishedDeepLinkSurvivesLongError asserts the deep link is the LAST
// thing in the payload (always survives truncation) when a very long error
// would otherwise push it past the cut. The old code appended the link then
// truncated from the head, so the link (appended last) was the first thing
// cut. The fix truncates the body first (reserving link space), then appends
// the link.
func TestJobFinishedDeepLinkSurvivesLongError(t *testing.T) {
	cap := &captureWebhook{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	longErr := strings.Repeat("x", 5000)
	n := NewDiscordWithLink(srv.URL, "http://ctrl.example:8080", testLog())
	n.JobFinished(context.Background(), &model.Job{
		ID: 99, Series: "LinkSurvive", Episode: "01", FlowID: 1,
		Status: model.JobFailed, Step: "encode", Error: longErr,
	}, "enc-09")

	body := cap.last()
	if body == nil {
		t.Fatal("no webhook call received")
	}
	content := body["content"]

	// Must fit under the Discord cap.
	if len(content) > discordCap {
		t.Fatalf("payload exceeds Discord cap: %d chars (cap %d)", len(content), discordCap)
	}

	// The deep link must survive — it must be present AND be at the end
	// of the content (the last line).
	wantLink := "http://ctrl.example:8080/jobs?job=99"
	if !strings.Contains(content, wantLink) {
		t.Fatalf("deep link lost under cap: %s...[truncated]", content[:min(len(content), 120)])
	}
	if !strings.HasSuffix(content, wantLink) {
		// The link should be the very last thing in the payload.
		tail := content[len(content)-len(wantLink):]
		if tail != wantLink {
			t.Fatalf("deep link not at end of payload. tail=%q, want %q", tail, wantLink)
		}
	}
}

// TestFleetLinkBuildsJobsBaseURL asserts fleetLink produces the jobs base URL
// (no query string) and trims trailing slashes.
func TestFleetLinkBuildsJobsBaseURL(t *testing.T) {
	cases := []struct {
		controllerURL string
		want          string
	}{
		{"http://ctrl:8080", "http://ctrl:8080/jobs"},
		{"http://ctrl:8080/", "http://ctrl:8080/jobs"},
		{"http://ctrl:8080//", "http://ctrl:8080/jobs"},
		{"  http://ctrl:8080  ", "http://ctrl:8080/jobs"},
	}
	for _, c := range cases {
		got := fleetLink(c.controllerURL)
		if got != c.want {
			t.Errorf("fleetLink(%q) = %q, want %q", c.controllerURL, got, c.want)
		}
	}
	// Empty controller URL → empty link (opt-in).
	if got := fleetLink(""); got != "" {
		t.Fatalf("fleetLink(\"\") = %q, want empty", got)
	}
}

// TestFleetLinkAbsentWhenControllerEmpty asserts the digest carries no fleet
// link when the controller URL is unset (back-compat).
func TestFleetLinkAbsentWhenControllerEmpty(t *testing.T) {
	if fleetLink("") != "" {
		t.Fatal("empty controller URL must yield empty fleet link")
	}
	if fleetLink("   ") != "" {
		t.Fatal("whitespace controller URL must yield empty fleet link")
	}
}

// TestDigestFleetLinkNoMagicJobZero asserts the digest summary contains the
// controller jobs URL and does NOT contain the "?job=0" magic-string artifact
// from the old jobLink(0)+TrimSuffix approach.
func TestDigestFleetLinkNoMagicJobZero(t *testing.T) {
	cap := &captureWebhook{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	b := NewDigestBuffer()
	b.Push(DigestEntry{JobID: 1, Series: "Fleet", Episode: "01", Status: model.JobDone, NodeName: "enc-01"})
	FlushDigest(testLog(), srv.URL, "http://ctrl:8080", b, time.Now())

	body := cap.last()
	if body == nil {
		t.Fatal("no webhook call received")
	}
	content := body["content"]

	// Must contain the clean fleet link.
	if !strings.Contains(content, "http://ctrl:8080/jobs") {
		t.Fatalf("digest missing fleet link: %s", content)
	}
	// Must NOT contain the magic "?job=0" artifact.
	if strings.Contains(content, "?job=0") {
		t.Fatalf("digest contains ?job=0 artifact (should use fleetLink): %s", content)
	}
}
