package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// Series script assignment API (phase 1): create/patch accept
// script_type + script_template, validated against the _templates dir;
// GET /api/series/{id}/templates lists the available templates.

// createSeriesForScriptTest registers a series and returns its row + the
// scripts root (parent of the series folder).
func scriptTestSetup(t *testing.T) (url string, sr *model.Series, root string) {
	t.Helper()
	e := newTestEnv(t)
	ts := e.serve(t)
	resp, body := doJSON(t, "POST", ts.URL+"/api/series", adminTok,
		map[string]any{"name": "Script Show", "episodes": 2})
	if resp.StatusCode != 201 {
		t.Fatalf("create series: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Series        *model.Series `json:"series"`
		ScriptsFolder string        `json:"scripts_folder"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return ts.URL, out.Series, filepath.Dir(out.ScriptsFolder)
}

func writeSeriesTemplate(t *testing.T, root, series, name, content string) {
	t.Helper()
	dir := filepath.Join(root, "_templates", series)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPatchSeriesScriptAssignment(t *testing.T) {
	url, sr, root := scriptTestSetup(t)
	writeSeriesTemplate(t, root, "Script Show", "1080.vpy", "# {{episode}}\n")

	resp, body := doJSON(t, "PATCH", url+"/api/series/"+itoa(sr.ID), adminTok,
		map[string]any{"script_type": "vpy", "script_template": "1080.vpy"})
	if resp.StatusCode != 200 {
		t.Fatalf("assign script: %d %s", resp.StatusCode, body)
	}
	var got model.Series
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.ScriptType != "vpy" || got.ScriptTemplate != "1080.vpy" {
		t.Fatalf("assignment not persisted: %+v", got)
	}

	// Clearing restores the unassigned state.
	resp, body = doJSON(t, "PATCH", url+"/api/series/"+itoa(sr.ID), adminTok,
		map[string]any{"script_type": "", "script_template": ""})
	if resp.StatusCode != 200 {
		t.Fatalf("clear script: %d %s", resp.StatusCode, body)
	}
	got = model.Series{}
	json.Unmarshal(body, &got)
	if got.ScriptType != "" || got.ScriptTemplate != "" {
		t.Fatalf("clear failed: %+v", got)
	}
}

func TestPatchSeriesScriptValidation(t *testing.T) {
	url, sr, root := scriptTestSetup(t)
	writeSeriesTemplate(t, root, "Script Show", "1080.vpy", "# template\n")
	cases := []map[string]any{
		{"script_type": "exe", "script_template": "1080.vpy"},
		{"script_type": "vpy", "script_template": "../escape.vpy"},
		{"script_type": "vpy", "script_template": "sub/dir.vpy"},
		{"script_type": "avs", "script_template": "1080.vpy"}, // ext mismatch
		{"script_type": "vpy", "script_template": "missing.vpy"},
		{"script_type": "vpy", "script_template": ""}, // half assignment
		{"script_type": "", "script_template": "1080.vpy"},
	}
	for i, c := range cases {
		resp, body := doJSON(t, "PATCH", url+"/api/series/"+itoa(sr.ID), adminTok, c)
		if resp.StatusCode != 400 {
			t.Errorf("case %d: want 400, got %d %s", i, resp.StatusCode, body)
		}
	}
}

func TestPatchSeriesScriptKeepsOtherFields(t *testing.T) {
	url, sr, root := scriptTestSetup(t)
	writeSeriesTemplate(t, root, "Script Show", "1080.vpy", "# template\n")
	resp, body := doJSON(t, "PATCH", url+"/api/series/"+itoa(sr.ID), adminTok,
		map[string]any{"tag": "1080p", "node_group": "gpu"})
	if resp.StatusCode != 200 {
		t.Fatalf("pre-patch: %d %s", resp.StatusCode, body)
	}
	resp, body = doJSON(t, "PATCH", url+"/api/series/"+itoa(sr.ID), adminTok,
		map[string]any{"script_type": "vpy", "script_template": "1080.vpy"})
	if resp.StatusCode != 200 {
		t.Fatalf("script patch: %d %s", resp.StatusCode, body)
	}
	var got model.Series
	json.Unmarshal(body, &got)
	if got.Tag != "1080p" || got.NodeGroup != "gpu" {
		t.Fatalf("script patch clobbered other fields: %+v", got)
	}
}

func TestCreateSeriesWithScriptAssignment(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	// Pre-create the template dir under the configured scripts root.
	st := e.server.currentSettings(t.Context())
	writeSeriesTemplate(t, st.ScriptsRoot, "Preassigned", "2160.avs", "# template\n")
	resp, body := doJSON(t, "POST", ts.URL+"/api/series", adminTok,
		map[string]any{"name": "Preassigned", "episodes": 1, "script_type": "avs", "script_template": "2160.avs"})
	if resp.StatusCode != 201 {
		t.Fatalf("create with assignment: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Series *model.Series `json:"series"`
	}
	json.Unmarshal(body, &out)
	if out.Series.ScriptType != "avs" || out.Series.ScriptTemplate != "2160.avs" {
		t.Fatalf("assignment not applied at create: %+v", out.Series)
	}
	// Missing template at create must fail closed.
	resp, body = doJSON(t, "POST", ts.URL+"/api/series", adminTok,
		map[string]any{"name": "NoTemplate", "episodes": 1, "script_type": "vpy", "script_template": "gone.vpy"})
	if resp.StatusCode != 400 {
		t.Fatalf("missing template must 400, got %d %s", resp.StatusCode, body)
	}
}

func TestCreateSeriesRejectsReservedTemplatesName(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	resp, body := doJSON(t, "POST", ts.URL+"/api/series", adminTok,
		map[string]any{"name": "_templates", "episodes": 1})
	if resp.StatusCode != 400 {
		t.Fatalf("_templates must be refused as a series name, got %d %s", resp.StatusCode, body)
	}
}

func TestSeriesTemplatesListing(t *testing.T) {
	url, sr, root := scriptTestSetup(t)
	writeSeriesTemplate(t, root, "Script Show", "1080.vpy", "# a\n")
	writeSeriesTemplate(t, root, "Script Show", "2160.avs", "# b\n")
	resp, body := doJSON(t, "GET", url+"/api/series/"+itoa(sr.ID)+"/templates", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("list templates: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Templates []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"templates"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Templates) != 2 {
		t.Fatalf("want 2 templates, got %+v", out.Templates)
	}
	names := map[string]bool{}
	for _, x := range out.Templates {
		names[x.Name] = true
		if x.Size <= 0 {
			t.Errorf("template size missing: %+v", x)
		}
	}
	if !names["1080.vpy"] || !names["2160.avs"] {
		t.Fatalf("template names wrong: %+v", out.Templates)
	}
	// Unknown series -> 404.
	resp, _ = doJSON(t, "GET", url+"/api/series/999999/templates", adminTok, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown series want 404, got %d", resp.StatusCode)
	}
}
