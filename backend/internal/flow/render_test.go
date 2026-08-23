package flow

import (
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

func testVars() Vars {
	return Vars{BinDir: `C:\bin`, ScriptsDir: `C:\Encodes\scripts`, ReleaseDir: `C:\Encodes\ReleaseFolders`, Group: "OldFartsSubs", Tag: "1080p"}
}

func TestRenderDefaultFlowProducesAllSteps(t *testing.T) {
	f := DefaultFlow()
	j := &model.Job{ID: 7, Series: "Ookami-san to Shichinin no Nakama-tachi", Episode: "01",
		EpisodeDir: "Ookami-san to Shichinin no Nakama-tachi/Ep 01", ScriptType: "vpy", FlowID: 1}

	script, err := Render(f, j, testVars(), nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	for _, want := range []string{
		"Invoke-SourceRename",
		"Invoke-DgIndex",
		"Invoke-AudioExtract",
		"Invoke-VideoEncode",
		"Invoke-Mux",
		"Invoke-ReleaseCopy",
		"Invoke-Keyframes",
		`$Series     = 'Ookami-san to Shichinin no Nakama-tachi'`,
		`$EpisodeDir = Join-Path $ScriptsDir 'Ookami-san to Shichinin no Nakama-tachi/Ep 01'`,
		`$OutputName = 'Ookami-san to Shichinin no Nakama-tachi - 01 [1080p].mkv'`,
		`$ReleaseFolder = '[OldFartsSubs] Ookami-san to Shichinin no Nakama-tachi - Raws [1080p]'`,
		"ENCODE_JOB_DONE",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q", want)
		}
	}
	// x265 args from the flow params must appear in the encode call.
	if !strings.Contains(script, "--aq-strength-edge 0.90") {
		t.Error("default x265 args not propagated to encode step")
	}
}

func TestRenderEscapesSingleQuotes(t *testing.T) {
	f := DefaultFlow()
	j := &model.Job{ID: 1, Series: "L'Arc ~en~ Ciel", Episode: "02",
		EpisodeDir: "L'Arc ~en~ Ciel/Ep 02", ScriptType: "avs"}

	script, err := Render(f, j, testVars(), nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// PowerShell single-quote escaping: ' becomes ''
	if !strings.Contains(script, `'L''Arc ~en~ Ciel'`) {
		t.Errorf("single quote not escaped in series literal:\n%s", script)
	}
	// Raw unescaped injection must not appear.
	if strings.Contains(script, `= 'L'Arc`) {
		t.Error("unescaped quote found — injection risk")
	}
}

func TestRenderRejectsUnknownStep(t *testing.T) {
	f := &model.Flow{Name: "bad", Steps: []model.Step{{Type: "teleport"}}}
	j := &model.Job{ID: 1, Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy"}
	if _, err := Render(f, j, testVars(), nil); err == nil {
		t.Fatal("expected error for unknown step type")
	}
}

func TestRenderRejectsEmptyFlow(t *testing.T) {
	j := &model.Job{ID: 1, Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy"}
	if _, err := Render(&model.Flow{Name: "empty"}, j, testVars(), nil); err == nil {
		t.Fatal("expected error for empty flow")
	}
}

func TestEpisodeNumberParsing(t *testing.T) {
	cases := map[string]string{
		"Series/Ep 01":  "01",
		"Series/Ep 12":  "12",
		"Series/Ep 007": "007",
		"Weird/Finale":  "Finale",
		"Series/Ep 3":   "3",
	}
	for dir, want := range cases {
		if got := EpisodeNumber(dir); got != want {
			t.Errorf("EpisodeNumber(%q) = %q, want %q", dir, got, want)
		}
	}
}

func TestAudioStepParamsPropagate(t *testing.T) {
	f := &model.Flow{Name: "audio-only", Steps: []model.Step{
		{Type: model.StepAudio, Params: map[string]string{"track": "3", "bitrate": "192"}},
	}}
	j := &model.Job{ID: 2, Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy"}
	script, err := Render(f, j, testVars(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "track = '3'") || !strings.Contains(script, "bitrate = '192'") {
		t.Errorf("audio params missing from step params literal:\n%s", script)
	}
}

func TestStepOrderPreserved(t *testing.T) {
	f := &model.Flow{Name: "reversed", Steps: []model.Step{
		{Type: model.StepMux},
		{Type: model.StepEncode},
	}}
	j := &model.Job{ID: 3, Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy"}
	script, err := Render(f, j, testVars(), nil)
	if err != nil {
		t.Fatal(err)
	}
	muxIdx := strings.Index(script, "Invoke-Mux")
	encIdx := strings.Index(script, "Invoke-VideoEncode")
	if muxIdx == -1 || encIdx == -1 || muxIdx > encIdx {
		t.Error("step order not preserved (mux must come before encode in this flow)")
	}
}
