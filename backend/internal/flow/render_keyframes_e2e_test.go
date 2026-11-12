//go:build linux

package flow

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// P7 stub-exec end-to-end tests for the keyframes builtin. VapourSynth is
// NOT a test-host dependency: the bash suite stubs vspipe with a contract-
// validating fake that writes a REAL-FORMAT xvid first-pass log (the exact
// artifact shape xvidcore's plugin_2pass1.c emits: two header comment
// lines, a blank line, then one "type quant kblks mblks ublks length
// hlength" line per frame). The PowerShell suite runs for real under pwsh
// when installed (it is on this host) with SCXvid.exe/ffmpeg.exe stubs
// writing the SAME fixture — proving the Linux artifact is byte-identical
// to what the Windows path produces for the same log content.
//
// Parity note (verified against upstream sources): there is no log PARSER
// on either side — SCXvid.exe (soyokaze/SCXvid-standalone) writes the xvid
// first-pass log directly as the Keyframes.txt artifact, and
// vapoursynth-scxvid's `log` output is the same xvid_plugin_2pass1 format
// with identical encoder settings. Byte parity of the artifact IS the
// contract; the stubs enforce it on the same fixture.

// kfXvidLogFixture: a realistic xvid first-pass stats log (16 frames,
// scene changes at frames 0, 7 and 12 → 'i' lines; the format Aegisub
// loads as keyframes). Both dialects' stubs write this exact content.
const kfXvidLogFixture = "# XviD 2pass stat file (core version 1.3.7)\n" +
	"# Please do not modify this file\n" +
	"\n" +
	"i 2 920 0 0 31209 3519\n" +
	"b 2 0 410 510 3266 512\n" +
	"b 2 0 395 525 3105 498\n" +
	"p 2 0 790 130 6497 999\n" +
	"b 2 0 402 518 3180 501\n" +
	"b 2 0 388 532 3054 489\n" +
	"p 2 0 812 108 6720 1022\n" +
	"i 2 905 0 0 29874 3401\n" +
	"b 2 0 415 505 3310 517\n" +
	"p 2 0 786 134 6455 991\n" +
	"b 2 0 399 521 3142 503\n" +
	"p 2 0 803 117 6631 1010\n" +
	"i 2 911 0 0 30552 3455\n" +
	"b 2 0 408 512 3225 509\n" +
	"p 2 0 795 125 6540 1004\n" +
	"p 2 0 781 139 6412 985\n"

// kfXvidLogHeaderOnly: the ZERO-FRAME failure edge. xvidcore writes the
// two header comment lines at plugin CREATE (plugin_2pass1.c
// rc_2pass1_create), BEFORE any frame is processed — so a header-only log
// means the run died before frame 1, NOT a "perfectly static clip" (a
// static clip still yields one b/p stats line per frame; see
// kfXvidLogStaticClip). The step must fail closed on this content.
const kfXvidLogHeaderOnly = "# XviD 2pass stat file (core version 1.3.7)\n" +
	"# Please do not modify this file\n" +
	"\n"

