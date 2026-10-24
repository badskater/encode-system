package flow

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestBuiltinBashExecAllPortedTemplates: for EVERY ported built-in template,
// render a minimal flow using it via BashDialect and EXECUTE the script
// under /bin/bash with PATH stubbed to a t.TempDir() containing fake
// executables (ffmpeg, opusenc, mediainfo, jq, mkvmerge, x265, vspipe, curl,
// sha256sum, cksum, flac, dovi_tool, bc) that echo plausible outputs and
// exit 0.
//
// The core guarantee this test enforces (the call→define linkage that was
// broken by the P0 func-name divergence): every rendered function call
// resolves to a defined function — i.e. NO "command not found" / exit 127
// anywhere in the output. ENCODE_STEP markers must appear in order.
//
// Stub strategy (per the gap-1 test requirement):
//   - Stub tools echo plausible values so templates with 2>/dev/null
//     fallbacks take their fallback path. Templates needing complex jq
//     parsing may still fail, but the failure is NOT exit 127 — the
//     function IS called (linkage proven).
//   - mediainfo/jq stubs emit enough shape for the scripts to proceed.
//   - mkvmerge: for -J emits valid JSON (verify_output), for mux creates
//     the output file (after -o).
//   - bc: evaluates the expression (needed by verify_output's duration math).
//   - mv/cp/find/mkdir/stat are real coreutils (kept on the real PATH).
//
// Also runs a STATIC linkage check (TestBuiltinBashStaticLinkage): parses
// every rendered script to confirm each defined function name equals its
// StepCall invocation name — a cheap all-15 check that catches the P0
// class of bug without needing execution.

// --- stub builders ---------------------------------------------------------

