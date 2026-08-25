package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// ---------- Create Series ----------

// TestCreateSeriesBuildsFolderTree covers the happy path: episode folders on
// the scripts share, the release folder on the release share, and the series
// row registered with tag + flow.
func TestCreateSeriesBuildsFolderTree(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "POST", ts.URL+"/api/series", adminTok,
		map[string]any{"name": "Ookami-san to Shichinin no Nakama-tachi", "episodes": 3, "tag": "1080p"})
	if resp.StatusCode != 201 {
		t.Fatalf("create series: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Series         *model.Series `json:"series"`
		ScriptsFolder  string        `json:"scripts_folder"`
		ReleaseFolder  string        `json:"release_folder"`
		EpisodeFolders []string      `json:"episode_folders"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if out.Series == nil || out.Series.Name != "Ookami-san to Shichinin no Nakama-tachi" || out.Series.Tag != "1080p" {
		t.Fatalf("series row wrong: %+v", out.Series)
	}
	if len(out.EpisodeFolders) != 3 || out.EpisodeFolders[0] != "Ep 01" || out.EpisodeFolders[2] != "Ep 03" {
		t.Fatalf("episode folders wrong: %+v", out.EpisodeFolders)
	}

	// Scripts share: series dir + empty episode folders.
	for _, ep := range []string{"Ep 01", "Ep 02", "Ep 03"} {
		dir := filepath.Join(out.ScriptsFolder, ep)
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Errorf("episode folder missing: %s (%v)", dir, err)
		}
	}
	// Release share: [Group] <Series> - Raws [Tag].
	wantRelease := "[OldFartsSubs] Ookami-san to Shichinin no Nakama-tachi - Raws [1080p]"
	if filepath.Base(out.ReleaseFolder) != wantRelease {
		t.Fatalf("release folder name wrong: %s", out.ReleaseFolder)
	}
	if info, err := os.Stat(out.ReleaseFolder); err != nil || !info.IsDir() {
		t.Fatalf("release folder not created: %v", err)
	}

	// The series shows up in GET /api/series.
	resp2, body2 := doJSON(t, "GET", ts.URL+"/api/series", adminTok, nil)
	if resp2.StatusCode != 200 {
		t.Fatalf("list series: %d", resp2.StatusCode)
	}
	if !strings.Contains(string(body2), "Ookami-san to Shichinin no Nakama-tachi") {
		t.Fatalf("created series not listed: %s", body2)
	}
}

// TestCreateSeriesTagDefaultsToGlobal: when no tag is given, the release
// folder uses the global settings tag and the series keeps the override
// empty (inheriting at render time).
func TestCreateSeriesTagDefaultsToGlobal(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "POST", ts.URL+"/api/series", adminTok,
		map[string]any{"name": "No Tag Show", "episodes": 1})
	if resp.StatusCode != 201 {
		t.Fatalf("create series: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Series        *model.Series `json:"series"`
		ReleaseFolder string        `json:"release_folder"`
	}
	json.Unmarshal(body, &out)
	if out.Series.Tag != "" {
		t.Fatalf("tag override must stay empty when not requested: %+v", out.Series)
	}
	// testEnv settings tag is "1080p" (see api_test.go).
	if !strings.HasSuffix(out.ReleaseFolder, "[1080p]") {
		t.Fatalf("release folder must use the global tag: %s", out.ReleaseFolder)
	}
}

// TestCreateSeriesIdempotent: re-running extends instead of failing and never
// duplicates episode folders.
func TestCreateSeriesIdempotent(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	for i := 0; i < 2; i++ {
		resp, body := doJSON(t, "POST", ts.URL+"/api/series", adminTok,
			map[string]any{"name": "Extend Me", "episodes": 2})
		if resp.StatusCode != 201 {
			t.Fatalf("create #%d: %d %s", i+1, resp.StatusCode, body)
		}
	}
	// Extend to 5 episodes.
	resp, body := doJSON(t, "POST", ts.URL+"/api/series", adminTok,
		map[string]any{"name": "Extend Me", "episodes": 5})
	if resp.StatusCode != 201 {
		t.Fatalf("extend: %d %s", resp.StatusCode, body)
	}
	var out struct {
		ScriptsFolder  string   `json:"scripts_folder"`
		EpisodeFolders []string `json:"episode_folders"`
	}
	json.Unmarshal(body, &out)
	if len(out.EpisodeFolders) != 5 {
		t.Fatalf("want 5 episode folders: %+v", out.EpisodeFolders)
	}
	entries, err := os.ReadDir(out.ScriptsFolder)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Fatalf("series dir must contain exactly 5 episode folders: %+v", entries)
	}
	// Still exactly one series row.
	srs, _ := e.server.Store.ListSeries(ctxBg())
	count := 0
	for _, s := range srs {
		if s.Name == "Extend Me" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("series row duplicated: %d", count)
	}
}

// TestCreateSeriesValidation: unsafe or nonsense names are refused before any
// filesystem touch.
func TestCreateSeriesValidation(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	cases := []map[string]any{
		{"name": "", "episodes": 3},                       // empty name
		{"name": "../escape", "episodes": 3},              // traversal
		{"name": "Bad/Slash", "episodes": 3},              // path separator
		{"name": "Colon:Bad", "episodes": 3},              // reserved char
		{"name": "  ", "episodes": 3},                     // whitespace only
		{"name": "Fine", "episodes": 0},                   // no episodes
		{"name": "Fine", "episodes": -2},                  // negative
		{"name": "Fine", "episodes": 1000},                // absurd count
		{"name": "Fine", "episodes": 2, "tag": "a/b"},     // tag traversal
		{"name": "Fine", "episodes": 2, "flow_id": 99999}, // unknown flow
	}
	for i, c := range cases {
		resp, body := doJSON(t, "POST", ts.URL+"/api/series", adminTok, c)
		if resp.StatusCode != 400 {
			t.Errorf("case %d (%+v): want 400, got %d %s", i, c, resp.StatusCode, body)
		}
	}
}

// TestCreateSeriesWithFlow assigns the flow at creation time.
func TestCreateSeriesWithFlow(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	flows, err := e.server.Store.ListFlows(ctx)
	if err != nil || len(flows) == 0 {
		t.Fatalf("no flows: %v", err)
	}
	resp, body := doJSON(t, "POST", ts.URL+"/api/series", adminTok,
		map[string]any{"name": "Flowed", "episodes": 1, "flow_id": flows[0].ID})
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Series *model.Series `json:"series"`
	}
	json.Unmarshal(body, &out)
	if out.Series.FlowID != flows[0].ID {
		t.Fatalf("flow not assigned: %+v", out.Series)
	}
}

// TestSeriesPatchTag covers the tag field on the PATCH endpoint, including
// rejection of unsafe tags.
func TestSeriesPatchTag(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "POST", ts.URL+"/api/series", adminTok,
		map[string]any{"name": "Tag Show", "episodes": 1})
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var created struct {
		Series *model.Series `json:"series"`
	}
	json.Unmarshal(body, &created)

	resp2, body2 := doJSON(t, "PATCH", ts.URL+"/api/series/"+itoa(created.Series.ID), adminTok,
		map[string]any{"tag": "2160p"})
	if resp2.StatusCode != 200 {
		t.Fatalf("patch tag: %d %s", resp2.StatusCode, body2)
	}
	var patched model.Series
	json.Unmarshal(body2, &patched)
	if patched.Tag != "2160p" {
		t.Fatalf("tag not set: %+v", patched)
	}

	resp3, _ := doJSON(t, "PATCH", ts.URL+"/api/series/"+itoa(created.Series.ID), adminTok,
		map[string]any{"tag": "a/b"})
	if resp3.StatusCode != 400 {
		t.Fatalf("want 400 for unsafe tag, got %d", resp3.StatusCode)
	}
}

// TestSeriesTagOverrideFlowsIntoRender: a series with its own tag renders job
// scripts with that tag in the output name and release folder, not the global
// settings tag.
func TestSeriesTagOverrideFlowsIntoRender(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Series with a 2160p tag override (created via the API).
	resp, body := doJSON(t, "POST", ts.URL+"/api/series", adminTok,
		map[string]any{"name": "FourK Show", "episodes": 1, "tag": "2160p"})
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}

	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.server.Store.CreateJob(ctx, &model.Job{
		Series: "FourK Show", Episode: "01", EpisodeDir: "FourK Show/Ep 01",
		ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp2, body2 := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 1, 0))
	if resp2.StatusCode != 200 {
		t.Fatalf("heartbeat: %d %s", resp2.StatusCode, body2)
	}
	var reply model.HeartbeatReply
	json.Unmarshal(body2, &reply)
	if reply.Job == nil {
		t.Fatalf("no job dispatched: %s", body2)
	}
	script := reply.Job.Script
	if !strings.Contains(script, "FourK Show - 01 [2160p].mkv") {
		t.Errorf("output name must carry the series tag:\n%s", script)
	}
	if !strings.Contains(script, "[OldFartsSubs] FourK Show - Raws [2160p]") {
		t.Errorf("release folder must carry the series tag:\n%s", script)
	}
	if strings.Contains(script, "[1080p]") {
		t.Errorf("global tag leaked into a tag-overridden render:\n%s", script)
	}
}