// kfVspipeStub validates the invocation shape AND the generated .vpy
// structure (P5 lesson: stubs must validate structure, not just exist),
// then writes the fixture log to ENCODE_KF_LOG. Sidecars (resolved via
// dirname $0): vspipe_exit (forced exit code), vspipe_plugin_error (emit
// a missing-scxvid traceback with the forced failure), vspipe_no_log
// (exit 0 without writing the log), kf_log_payload (alternate log bytes).
const kfVspipeStub = `#!/usr/bin/env bash
dir="$(cd "$(dirname "$0")" && pwd)"
printf '%s\n' "$@" > "$dir/vspipe_args.txt"
if [ -f "$dir/vspipe_exit" ] && [ ! -f "$dir/vspipe_progress" ]; then
  rc=$(cat "$dir/vspipe_exit")
  if [ -f "$dir/vspipe_plugin_error" ]; then
    printf "Failed to evaluate the script: Python exception: No module named 'scxvid'\n" >&2
  else
    echo "vspipe stub: forced failure (rc=$rc)" >&2
  fi
  exit "$rc"
fi
# Invocation contract: vspipe -p -c y4m <script> - (null sink via stdout;
# -p streams live Frame: N/M progress for the stall watchdog).
if [ "$1" != "-p" ] || [ "$2" != "-c" ] || [ "$3" != "y4m" ] || [ "$5" != "-" ]; then
  echo "vspipe stub: expected '-p -c y4m <script> -', got: $*" >&2
  exit 1
fi
script="$4"
if [ -f "$dir/vspipe_exit" ] && [ -f "$dir/vspipe_progress" ]; then
  # Combined mode: stream progress, THEN fail (rc capture must survive
  # the live pipeline even when output preceded the failure).
  for ((i = 1; i <= 10; i++)); do
    printf 'Frame: %d/10 (10.00 fps)\r' "$i" >&2
    sleep 0.05
  done
  echo "vspipe stub: simulated crash after progress" >&2
  exit "$(cat "$dir/vspipe_exit")"
fi
if [ ! -f "$script" ]; then
  echo "vspipe stub: script file not found: $script" >&2
  exit 1
fi
# Generated .vpy structural contract (loader, format conversion, scxvid
# call with the parity threading decision, env-var paths, set_output).
for pat in \
  'import scxvid' \
  'os.environ["ENCODE_KF_SOURCE"]' \
  'os.environ["ENCODE_KF_LOG"]' \
  'core.lsmas.LWLibavSource(source=source)' \
  'core.ffms2.Source(source=source)' \
  'core.resize.Bicubic(clip, width=640, height=360, format=vs.YUV420P8)' \
  'scxvid.Scxvid(clip, log=log, use_slices=True)' \
  'clip.set_output()'
do
  if ! grep -qF "$pat" "$script"; then
    echo "vspipe stub: .vpy missing contract element: $pat" >&2
    exit 1
  fi
done
if [ -z "${ENCODE_KF_SOURCE:-}" ] || [ ! -f "$ENCODE_KF_SOURCE" ]; then
  echo "vspipe stub: ENCODE_KF_SOURCE must point at an existing file" >&2
  exit 1
fi
case "$ENCODE_KF_SOURCE" in
  *.mkv) ;;
  *) echo "vspipe stub: ENCODE_KF_SOURCE must be the release MKV, got: $ENCODE_KF_SOURCE" >&2; exit 1 ;;
esac
if [ -z "${ENCODE_KF_LOG:-}" ]; then
  echo "vspipe stub: ENCODE_KF_LOG not set" >&2
  exit 1
fi
if [ ! -f "$dir/vspipe_no_log" ]; then
  if [ -f "$dir/kf_log_payload" ]; then
    cat "$dir/kf_log_payload" > "$ENCODE_KF_LOG"
  else
    cat "$dir/kf_log_fixture" > "$ENCODE_KF_LOG"
  fi
fi
if [ -f "$dir/vspipe_progress" ]; then
  # Live-progress mode: CR-terminated Frame: N/M lines exactly like real
  # vspipe -p (fprintf(stderr, "Frame: %d/%d (%.2f fps)\r", ...)).
  # First 5 frames paced 0.25s apart (liveness oracle); the rest fast
  # (throttle oracle: 100 frame lines must yield far fewer markers).
  for ((i = 1; i <= 100; i++)); do
    if (( i <= 5 )); then sleep 0.25; fi
    printf 'Frame: %d/100 (10.00 fps)\r' "$i" >&2
  done
  printf 'Output 100 frames in 1.30 seconds (76.92 fps)\n' >&2
else
  echo "Frame: 16/16 (stub)" >&2
fi
exit 0
`

