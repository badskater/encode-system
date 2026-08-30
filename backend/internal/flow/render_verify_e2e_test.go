package flow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestRenderedVerifyOutputExecutesInPowerShell runs the verify_output step
// end to end under pwsh with stubbed mkvmerge and MediaInfo, covering the
// pass path (source-compare mode) and three failure modes (missing file,
// zero-length, no audio track). Skipped when pwsh is absent.
//
// The stub mechanism mirrors render_e2e_test.go: bash scripts named *.exe
// that pwsh executes via shebang on Linux. The mkvmerge stub detects -J
// (identify mode) and emits a JSON payload read from a sidecar file, so each
// subtest swaps the JSON without rewriting the stub.
func TestRenderedVerifyOutputExecutesInPowerShell(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not installed; skipping PowerShell integration test")
	}

	// mkvmerge -J JSON payloads.
	const mkvJSONGood = `{
  "duration": 1440000,
  "tracks": [
    { "type": "video" },
    { "type": "audio" }
  ]
}`
	// Same duration but only a video track — verify must reject this.
	const mkvJSONNoAudio = `{
  "duration": 1440000,
  "tracks": [
    { "type": "video" }
  ]
}`

	// MediaInfo stub payload: General track Duration matching the mkvmerge
	// duration (1440000) so source-compare passes with delta 0.
	const mediaInfoJSON = `{
  "media": {
    "track": [
      { "@type": "General", "Duration": 1440000.0 }
    ]
  }
}`

	// mkvmerge stub: -J emits the JSON sidecar; any other invocation is a
	// no-op (verify_output only calls it with -J in this flow).
	const mkvmergeStub = `#!/usr/bin/env bash
for a in "$@"; do
  if [ "$a" = "-J" ]; then
    cat "$(dirname "$0")/mkvmerge.json"
    exit 0
  fi
done
echo "mkvmerge stub (non-J mode)"
`

	// MediaInfo stub: always emit the source-duration JSON.
	const mediaInfoStub = `#!/usr/bin/env bash
cat "$(dirname "$0")/mediainfo.json"
`

	cases := []struct {
		name       string
		mkvJSON    string
		hasMedia   bool // create MediaInfo.exe + mediainfo.json (source-compare)
		hasSrc     bool // create src.mkv (source for Find-SourceFile)
		createMKV  bool // create the muxed MKV at the output path
		mkvBytes   int  // bytes to write (0 = zero-length file)
		wantFail   bool
		wantSubstr string // expected failure substring (empty for pass)
	}{
		{
			name:      "pass_source_compare",
			mkvJSON:   mkvJSONGood,
			hasMedia:  true,
			hasSrc:    true,
			createMKV: true,
			mkvBytes:  9,
			wantFail:  false,
		},
		{
			name:       "missing_file",
			mkvJSON:    mkvJSONGood,
			createMKV:  false,
			wantFail:   true,
			wantSubstr: "muxed MKV not found",
		},
		{
			name:       "zero_length",
			mkvJSON:    mkvJSONGood,
			createMKV:  true,
			mkvBytes:   0,
			wantFail:   true,
			wantSubstr: "zero-length",
		},
		{
			name:       "no_audio_track",
			mkvJSON:    mkvJSONNoAudio,
			createMKV:  true,
			mkvBytes:   9,
			wantFail:   true,
			wantSubstr: "no audio track",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			binDir := filepath.Join(root, "bin")
			scriptsDir := filepath.Join(root, "scripts")
			releaseDir := filepath.Join(root, "release")
			for _, d := range []string{binDir, scriptsDir, releaseDir} {
				os.MkdirAll(d, 0o755)
			}

			// Stub tools (bash scripts named *.exe; pwsh runs them on Linux).
			stubs := map[string]string{
				"mkvmerge.exe":   mkvmergeStub,
				"mkvmerge.json":  tc.mkvJSON,
				"MediaInfo.exe":  mediaInfoStub,
				"mediainfo.json": mediaInfoJSON,
			}
			for name, body := range stubs {
				p := filepath.Join(binDir, name)
				if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			// Episode folder.
			epDir := filepath.Join(scriptsDir, "Verify Series", "Ep 01")
			os.MkdirAll(epDir, 0o755)

			// Source media (for source-compare mode): src.mkv.
			if tc.hasSrc {
				os.WriteFile(filepath.Join(epDir, "src.mkv"), []byte("fake-source"), 0o644)
			}

			// Muxed MKV at the output path the mux step writes to.
			series := "Verify Series"
			episode := "01"
			tag := "1080p"
			outputName := OutputName(series, episode, tag)
			if tc.createMKV {
				mkvPath := filepath.Join(epDir, outputName)
				data := make([]byte, tc.mkvBytes)
				for i := range data {
					data[i] = 'x'
				}
				os.WriteFile(mkvPath, data, 0o644)
			}

			// Flow: verify_output only (the MKV is pre-placed).
			f := &model.Flow{Name: "verify-e2e", Steps: []model.Step{
				{Type: model.StepType("verify_output")},
			}}
			j := &model.Job{ID: 50, Series: series, Episode: episode,
				EpisodeDir: "Verify Series/Ep 01", ScriptType: "vpy"}

			script, err := Render(f, j, Vars{
				BinDir: binDir, ScriptsDir: scriptsDir, ReleaseDir: releaseDir,
				Group: "OldFartsSubs", Tag: tag,
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
			out, runErr := cmd.CombinedOutput()
			t.Logf("pwsh output:\n%s", out)

			text := string(out)
			if tc.wantFail {
				if runErr == nil {
					t.Fatalf("expected failure but script exited 0")
				}
				if !strings.Contains(text, "ENCODE_STEP_FAILED verify_output") {
					t.Errorf("output missing ENCODE_STEP_FAILED verify_output")
				}
				if tc.wantSubstr != "" && !strings.Contains(text, tc.wantSubstr) {
					t.Errorf("output missing %q", tc.wantSubstr)
				}
			} else {
				if runErr != nil {
					t.Fatalf("expected pass but script failed: %v", runErr)
				}
				if !strings.Contains(text, "ENCODE_STEP verify_output 100") {
					t.Errorf("output missing ENCODE_STEP verify_output 100")
				}
				if strings.Contains(text, "ENCODE_STEP_FAILED") {
					t.Errorf("unexpected ENCODE_STEP_FAILED in passing case")
				}
				// Source-compare mode must be reported.
				if !strings.Contains(text, "mode: source-compare") {
					t.Errorf("output missing 'mode: source-compare' (expected source-compare mode)")
				}
			}
		})
	}
}
