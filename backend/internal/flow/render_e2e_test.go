package flow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestRenderedScriptExecutesInPowerShell is an integration test: it renders a
// real flow into a PowerShell script and runs it under pwsh with stub encode
// tools. Skipped automatically when pwsh is not installed.
//
// The stubs mimic each tool's output-file contract:
//   - DGIndexNV/x265/mkvmerge create the file named after -o
//   - eac3to creates the *.wav argument, opusenc creates the *.opus argument
func TestRenderedScriptExecutesInPowerShell(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not installed; skipping PowerShell integration test")
	}

	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	scriptsDir := filepath.Join(root, "scripts")
	releaseDir := filepath.Join(root, "release")
	for _, d := range []string{binDir, scriptsDir, releaseDir} {
		os.MkdirAll(d, 0o755)
	}

	// Stub tools (bash scripts named *.exe; pwsh executes them on Linux).
	stubs := map[string]string{
		"DGIndexNV.exe": `#!/usr/bin/env bash
prev=""
for a in "$@"; do [ "$prev" = "-o" ] && : > "$a"; prev="$a"; done
echo "DGIndexNV stub done"`,
		"eac3to.exe": `#!/usr/bin/env bash
for a in "$@"; do case "$a" in *.wav) : > "$a";; esac; done
echo "eac3to stub done"`,
		"opusenc.exe": `#!/usr/bin/env bash
for a in "$@"; do case "$a" in *.opus) : > "$a";; esac; done
echo "opusenc stub done"`,
		"x265_x64.exe": `#!/usr/bin/env bash
prev=""
for a in "$@"; do [ "$prev" = "-o" ] && : > "$a"; prev="$a"; done
echo "x265 stub done"`,
		"mkvmerge.exe": `#!/usr/bin/env bash
prev=""
for a in "$@"; do [ "$prev" = "-o" ] && : > "$a"; prev="$a"; done
echo "mkvmerge stub done"`,
		"ffmpeg.exe": `#!/usr/bin/env bash
args=("$@"); : > "${args[${#args[@]}-1]}"
echo "ffmpeg stub done"`,
		"SCXvid.exe": `#!/usr/bin/env bash
# real contract: scxvid {output_log_file} < {input y4m on stdin}
cat > /dev/null
: > "$1"
echo "SCXvid stub done"`,
	}
	for name, body := range stubs {
		p := filepath.Join(binDir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Episode folder with source + filter script.
	epDir := filepath.Join(scriptsDir, "Test Series", "Ep 01")
	os.MkdirAll(epDir, 0o755)
	os.WriteFile(filepath.Join(epDir, "src.m2ts"), []byte("fake"), 0o644)
	os.WriteFile(filepath.Join(epDir, "1080.vpy"), []byte("# stub vs script"), 0o644)

	// Full default flow including keyframes — regression test for the
	// cmd.exe-only pipe bug (keyframes now uses a temp y4m file).
	f := &model.Flow{Name: "e2e", Steps: []model.Step{
		{Type: model.StepSourceRename, Params: map[string]string{"source_name": "src"}},
		{Type: model.StepDGIndex},
		{Type: model.StepAudio, Params: map[string]string{"track": "2", "bitrate": "320"}},
		{Type: model.StepEncode},
		{Type: model.StepMux},
		{Type: model.StepReleaseCopy},
		{Type: model.StepKeyframes},
	}}
	j := &model.Job{ID: 42, Series: "Test Series", Episode: "01",
		EpisodeDir: "Test Series/Ep 01", ScriptType: "vpy"}

	script, err := Render(f, j, Vars{
		BinDir: binDir, ScriptsDir: scriptsDir, ReleaseDir: releaseDir,
		Group: "OldFartsSubs", Tag: "1080p",
	}, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	scriptPath := filepath.Join(root, "job.ps1")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	libPath, err := filepath.Abs("../../../powershell/EncodeLib.ps1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(libPath); err != nil {
		t.Fatalf("EncodeLib.ps1 not found at %s", libPath)
	}

	cmd := exec.Command(pwsh, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", scriptPath, "-LibPath", libPath)
	cmd.Env = append(os.Environ(), "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1")
	out, err := cmd.CombinedOutput()
	t.Logf("pwsh output:\n%s", out)
	if err != nil {
		t.Fatalf("rendered script failed under pwsh: %v", err)
	}

	text := string(out)
	for _, want := range []string{
		"ENCODE_STEP source_rename",
		"ENCODE_STEP dgindex",
		"ENCODE_STEP audio",
		"ENCODE_STEP encode",
		"ENCODE_STEP mux",
		"ENCODE_STEP release_copy",
		"ENCODE_STEP keyframes",
		"ENCODE_JOB_DONE",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q", want)
		}
	}

	// Artifacts must exist where the pipeline puts them.
	releaseMkv := filepath.Join(releaseDir, "[OldFartsSubs] Test Series - Raws [1080p]", "Test Series - 01 [1080p].mkv")
	for _, p := range []string{
		filepath.Join(epDir, "src.dgi"),
		filepath.Join(epDir, "audio.opus"),
		filepath.Join(epDir, "1080.hevc"),
		filepath.Join(epDir, "Test Series - 01 [1080p].mkv"),
		releaseMkv,
		filepath.Join(releaseDir, "[OldFartsSubs] Test Series - Raws [1080p]", "Test Series - 01 Keyframes.txt"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected artifact missing: %s", p)
		}
	}
	// WAV intermediate must be cleaned up.
	if _, err := os.Stat(filepath.Join(epDir, "audio.wav")); !os.IsNotExist(err) {
		t.Error("intermediate WAV should have been removed")
	}
}
