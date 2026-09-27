package api

import (
	"encoding/json"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestSeriesPauseHoldsDispatch is the end-to-end pause contract: PATCH
// paused=true makes the series' queued job invisible to node heartbeats;
// paused=false restores dispatch. The paused job keeps its queue position
// (still pending, unchanged created order).
func TestSeriesPauseHoldsDispatch(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	sr, err := e.server.Store.UpsertSeriesByName(ctx, "Pause Show")
	if err != nil {
		t.Fatal(err)
	}
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "Pause Show", Episode: "01", EpisodeDir: "Pause Show/Ep 01",
		ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Pause via the API.
	resp, body := doJSON(t, "PATCH", ts.URL+"/api/series/"+i64s(sr.ID), adminTok, map[string]any{"paused": true})
	if resp.StatusCode != 200 {
		t.Fatalf("pause: %d %s", resp.StatusCode, body)
	}
	var patched model.Series
	json.Unmarshal(body, &patched)
	if !patched.Paused {
		t.Fatalf("series not paused in response: %+v", patched)
	}

	// Idle heartbeat must NOT dispatch the paused job.
	var reply model.HeartbeatReply
	resp, body = doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 2, 0))
	json.Unmarshal(body, &reply)
	if reply.Instruction != "none" {
		t.Fatalf("paused job dispatched anyway: %+v", reply)
	}

	// Unpause: dispatch resumes.
	if resp, body = doJSON(t, "PATCH", ts.URL+"/api/series/"+i64s(sr.ID), adminTok, map[string]any{"paused": false}); resp.StatusCode != 200 {
		t.Fatalf("unpause: %d %s", resp.StatusCode, body)
	}
	resp, body = doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 3, 0))
	json.Unmarshal(body, &reply)
	if reply.Instruction != "job" || reply.Job.ID != job.ID {
		t.Fatalf("post-unpause dispatch = %+v, want job %d", reply, job.ID)
	}
}

// TestSeriesPauseIndependentOfEnabled verifies pause and enabled are
// orthogonal flags: enabled=false alone still dispatches queued jobs (only
// the scanner stops), paused=true holds them.
func TestSeriesPauseIndependentOfEnabled(t *testing.T) {
	e := newTestEnv(t)
	ctx := ctxBg()
	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	sr, err := e.server.Store.UpsertSeriesByName(ctx, "Disabled Show")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.SetSeriesEnabled(ctx, sr.ID, false); err != nil {
		t.Fatal(err)
	}
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "Disabled Show", Episode: "01", EpisodeDir: "Disabled Show/Ep 01",
		ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	// enabled=false does NOT hold dispatch (documented pre-pause behavior).
	got, err := e.server.Store.NextAssignableJobForNode(ctx, e.node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != job.ID {
		t.Fatalf("disabled-but-unpaused job not assignable: %v", got)
	}
	// paused=true DOES.
	if err := e.server.Store.SetSeriesPaused(ctx, sr.ID, true); err != nil {
		t.Fatal(err)
	}
	got, err = e.server.Store.NextAssignableJobForNode(ctx, e.node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("paused job assignable: %d", got.ID)
	}
}

func i64s(i int64) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