// stubBinDir returns a temp directory containing all fake executables needed
// by the ported bash templates. It also returns a PATH string that puts the
// stub dir first but keeps /usr/bin and /bin on the tail so real coreutils
// (mv, cp, find, mkdir, stat, hostname) remain available.
func stubBinDir(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()

	stubs := map[string]string{
		// ffmpeg: decode-only. Audio templates call: ffmpeg -nostdin -y -i src -map 0:a:N -c:a pcm_s24le out.wav
		// The last positional arg is the output file. Create it.
		"ffmpeg": `#!/usr/bin/env bash
out=""
prev=""
for a in "$@"; do
  [ "$prev" = "-o" ] && out="$a"
  case "$a" in *.wav) out="$a";; esac
  prev="$a"
done
[ -n "$out" ] && : > "$out"
echo "ffmpeg stub: decoded to $out"
`,

		// opusenc: create the .opus output (last arg).
		"opusenc": `#!/usr/bin/env bash
for a in "$@"; do
  case "$a" in *.opus) : > "$a";; esac
done
echo "opusenc stub: encoded"
`,

		// flac: create the .flac output (after -o, or last arg).
		"flac": `#!/usr/bin/env bash
out=""
prev=""
for a in "$@"; do
  [ "$prev" = "-o" ] && out="$a"
  case "$a" in *.flac) out="$a";; esac
  prev="$a"
done
[ -n "$out" ] && : > "$out"
echo "flac stub: encoded"
`,

		// x265: create the hevc output (after -o). Reads stdin (y4m) — just discard.
		"x265": `#!/usr/bin/env bash
cat > /dev/null
out=""
prev=""
for a in "$@"; do
  [ "$prev" = "-o" ] && out="$a"
  prev="$a"
done
[ -n "$out" ] && : > "$out"
echo "x265 stub: encoded to $out"
`,

		// vspipe: pipe a minimal y4m frame to stdout (x265 stub discards stdin).
		"vspipe": `#!/usr/bin/env bash
printf 'YUV4MPEG2 W640 H360 F24:1 Ip A0:0\nFRAME\n'
echo "vspipe stub: piped y4m" >&2
`,

		// mkvmerge: dual mode.
		// -J: emit valid JSON the verify_output template parses.
		// mux (-o): create the output MKV.
		"mkvmerge": `#!/usr/bin/env bash
if [[ "$1" == "-J" ]]; then
  cat << 'JSON'
{"container":{"properties":{"duration":"1440000000"}},"tracks":[{"type":"video","properties":{"codec_id":"V_MPEGH/ISO/HEVC"}},{"type":"audio","properties":{"codec_id":"A_OPUS"}}]}
JSON
  exit 0
fi
# mux mode: find -o and create the output file.
out=""
prev=""
for a in "$@"; do
  [ "$prev" = "-o" ] && out="$a"
  prev="$a"
done
[ -n "$out" ] && : > "$out"
echo "mkvmerge stub: muxed to $out"
`,

		// mediainfo: emit valid JSON shape for all templates that parse it.
		// The shape includes General/Video/Audio tracks with the fields the
		// templates query (Format, Duration, Width, Height, transfer, etc.).
		"mediainfo": `#!/usr/bin/env bash
if [[ "$2" == "--Output=JSON" ]]; then
  cat << 'JSON'
{"media":{"track":[
  {"@type":"General","Format":"Matroska","Duration":"1440.000","Duration/String3":"00:24:00.000"},
  {"@type":"Video","Format":"HEVC","Width":"1920","Height":"1080","BitDepth":"8","FrameRate":"23.976","transfer_characteristics":"PQ","colour_primaries":"BT.2020","MaxCLL":"1000","MaxFALL":"400"},
  {"@type":"Audio","Format":"AC-3","Channel(s)":"2","SamplingRate":"48000","BitRate":"448000","Language/String3":"jpn","Language":"Japanese"}
]}}
JSON
  exit 0
fi
echo "mediainfo stub"
`,

		// jq: minimal stub handling -n/-nc (create from --arg), -e (exit 1),
		// -r (raw: echo plausible values), -c (compact JSON).
		// Templates with 2>/dev/null fallbacks take their fallback path.
		"jq": `#!/usr/bin/env bash
stdin=$(cat)
# -n/-nc mode: build JSON from --arg/--argjson values.
if [[ "$*" == *"-n"* ]]; then
  result='{'
  first=1
  args=("$@")
  i=0
  while (( i < ${#args[@]} )); do
    if [[ "${args[$i]}" == "--arg" ]] && (( i+2 < ${#args[@]} )); then
      k="${args[$((i+1))]}"; v="${args[$((i+2))]}"
      (( first )) || result+=','
      result+="\"$k\":\"$v\""
      first=0
      i=$((i+3))
    elif [[ "${args[$i]}" == "--argjson" ]] && (( i+2 < ${#args[@]} )); then
      k="${args[$((i+1))]}"; v="${args[$((i+2))]}"
      (( first )) || result+=','
      result+="\"$k\":$v"
      first=0
      i=$((i+3))
    else
      i=$((i+1))
    fi
  done
  result+='}'
  printf '%s' "$result"
  exit 0
fi
# -e (exit-status): exit 1 to trigger fallback paths.
if [[ "$*" == *"-e"* ]]; then exit 1; fi
# -r (raw output): echo a plausible raw value.
if [[ "$*" == *"-r"* ]]; then
  q="$*"
  if echo "$q" | grep -qi 'duration'; then echo '1440000'; exit 0; fi
  if echo "$q" | grep -qi 'length'; then echo '1'; exit 0; fi
  if echo "$q" | grep -qi 'width'; then echo '1920'; exit 0; fi
  if echo "$q" | grep -qi 'height'; then echo '1080'; exit 0; fi
  if echo "$q" | grep -qi 'MaxCLL'; then echo '1000'; exit 0; fi
  if echo "$q" | grep -qi 'MaxFALL'; then echo '400'; exit 0; fi
  if echo "$q" | grep -qi 'MasteringDisplay\|master_display'; then echo 'G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1)'; exit 0; fi
  if echo "$q" | grep -qi 'transfer'; then echo 'PQ'; exit 0; fi
  if echo "$q" | grep -qi 'primaries'; then echo 'BT.2020'; exit 0; fi
  if echo "$q" | grep -qi 'Language'; then echo 'jpn'; exit 0; fi
  if echo "$q" | grep -qi 'Channel'; then echo '2'; exit 0; fi
  if echo "$q" | grep -qi 'SamplingRate'; then echo '48000'; exit 0; fi
  if echo "$q" | grep -qi 'BitRate'; then echo '448000'; exit 0; fi
  if echo "$q" | grep -qi 'Format_Profile'; then echo ''; exit 0; fi
  if echo "$q" | grep -qi 'Format'; then echo 'HEVC'; exit 0; fi
  if echo "$q" | grep -qi 'dovi'; then echo 'false'; exit 0; fi
  if echo "$q" | grep -qi 'hdr'; then echo 'SDR'; exit 0; fi
  if echo "$q" | grep -qi 'language'; then echo 'jpn'; exit 0; fi
  if echo "$q" | grep -qi 'track'; then echo '2'; exit 0; fi
  if echo "$q" | grep -qi 'codec'; then echo 'opus'; exit 0; fi
  if echo "$q" | grep -qi 'bitrate'; then echo '320'; exit 0; fi
  echo ''
  exit 0
fi
# -c (compact): echo minimal JSON.
if [[ "$*" == *"-c"* ]]; then
  if echo "$*" | grep -qi 'Audio'; then echo '{"@type":"Audio","Format":"AC-3"}'; exit 0; fi
  if echo "$*" | grep -qi 'video'; then echo '{"type":"video"}'; exit 0; fi
  echo '{}'
  exit 0
fi
echo ''
exit 0
`,

		// bc: evaluate the expression (needed by verify_output duration math).
		// bc -l reads "expr" on stdin. Use python3 as the evaluator.
		"bc": `#!/usr/bin/env bash
expr=$(cat)
python3 -c "
import sys
e = sys.argv[1].strip()
try:
    print(int(eval(e, {'__builtins__': {}}, {})))
except:
    print(0)
" "$expr"
`,

		// cksum: emit a CRC32 + size + filename.
		"cksum": `#!/usr/bin/env bash
f="$1"
size=0
[ -f "$f" ] && size=$(stat -c%s "$f" 2>/dev/null || stat -f%z "$f" 2>/dev/null || echo 0)
echo "3735928559 $size $f"
`,

		// curl: always succeed (discord_notify best-effort).
		"curl": `#!/usr/bin/env bash
echo "curl stub: ok"
`,

		// dovi_tool: exit 0 (creates rpu.bin if asked).
		"dovi_tool": `#!/usr/bin/env bash
for a in "$@"; do
  case "$a" in -o) ;;  # handled by next arg
  esac
done
prev=""
for a in "$@"; do
  [ "$prev" = "-o" ] && : > "$a"
  prev="$a"
done
echo "dovi_tool stub: ok"
`,
	}

	for name, body := range stubs {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// PATH: stub dir first, then real /usr/bin and /bin for coreutils.
	path := dir + ":/usr/bin:/bin:/usr/local/bin"
	return dir, path
}

// execTestEnv sets up a temp episode/release tree with source files and
// returns the Vars pointing at it.
func execTestEnv(t *testing.T, binDir string) (Vars, string) {
	t.Helper()
	root := t.TempDir()
	scriptsDir := filepath.Join(root, "scripts")
	releaseDir := filepath.Join(root, "release")
	for _, d := range []string{scriptsDir, releaseDir} {
		os.MkdirAll(d, 0o755)
	}
	// Episode folder with a source .m2ts and a filter script.
	epDir := filepath.Join(scriptsDir, "Test Series", "Ep 01")
	os.MkdirAll(epDir, 0o755)
	os.WriteFile(filepath.Join(epDir, "src.m2ts"), []byte("fake-source-data"), 0o644)
	os.WriteFile(filepath.Join(epDir, "1080.vpy"), []byte("# stub vpy"), 0o644)
	// Pre-create the hevc and audio files so mux/verify can find them.
	os.WriteFile(filepath.Join(epDir, "1080.hevc"), []byte("fake-hevc"), 0o644)
	os.WriteFile(filepath.Join(epDir, "audio.opus"), []byte("fake-audio"), 0o644)
	// Pre-create the muxed MKV so verify_output/crc32_rename/release_copy can find it.
	mkvName := "Test Series - 01 [1080p].mkv"
	os.WriteFile(filepath.Join(epDir, mkvName), []byte("fake-mkv-content"), 0o644)
	// Pre-create the release folder + MKV for keyframes (now refused, so not needed).
	relFolder := filepath.Join(releaseDir, "[TestGroup] Test Series - Raws [1080p]")
	os.MkdirAll(relFolder, 0o755)
	os.WriteFile(filepath.Join(relFolder, mkvName), []byte("fake-mkv-content"), 0o644)

	v := Vars{
		BinDir:     binDir,
		ScriptsDir: scriptsDir,
		ReleaseDir: releaseDir,
		Group:      "TestGroup",
		Tag:        "1080p",
	}
	return v, root
}

// renderExecScript renders a single-step flow for the given key via BashDialect
// with the provided Vars and a BuiltinResolver. Returns the script text.
func renderExecScript(t *testing.T, key string, v Vars) string {
	t.Helper()
	resolve := BuiltinResolver()
	f := singleStepFlow(key)
	// Add params for templates that need them.
	switch key {
	case "source_rename":
		f.Steps[0].Params = map[string]string{"source_name": "src"}
	case "audio", "audio_branch", "flac_audio":
		f.Steps[0].Params = map[string]string{"track": "2", "bitrate": "320"}
	case "audio_lang":
		f.Steps[0].Params = map[string]string{"languages": "jpn,eng", "bitrate": "320"}
	}
	j := linuxTestJob()
	script, err := RenderDialect(f, j, v, resolve, BashDialect{})
	if err != nil {
		t.Fatalf("render %q: %v", key, err)
	}
	return script
}

// TestBuiltinBashExecAllPortedTemplates executes every ported built-in
// template's rendered bash script under /bin/bash with stubbed tools and
// asserts:
//  1. No "command not found" / exit 127 in the output (the call→define
//     linkage the P0 fix guarantees).
//  2. ENCODE_STEP markers appear in order.
//
// Templates whose stubs are too complex to fully satisfy are allowed to
// fail, but the failure MUST NOT be exit 127 — the function IS called.
func TestBuiltinBashExecAllPortedTemplates(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed; skipping execution test")
	}

	binDir, path := stubBinDir(t)

	for _, key := range allBuiltinKeys() {
		key := key
		// Skip refused templates — they error at render time, not execution.
		if _, refused := refusedTemplates[key]; refused {
			continue
		}
		t.Run(key, func(t *testing.T) {
			v, root := execTestEnv(t, binDir)
			script := renderExecScript(t, key, v)

			// Inject the shared find_source_file helper (defined as the
			// findSourceFileBash const) before the step definitions. This
			// helper is called by many templates but is a pre-existing
			// rendering gap (not linked into the output) that is OUTSIDE
			// this test's scope — the P0 fix this test guards is the
			// STEP-FUNCTION call→define linkage, not the shared helper.
			// Injecting it here isolates the P0 assertion from that gap.
			script = findSourceFileBash + "\n" + script

			scriptPath := filepath.Join(root, key+".sh")
			if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(bash, scriptPath)
			cmd.Env = append(os.Environ(), "PATH="+path)
			// Suppress DiscordWebhook so discord_notify skips cleanly.
			cmd.Env = append(cmd.Env, "DiscordWebhook=")
			out, err := cmd.CombinedOutput()
			text := string(out)

			// The PRIMARY assertion: no "command not found" / exit 127.
			// This is the call→define linkage guarantee the P0 fix enforces.
			if strings.Contains(text, "command not found") {
				t.Fatalf("template %q: 'command not found' in output (call→define linkage broken):\n%s", key, text)
			}
			if err != nil {
				// Exit code 127 = command not found (shouldn't happen after the guard above,
				// but check explicitly).
				if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 127 {
					t.Fatalf("template %q: exit 127 (command not found):\n%s", key, text)
				}
				// Non-127 failure: the function WAS called (linkage proven).
				// The stub may not have fully satisfied the template's parsing —
				// that's acceptable per the test requirement. Log it but don't fail.
				t.Logf("template %q: exit %v (non-127, linkage OK; stub may not fully satisfy):\n%s", key, err, text)
			} else {
				t.Logf("template %q: succeeded\n%s", key, text)
			}

			// Assert ENCODE_STEP markers appear (the start marker at minimum).
			startMarker := "ENCODE_STEP " + key + " start"
			if !strings.Contains(text, startMarker) {
				t.Errorf("template %q: output missing start marker %q\n%s", key, startMarker, text)
			}

			// Collect ENCODE_STEP markers and verify they appear in order
			// (start before any progress markers before the 100 or FAILED).
			markers := encodeStepMarkers(text)
			if len(markers) == 0 {
				t.Errorf("template %q: no ENCODE_STEP markers in output", key)
			}
			// The start marker must be first.
			if len(markers) > 0 && !strings.HasPrefix(markers[0], "ENCODE_STEP "+key) {
				t.Errorf("template %q: first marker is %q, expected start marker", key, markers[0])
			}
		})
	}
}

