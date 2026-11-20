package scanner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// Phase 1 of series-scripts-scene-zones: the scanner materializes a series'
// script template into detected episodes that lack their own script.

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func ageSource(t *testing.T, p string) {
	t.Helper()
	past := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(p, past, past); err != nil {
		t.Fatal(err)
	}
}

func writeTemplate(t *testing.T, root, series, name, content string) {
	t.Helper()
	dir := filepath.Join(root, "_templates", series)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// _templates is a reserved top-level folder, never a series.
func TestScanSkipsTemplatesDir(t *testing.T) {
	root := t.TempDir()
	writeTemplate(t, root, "S", "1080.vpy", "# template")
	mkEp(t, root, "S", "Ep 01", "src.m2ts", "1080.vpy")
	ageSource(t, filepath.Join(root, "S", "Ep 01", "src.m2ts"))
	cands, _, err := Scan(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		if c.Series == "_templates" {
			t.Fatalf("_templates must never scan as a series: %+v", c)
		}
	}
	if len(cands) != 1 {
		t.Fatalf("want only the real episode, got %+v", cands)
	}
}

func TestScanSourceOnlyFindsScriptlessEpisodes(t *testing.T) {
	root := t.TempDir()
	mkEp(t, root, "S", "Ep 01", "src.m2ts")             // scriptless -> wanted
	mkEp(t, root, "S", "Ep 02", "src.m2ts", "1080.vpy") // has script -> not wanted
	mkEp(t, root, "S", "Ep 03", "src.m2ts")             // fresh source -> deferred
	ageSource(t, filepath.Join(root, "S", "Ep 01", "src.m2ts"))
	ageSource(t, filepath.Join(root, "S", "Ep 02", "src.m2ts"))
	cands, _, err := ScanSourceOnly(root, SourceStableFor)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].EpisodeDir != "S/Ep 01" {
		t.Fatalf("want only the stable scriptless episode, got %+v", cands)
	}
	c := cands[0]
	if c.ScriptFile != "" || c.ScriptType != "" || c.SourceFile != "src.m2ts" {
		t.Fatalf("source-only candidate must carry no script: %+v", c)
	}
}