// kfFfmpegExeStub (Windows side): the PS template's ffmpeg call writes a
// temp y4m as its LAST argument. Create it with a minimal y4m header.
const kfFfmpegExeStub = `#!/usr/bin/env bash
out=""
for a in "$@"; do out="$a"; done
printf 'YUV4MPEG2 W640 H360 F24:1 Ip A0:0\nFRAME\n' > "$out"
echo "ffmpeg stub: y4m written to $out"
`

// kfScxvidExeStub (Windows side): real contract — the output log file is
// the ONLY argument, y4m arrives on STDIN. Writes the SAME fixture the
// bash-side vspipe stub writes (byte-parity oracle). Sidecars:
// scxvid_exit (forced exit code), kf_log_payload (alternate log bytes),
// scxvid_slow_lines (paced stderr lines — the live-read oracle).
const kfScxvidExeStub = `#!/usr/bin/env bash
dir="$(cd "$(dirname "$0")" && pwd)"
cat > /dev/null
printf '%s\n' "$@" > "$dir/scxvid_args.txt"
if [ -f "$dir/scxvid_exit" ]; then exit "$(cat "$dir/scxvid_exit")"; fi
if [ -f "$dir/scxvid_slow_lines" ]; then
  # Paced stderr lines with a 0.4s tail sleep: live line reads echo each
  # line as it arrives; ReadToEndAsync buffers all of them until exit.
  echo "SCXvid stub: line one" >&2; sleep 0.4
  echo "SCXvid stub: line two" >&2; sleep 0.4
  echo "SCXvid stub: line three" >&2; sleep 0.4
fi
if [ -f "$dir/kf_log_payload" ]; then
  cat "$dir/kf_log_payload" > "$1"
else
  cat "$dir/kf_log_fixture" > "$1"
fi
echo "SCXvid stub: log written to $1"
`

// kfEnv carries the temp tree for one keyframes e2e run.
type kfEnv struct {
	v         Vars
	epDir     string
	relFolder string
	binDir    string
	path      string
	mkvPath   string
	kfPath    string
}

