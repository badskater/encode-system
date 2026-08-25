package flow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestHdr4kFlowExecutesInPowerShell runs the HDR/4K step set end to end
// under pwsh with stub tools: hdr_probe writes hdr.json from an HDR10
// MediaInfo stub, audio_lang picks the Japanese track by language, encode_4k
// emits bt2020/PQ signaling, and mux tags the audio track from audio.json.
// Skipped automatically when pwsh is not installed.
func TestHdr4kFlowExecutesInPowerShell(t *testing.T) {
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
		// MediaInfo stub: HDR10 source (PQ transfer, bt2020 primaries, 4K),
		// two audio tracks — English first, Japanese second — so the
		// language-priority selection actually has to search (jpn wins even
		// though eng is stream #1).
		"MediaInfo.exe": `#!/usr/bin/env bash
cat <<'JSON'
{
  "media": {
    "track": [
      { "@type": "General", "Format": "Matroska", "Duration/String3": "00:24:00" },
      { "@type": "Video", "Format": "HEVC", "Width": 3840, "Height": 2160, "BitDepth": 10, "FrameRate": "23.976", "transfer_characteristics": "PQ", "colour_primaries": "BT.2020", "MaxCLL": "1000 cd/m2", "MaxFALL": "400 cd/m2" },
      { "@type": "Audio", "Format": "AC-3", "Channel(s)": 2, "SamplingRate": 48000, "SamplingRate/String": "48.0 kHz", "BitRate": 192000, "Language/String3": "eng" },
      { "@type": "Audio", "Format": "AC-3", "Channel(s)": 2, "SamplingRate": 48000, "SamplingRate/String": "48.0 kHz", "BitRate": 192000, "Language/String3": "jpn" }
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

	epDir := filepath.Join(scriptsDir, "HDR Series", "Ep 01")
	os.MkdirAll(epDir, 0o755)
	os.WriteFile(filepath.Join(epDir, "src.mkv"), []byte("fake"), 0o644)
	os.WriteFile(filepath.Join(epDir, "2160.vpy"), []byte("# stub"), 0o644)

	f := &model.Flow{Name: "hdr-4k-e2e", Steps: []model.Step{
		{Type: model.StepType("hdr_probe")},
		{Type: model.StepType("audio_lang"), Params: map[string]string{"languages": "jpn,eng", "bitrate": "320"}},
		{Type: model.StepType("encode_4k")},
		{Type: model.StepMux},
		{Type: model.StepReleaseCopy},
	}}
	j := &model.Job{ID: 100, Series: "HDR Series", Episode: "01",
		EpisodeDir: "HDR Series/Ep 01", ScriptType: "vpy", ScriptFile: "2160.vpy"}
	script, err := Render(f, j, Vars{
		BinDir: binDir, ScriptsDir: scriptsDir, ReleaseDir: releaseDir,
		Group: "OldFartsSubs", Tag: "2160p",
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
		t.Fatalf("rendered HDR/4K script failed under pwsh: %v", err)
	}

	text := string(out)
	for _, want := range []string{
		// hdr_probe detected PQ/bt2020 and wrote the sidecar.
		"ENCODE_STEP hdr_probe",
		"3840 x 2160",
		"HDR10",
		// audio_lang searched the priority list and picked the Japanese
		// stream even though English came first in the file.
		"ENCODE_STEP audio_lang",
		"selected audio #2 (language 'jpn')",
		// encode_4k switched to bt2020/PQ signaling from hdr.json.
		"ENCODE_STEP encode_4k",
		"hdr.json reports HDR10 -> bt2020/PQ signaling",
		"--colorprim bt2020",
		"--transfer smpte-st2084",
		"--ctu 64",
		// mux read the recorded language.
		"audio track language from audio.json: jpn",
		"ENCODE_JOB_DONE",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q", want)
		}
	}
	// The release MKV must exist under the 2160p release folder.
	relDir := filepath.Join(releaseDir, "[OldFartsSubs] HDR Series - Raws [2160p]")
	entries, err := os.ReadDir(relDir)
	if err != nil {
		t.Fatalf("release dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no release artifact written")
	}

	// audio.json must carry the selection for downstream tools.
	audioJSON, err := os.ReadFile(filepath.Join(epDir, "audio.json"))
	if err != nil {
		t.Fatalf("audio.json missing: %v", err)
	}
	if !strings.Contains(string(audioJSON), `"language":"jpn"`) {
		t.Errorf("audio.json wrong: %s", audioJSON)
	}
	// hdr.json must report HDR10.
	hdrJSON, err := os.ReadFile(filepath.Join(epDir, "hdr.json"))
	if err != nil {
		t.Fatalf("hdr.json missing: %v", err)
	}
	if !strings.Contains(string(hdrJSON), `"hdr":"HDR10"`) {
		t.Errorf("hdr.json wrong: %s", hdrJSON)
	}
}

// TestAudioLangFallsBackToFirstTrack: when no priority language matches,
// audio_lang still completes using the first audio track and warns loudly.
func TestAudioLangFallsBackToFirstTrack(t *testing.T) {
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
		"MediaInfo.exe": `#!/usr/bin/env bash
cat <<'JSON'
{
  "media": {
    "track": [
      { "@type": "General", "Format": "Matroska" },
      { "@type": "Video", "Format": "AVC", "Width": 1920, "Height": 1080, "BitDepth": 8 },
      { "@type": "Audio", "Format": "AC-3", "Channel(s)": 2, "SamplingRate": 48000, "Language/String3": "fra" }
    ]
  }
}
JSON`,
		"eac3to.exe": `#!/usr/bin/env bash
for a in "$@"; do case "$a" in *.wav) : > "$a";; esac; done`,
		"opusenc.exe": `#!/usr/bin/env bash
for a in "$@"; do case "$a" in *.opus) : > "$a";; esac; done`,
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	epDir := filepath.Join(scriptsDir, "Fallback", "Ep 01")
	os.MkdirAll(epDir, 0o755)
	os.WriteFile(filepath.Join(epDir, "src.mkv"), []byte("fake"), 0o644)

	f := &model.Flow{Name: "lang-fallback", Steps: []model.Step{
		{Type: model.StepType("audio_lang"), Params: map[string]string{"languages": "jpn,eng"}},
	}}
	j := &model.Job{ID: 101, Series: "Fallback", Episode: "01",
		EpisodeDir: "Fallback/Ep 01", ScriptType: "vpy"}
	script, err := Render(f, j, Vars{
		BinDir: binDir, ScriptsDir: scriptsDir, ReleaseDir: releaseDir,
		Group: "OldFartsSubs", Tag: "1080p",
	}, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	scriptPath := filepath.Join(root, "job.ps1")
	os.WriteFile(scriptPath, []byte(script), 0o644)
	libPath, _ := filepath.Abs("../../../powershell/EncodeLib.ps1")

	cmd := exec.Command(pwsh, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", scriptPath, "-LibPath", libPath)
	cmd.Env = append(os.Environ(), "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1")
	out, err := cmd.CombinedOutput()
	t.Logf("pwsh output:\n%s", out)
	if err != nil {
		t.Fatalf("fallback flow failed under pwsh: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"WARNING: none of [jpn,eng] matched",
		"falling back to audio #1 (fra)",
		"ENCODE_JOB_DONE",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q", want)
		}
	}
}
