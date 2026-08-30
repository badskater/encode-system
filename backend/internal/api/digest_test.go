package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/notify"
)

// ---------- Phase F2 Part 2: hourly digest mode ----------

// newDigestTestEnv builds a testEnv with NotifyDigest ON and a capture
// webhook in the live settings, plus a recording notifier so the per-job
// path's silence is observable. Returns the capture server (caller closes).
func newDigestTestEnv(t *testing.T) (*testEnv, *recordingNotifier, *captureWebhookServer) {
	t.Helper()
	e := newTestEnv(t)
	rec := &recordingNotifier{}
	e.server.Notifier = rec
	ctx := ctxBg()

	cap := newCaptureWebhookServer()
	st := e.server.defaultsSettings()
	st.NotifyDigest = true
	st.DiscordWebhook = cap.URL
	st.ControllerURL = "http://ctrl.digest:8080"
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	return e, rec, cap
}

// TestDigestOnBuffersAndDoesNotNotify: with NotifyDigest ON, completing a
// job does NOT fire the per-job notifier (recordingNotifier observes zero
// calls); instead the event lands in the in-memory digest buffer.
func TestDigestOnBuffersAndDoesNotNotify(t *testing.T) {
	e, rec, _ := newDigestTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "DigestShow", Episode: "01", EpisodeDir: "DigestShow/Ep 01",
		ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}

	completeJob(t, ts.URL, e, job.ID, "done")

	if rec.count() != 0 {
		t.Fatalf("digest ON must NOT fire per-job notifier, got %d calls", rec.count())
	}
	// The outcome must be buffered.
	if got := e.server.digest.buf.Len(); got != 1 {
		t.Fatalf("buffer Len = %d, want 1", got)
	}
}

// TestDigestFlushPostsOneSummary: after buffering two job outcomes, a flush
// posts exactly ONE summary containing both jobs + the digest header.
func TestDigestFlushPostsOneSummary(t *testing.T) {
	e, rec, cap := newDigestTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range []string{"01", "02"} {
		job, err := e.server.Store.CreateJob(ctx, &model.Job{
			Series: "DigestShow", Episode: ep, EpisodeDir: "DigestShow/Ep " + ep,
			ScriptType: "vpy", FlowID: fl.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
			t.Fatal(err)
		}
		status := "done"
		if ep == "02" {
			status = "failed"
		}
		completeJob(t, ts.URL, e, job.ID, status)
	}

	// Per-job notifier must be silent (digest ON buffers).
	if rec.count() != 0 {
		t.Fatalf("digest ON must not fire per-job notifier, got %d", rec.count())
	}
	if got := e.server.digest.buf.Len(); got != 2 {
		t.Fatalf("buffer Len = %d, want 2", got)
	}

	// Simulate a digest tick: flush via the live settings path the loop uses.
	st := e.server.currentSettings(ctx)
	notify.FlushDigest(e.server.Log, st.DiscordWebhook, st.ControllerURL, e.server.digest.buf, time.Now().UTC())

	// Exactly one webhook post (the summary), not two per-job alerts.
	if len(cap.bodies) != 1 {
		t.Fatalf("flush must post exactly ONE summary, got %d posts", len(cap.bodies))
	}
	content := cap.bodies[0]["content"]
	for _, want := range []string{
		"Encode digest (1h): ✅ 1 done · ❌ 1 failed",
		"✅ DigestShow Ep 01",
		"❌ DigestShow Ep 02",
		"http://ctrl.digest:8080/jobs",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("digest summary missing %q:\n%s", want, content)
		}
	}
	// Buffer drained after flush.
	if got := e.server.digest.buf.Len(); got != 0 {
		t.Fatalf("buffer must drain after flush, Len = %d", got)
	}
}

// TestDigestOffNotifiesImmediately: with NotifyDigest OFF, completing a job
// fires the per-job notifier immediately (zero behavior change vs pre-F2).
func TestDigestOffNotifiesImmediately(t *testing.T) {
	e := newTestEnv(t)
	rec := &recordingNotifier{}
	e.server.Notifier = rec
	ts := e.serve(t)
	ctx := ctxBg()

	// Digest OFF (default): leave settings as-is.
	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "LoudShow", Episode: "01", EpisodeDir: "LoudShow/Ep 01",
		ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}

	completeJob(t, ts.URL, e, job.ID, "failed")

	if rec.count() != 1 {
		t.Fatalf("digest OFF must fire per-job notifier immediately, got %d", rec.count())
	}
}

