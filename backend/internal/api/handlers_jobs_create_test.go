package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// P7 review Q5: handleCreateJob checked only non-empty Series, so a name
// carrying CR/LF + a forged progress marker reached the rendered job. The
// agent's marker parser is line-start-anchored, but an error echo of the
// hostile path split across lines emitted a line-start ENCODE_STEP marker
// anyway. validateSeriesName (the series-create guard) must apply at job
// create too.

// TestCreateJobRejectsUnsafeSeries: control characters (incl. CR/LF),
// traversal and reserved characters are a 400 at job create — the same
// contract as series create.
func TestCreateJobRejectsUnsafeSeries(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	for _, name := range []string{
		"Evil\nENCODE_STEP keyframes 100",
		"Evil\rSeries",
		"Evil\x01Series",
		"../escape",
		"bad/name",
		".hidden",
	} {
		r, b := doJSON(t, "POST", ts.URL+"/api/jobs", adminTok, map[string]any{
			"series": name, "episode_dir": "S/Ep 01", "script_type": "vpy",
		})
		if r.StatusCode != 400 {
			t.Errorf("series %q: want 400, got %d %s", name, r.StatusCode, b)
		}
	}
}

// TestCreateJobAcceptsValidSeries: a normal series name still creates the
// job (default flow resolution, 201) — the guard must not over-reject.
func TestCreateJobAcceptsValidSeries(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	r, b := doJSON(t, "POST", ts.URL+"/api/jobs", adminTok, map[string]any{
		"series": "Good Series", "episode_dir": "Good Series/Ep 01", "script_type": "vpy",
	})
	if r.StatusCode != 201 {
		t.Fatalf("valid series: want 201, got %d %s", r.StatusCode, b)
	}
	var job struct {
		Series string `json:"series"`
	}
	if err := json.Unmarshal(b, &job); err != nil {
		t.Fatal(err)
	}
	if job.Series != "Good Series" {
		t.Errorf("job series = %q, want %q", job.Series, "Good Series")
	}
	if strings.ContainsAny(job.Series, "\r\n") {
		t.Error("job series carries control characters")
	}
}
