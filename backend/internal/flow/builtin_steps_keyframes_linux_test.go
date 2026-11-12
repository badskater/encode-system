package flow

import (
	"strings"
	"testing"
)

// P7 render-contract tests for the Linux keyframes builtin
// (vapoursynth-scxvid). These pin the .vpy the bash body generates and the
// vspipe invocation, mirroring the Windows SCXvid contract:
//
//   - input = the RELEASE MKV (Windows runs ffmpeg on the released encode),
//     downscaled to 640x360 and converted to 8-bit 4:2:0 — the exact
//     scale=640:360 -pix_fmt yuv420p shape the PS template feeds SCXvid.
//   - scxvid.Scxvid writes the xvid first-pass log, which IS the
//     "<Series> - <Episode> Keyframes.txt" artifact (same format the
//     Windows SCXvid-standalone writes — no conversion step).
//   - use_slices=True matches SCXvid-standalone's num_slices=num_threads.
//   - paths reach the .vpy via env vars so the heredoc stays quoted
//     (injection-safe for series/episode names with quotes/spaces).

// TestBashKeyframesVpyContract pins the generated .vpy content: source
// loader family, format conversion, scxvid call and set_output.
func TestBashKeyframesVpyContract(t *testing.T) {
	script, err := renderBuiltinBash(t, "keyframes")
	if err != nil {
		t.Fatalf("render keyframes: %v", err)
	}
	for _, want := range []string{
		// Source loading: L-SMASH Works with FFMS2 fallback (the CPU
		// source-filter family the Linux encode path documents).
		"core.lsmas.LWLibavSource(source=source)",
		"core.ffms2.Source(source=source)",
		// Windows input contract: scale=640:360, yuv420p (8-bit).
		"core.resize.Bicubic(clip, width=640, height=360, format=vs.YUV420P8)",
		// scxvid call: log path + parity threading decision, then output.
		"scxvid.Scxvid(clip, log=log, use_slices=True)",
		"clip.set_output()",
		// Plugin import (missing plugin must fail loudly, not silently).
		"import scxvid",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("keyframes .vpy contract missing %q", want)
		}
	}
	// The heredoc must be QUOTED (no shell expansion inside the .vpy) and
	// paths must flow through env vars — never interpolated Python literals.
	if !strings.Contains(script, "<<'VPY'") {
		t.Error("keyframes .vpy heredoc must be quoted (<<'VPY') so paths are never shell-interpolated into Python")
	}
	for _, want := range []string{
		`os.environ["ENCODE_KF_SOURCE"]`,
		`os.environ["ENCODE_KF_LOG"]`,
		`export ENCODE_KF_SOURCE="$mkv"`,
		`export ENCODE_KF_LOG="$log"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("keyframes must pass paths via env vars, missing %q", want)
		}
	}
}

// TestBashKeyframesVspipeInvocation pins the vspipe run shape: y4m
// container, the generated script, and a null sink so the WHOLE clip
// processes (scxvid's log is only complete after the last frame; seeking
// re-processes from frame 0 and is never used).
func TestBashKeyframesVspipeInvocation(t *testing.T) {
	script, err := renderBuiltinBash(t, "keyframes")
	if err != nil {
		t.Fatalf("render keyframes: %v", err)
	}
	if !strings.Contains(script, `"$vspipe" -c y4m "$tmpd/script.vpy" - > /dev/null`) {
		t.Error("keyframes must run vspipe -c y4m <script> - to a null sink (whole-clip processing)")
	}
	// Tool resolution follows the bash builtin convention: PATH first,
	// BinDir fallback, executable check with an actionable error.
	if !strings.Contains(script, "command -v vspipe") {
		t.Error("keyframes must resolve vspipe via command -v (bash tool convention)")
	}
	if !strings.Contains(script, `vspipe="$BinDir/vspipe"`) {
		t.Error("keyframes must fall back to $BinDir/vspipe")
	}
	if !strings.Contains(script, "VapourSynth (vspipe) not found") ||
		!strings.Contains(script, "pip install vapoursynth-scxvid") {
		t.Error("missing-vspipe error must name VapourSynth and pip install vapoursynth-scxvid")
	}
}

// TestBashKeyframesArtifactContract pins the artifact path, the Windows
// skip/stale semantics and the failure texts (release MKV missing, log
// missing, vspipe failure).
func TestBashKeyframesArtifactContract(t *testing.T) {
	script, err := renderBuiltinBash(t, "keyframes")
	if err != nil {
		t.Fatalf("render keyframes: %v", err)
	}
	for _, want := range []string{
		// Same artifact location/name as the PS template.
		`local dest_dir="$ReleaseDir/$ReleaseFolder"`,
		`local mkv="$dest_dir/$OutputName"`,
		`local kf="$dest_dir/$Series - $Episode Keyframes.txt"`,
		// Input is the RELEASE MKV, not the episode source.
		`export ENCODE_KF_SOURCE="$mkv"`,
		// Skip/stale semantics (verbatim PS messages).
		`[[ "$kf" -nt "$mkv" ]]`,
		"keyframes file exists and is newer than the MKV, skipping",
		"keyframes file is stale (older than MKV), regenerating",
		// Failure texts. -s (non-empty), not -e: mktemp pre-creates the
		// temp file, so a plugin that wrote nothing must still fail.
		"release MKV not found: $mkv",
		"scxvid finished but the keyframes log was not created",
		"vspipe exited with code $rc",
		`if ! [[ -s "$log" ]]; then`,
		// Atomic publish: temp log next to the artifact, renamed on success.
		`log=$(mktemp "$dest_dir/.scxvid.XXXXXX")`,
		`mv -f -- "$log" "$kf"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("keyframes artifact contract missing %q", want)
		}
	}
	// Unique work dir for the generated script (no fixed-name collisions
	// between concurrent jobs in the same episode dir).
	if !strings.Contains(script, `tmpd=$(mktemp -d "$EpisodeDir/.keyframes.XXXXXX")`) {
		t.Error("keyframes must use a unique mktemp work dir (concurrent-job safety)")
	}
}

// TestBashKeyframesNoFakeDetection: the port must be REAL scene-change
// detection — no every-Nth-frame placeholder may survive anywhere in the
// body (the GAP 2 fake was rejected in review; this pins the rejection).
func TestBashKeyframesNoFakeDetection(t *testing.T) {
	script, err := renderBuiltinBash(t, "keyframes")
	if err != nil {
		t.Fatalf("render keyframes: %v", err)
	}
	for _, bad := range []string{
		"range(0,",      // python every-Nth-frame loop
		"frame % 240",   // modulo fake
		"every 240",     // fake description
		"ENCODE_METRIC", // keyframes emits no metrics (PS parity)
	} {
		if strings.Contains(script, bad) {
			t.Errorf("keyframes body contains fake-detection/parity-violating token %q", bad)
		}
	}
}