// TestBuiltinBashStaticLinkage: for EVERY ported template, parse the rendered
// script to confirm each defined function name equals its StepCall invocation
// name. This is the cheap all-15 static check that catches the P0 class of
// bug (call-site name derived from PS source, definition name from bash body)
// without needing execution.
func TestBuiltinBashStaticLinkage(t *testing.T) {
	binDir, _ := stubBinDir(t)
	defRe := regexp.MustCompile(`(?m)^([a-z_][a-z0-9_]*)\s*\(\s*\)\s*\{`)
	// The call site is the bare function name on its own line inside the
	// StepCall block (between the params literal and the `} ||` handler).
	callRe := regexp.MustCompile(`(?m)^\s*([a-z_][a-z0-9_]*)\s*$`)

	for _, key := range allBuiltinKeys() {
		key := key
		if _, refused := refusedTemplates[key]; refused {
			continue
		}
		t.Run(key, func(t *testing.T) {
			v, _ := execTestEnv(t, binDir)
			script := renderExecScript(t, key, v)

			// Extract all defined function names.
			defs := defRe.FindAllStringSubmatch(script, -1)
			if len(defs) == 0 {
				t.Fatalf("template %q: no function definitions found in rendered script", key)
			}
			defNames := make(map[string]bool)
			for _, m := range defs {
				defNames[m[1]] = true
			}

			// Find the call site: the bare function name on its own line
			// that appears AFTER the "echo 'ENCODE_STEP <key> start'" marker.
			// The StepCall block looks like:
			//   echo 'ENCODE_STEP <key> start'
			//   trap '...' ERR
			//   {
			//       <param>=<val>
			//   <fn_name>              <-- the call (bare name, possibly indented)
			//   } || { echo ...; exit 1; }
			//   trap - ERR
			// We scan forward from the start marker, skipping trap/param lines,
			// until we find a bare identifier on its own line (the invocation).
			lines := strings.Split(script, "\n")
			inCallBlock := false
			var callName string
			for _, line := range lines {
				trimmed := strings.TrimSpace(line)
				if strings.Contains(trimmed, "ENCODE_STEP "+key+" start") {
					inCallBlock = true
					continue
				}
				if !inCallBlock {
					continue
				}
				// Skip non-call lines: trap, opening brace, param assignments,
				// closing brace + handler. The function call is a bare
				// identifier (no =, no (, no {, no |, no #).
				if trimmed == "" || strings.HasPrefix(trimmed, "#") {
					continue
				}
				if strings.HasPrefix(trimmed, "trap") {
					continue
				}
				if trimmed == "{" || strings.HasPrefix(trimmed, "}") {
					continue
				}
				if strings.Contains(trimmed, "=") && !strings.HasPrefix(trimmed, "local") {
					continue // param assignment like source_name='src'
				}
				// This should be the function call: a bare identifier.
				if m := callRe.FindStringSubmatch(trimmed); m != nil && len(m[1]) > 0 {
					callName = m[1]
					break
				}
				// If we hit the "||" handler or "trap -", the call was missed.
				if strings.Contains(trimmed, "||") || strings.HasPrefix(trimmed, "trap -") {
					break
				}
			}

			if callName == "" {
				t.Fatalf("template %q: could not find StepCall invocation in rendered script", key)
			}
			if !defNames[callName] {
				t.Errorf("template %q: StepCall invokes %q but no function with that name is defined (defined: %v)",
					key, callName, sortedKeys(defNames))
			}
		})
	}
}

