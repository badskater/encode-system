package flow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeStub(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func absLibPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../../powershell/EncodeLib.ps1")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestFlacAudioTemplateRegistered checks the template is seeded and its
// contract (function name, output file, no bogus eac3to switches).
func TestFlacAudioTemplateRegistered(t *testing.T) {
	var found *model.StepTemplate
	for _, tpl := range BuiltinStepTemplates() {
		if tpl.Key == "flac_audio" {
			found = tpl
		}
	}
	if found == nil {
		t.Fatal("flac_audio template not registered")
	}
	for _, want := range []string{
		"function Invoke-FlacAudio",
		"'audio.flac'",
	} {
		if !strings.Contains(found.PowerShell, want) {
			t.Errorf("flac_audio script missing %q", want)
		}
	}
	// eac3to v3.34 has NO FLAC compression switch — passing one would make
	// the tool reject the invocation entirely.
	if strings.Contains(found.PowerShell, "-compression") {
		t.Error("flac_audio must not pass a compression flag (eac3to has no such switch)")
	}
}

// TestRenderFlacFlow verifies a flow using flac_audio renders the step and
// the mux step's audio-selection logic.
func TestRenderFlacFlow(t *testing.T) {
	f := &model.Flow{Name: "flac-e2e", Steps: []model.Step{
		{Type: model.StepSourceRename, Params: map[string]string{"source_name": "src"}},
		{Type: model.StepType("flac_audio"), Params: map[string]string{"track": "2"}},
		{Type: model.StepEncode},
		{Type: model.StepMux},
	}}
	j := &model.Job{ID: 8, Series: "Flac Series", Episode: "01",
		EpisodeDir: "Flac Series/Ep 01", ScriptType: "vpy", FlowID: 1}
	script, err := Render(f, j, testVars(), nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"function Invoke-FlacAudio",
		"Invoke-FlacAudio -Job $Job -Params",
		"audio.flac",
		// mux must now contain the selection logic, not a hardcoded opus path
		"$flacExists = Test-Path",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q", want)
		}
	}
}

// TestRenderedFlacScriptExecutesInPowerShell runs a FLAC audio flow end to
// end under pwsh with stub tools; the eac3to stub writes a real FLAC header
// so the size guard passes. Skipped when pwsh is absent.
func TestRenderedFlacScriptExecutesInPowerShell(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not installed; skipping PowerShell integration test")
	}
	root := t.TempDir()
	binDir := root + "/bin"
	scriptsDir := root + "/scripts"
	releaseDir := root + "/release"
	for _, d := range []string{binDir, scriptsDir, releaseDir} {
		mustMkdir(t, d)
	}
	stubs := map[string]string{
		"eac3to.exe": `#!/usr/bin/env bash
for a in "$@"; do
  case "$a" in
    *.flac) printf 'fLaC' ; head -c 2000 /dev/zero > "$a"; printf 'fLaC' > "$a"; head -c 2048 /dev/zero >> "$a" ;;
  esac
done
echo "eac3to stub done"`,
		"x265_x64.exe": `#!/usr/bin/env bash
prev=""
for a in "$@"; do [ "$prev" = "-o" ] && : > "$a"; prev="$a"; done
echo "x265 stub done"`,
		"mkvmerge.exe": `#!/usr/bin/env bash
prev=""
# record which audio file got muxed, for the assertion below
for a in "$@"; do case "$a" in audio.flac|audio.opus|*.flac|*.opus) echo "MUXED_AUDIO=$a" >> "$WORKLOG";; esac; done
for a in "$@"; do [ "$prev" = "-o" ] && printf '123456789' > "$a"; prev="$a"; done
echo "mkvmerge stub done"`,
	}
	for name, body := range stubs {
		writeStub(t, binDir, name, body)
	}

	epDir := scriptsDir + "/Flac Series/Ep 01"
	mustMkdir(t, epDir)
	writeStub(t, epDir, "src.mkv", "fake")
	writeStub(t, epDir, "1080.vpy", "# stub")

	f := &model.Flow{Name: "flac-e2e", Steps: []model.Step{
		{Type: model.StepSourceRename, Params: map[string]string{"source_name": "src"}},
		{Type: model.StepType("flac_audio"), Params: map[string]string{"track": "2"}},
		{Type: model.StepEncode},
		{Type: model.StepMux},
	}}
	j := &model.Job{ID: 100, Series: "Flac Series", Episode: "01",
		EpisodeDir: "Flac Series/Ep 01", ScriptType: "vpy"}
	script, err := Render(f, j, Vars{
		BinDir: binDir, ScriptsDir: scriptsDir, ReleaseDir: releaseDir,
		Group: "OldFartsSubs", Tag: "1080p",
	}, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	worklog := root + "/worklog.txt"
	scriptPath := root + "/job.ps1"
	mustWrite(t, scriptPath, script)
	libPath := absLibPath(t)

	cmd := exec.Command(pwsh, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", scriptPath, "-LibPath", libPath)
	// Start from os.Environ(): a nil Env would leave pwsh with no PATH and
	// its child stub scripts couldn't find their interpreter.
	cmd.Env = append(os.Environ(), "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1", "WORKLOG="+worklog)
	out, err := cmd.CombinedOutput()
	t.Logf("pwsh output:\n%s", out)
	if err != nil {
		t.Fatalf("rendered FLAC script failed under pwsh: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"ENCODE_STEP flac_audio",
		"flac_audio done:",
		"muxing audio track: audio.flac",
		"ENCODE_JOB_DONE",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q", want)
		}
	}
	// The mux stub recorded the audio file it was handed: it must be the
	// FLAC track, proving the mux step's selection logic end to end.
	wl, err := os.ReadFile(worklog)
	if err != nil {
		t.Fatalf("worklog: %v", err)
	}
	if !strings.Contains(string(wl), "audio.flac") {
		t.Errorf("mkvmerge did not receive audio.flac, worklog: %q", string(wl))
	}
}