func TestScanSourceOnlyIncludesBdmvLayout(t *testing.T) {
	root := t.TempDir()
	ep := filepath.Join(root, "S", "Ep 01")
	for _, sub := range []string{"BDMV/PLAYLIST", "BDMV/STREAM"} {
		if err := os.MkdirAll(filepath.Join(ep, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(ep, "BDMV/PLAYLIST/00000.mpls"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(ep, "BDMV/STREAM/00000.m2ts"), []byte("x"), 0o644)
	ageSource(t, filepath.Join(ep, "BDMV/STREAM/00000.m2ts"))
	ageSource(t, filepath.Join(ep, "BDMV/PLAYLIST/00000.mpls"))
	cands, _, err := ScanSourceOnly(root, SourceStableFor)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].SourceFile != "BDMV/PLAYLIST/00000.mpls" {
		t.Fatalf("BDMV-only episode must qualify for template materialization: %+v", cands)
	}
}

func TestRenderTemplateSubstitutions(t *testing.T) {
	in := []byte("src={{source}} series={{series}} ep={{episode}} dir={{episode_dir}} keep={{unknown}}")
	out := string(RenderTemplate(in, "My Show", "My Show/Ep 07", "src.m2ts"))
	want := "src=src.m2ts series=My Show ep=Ep 07 dir=My Show/Ep 07 keep={{unknown}}"
	if out != want {
		t.Fatalf("substitution wrong:\n got %q\nwant %q", out, want)
	}
}

func TestValidTemplateName(t *testing.T) {
	for _, ok := range []string{"1080.vpy", "2160.avs", "my script.vpy"} {
		if !ValidTemplateName(ok) {
			t.Errorf("must accept %q", ok)
		}
	}
	for _, bad := range []string{"", "../evil.vpy", "a/b.vpy", "a\\b.vpy", "..", ".hidden.vpy", "x..y.vpy"} {
		if ValidTemplateName(bad) {
			t.Errorf("must reject %q", bad)
		}
	}
}

func TestMaterializeScriptWritesSubstitutedCopy(t *testing.T) {
	root := t.TempDir()
	mkEp(t, root, "S", "Ep 01", "src.m2ts")
	writeTemplate(t, root, "S", "1080.vpy", "import vapoursynth as vs\n# {{series}} {{episode}}\nclip = vs.core.ffms2.Source(r'{{source}}')\n")
	sr := &model.Series{Name: "S", ScriptType: "vpy", ScriptTemplate: "1080.vpy"}
	c := Candidate{Series: "S", EpisodeDir: "S/Ep 01", SourceFile: "src.m2ts"}
	got, err := MaterializeScript(root, sr, c)
	if err != nil {
		t.Fatal(err)
	}
	if got.ScriptFile != "1080.vpy" || got.ScriptType != "vpy" {
		t.Fatalf("candidate not updated: %+v", got)
	}
	body, err := os.ReadFile(filepath.Join(root, "S", "Ep 01", "1080.vpy"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "# S Ep 01") || !strings.Contains(string(body), "r'src.m2ts'") {
		t.Fatalf("placeholders not substituted: %s", body)
	}
	// No temp leftovers in the episode dir.
	entries, _ := os.ReadDir(filepath.Join(root, "S", "Ep 01"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp") || strings.Contains(e.Name(), "materialize") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestMaterializeScriptNeverOverwritesExistingScript(t *testing.T) {
	root := t.TempDir()
	mkEp(t, root, "S", "Ep 01", "src.m2ts")
	custom := filepath.Join(root, "S", "Ep 01", "1080.vpy")
	os.WriteFile(custom, []byte("# hand-authored"), 0o644)
	writeTemplate(t, root, "S", "1080.vpy", "# template")
	sr := &model.Series{Name: "S", ScriptType: "vpy", ScriptTemplate: "1080.vpy"}
	_, err := MaterializeScript(root, sr, Candidate{Series: "S", EpisodeDir: "S/Ep 01", SourceFile: "src.m2ts"})
	if err == nil {
		t.Fatal("must refuse when a per-episode script exists")
	}
	body, _ := os.ReadFile(custom)
	if string(body) != "# hand-authored" {
		t.Fatalf("existing script was modified: %s", body)
	}
}

func TestMaterializeScriptValidatesAssignment(t *testing.T) {
	root := t.TempDir()
	mkEp(t, root, "S", "Ep 01", "src.m2ts")
	writeTemplate(t, root, "S", "1080.vpy", "# template")
	c := Candidate{Series: "S", EpisodeDir: "S/Ep 01", SourceFile: "src.m2ts"}
	cases := []*model.Series{
		{Name: "S", ScriptType: "vpy", ScriptTemplate: "../escape.vpy"},
		{Name: "S", ScriptType: "vpy", ScriptTemplate: "sub/dir.vpy"},
		{Name: "S", ScriptType: "avs", ScriptTemplate: "1080.vpy"}, // ext/type mismatch
		{Name: "S", ScriptType: "vpy", ScriptTemplate: "missing.vpy"},
		{Name: "S", ScriptType: "", ScriptTemplate: "1080.vpy"},
	}
	for i, sr := range cases {
		if _, err := MaterializeScript(root, sr, c); err == nil {
			t.Errorf("case %d: must reject %+v", i, sr)
		}
	}
}

// End-to-end loop: series assignment + scriptless episode -> materialized
// script + job; a second cycle must not duplicate the job.
func TestScanOnceMaterializesSeriesTemplate(t *testing.T) {
	root := t.TempDir()
	mkEp(t, root, "S", "Ep 01", "src.m2ts")
	ageSource(t, filepath.Join(root, "S", "Ep 01", "src.m2ts"))
	writeTemplate(t, root, "S", "1080.vpy", "# {{episode}} source={{source}}\n")
	st := &fakeStore{
		existing: map[string]bool{},
		flow:     &model.Flow{ID: 9, Name: "default-1080"},
		series: map[string]*model.Series{
			"S": {Name: "S", Enabled: true, ScriptType: "vpy", ScriptTemplate: "1080.vpy"},
		},
	}
	scanOnce(context.Background(), discardLog(), st, Target{Root: root}, "default-1080")
	if len(st.created) != 1 {
		t.Fatalf("want 1 job from materialization, got %d", len(st.created))
	}
	j := st.created[0]
	if j.ScriptFile != "1080.vpy" || j.ScriptType != "vpy" || j.EpisodeDir != "S/Ep 01" || j.FlowID != 9 {
		t.Fatalf("wrong job fields: %+v", j)
	}
	body, err := os.ReadFile(filepath.Join(root, "S", "Ep 01", "1080.vpy"))
	if err != nil || !strings.Contains(string(body), "# Ep 01 source=src.m2ts") {
		t.Fatalf("materialized script wrong: %v %s", err, body)
	}
	// Second cycle: the episode now has a script AND a job -> no duplicate.
	st.existing["S/Ep 01"] = true
	scanOnce(context.Background(), discardLog(), st, Target{Root: root}, "default-1080")
	if len(st.created) != 1 {
		t.Fatalf("second cycle duplicated the job: %d", len(st.created))
	}
}

func TestScanOnceWithoutAssignmentLeavesScriptlessAlone(t *testing.T) {
	root := t.TempDir()
	mkEp(t, root, "S", "Ep 01", "src.m2ts")
	ageSource(t, filepath.Join(root, "S", "Ep 01", "src.m2ts"))
	writeTemplate(t, root, "S", "1080.vpy", "# template")
	st := &fakeStore{existing: map[string]bool{}, flow: &model.Flow{ID: 9, Name: "default-1080"}}
	scanOnce(context.Background(), discardLog(), st, Target{Root: root}, "default-1080")
	if len(st.created) != 0 {
		t.Fatalf("no assignment means no job, got %+v", st.created)
	}
	if _, err := os.Stat(filepath.Join(root, "S", "Ep 01", "1080.vpy")); !os.IsNotExist(err) {
		t.Fatal("script must not be materialized without an assignment")
	}
}

func TestScanOnceMissingTemplateSkipsEpisode(t *testing.T) {
	root := t.TempDir()
	mkEp(t, root, "S", "Ep 01", "src.m2ts")
	ageSource(t, filepath.Join(root, "S", "Ep 01", "src.m2ts"))
	st := &fakeStore{
		existing: map[string]bool{},
		flow:     &model.Flow{ID: 9, Name: "default-1080"},
		series: map[string]*model.Series{
			"S": {Name: "S", Enabled: true, ScriptType: "vpy", ScriptTemplate: "gone.vpy"},
		},
	}
	scanOnce(context.Background(), discardLog(), st, Target{Root: root}, "default-1080")
	if len(st.created) != 0 {
		t.Fatalf("missing template must fail closed, got %+v", st.created)
	}
}

func TestScanOnceDisabledSeriesNoMaterialize(t *testing.T) {
	root := t.TempDir()
	mkEp(t, root, "S", "Ep 01", "src.m2ts")
	ageSource(t, filepath.Join(root, "S", "Ep 01", "src.m2ts"))
	writeTemplate(t, root, "S", "1080.vpy", "# template")
	st := &fakeStore{
		existing: map[string]bool{},
		flow:     &model.Flow{ID: 9, Name: "default-1080"},
		series: map[string]*model.Series{
			"S": {Name: "S", Enabled: false, ScriptType: "vpy", ScriptTemplate: "1080.vpy"},
		},
	}
	scanOnce(context.Background(), discardLog(), st, Target{Root: root}, "default-1080")
	if len(st.created) != 0 {
		t.Fatalf("disabled series must not materialize, got %+v", st.created)
	}
	if _, err := os.Stat(filepath.Join(root, "S", "Ep 01", "1080.vpy")); !os.IsNotExist(err) {
		t.Fatal("disabled series script was materialized")
	}
}
