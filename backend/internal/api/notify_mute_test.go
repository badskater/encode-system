package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// ---------- Phase F2: per-series notify mute + deep links ----------

// TestMutedSeriesSkipsNotify: a job whose series has Notify=false does NOT
// fire the notifier (the recordingNotifier observes zero calls). The series
// is muted via the store's field-scoped SetSeriesNotify before the job
// completes.
func TestMutedSeriesSkipsNotify(t *testing.T) {
	e := newTestEnv(t)
	rec := &recordingNotifier{}
	e.server.Notifier = rec
	ts := e.serve(t)
	ctx := ctxBg()

	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	// Create + mute a series, then create a job for it.
	sr, err := e.server.Store.UpsertSeriesByName(ctx, "MutedShow")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.SetSeriesNotify(ctx, sr.ID, false); err != nil {
		t.Fatal(err)
	}
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "MutedShow", Episode: "01", EpisodeDir: "MutedShow/Ep 01",
		ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}

	completeJob(t, ts.URL, e, job.ID, "failed")

	if rec.count() != 0 {
		t.Fatalf("muted series must NOT notify, got %d calls", rec.count())
	}
}

// TestUnmutedSeriesNotifies: the same setup but with Notify=true (default)
// fires the notifier. Proves the mute is what gated it, not a broken path.
func TestUnmutedSeriesNotifies(t *testing.T) {
	e := newTestEnv(t)
	rec := &recordingNotifier{}
	e.server.Notifier = rec
	ts := e.serve(t)
	ctx := ctxBg()

	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	sr, err := e.server.Store.UpsertSeriesByName(ctx, "LoudShow")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.SetSeriesNotify(ctx, sr.ID, true); err != nil {
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
		t.Fatalf("unmuted series must notify, got %d calls", rec.count())
	}
}

// TestUnknownSeriesNotifies: a job whose series row does not exist (scanner
// created the job before the series row existed, or it was deleted) still
// notifies — muting is opt-in, never a silent default.
func TestUnknownSeriesNotifies(t *testing.T) {
	e := newTestEnv(t)
	rec := &recordingNotifier{}
	e.server.Notifier = rec
	ts := e.serve(t)
	ctx := ctxBg()

	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	// Create a job for a series that has NO series row.
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "GhostShow", Episode: "01", EpisodeDir: "GhostShow/Ep 01",
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
		t.Fatalf("unknown series must notify (mute is opt-in), got %d calls", rec.count())
	}
}

// TestNotifyDeepLinkPresent: when settings.ControllerURL is set, the Discord
// alert carries a job deep link (…/jobs?job=<id>). Uses a capture webhook so
// the message body is observable. Mute does not apply (series default notify).
func TestNotifyDeepLinkPresent(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Set a controller URL + a loopback webhook via the live settings row.
	st := e.server.defaultsSettings()
	st.ControllerURL = "http://ctrl.deep:8080"
	st.DiscordWebhook = "http://127.0.0.1:9999/hook" // loopback allowed
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}

	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "DeepShow", Episode: "01", EpisodeDir: "DeepShow/Ep 01",
		ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}

	// Swap the webhook to a capture server AFTER settings are saved, so the
	// live resolution picks up the capture URL on the notify call.
	cap := newCaptureWebhookServer()
	defer cap.Close()
	st.DiscordWebhook = cap.URL
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}

	completeJob(t, ts.URL, e, job.ID, "done")

	body := cap.LastBody()
	if body == nil {
		t.Fatal("no webhook call received")
	}
	content := body["content"]
	want := "http://ctrl.deep:8080/jobs?job=" + itoa(job.ID)
	if !strings.Contains(content, want) {
		t.Fatalf("alert missing deep link %q: %s", want, content)
	}
}

// TestNotifyDeepLinkAbsentWhenControllerEmpty: without a ControllerURL, the
// alert has no deep link (back-compat with pre-F2 behavior).
func TestNotifyDeepLinkAbsentWhenControllerEmpty(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	st := e.server.defaultsSettings()
	st.ControllerURL = "" // no deep links
	cap := newCaptureWebhookServer()
	defer cap.Close()
	st.DiscordWebhook = cap.URL
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}

	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "PlainShow", Episode: "01", EpisodeDir: "PlainShow/Ep 01",
		ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}

	completeJob(t, ts.URL, e, job.ID, "done")

	body := cap.LastBody()
	if body == nil {
		t.Fatal("no webhook call received")
	}
	content := body["content"]
	if strings.Contains(content, "/jobs?job=") {
		t.Fatalf("deep link must be absent without controller URL: %s", content)
	}
}

// TestPatchSeriesNotify: PATCH /api/series/{id} with {notify:false} mutes the
// series, and {notify:true} unmutes it. Round-trips the notify flag through
// field-scoped SQL.
func TestPatchSeriesNotify(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	sr, err := e.server.Store.UpsertSeriesByName(ctx, "PatchShow")
	if err != nil {
		t.Fatal(err)
	}
	if !sr.Notify {
		t.Fatal("series must default to notify=true")
	}

	// Mute via PATCH.
	resp, body := doJSON(t, "PATCH", ts.URL+"/api/series/"+itoa(sr.ID), adminTok,
		map[string]any{"notify": false})
	if resp.StatusCode != 200 {
		t.Fatalf("patch notify: %d %s", resp.StatusCode, body)
	}
	var patched model.Series
	if err := json.Unmarshal(body, &patched); err != nil {
		t.Fatal(err)
	}
	if patched.Notify {
		t.Fatal("PATCH did not mute the series")
	}
	got, _ := e.server.Store.GetSeries(ctx, sr.ID)
	if got.Notify {
		t.Fatal("notify=false not persisted")
	}

	// Unmute via PATCH.
	resp2, body2 := doJSON(t, "PATCH", ts.URL+"/api/series/"+itoa(sr.ID), adminTok,
		map[string]any{"notify": true})
	if resp2.StatusCode != 200 {
		t.Fatalf("patch notify back on: %d %s", resp2.StatusCode, body2)
	}
	got2, _ := e.server.Store.GetSeries(ctx, sr.ID)
	if !got2.Notify {
		t.Fatal("notify=true not persisted after unmute")
	}
}
