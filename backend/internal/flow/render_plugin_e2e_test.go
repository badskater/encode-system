package flow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestRenderedPluginScriptExecutesInPowerShell runs the FileFlows-inspired
// plugin steps (media_probe, audio_branch, crc32_rename) end to end under
// pwsh with stub tools, including a MediaInfo stub that emits valid JSON.
// Skipped automatically when pwsh is not installed.
func TestRenderedPluginScriptExecutesInPowerShell(t *testing.T) {
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

	stubs := map[string]string{
		// MediaInfo stub: valid JSON so media_probe/audio_branch parse it.
		"MediaInfo.exe": `#!/usr/bin/env bash
cat <<'JSON'
{
  "media": {
    "track": [
      { "@type": "General", "Format": "Matroska", "Duration/String3": "00:24:00" },
      { "@type": "Video", "Format": "AVC", "Width": 1920, "Height": 1080, "BitDepth": 8, "FrameRate": "23.976" },
      { "@type": "Audio", "Format": "AC-3", "Channel(s)": 2, "SamplingRate": 48000, "SamplingRate/String": "48.0 kHz", "BitRate": 192000 }
    ]
  }
}
JSON`,
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
for a in "$@"; do [ "$prev" = "-o" ] && printf '123456789' > "$a"; prev="$a"; done
echo "mkvmerge stub done"`,
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	epDir := filepath.Join(scriptsDir, "Plugin Series", "Ep 02")
	os.MkdirAll(epDir, 0o755)
	os.WriteFile(filepath.Join(epDir, "src.mkv"), []byte("fake"), 0o644)
	os.WriteFile(filepath.Join(epDir, "1080.vpy"), []byte("# stub"), 0o644)

	// Flow: probe -> audio_branch -> encode -> mux -> crc32_rename -> release.
	f := &model.Flow{Name: "plugin-e2e", Steps: []model.Step{
		{Type: model.StepSourceRename, Params: map[string]string{"source_name": "src"}},
		{Type: model.StepType("media_probe")},
		{Type: model.StepType("audio_branch"), Params: map[string]string{"track": "2"}},
		{Type: model.StepEncode},
		{Type: model.StepMux},
		{Type: model.StepType("crc32_rename")},
		{Type: model.StepReleaseCopy},
	}}
	j := &model.Job{ID: 99, Series: "Plugin Series", Episode: "02",
		EpisodeDir: "Plugin Series/Ep 02", ScriptType: "vpy"}

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

	cmd := exec.Command(pwsh, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", scriptPath, "-LibPath", libPath)
	cmd.Env = append(os.Environ(), "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1")
	out, err := cmd.CombinedOutput()
	t.Logf("pwsh output:\n%s", out)
	if err != nil {
		t.Fatalf("rendered plugin script failed under pwsh: %v", err)
	}

	text := string(out)
	for _, want := range []string{
		"ENCODE_STEP media_probe",
		"[probe] container: Matroska",
		"[probe] video: AVC 1920x1080",
		"[probe] AUDIO #1 -> eac3to track 2",
		"ENCODE_STEP audio_branch",
		"[branch] track 2 : AC-3 -> Opus @ 192 kbps",
		// Structured encode assembly: defaults applied for omitted params,
		// including the bool flags (regression for the dropped-flag bug).
		"[x265] args:",
		"--preset slow",
		"--crf 15",
		"--aq-mode 5",
		"--no-sao",
		"--b-pyramid",
		"--open-gop",
		"ENCODE_STEP crc32_rename",
		"ENCODE_JOB_DONE",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q", want)
		}
	}

	// crc32_rename must have appended the CANONICAL CRC32 of the stub MKV
	// content ("123456789" -> CBF43926), proving the checksum path end to end.
	relDir := filepath.Join(releaseDir, "[OldFartsSubs] Plugin Series - Raws [1080p]")
	entries, err := os.ReadDir(relDir)
	if err != nil {
		t.Fatalf("release dir: %v", err)
	}
	foundChecksummed := false
	for _, e := range entries {
		if strings.Contains(e.Name(), "[CBF43926]") {
			foundChecksummed = true
		}
	}
	if !foundChecksummed {
		t.Errorf("no [CBF43926] release MKV found in %s (entries: %v)", relDir, entries)
	}
}