// kfSetup builds the episode/release tree with a release MKV and the
// dialect-appropriate stubs. Bash reuses the generic stub set (vspipe
// replaced by the contract-validating stub); PS gets ffmpeg.exe +
// SCXvid.exe stubs in a fresh bin dir.
func kfSetup(t *testing.T, ps bool) kfEnv {
	t.Helper()
	root := t.TempDir()
	scriptsDir := filepath.Join(root, "scripts")
	releaseDir := filepath.Join(root, "release")
	epDir := filepath.Join(scriptsDir, "Test Series", "Ep 01")
	relFolder := filepath.Join(releaseDir, "[TestGroup] Test Series - Raws [1080p]")
	for _, d := range []string{epDir, relFolder} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(epDir, "src.m2ts"), []byte("fake-source"), 0o644); err != nil {
		t.Fatal(err)
	}
	mkvPath := filepath.Join(relFolder, "Test Series - 01 [1080p].mkv")
	if err := os.WriteFile(mkvPath, []byte("fake-release-mkv"), 0o644); err != nil {
		t.Fatal(err)
	}

	var binDir, path string
	if ps {
		binDir = t.TempDir()
		for name, body := range map[string]string{
			"ffmpeg.exe": kfFfmpegExeStub,
			"SCXvid.exe": kfScxvidExeStub,
		} {
			if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		path = binDir + ":/usr/bin:/bin:/usr/local/bin"
	} else {
		binDir, path = stubBinDir(t)
		if err := os.WriteFile(filepath.Join(binDir, "vspipe"), []byte(kfVspipeStub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(binDir, "kf_log_fixture"), []byte(kfXvidLogFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	return kfEnv{
		v: Vars{
			BinDir:     binDir,
			ScriptsDir: scriptsDir,
			ReleaseDir: releaseDir,
			Group:      "TestGroup",
			Tag:        "1080p",
		},
		epDir:     epDir,
		relFolder: relFolder,
		binDir:    binDir,
		path:      path,
		mkvPath:   mkvPath,
		kfPath:    filepath.Join(relFolder, "Test Series - 01 Keyframes.txt"),
	}
}

// kfRender renders the single-step keyframes flow for the given dialect.
func kfRender(t *testing.T, e kfEnv, ps bool) string {
	t.Helper()
	d := Dialect(BashDialect{})
	if ps {
		d = PowerShellDialect{}
	}
	script, err := RenderDialect(singleStepFlow("keyframes"), linuxTestJob(), e.v, BuiltinResolver(), d)
	if err != nil {
		t.Fatalf("render keyframes: %v", err)
	}
	return script
}

// kfRunBash executes the rendered bash script under the stub PATH.
func kfRunBash(t *testing.T, e kfEnv) (string, error) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed; skipping keyframes bash e2e")
	}
	scriptPath := filepath.Join(e.v.ScriptsDir, "job.sh")
	if err := os.WriteFile(scriptPath, []byte(kfRender(t, e, false)), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+e.path)
	out, runErr := cmd.CombinedOutput()
	return string(out), runErr
}

// kfRunPS executes the rendered PS script under pwsh with the real
// EncodeLib.ps1 dot-sourced (skips when pwsh is absent).
func kfRunPS(t *testing.T, e kfEnv) (string, error) {
	t.Helper()
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not installed; skipping keyframes PowerShell e2e")
	}
	libPath, err := filepath.Abs("../../../powershell/EncodeLib.ps1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(libPath); err != nil {
		t.Fatalf("EncodeLib.ps1 not found at %s", libPath)
	}
	scriptPath := filepath.Join(e.v.ScriptsDir, "job.ps1")
	if err := os.WriteFile(scriptPath, []byte(kfRender(t, e, true)), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(pwsh, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", scriptPath, "-LibPath", libPath)
	cmd.Env = append(os.Environ(), "PATH="+e.path, "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1")
	out, runErr := cmd.CombinedOutput()
	return string(out), runErr
}

// kfSidecar writes a stub sidecar file into the bin dir.
func kfSidecar(t *testing.T, e kfEnv, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.binDir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// kfAssertNoTempLeftovers: the unique work dir and log temp must be gone
// after the step (success or failure) — no .keyframes.* / .scxvid.* debris.
func kfAssertNoTempLeftovers(t *testing.T, e kfEnv) {
	t.Helper()
	for _, dir := range []string{e.epDir, e.relFolder} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, ent := range entries {
			if strings.HasPrefix(ent.Name(), ".keyframes.") || strings.HasPrefix(ent.Name(), ".scxvid.") {
				t.Errorf("temp leftover in %s: %s", dir, ent.Name())
			}
		}
	}
}

// TestKeyframesBashE2EWritesXvidLog: the bash port runs vspipe over the
// generated .vpy (contract validated by the stub) and publishes the xvid
// first-pass log byte-for-byte as the Keyframes.txt artifact, with the
// Windows marker sequence and no temp debris.
func TestKeyframesBashE2EWritesXvidLog(t *testing.T) {
	e := kfSetup(t, false)
	text, err := kfRunBash(t, e)
	t.Logf("output:\n%s", text)
	if err != nil {
		t.Fatalf("keyframes bash run failed: %v", err)
	}
	for _, want := range []string{
		"ENCODE_STEP keyframes start",
		"ENCODE_STEP keyframes 0",
		"[keyframes] vspipe -> scxvid",
		"[vspipe] Frame: 16/16 (stub)",
		"ENCODE_STEP keyframes 100",
		"ENCODE_JOB_DONE",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q", want)
		}
	}
	if strings.Contains(text, "ENCODE_STEP_FAILED") {
		t.Errorf("unexpected failure marker:\n%s", text)
	}
	got, err := os.ReadFile(e.kfPath)
	if err != nil {
		t.Fatalf("Keyframes.txt not created: %v", err)
	}
	if !bytes.Equal(got, []byte(kfXvidLogFixture)) {
		t.Errorf("Keyframes.txt bytes differ from the xvid log fixture\ngot:\n%s\nwant:\n%s", got, kfXvidLogFixture)
	}
	// The stub recorded the invocation: -p -c y4m <script> -.
	args, err := os.ReadFile(filepath.Join(e.binDir, "vspipe_args.txt"))
	if err != nil {
		t.Fatalf("vspipe was not invoked: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(args)), "\n")
	if len(lines) != 5 || lines[0] != "-p" || lines[1] != "-c" || lines[2] != "y4m" || lines[4] != "-" {
		t.Errorf("vspipe invocation shape wrong: %q", lines)
	}
	kfAssertNoTempLeftovers(t, e)
}

// TestKeyframesPSE2EWritesXvidLog: the WINDOWS template under pwsh with
// stubbed ffmpeg.exe/SCXvid.exe produces the same fixture bytes — the
// oracle half of the cross-dialect parity proof.
func TestKeyframesPSE2EWritesXvidLog(t *testing.T) {
	e := kfSetup(t, true)
	text, err := kfRunPS(t, e)
	t.Logf("output:\n%s", text)
	if err != nil {
		t.Fatalf("keyframes PS run failed: %v", err)
	}
	for _, want := range []string{
		"ENCODE_STEP keyframes 0",
		"ENCODE_STEP keyframes 100",
		"ENCODE_JOB_DONE",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q", want)
		}
	}
	got, err := os.ReadFile(e.kfPath)
	if err != nil {
		t.Fatalf("Keyframes.txt not created: %v", err)
	}
	if !bytes.Equal(got, []byte(kfXvidLogFixture)) {
		t.Errorf("Windows Keyframes.txt bytes differ from the xvid log fixture\ngot:\n%s", got)
	}
}

// TestKeyframesE2ECrossDialectByteParity: run BOTH dialects on the same
// fixture and compare the artifacts byte-for-byte. This is the P7 parity
// contract: the Linux vapoursynth-scxvid log and the Windows SCXvid log
// are the same artifact format, so equal log content must yield equal
// Keyframes.txt bytes. (PS half skips without pwsh; the bash-vs-fixture
// assertion still runs.)
func TestKeyframesE2ECrossDialectByteParity(t *testing.T) {
	bashEnv := kfSetup(t, false)
	if _, err := kfRunBash(t, bashEnv); err != nil {
		t.Fatalf("bash run failed: %v", err)
	}
	bashBytes, err := os.ReadFile(bashEnv.kfPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bashBytes, []byte(kfXvidLogFixture)) {
		t.Fatal("bash artifact != fixture (stub or publish path broken)")
	}

	psEnv := kfSetup(t, true)
	if _, err := exec.LookPath("pwsh"); err != nil {
		t.Log("pwsh not installed; bash-vs-fixture parity verified, PS half skipped")
		return
	}
	if _, err := kfRunPS(t, psEnv); err != nil {
		t.Fatalf("PS run failed: %v", err)
	}
	psBytes, err := os.ReadFile(psEnv.kfPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bashBytes, psBytes) {
		t.Errorf("cross-dialect byte parity broken\nbash (%d bytes):\n%s\nPS (%d bytes):\n%s",
			len(bashBytes), bashBytes, len(psBytes), psBytes)
	}
}

// TestKeyframesBashE2ESkipWhenFresh: a Keyframes.txt newer than the MKV
// skips detection entirely (Windows semantics) — vspipe is never invoked.
func TestKeyframesBashE2ESkipWhenFresh(t *testing.T) {
	e := kfSetup(t, false)
	if err := os.WriteFile(e.kfPath, []byte("EXISTING-KEYFRAMES"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Make the kf strictly newer than the MKV.
	future := time.Now().Add(1 * time.Hour)
	if err := os.Chtimes(e.kfPath, future, future); err != nil {
		t.Fatal(err)
	}
	text, err := kfRunBash(t, e)
	t.Logf("output:\n%s", text)
	if err != nil {
		t.Fatalf("skip run failed: %v", err)
	}
	if !strings.Contains(text, "keyframes file exists and is newer than the MKV, skipping") {
		t.Errorf("missing skip message:\n%s", text)
	}
	if !strings.Contains(text, "ENCODE_STEP keyframes 100") {
		t.Errorf("missing completion marker:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(e.binDir, "vspipe_args.txt")); !os.IsNotExist(err) {
		t.Error("vspipe must NOT be invoked on the fresh-skip path")
	}
	got, _ := os.ReadFile(e.kfPath)
	if string(got) != "EXISTING-KEYFRAMES" {
		t.Errorf("fresh keyframes file was modified: %q", got)
	}
}

// TestKeyframesBashE2ERegeneratesStale: a Keyframes.txt older than the MKV
// is regenerated (Windows semantics).
func TestKeyframesBashE2ERegeneratesStale(t *testing.T) {
	e := kfSetup(t, false)
	if err := os.WriteFile(e.kfPath, []byte("STALE-KEYFRAMES"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Age the kf behind the MKV.
	past := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(e.kfPath, past, past); err != nil {
		t.Fatal(err)
	}
	text, err := kfRunBash(t, e)
	t.Logf("output:\n%s", text)
	if err != nil {
		t.Fatalf("stale run failed: %v", err)
	}
	if !strings.Contains(text, "keyframes file is stale (older than MKV), regenerating") {
		t.Errorf("missing stale message:\n%s", text)
	}
	got, err := os.ReadFile(e.kfPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(kfXvidLogFixture)) {
		t.Errorf("stale keyframes not regenerated with the fixture:\n%s", got)
	}
	kfAssertNoTempLeftovers(t, e)
}

// TestKeyframesBashE2EMissingVspipe: with no vspipe anywhere on PATH and
// an empty BinDir, the step fails with an actionable error naming
// VapourSynth and the pip package. Minimal PATH so a host-installed
// vspipe can never satisfy the lookup (P5 minbin pattern).
func TestKeyframesBashE2EMissingVspipe(t *testing.T) {
	e := kfSetup(t, false)
	root := t.TempDir()
	minDir := filepath.Join(root, "minbin")
	if err := os.MkdirAll(minDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"bash", "cat", "dirname", "mktemp", "rm", "mv", "grep", "find", "sort", "head"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("coreutils tool %q not available: %v", tool, err)
		}
		if err := os.Symlink(p, filepath.Join(minDir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	e.path = minDir
	e.v.BinDir = filepath.Join(root, "empty-bin")
	if err := os.MkdirAll(e.v.BinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	text, err := kfRunBash(t, e)
	t.Logf("output:\n%s", text)
	if err == nil {
		t.Fatal("expected failure with vspipe missing")
	}
	for _, want := range []string{
		"VapourSynth (vspipe) not found",
		"pip install vapoursynth-scxvid",
		"ENCODE_STEP_FAILED keyframes",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing-vspipe output missing %q", want)
		}
	}
	if _, err := os.Stat(e.kfPath); !os.IsNotExist(err) {
		t.Error("no Keyframes.txt may be created when vspipe is missing")
	}
}

// TestKeyframesBashE2EMissingPlugin: vspipe present but the scxvid plugin
// absent (Python ImportError) — the failure must name the pip package so
// an operator can fix the node without reading the traceback.
func TestKeyframesBashE2EMissingPlugin(t *testing.T) {
	e := kfSetup(t, false)
	kfSidecar(t, e, "vspipe_exit", "1")
	kfSidecar(t, e, "vspipe_plugin_error", "1")
	text, err := kfRunBash(t, e)
	t.Logf("output:\n%s", text)
	if err == nil {
		t.Fatal("expected failure with the scxvid plugin missing")
	}
	for _, want := range []string{
		"No module named 'scxvid'",
		"pip install vapoursynth-scxvid",
		"vspipe exited with code 1",
		"ENCODE_STEP_FAILED keyframes",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing-plugin output missing %q", want)
		}
	}
	if _, err := os.Stat(e.kfPath); !os.IsNotExist(err) {
		t.Error("no Keyframes.txt may be created when the plugin is missing")
	}
	kfAssertNoTempLeftovers(t, e)
}

// TestKeyframesBashE2EVspipeFails: a generic vspipe failure propagates the
// exit code, emits the failure marker and leaves no partial artifact or
// temp debris (the atomic-publish guarantee).
func TestKeyframesBashE2EVspipeFails(t *testing.T) {
	e := kfSetup(t, false)
	kfSidecar(t, e, "vspipe_exit", "3")
	text, err := kfRunBash(t, e)
	t.Logf("output:\n%s", text)
	if err == nil {
		t.Fatal("expected failure when vspipe exits 3")
	}
	for _, want := range []string{
		"vspipe exited with code 3",
		"ENCODE_STEP_FAILED keyframes",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("vspipe-failure output missing %q", want)
		}
	}
	if _, err := os.Stat(e.kfPath); !os.IsNotExist(err) {
		t.Error("no Keyframes.txt may survive a failed run (partial-file skip hazard)")
	}
	kfAssertNoTempLeftovers(t, e)
}

// TestKeyframesBashE2ENoLogWritten: vspipe exits 0 but the plugin never
// wrote the log — the step must fail loudly (mirrors the Windows
// "SCXvid finished but ... was not created" guard).
func TestKeyframesBashE2ENoLogWritten(t *testing.T) {
	e := kfSetup(t, false)
	kfSidecar(t, e, "vspipe_no_log", "1")
	text, err := kfRunBash(t, e)
	t.Logf("output:\n%s", text)
	if err == nil {
		t.Fatal("expected failure when no log was written")
	}
	for _, want := range []string{
		"scxvid finished but the keyframes log was not created",
		"ENCODE_STEP_FAILED keyframes",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no-log output missing %q", want)
		}
	}
	if _, err := os.Stat(e.kfPath); !os.IsNotExist(err) {
		t.Error("no Keyframes.txt may be created without a log")
	}
	kfAssertNoTempLeftovers(t, e)
}

// TestKeyframesBashE2EMissingReleaseMkv: the step operates on the RELEASE
// MKV (Windows parity) — without it, fail with the same text as the PS
// template before any detection runs.
func TestKeyframesBashE2EMissingReleaseMkv(t *testing.T) {
	e := kfSetup(t, false)
	if err := os.Remove(e.mkvPath); err != nil {
		t.Fatal(err)
	}
	text, err := kfRunBash(t, e)
	t.Logf("output:\n%s", text)
	if err == nil {
		t.Fatal("expected failure without the release MKV")
	}
	for _, want := range []string{
		"release MKV not found",
		"ENCODE_STEP_FAILED keyframes",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing-mkv output missing %q", want)
		}
	}
	if _, err := os.Stat(filepath.Join(e.binDir, "vspipe_args.txt")); !os.IsNotExist(err) {
		t.Error("vspipe must NOT run without the release MKV")
	}
}

// TestKeyframesBashE2EHeaderOnlyLog: INVERTED by the P7 quality review —
// a header-only log is the ZERO-FRAME failure edge (the header is written
// at plugin CREATE, before any frame; see kfXvidLogHeaderOnly). Coverage
// moved to TestKeyframesE2EInvalidLogFailsClosed (both dialects) in
// render_keyframes_guards_test.go; the valid zero-scene-change case is
// TestKeyframesE2EStaticClipLogPasses.