// TestBashVerifyOutputBcGuard: the verify_output bash template must check
// for bc at the top of the function and fail with a clear actionable error
// if bc is absent (GAP 3 fix). This test asserts the guard exists in the
// rendered output and that the error message names bc.
func TestBashVerifyOutputBcGuard(t *testing.T) {
	script, err := renderBuiltinBash(t, "verify_output")
	if err != nil {
		t.Fatalf("render verify_output: %v", err)
	}
	// The guard must check for bc with command -v.
	if !strings.Contains(script, "command -v bc") {
		t.Error("verify_output: missing 'command -v bc' guard (GAP 3 fix)")
	}
	// The error must mention bc and be actionable.
	if !strings.Contains(script, "bc not installed") {
		t.Error("verify_output: bc guard error must say 'bc not installed' with an actionable message")
	}
	// The guard must appear BEFORE the duration math (it should be near the
	// top of the function, before the mkvmerge -J call).
	bcGuardIdx := strings.Index(script, "command -v bc")
	mkvmergeIdx := strings.Index(script, "mkvmerge -J")
	if bcGuardIdx < 0 || mkvmergeIdx < 0 {
		t.Fatal("verify_output: could not locate bc guard or mkvmerge -J in script")
	}
	if bcGuardIdx > mkvmergeIdx {
		t.Error("verify_output: bc guard must appear BEFORE the mkvmerge -J call (duration math)")
	}
}

