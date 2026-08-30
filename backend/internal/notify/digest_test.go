package notify

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestDigestBufferPushDrainRoundTrip: Push appends, Drain returns in order
// and clears the buffer so a second Drain is empty.
func TestDigestBufferPushDrainRoundTrip(t *testing.T) {
	b := NewDigestBuffer()
	if b.Len() != 0 {
		t.Fatalf("fresh buffer Len = %d, want 0", b.Len())
	}
	b.Push(DigestEntry{JobID: 1, Series: "A", Episode: "01", Status: model.JobDone, NodeName: "n1"})
	b.Push(DigestEntry{JobID: 2, Series: "B", Episode: "02", Status: model.JobFailed, NodeName: "n2"})
	if b.Len() != 2 {
		t.Fatalf("Len = %d, want 2", b.Len())
	}
	out := b.Drain()
	if len(out) != 2 {
		t.Fatalf("Drain returned %d entries, want 2", len(out))
	}
	if out[0].JobID != 1 || out[1].JobID != 2 {
		t.Fatalf("order not preserved: %+v", out)
	}
	// Second Drain must be empty (buffer cleared).
	if got := b.Drain(); len(got) != 0 {
		t.Fatalf("Drain after clear returned %d entries, want 0", len(got))
	}
	if b.Len() != 0 {
		t.Fatalf("Len after Drain = %d, want 0", b.Len())
	}
}

// TestDigestBufferConcurrentPush: concurrent Push calls are safe and all land.
func TestDigestBufferConcurrentPush(t *testing.T) {
	b := NewDigestBuffer()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b.Push(DigestEntry{JobID: int64(i)})
		}(i)
	}
	wg.Wait()
	if b.Len() != 50 {
		t.Fatalf("Len = %d, want 50", b.Len())
	}
	out := b.Drain()
	if len(out) != 50 {
		t.Fatalf("Drain returned %d, want 50", len(out))
	}
}

// TestFlushDigestPostsSummary: a buffered done+failed pair flushes ONE
// message with the header, one line per job, and a controller fleet link.
func TestFlushDigestPostsSummary(t *testing.T) {
	cap := &captureWebhook{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	b := NewDigestBuffer()
	b.Push(DigestEntry{JobID: 1, Series: "Alpha", Episode: "01", Status: model.JobDone, NodeName: "enc-01"})
	b.Push(DigestEntry{JobID: 2, Series: "Beta", Episode: "02", Status: model.JobFailed, NodeName: "enc-02"})

	FlushDigest(testLog(), srv.URL, "http://ctrl:8080", b, time.Now())

	body := cap.last()
	if body == nil {
		t.Fatal("no webhook call received")
	}
	content := body["content"]
	for _, want := range []string{
		"Encode digest (1h): ✅ 1 done · ❌ 1 failed",
		"✅ Alpha Ep 01 · enc-01",
		"❌ Beta Ep 02 · enc-02",
		"http://ctrl:8080/jobs",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("digest missing %q:\n%s", want, content)
		}
	}
	// Buffer must be drained after a flush.
	if b.Len() != 0 {
		t.Fatalf("buffer not drained after flush, Len = %d", b.Len())
	}
}

// TestFlushDigestEmptyIsSilent: an empty buffer flushes nothing.
func TestFlushDigestEmptyIsSilent(t *testing.T) {
	cap := &captureWebhook{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	b := NewDigestBuffer()
	FlushDigest(testLog(), srv.URL, "http://ctrl:8080", b, time.Now())

	if cap.last() != nil {
		t.Fatal("empty buffer must not post a digest")
	}
}

// TestFlushDigestNoWebhookDrainsSilently: when the webhook is empty
// (notifications off), the flush still drains the buffer and posts nothing.
func TestFlushDigestNoWebhookDrainsSilently(t *testing.T) {
	cap := &captureWebhook{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	b := NewDigestBuffer()
	b.Push(DigestEntry{JobID: 1, Series: "X", Episode: "01", Status: model.JobDone, NodeName: "n"})
	FlushDigest(testLog(), "", "http://ctrl:8080", b, time.Now())

	if cap.last() != nil {
		t.Fatal("empty webhook must not post")
	}
	if b.Len() != 0 {
		t.Fatalf("buffer must be drained even with no webhook, Len = %d", b.Len())
	}
}

// TestFlushDigestCapsAtMaxLines: >digestMaxLines jobs produce digestMaxLines
// lines plus an "…and K more" trailer, never one line per job.
func TestFlushDigestCapsAtMaxLines(t *testing.T) {
	cap := &captureWebhook{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	b := NewDigestBuffer()
	for i := 0; i < digestMaxLines+5; i++ {
		b.Push(DigestEntry{JobID: int64(i), Series: "S", Episode: "01", Status: model.JobDone, NodeName: "n"})
	}
	FlushDigest(testLog(), srv.URL, "", b, time.Now())

	content := cap.last()["content"]
	if !strings.Contains(content, "…and 5 more") {
		t.Fatalf("missing trailer for 5 overflow jobs:\n%s", content)
	}
	// Count the per-job lines (each starts with ✅). Must be exactly
	// digestMaxLines, not digestMaxLines+5.
	lines := strings.Count(content, "✅ S Ep")
	if lines != digestMaxLines {
		t.Fatalf("per-job lines = %d, want %d", lines, digestMaxLines)
	}
}

// TestFlushDigestTruncatesAtDiscordCap: a pathological number of jobs with
// long series names must not exceed Discord's 2000-char content limit.
func TestFlushDigestTruncatesAtDiscordCap(t *testing.T) {
	cap := &captureWebhook{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	b := NewDigestBuffer()
	longSeries := strings.Repeat("S", 200)
	for i := 0; i < 500; i++ {
		b.Push(DigestEntry{JobID: int64(i), Series: longSeries, Episode: "01", Status: model.JobDone, NodeName: "node-name"})
	}
	FlushDigest(testLog(), srv.URL, "", b, time.Now())

	content := cap.last()["content"]
	// truncate caps at discordCap then appends a 3-byte ellipsis, so the
	// post-truncation max is discordCap + 3 bytes.
	if len(content) > discordCap+3 {
		t.Fatalf("digest content %d chars exceeds cap+ellipsis %d", len(content), discordCap+3)
	}
}

// TestFlushDigestContextDoesNotBlock: a slow/dead webhook must not block the
// flush (the post path uses a bounded HTTP client + context). This is a
// smoke test — the real timeout is the sharedHTTP client's 10s.
func TestFlushDigestContextDoesNotBlock(t *testing.T) {
	b := NewDigestBuffer()
	b.Push(DigestEntry{JobID: 1, Series: "X", Episode: "01", Status: model.JobDone, NodeName: "n"})
	done := make(chan struct{})
	go func() {
		FlushDigest(testLog(), "http://127.0.0.1:0/unreachable", "", b, time.Now())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("flush blocked on an unreachable webhook")
	}
}

// TestFlushDigestControllerLinkAbsentWhenEmpty: no controller URL → no fleet
// link in the digest (back-compat with pre-F2 behavior).
func TestFlushDigestControllerLinkAbsentWhenEmpty(t *testing.T) {
	cap := &captureWebhook{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	b := NewDigestBuffer()
	b.Push(DigestEntry{JobID: 1, Series: "X", Episode: "01", Status: model.JobDone, NodeName: "n"})
	FlushDigest(testLog(), srv.URL, "", b, time.Now())

	content := cap.last()["content"]
	if strings.Contains(content, "/jobs") {
		t.Fatalf("digest must not carry a fleet link without controller URL:\n%s", content)
	}
}
