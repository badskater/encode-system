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
// end to end under pwsh with stubbed mkvmerge and MediaInfo, covering:
//   - pass_source_compare_ns:   real mkvmerge schema-v20 shape
//     (container.properties.duration in ns) + MediaInfo General Duration
//     in ms float (modern build).
//   - pass_source_compare_seconds: MediaInfo emits SECONDS instead of ms;
//     the unit-normalization must still produce delta 0 (proves itself).
//   - pass_top_level_duration_fallback: mkvmerge reports only the top-level
//     duration (ns, legacy shape) — exercises the last fallback branch.
//   - pass_container_flat_duration_fallback: duration directly on container
//     (non-schema-v20 legacy shape) — exercises the middle fallback branch.
//   - pass_sanity_mode_no_source: no source media + no MediaInfo stub →
//     falls back to the >60s sanity floor.
//   - no_audio_track_ns: video-only tracks in the real schema-v20 shape;
//     verify must reject the missing audio track.
//   - missing_file / zero_length: failure modes (unchanged shape).
//
// Skipped when pwsh is absent.
//
// The stub mechanism mirrors render_e2e_test.go: bash scripts named *.exe
// that pwsh executes via shebang on Linux. The mkvmerge stub detects -J
// (identify mode) and emits a JSON payload read from a sidecar file, so each
// subtest swaps the JSON without rewriting the stub.
//
// Duration values are in NANOSECONDS for the mkvmerge payloads (1440000000000
// ns = 1440000 ms = a 24-minute episode) to match the real MKVToolNix JSON
// identify convention. MediaInfo payloads carry the General-track Duration
// in the unit under test (ms float or seconds float).
func TestRenderedVerifyOutputExecutesInPowerShell(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not installed; skipping PowerShell integration test")
	}

	// mkvmerge -J JSON payloads. The REAL shape (identification schema v20,
	// verified against mkvtoolnix.download/doc/mkvmerge-identification-
	// output-schema-v20.json) nests the segment duration at
	// container.properties.duration in NANOSECONDS — container has exactly
	// {properties, recognized, supported, type}, no direct duration field.
	// 1440000000000 ns = 1440000 ms = a 24-min episode.
	const mkvJSONContainerNs = `{
  "container": {
    "type": "Matroska",
    "recognized": true,
    "supported": true,
    "properties": {
      "container_type": 17,
      "duration": 1440000000000
    }
  },
  "tracks": [
    { "type": "video" },
    { "type": "audio" }
  ]
}`
	// Fallback shape: duration directly on container (not schema-v20 output,
	// but the factory supports it defensively). Exercises the middle branch.
	const mkvJSONContainerFlatNs = `{
  "container": { "type": "Matroska", "duration": 1440000000000 },
  "tracks": [
    { "type": "video" },
    { "type": "audio" }
  ]
}`
	// Top-level duration fallback: legacy/alternate shape (ns) with no
	// container subobject. Exercises the last fallback branch.
	const mkvJSONTopLevelNs = `{
  "duration": 1440000000000,
  "tracks": [
    { "type": "video" },
    { "type": "audio" }
  ]
}`
	// Real schema-v20 shape but only a video track — verify must reject
	// this for the missing audio track.
	const mkvJSONNoAudioNs = `{
  "container": {
    "type": "Matroska",
    "recognized": true,
    "supported": true,
    "properties": {
      "container_type": 17,
      "duration": 1440000000000
    }
  },
  "tracks": [
    { "type": "video" }
  ]
}`

	// MediaInfo stub payload: General track Duration in MILLISECONDS (float)
	// — modern build convention. Matches the mkv duration (1440000 ms) so
	// source-compare passes with delta 0 after normalization picks 'ms'.
	const mediaInfoJSONMs = `{
  "media": {
    "track": [
      { "@type": "General", "Duration": 1440000.0 }
    ]
  }
}`
	// MediaInfo emitting SECONDS (1440.0) — an older/alternate build
	// convention. Normalization must interpret raw*1000 and still hit
	// delta 0 against the 1440000 ms mkv duration.
	const mediaInfoJSONSeconds = `{
  "media": {
    "track": [
      { "@type": "General", "Duration": 1440.0 }
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
		name        string
		mkvJSON     string
		mediaJSON   string // MediaInfo payload (empty = no MediaInfo stub)
		hasSrc      bool   // create src.mkv (source for Find-SourceFile)
		createMKV   bool   // create the muxed MKV at the output path
		mkvBytes    int    // bytes to write (0 = zero-length file)
		wantFail    bool
		wantSubstr  string // expected failure substring (empty for pass)
		wantNormSub string // expected normalization log substring (pass cases)
	}{
		{
			name:        "pass_source_compare_ns",
			mkvJSON:     mkvJSONContainerNs,
			mediaJSON:   mediaInfoJSONMs,
			hasSrc:      true,
			createMKV:   true,
			mkvBytes:    9,
			wantFail:    false,
			wantNormSub: "interpreted as ms",
		},
		{
			name:        "pass_source_compare_seconds",
			mkvJSON:     mkvJSONContainerNs,
			mediaJSON:   mediaInfoJSONSeconds,
			hasSrc:      true,
			createMKV:   true,
			mkvBytes:    9,
			wantFail:    false,
			wantNormSub: "interpreted as seconds",
		},
		{
			name:      "pass_top_level_duration_fallback",
			mkvJSON:   mkvJSONTopLevelNs,
			mediaJSON: mediaInfoJSONMs,
			hasSrc:    true,
			createMKV: true,
			mkvBytes:  9,
			wantFail:  false,
			// Top-level duration fallback still normalizes MediaInfo ms.
			wantNormSub: "interpreted as ms",
		},
		{
			name:      "pass_container_flat_duration_fallback",
			mkvJSON:   mkvJSONContainerFlatNs,
			mediaJSON: mediaInfoJSONMs,
			hasSrc:    true,
			createMKV: true,
			mkvBytes:  9,
			wantFail:  false,
			// container.duration (non-schema-v20 shape) still normalizes ms.
			wantNormSub: "interpreted as ms",
		},
		{
			name:      "pass_sanity_mode_no_source",
			mkvJSON:   mkvJSONContainerNs,
			hasSrc:    false,
			createMKV: true,
			mkvBytes:  9,
			wantFail:  false,
			// No source discoverable → sanity mode; 1440000 ms > 60s floor.
			wantNormSub: "mode: sanity",
		},
		{
			name:       "no_audio_track_ns",
			mkvJSON:    mkvJSONNoAudioNs,
			mediaJSON:  mediaInfoJSONMs,
			createMKV:  true,
			mkvBytes:   9,
			wantFail:   true,
			wantSubstr: "no audio track",
		},
		{
			name:       "missing_file",
			mkvJSON:    mkvJSONContainerNs,
			createMKV:  false,
			wantFail:   true,
			wantSubstr: "muxed MKV not found",
		},
		{
			name:       "zero_length",
			mkvJSON:    mkvJSONContainerNs,
			createMKV:  true,
			mkvBytes:   0,
			wantFail:   true,
			wantSubstr: "zero-length",
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
				"mkvmerge.exe":  mkvmergeStub,
				"mkvmerge.json": tc.mkvJSON,
			}
			if tc.mediaJSON != "" {
				stubs["MediaInfo.exe"] = mediaInfoStub
				stubs["mediainfo.json"] = tc.mediaJSON
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
				// Mode assertion: source-compare when a source AND a MediaInfo
				// stub are present; sanity otherwise (no source to compare).
				if tc.hasSrc && tc.mediaJSON != "" {
					if !strings.Contains(text, "mode: source-compare") {
						t.Errorf("output missing 'mode: source-compare' (expected source-compare mode)")
					}
				} else if !strings.Contains(text, "mode: sanity") {
					t.Errorf("output missing 'mode: sanity' (expected sanity fallback)")
				}
				// Normalization log must appear and name the chosen unit.
				if tc.wantNormSub != "" && !strings.Contains(text, tc.wantNormSub) {
					t.Errorf("output missing normalization log %q", tc.wantNormSub)
				}
				// The MKV duration must be reported in ms (converted from ns),
				// i.e. 1440000 — not the raw 1440000000000.
				if !strings.Contains(text, "MKV duration: 1440000 ms") {
					t.Errorf("output missing 'MKV duration: 1440000 ms' (ns→ms conversion)")
				}
			}
		})
	}
}