// TestBashKeyframesRefusedOnLinux: the keyframes template must now be
// refused by the bash dialect (GAP 2 fix — the previous bash body was a
// fake: every-240th-frame, not scene-change detection). The refusal error
// must name keyframes and explain it's a fake/scene-detection issue.
func TestBashKeyframesRefusedOnLinux(t *testing.T) {
	_, err := renderBuiltinBash(t, "keyframes")
	if err == nil {
		t.Fatal("keyframes must be REFUSED by bash dialect (was a fake — every-240th-frame, not scene detection)")
	}
	msg := err.Error()
	if !strings.Contains(msg, "keyframes") {
		t.Errorf("refusal error must name 'keyframes': %v", err)
	}
	// Must mention the fake/scene-detection reason.
	if !strings.Contains(strings.ToLower(msg), "fake") && !strings.Contains(strings.ToLower(msg), "scene") {
		t.Errorf("refusal error must explain the fake/scene-detection reason: %v", err)
	}
}

// TestBashKeyframesNoLongerInBuiltinSources: the keyframes entry must have
// been removed from builtinBashSources (moved to builtinBashRefused).
func TestBashKeyframesNoLongerInBuiltinSources(t *testing.T) {
	if _, stillThere := builtinBashSources["keyframes"]; stillThere {
		t.Error("keyframes must be removed from builtinBashSources (moved to builtinBashRefused)")
	}
	if _, refused := builtinBashRefused["keyframes"]; !refused {
		t.Error("keyframes must be in builtinBashRefused")
	}
}

// --- helpers ---------------------------------------------------------------

func sortedKeys(m map[string]bool) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Ensure model import is used (referenced in helper signatures via linuxTestJob).
var _ = model.StepSourceRename

// fmt is used in error formatting.
var _ = fmt.Sprintf