// TestDigestEmptyFlushPostsNothing: when digest is ON but no jobs buffered,
// a flush tick posts nothing (silent tick — no empty-message post).
func TestDigestEmptyFlushPostsNothing(t *testing.T) {
	e, _, cap := newDigestTestEnv(t)
	ctx := ctxBg()

	// Buffer is empty; flush must be a no-op.
	st := e.server.currentSettings(ctx)
	notify.FlushDigest(e.server.Log, st.DiscordWebhook, st.ControllerURL, e.server.digest.buf, time.Now().UTC())

	if len(cap.bodies) != 0 {
		t.Fatalf("empty buffer must not post, got %d posts", len(cap.bodies))
	}
}

// TestDigestMutedSeriesNotBuffered: a muted series is excluded from the
// digest buffer — the mute check runs BEFORE the digest branch, so a muted
// job produces neither a per-job alert nor a buffered digest entry.
func TestDigestMutedSeriesNotBuffered(t *testing.T) {
	e, rec, _ := newDigestTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	// Create + mute a series.
	sr, err := e.server.Store.UpsertSeriesByName(ctx, "MutedDigest")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.SetSeriesNotify(ctx, sr.ID, false); err != nil {
		t.Fatal(err)
	}
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "MutedDigest", Episode: "01", EpisodeDir: "MutedDigest/Ep 01",
		ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}

	completeJob(t, ts.URL, e, job.ID, "done")

	// No per-job alert (muted).
	if rec.count() != 0 {
		t.Fatalf("muted series must not notify, got %d", rec.count())
	}
	// And NOT buffered (mute-before-buffer ordering).
	if got := e.server.digest.buf.Len(); got != 0 {
		t.Fatalf("muted series must not be buffered, got %d", got)
	}
}

// TestDigestOffTickIsSilent: flushDigestTick with digest OFF does nothing
// (no post, buffer untouched). Proves the live-settings gate in the tick
// path: a previous-ON buffer is retained until the next ON tick.
func TestDigestOffTickIsSilent(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	cap := newCaptureWebhookServer()
	defer cap.Close()

	st := e.server.defaultsSettings()
	st.NotifyDigest = false
	st.DiscordWebhook = cap.URL
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	// Seed the buffer so we can prove the OFF tick does NOT drain it.
	e.server.digest.buf.Push(notify.DigestEntry{JobID: 1, Series: "X", Episode: "01", Status: model.JobDone, NodeName: "n"})

	e.server.flushDigestTick(ctx)

	if len(cap.bodies) != 0 {
		t.Fatalf("digest OFF tick must not post, got %d", len(cap.bodies))
	}
	// Buffer must be retained (the OFF tick does not drain).
	if got := e.server.digest.buf.Len(); got != 1 {
		t.Fatalf("digest OFF tick must retain buffer, got Len = %d", got)
	}
}

// TestDigestOnTickFlushesBuffer: flushDigestTick with digest ON drains the
// buffer and posts the summary through the live webhook.
func TestDigestOnTickFlushesBuffer(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	cap := newCaptureWebhookServer()
	defer cap.Close()

	st := e.server.defaultsSettings()
	st.NotifyDigest = true
	st.DiscordWebhook = cap.URL
	st.ControllerURL = "http://ctrl.tick:8080"
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	e.server.digest.buf.Push(notify.DigestEntry{JobID: 1, Series: "TickShow", Episode: "01", Status: model.JobDone, NodeName: "enc-01"})

	e.server.flushDigestTick(ctx)

	if len(cap.bodies) != 1 {
		t.Fatalf("digest ON tick must post one summary, got %d", len(cap.bodies))
	}
	content := cap.bodies[0]["content"]
	if !strings.Contains(content, "Encode digest (1h)") || !strings.Contains(content, "TickShow") {
		t.Fatalf("tick summary missing expected content:\n%s", content)
	}
	if got := e.server.digest.buf.Len(); got != 0 {
		t.Fatalf("tick must drain the buffer, got Len = %d", got)
	}
}
