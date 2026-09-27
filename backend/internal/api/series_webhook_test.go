package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestSeriesWebhookOverride verifies a series with webhook_url set routes
// its job-outcome alert to THAT webhook instead of the global one, and a
// series without one keeps using the global webhook.
func TestSeriesWebhookOverride(t *testing.T) {
	e := newTestEnv(t)
	ctx := ctxBg()

	var mu sync.Mutex
	var globalPosts, seriesPosts int
	global := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		globalPosts++
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer global.Close()
	perSeries := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seriesPosts++
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer perSeries.Close()

	st := &model.Settings{DiscordWebhook: global.URL}
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}

	routed, err := e.server.Store.UpsertSeriesByName(ctx, "Routed Show")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.server.Store.UpsertSeriesByName(ctx, "Global Show"); err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.SetSeriesWebhook(ctx, routed.ID, perSeries.URL); err != nil {
		t.Fatal(err)
	}

	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	mk := func(series string) int64 {
		j, err := e.server.Store.CreateJob(ctx, &model.Job{
			Series: series, Episode: "01", EpisodeDir: series + "/Ep 01",
			ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending})
		if err != nil {
			t.Fatal(err)
		}
		e.server.Store.AssignJob(ctx, j.ID, e.node.ID)
		e.server.Store.UpdateJobStatus(ctx, j.ID, model.JobRunning, "encode", 50, "")
		e.server.Store.FinishJobWithReport(ctx, j.ID, model.JobDone, 0, "", nil, "t", "l", nil, nil)
		return j.ID
	}

	e.server.notifyJobFinished(ctx, mk("Routed Show"), "enc-01")
	e.server.notifyJobFinished(ctx, mk("Global Show"), "enc-01")

	mu.Lock()
	defer mu.Unlock()
	if seriesPosts != 1 {
		t.Fatalf("series webhook posts = %d, want 1", seriesPosts)
	}
	if globalPosts != 1 {
		t.Fatalf("global webhook posts = %d, want 1", globalPosts)
	}
}

// TestPatchSeriesWebhook verifies the PATCH field round-trips through the
// series row and the API response.
func TestPatchSeriesWebhook(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	if _, err := e.server.Store.UpsertSeriesByName(ctx, "Hook Show"); err != nil {
		t.Fatal(err)
	}
	sr, _ := e.server.Store.SeriesByName(ctx, "Hook Show")

	resp, body := doJSON(t, "PATCH", ts.URL+"/api/series/"+i64s(sr.ID), adminTok,
		map[string]any{"webhook_url": "https://discord.com/api/webhooks/123/abc"})
	if resp.StatusCode != 200 {
		t.Fatalf("patch: %d %s", resp.StatusCode, body)
	}
	var got model.Series
	json.Unmarshal(body, &got)
	if got.WebhookURL != "https://discord.com/api/webhooks/123/abc" {
		t.Fatalf("webhook_url = %q, want the patched value", got.WebhookURL)
	}
	// Clearing it returns to the global webhook.
	resp, body = doJSON(t, "PATCH", ts.URL+"/api/series/"+i64s(sr.ID), adminTok,
		map[string]any{"webhook_url": ""})
	if resp.StatusCode != 200 {
		t.Fatalf("clear: %d %s", resp.StatusCode, body)
	}
	// Fresh var: webhook_url is omitempty, so a cleared value is ABSENT
	// from the JSON and a reused struct would keep its stale field.
	var cleared model.Series
	json.Unmarshal(body, &cleared)
	if cleared.WebhookURL != "" {
		t.Fatalf("cleared webhook_url = %q, want empty", cleared.WebhookURL)
	}
}
