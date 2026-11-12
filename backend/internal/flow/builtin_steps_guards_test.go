//go:build linux

package flow

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// Final integration-review regressions (W1–W6): fail-closed guards for the
// bash builtins under the errexit-dead StepCall wrapper.
//
// Background (P7 review Q1, re-confirmed by the integration reviewer's
// wrapper probes): the StepCall wrapper `{ fn; } || { ...; }` puts the step
// function in a conditional context, which DISABLES set -e and the ERR trap
// for every command inside it. Any unchecked critical command (rm/mv/cp/
// mkdir/publish-redirect) can therefore fail silently while the step reports
// ENCODE_STEP 100 + ENCODE_JOB_DONE. keyframes was fixed in P7; these tests
// pin the same fail-closed contract for source_rename, crc32_rename,
// hdr_probe, audio_lang, release_copy and chapter_extract, plus the crc32
// uppercase flow-param parity with the PowerShell template.

// guardEnv carries the temp tree for one builtin-guard run. Layout mirrors
// kfSetup so OutputName/ReleaseFolder render to the same canonical names.
type guardEnv struct {
	v         Vars
	epDir     string
	relFolder string
	binDir    string
	path      string
}

func guardSetup(t *testing.T) guardEnv {
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
	binDir, path := stubBinDir(t)
	return guardEnv{
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
	}
}

// guardWriteStub adds (or replaces) a stub executable in the env's bin dir.
func guardWriteStub(t *testing.T, e guardEnv, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.binDir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// guardRender renders a single-step flow for the given builtin with explicit
// step params through the bash dialect.
func guardRender(t *testing.T, e guardEnv, key string, params map[string]string) string {
	t.Helper()
	f := &model.Flow{
		Name:  "guard-" + key,
		Steps: []model.Step{{Type: model.StepType(key), Params: params}},
	}
	script, err := RenderDialect(f, linuxTestJob(), e.v, BuiltinResolver(), BashDialect{})
	if err != nil {
		t.Fatalf("render %s: %v", key, err)
	}
	return script
}

// guardRunBash executes the rendered script under the stub PATH.
func guardRunBash(t *testing.T, e guardEnv, script string) (string, error) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed; skipping builtin guard e2e")
	}
	scriptPath := filepath.Join(e.v.ScriptsDir, "job.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+e.path)
	out, runErr := cmd.CombinedOutput()
	return string(out), runErr
}

// guardAssertFailedClosed pins the failure contract: nonzero exit, failure
// marker, NO false completion marker and no JOB_DONE.
func guardAssertFailedClosed(t *testing.T, step, text string, runErr error) {
	t.Helper()
	if runErr == nil {
		t.Errorf("expected nonzero exit, got success:\n%s", text)
	}
	if !strings.Contains(text, "ENCODE_STEP_FAILED "+step) {
		t.Errorf("missing ENCODE_STEP_FAILED %s marker:\n%s", step, text)
	}
	if strings.Contains(text, "ENCODE_STEP "+step+" 100") {
		t.Errorf("false completion marker emitted:\n%s", text)
	}
	if strings.Contains(text, "ENCODE_JOB_DONE") {
		t.Errorf("JOB_DONE emitted on a failed run:\n%s", text)
	}
}

// guardAssertSucceeded pins the happy path so a guard never over-rejects.
func guardAssertSucceeded(t *testing.T, step, text string, runErr error) {
	t.Helper()
	if runErr != nil {
		t.Fatalf("%s happy path failed: %v\n%s", step, runErr, text)
	}
	if !strings.Contains(text, "ENCODE_STEP "+step+" 100") {
		t.Errorf("missing completion marker:\n%s", text)
	}
	if !strings.Contains(text, "ENCODE_JOB_DONE") {
		t.Errorf("missing JOB_DONE:\n%s", text)
	}
	if strings.Contains(text, "ENCODE_STEP_FAILED") {
		t.Errorf("unexpected failure marker:\n%s", text)
	}
}

// guardReadOnlyEp makes the episode dir unwritable (permission-based failure
// injection, skipped under root where chmod is ineffective).
func guardReadOnlyEp(t *testing.T, e guardEnv) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based failure not reproducible")
	}
	if err := os.Chmod(e.epDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(e.epDir, 0o755) })
}

// --- W1: source_rename ------------------------------------------------------

// TestSourceRenameBashMvFailureFailsClosed: mv into a read-only episode dir
// fails; the old unchecked mv reported 100 + JOB_DONE with the source never
// renamed (reviewer probe P1).
func TestSourceRenameBashMvFailureFailsClosed(t *testing.T) {
	e := guardSetup(t)
	if err := os.WriteFile(filepath.Join(e.epDir, "raw.mkv"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	guardReadOnlyEp(t, e)
	text, err := guardRunBash(t, e, guardRender(t, e, "source_rename", map[string]string{"source_name": "src"}))
	t.Logf("output:\n%s", text)
	guardAssertFailedClosed(t, "source_rename", text, err)
	if _, statErr := os.Stat(filepath.Join(e.epDir, "src.mkv")); statErr == nil {
		t.Error("src.mkv must not exist after a failed rename")
	}
}

// TestSourceRenameBashHappyPath: the guard must not break a normal rename.
func TestSourceRenameBashHappyPath(t *testing.T) {
	e := guardSetup(t)
	if err := os.WriteFile(filepath.Join(e.epDir, "raw.mkv"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	text, err := guardRunBash(t, e, guardRender(t, e, "source_rename", map[string]string{"source_name": "src"}))
	t.Logf("output:\n%s", text)
	guardAssertSucceeded(t, "source_rename", text, err)
	if _, statErr := os.Stat(filepath.Join(e.epDir, "src.mkv")); statErr != nil {
		t.Errorf("renamed source missing: %v", statErr)
	}
}

// --- W2: crc32_rename -------------------------------------------------------

// crc32CanonicalContent is the standard CRC-32 check vector. NOTE: the PS
// template computes IEEE CRC-32 (C# reflected 0xEDB88320 → CBF43926 for this
// content) while the bash port uses POSIX cksum (a DIFFERENT algorithm —
// 377A6011 here; cross-dialect value parity is a separate follow-up). These
// tests therefore derive the expected tag from the SAME cksum binary the
// rendered script runs, so they pin param handling and guards regardless of
// the host's cksum implementation. The generic cksum stub is removed so the
// real coreutils cksum (PATH tail) runs.
const crc32CanonicalContent = "123456789"

func crc32Setup(t *testing.T) guardEnv {
	t.Helper()
	e := guardSetup(t)
	// Use the real cksum (PATH tail) for canonical checksum values.
	if err := os.Remove(filepath.Join(e.binDir, "cksum")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.epDir, "Test Series - 01 [1080p].mkv"), []byte(crc32CanonicalContent), 0o644); err != nil {
		t.Fatal(err)
	}
	return e
}

// crc32ExpectedHex runs the same cksum the rendered script will use and
// returns the formatted tag in upper and lower case.
func crc32ExpectedHex(t *testing.T, e guardEnv) (string, string) {
	t.Helper()
	mkv := filepath.Join(e.epDir, "Test Series - 01 [1080p].mkv")
	cmd := exec.Command("cksum", "--", mkv)
	cmd.Env = append(os.Environ(), "PATH="+e.path)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("reference cksum: %v", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		t.Fatalf("reference cksum output empty: %q", out)
	}
	var dec uint64
	if _, err := fmt.Sscanf(fields[0], "%d", &dec); err != nil {
		t.Fatalf("reference cksum field %q: %v", fields[0], err)
	}
	upper := fmt.Sprintf("%08X", dec)
	return upper, strings.ToLower(upper)
}

// TestCrc32RenameBashUppercaseParam: the flow param 'uppercase' must be
// honored (PS parity — builtin_steps_plugins.go reads $Params.uppercase).
// The old body's `local uppercase=true` SHADOWED the injected param, so
// uppercase=false still produced an uppercase tag (reviewer probe P2c).
func TestCrc32RenameBashUppercaseParam(t *testing.T) {
	cases := []struct {
		name      string
		params    map[string]string
		lowercase bool
	}{
		{"uppercase_false", map[string]string{"uppercase": "false"}, true},
		{"uppercase_true", map[string]string{"uppercase": "true"}, false},
		{"unset_defaults_upper", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := crc32Setup(t)
			upper, lower := crc32ExpectedHex(t, e)
			tag := upper
			if tc.lowercase {
				tag = lower
			}
			want := "Test Series - 01 [1080p] [" + tag + "].mkv"
			text, err := guardRunBash(t, e, guardRender(t, e, "crc32_rename", tc.params))
			t.Logf("output:\n%s", text)
			guardAssertSucceeded(t, "crc32_rename", text, err)
			if _, statErr := os.Stat(filepath.Join(e.epDir, want)); statErr != nil {
				entries, _ := os.ReadDir(e.epDir)
				t.Errorf("expected renamed file %q, entries: %v", want, entries)
			}
		})
	}
}

// TestCrc32RenamePSUppercaseFalseParity: the PowerShell template honors
// uppercase=false — the bash port must produce the SAME lowercase tag.
func TestCrc32RenamePSUppercaseFalseParity(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not installed; skipping crc32 PS parity")
	}
	e := crc32Setup(t)
	f := &model.Flow{
		Name:  "guard-crc32-ps",
		Steps: []model.Step{{Type: model.StepType("crc32_rename"), Params: map[string]string{"uppercase": "false"}}},
	}
	script, err := RenderDialect(f, linuxTestJob(), e.v, BuiltinResolver(), PowerShellDialect{})
	if err != nil {
		t.Fatalf("render PS crc32_rename: %v", err)
	}
	libPath, err := filepath.Abs("../../../powershell/EncodeLib.ps1")
	if err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(e.v.ScriptsDir, "job.ps1")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(pwsh, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", scriptPath, "-LibPath", libPath)
	cmd.Env = append(os.Environ(), "PATH="+e.path, "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1")
	out, runErr := cmd.CombinedOutput()
	text := string(out)
	t.Logf("output:\n%s", text)
	if runErr != nil {
		t.Fatalf("PS crc32_rename failed: %v", runErr)
	}
	if _, statErr := os.Stat(filepath.Join(e.epDir, "Test Series - 01 [1080p] [cbf43926].mkv")); statErr != nil {
		entries, _ := os.ReadDir(e.epDir)
		t.Errorf("PS lowercase tag missing (bash must match), entries: %v", entries)
	}
}

// TestCrc32RenameBashCksumFailureFailsClosed: a failing cksum must fail the
// step — the old unchecked pipeline published an EMPTY CRC tag
// ("Show - 01 [].mkv") with 100 + JOB_DONE (reviewer probe P2a).
func TestCrc32RenameBashCksumFailureFailsClosed(t *testing.T) {
	e := crc32Setup(t)
	guardWriteStub(t, e, "cksum", "#!/usr/bin/env bash\nexit 1\n")
	text, err := guardRunBash(t, e, guardRender(t, e, "crc32_rename", map[string]string{"uppercase": "true"}))
	t.Logf("output:\n%s", text)
	guardAssertFailedClosed(t, "crc32_rename", text, err)
	entries, _ := os.ReadDir(e.epDir)
	for _, en := range entries {
		if strings.Contains(en.Name(), "[]") || strings.Contains(en.Name(), "[00000000]") {
			t.Errorf("empty/zero CRC tag published: %s", en.Name())
		}
	}
	if _, statErr := os.Stat(filepath.Join(e.epDir, "Test Series - 01 [1080p].mkv")); statErr != nil {
		t.Error("original MKV must be untouched after a cksum failure")
	}
}

// TestCrc32RenameBashMvFailureFailsClosed: mv into a read-only dir must fail
// the step (reviewer probe P2b).
func TestCrc32RenameBashMvFailureFailsClosed(t *testing.T) {
	e := crc32Setup(t)
	guardReadOnlyEp(t, e)
	text, err := guardRunBash(t, e, guardRender(t, e, "crc32_rename", map[string]string{"uppercase": "true"}))
	t.Logf("output:\n%s", text)
	guardAssertFailedClosed(t, "crc32_rename", text, err)
}

// TestCrc32RenameBashDestDirectoryFailsClosed: a DIRECTORY at the target name
// makes mv succeed by moving the MKV INSIDE it — only a regular file at the
// target is a published rename (same post-check class as the P7 keyframes
// fix). Must fail closed.
func TestCrc32RenameBashDestDirectoryFailsClosed(t *testing.T) {
	e := crc32Setup(t)
	upper, _ := crc32ExpectedHex(t, e)
	destDir := filepath.Join(e.epDir, "Test Series - 01 [1080p] ["+upper+"].mkv")
	if err := os.Mkdir(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	text, err := guardRunBash(t, e, guardRender(t, e, "crc32_rename", map[string]string{"uppercase": "true"}))
	t.Logf("output:\n%s", text)
	guardAssertFailedClosed(t, "crc32_rename", text, err)
	if fi, statErr := os.Stat(destDir); statErr != nil || !fi.IsDir() {
		t.Error("target directory must still be a directory (no regular-file publish)")
	}
}

// --- W3: hdr_probe ----------------------------------------------------------

// TestHdrProbeBashUnwritableDirFailsClosed: rm + the hdr.json write redirect
// were unchecked — a read-only episode dir produced 100 + JOB_DONE with NO
// hdr.json, and encode_4k then silently dropped HDR signaling (reviewer
// probe P4).
func TestHdrProbeBashUnwritableDirFailsClosed(t *testing.T) {
	e := guardSetup(t)
	if err := os.WriteFile(filepath.Join(e.epDir, "src.mkv"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	guardReadOnlyEp(t, e)
	text, err := guardRunBash(t, e, guardRender(t, e, "hdr_probe", nil))
	t.Logf("output:\n%s", text)
	guardAssertFailedClosed(t, "hdr_probe", text, err)
	if _, statErr := os.Stat(filepath.Join(e.epDir, "hdr.json")); statErr == nil {
		t.Error("hdr.json must not exist after a failed run")
	}
}

// TestHdrProbeBashWriteFailureFailsClosed: a failing jq must fail the step
// (the write guard), and the partial artifact must not survive for the
// freshness skip to reuse on retry.
func TestHdrProbeBashWriteFailureFailsClosed(t *testing.T) {
	e := guardSetup(t)
	if err := os.WriteFile(filepath.Join(e.epDir, "src.mkv"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	guardWriteStub(t, e, "jq", "#!/usr/bin/env bash\nexit 1\n")
	text, err := guardRunBash(t, e, guardRender(t, e, "hdr_probe", nil))
	t.Logf("output:\n%s", text)
	guardAssertFailedClosed(t, "hdr_probe", text, err)
	if fi, statErr := os.Stat(filepath.Join(e.epDir, "hdr.json")); statErr == nil && fi.Size() > 0 {
		t.Error("a non-empty hdr.json must not survive a failed write")
	}
}

// TestHdrProbeBashHappyPath: the guards must not break a normal probe.
func TestHdrProbeBashHappyPath(t *testing.T) {
	e := guardSetup(t)
	if err := os.WriteFile(filepath.Join(e.epDir, "src.mkv"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	text, err := guardRunBash(t, e, guardRender(t, e, "hdr_probe", nil))
	t.Logf("output:\n%s", text)
	guardAssertSucceeded(t, "hdr_probe", text, err)
	b, readErr := os.ReadFile(filepath.Join(e.epDir, "hdr.json"))
	if readErr != nil {
		t.Fatalf("hdr.json not written: %v", readErr)
	}
	if !strings.Contains(string(b), `"hdr":"HDR10"`) {
		t.Errorf("hdr.json missing HDR10 classification (stub transfer=PQ): %s", b)
	}
}

// --- W4: audio_lang ---------------------------------------------------------

// audioLangJqStub is a jq stub faithful to audio_lang's exact queries: the
// track-count query has NO -r flag (the generic exec-test stub only answers
// flagged queries, which would fail the step before the manifest write and
// mask the regression). failN makes ONLY the final -nc construction fail.
func audioLangJqStub(failN bool) string {
	nBranch := `  result='{'
  first=1
  a=("$@")
  i=0
  while (( i < ${#a[@]} )); do
    if [[ "${a[$i]}" == "--arg" || "${a[$i]}" == "--argjson" ]] && (( i+2 < ${#a[@]} )); then
      k="${a[$((i+1))]}"; v="${a[$((i+2))]}"
      (( first )) || result+=','
      if [[ "${a[$i]}" == "--arg" ]]; then result+="\"$k\":\"$v\""; else result+="\"$k\":$v"; fi
      first=0
      i=$((i+3))
    else
      i=$((i+1))
    fi
  done
  result+='}'
  printf '%s' "$result"
  exit 0`
	if failN {
		nBranch = `  exit 1`
	}
	return `#!/usr/bin/env bash
args="$*"
if [[ "$args" == *"-n"* ]]; then
` + nBranch + `
fi
if [[ "$args" == *"length"* ]]; then echo 1; exit 0; fi
if [[ "$args" == *"-c"* ]]; then echo '{"@type":"Audio","Language/String3":"jpn"}'; exit 0; fi
echo 'jpn'
`
}

// audioLangSetup adds a source file and the audio_lang-faithful jq stub.
func audioLangSetup(t *testing.T, failN bool) guardEnv {
	t.Helper()
	e := guardSetup(t)
	if err := os.WriteFile(filepath.Join(e.epDir, "src.mkv"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	guardWriteStub(t, e, "jq", audioLangJqStub(failN))
	return e
}

// TestAudioLangBashManifestDirFailsClosed: a DIRECTORY named audio.json makes
// rm -f fail and the write redirect fail — the old unchecked body reported
// 100 + JOB_DONE and mux fell back to jpn (reviewer probe P5).
func TestAudioLangBashManifestDirFailsClosed(t *testing.T) {
	e := audioLangSetup(t, false)
	if err := os.Mkdir(filepath.Join(e.epDir, "audio.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	text, err := guardRunBash(t, e, guardRender(t, e, "audio_lang", map[string]string{"languages": "jpn,eng", "bitrate": "320"}))
	t.Logf("output:\n%s", text)
	guardAssertFailedClosed(t, "audio_lang", text, err)
}

// TestAudioLangBashUnwritableDirFailsClosed: read-only episode dir → the
// stale rm fails → step must fail closed.
func TestAudioLangBashUnwritableDirFailsClosed(t *testing.T) {
	e := audioLangSetup(t, false)
	guardReadOnlyEp(t, e)
	text, err := guardRunBash(t, e, guardRender(t, e, "audio_lang", map[string]string{"languages": "jpn,eng", "bitrate": "320"}))
	t.Logf("output:\n%s", text)
	guardAssertFailedClosed(t, "audio_lang", text, err)
}

// TestAudioLangBashManifestWriteFailureFailsClosed: jq fails ONLY on the
// final -nc manifest construction (selection queries still work) — the
// audio.json write guard must fail the step, not report 100 with no manifest.
func TestAudioLangBashManifestWriteFailureFailsClosed(t *testing.T) {
	e := audioLangSetup(t, true)
	text, err := guardRunBash(t, e, guardRender(t, e, "audio_lang", map[string]string{"languages": "jpn,eng", "bitrate": "320"}))
	t.Logf("output:\n%s", text)
	guardAssertFailedClosed(t, "audio_lang", text, err)
}

// TestAudioLangBashHappyPath: normal run writes audio.json with the selected
// language.
func TestAudioLangBashHappyPath(t *testing.T) {
	e := audioLangSetup(t, false)
	text, err := guardRunBash(t, e, guardRender(t, e, "audio_lang", map[string]string{"languages": "jpn,eng", "bitrate": "320"}))
	t.Logf("output:\n%s", text)
	guardAssertSucceeded(t, "audio_lang", text, err)
	b, readErr := os.ReadFile(filepath.Join(e.epDir, "audio.json"))
	if readErr != nil {
		t.Fatalf("audio.json not written: %v", readErr)
	}
	if !strings.Contains(string(b), `"language":"jpn"`) {
		t.Errorf("audio.json missing selected language: %s", b)
	}
}

// --- W5: release_copy -------------------------------------------------------

// TestReleaseCopyBashCpFailureFailsClosed: cp fails while a SAME-SIZE stale
// destination exists — the size guard alone cannot catch it, and the old
// unchecked cp published the stale release as OVERWROTE with 100 + JOB_DONE
// (reviewer probe P3b).
func TestReleaseCopyBashCpFailureFailsClosed(t *testing.T) {
	e := guardSetup(t)
	src := filepath.Join(e.epDir, "Test Series - 01 [1080p].mkv")
	if err := os.WriteFile(src, []byte(strings.Repeat("N", 500)), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(e.relFolder, "Test Series - 01 [1080p].mkv")
	stale := "OLD" + strings.Repeat("O", 497)
	if err := os.WriteFile(dest, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	guardWriteStub(t, e, "cp", "#!/usr/bin/env bash\nexit 1\n")
	text, err := guardRunBash(t, e, guardRender(t, e, "release_copy", nil))
	t.Logf("output:\n%s", text)
	guardAssertFailedClosed(t, "release_copy", text, err)
	if strings.Contains(text, "OVERWROTE") {
		t.Error("false OVERWROTE log on a failed copy")
	}
	b, readErr := os.ReadFile(dest)
	if readErr != nil || string(b) != stale {
		t.Errorf("stale destination must be untouched after a failed copy: %q err=%v", b, readErr)
	}
}

// TestReleaseCopyBashMkdirFailureFailsClosed: an unwritable release root makes
// mkdir fail — the step must fail closed, not continue into cp.
func TestReleaseCopyBashMkdirFailureFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based failure not reproducible")
	}
	e := guardSetup(t)
	if err := os.WriteFile(filepath.Join(e.epDir, "Test Series - 01 [1080p].mkv"), []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Remove the pre-created release folder so mkdir must run, then make the
	// release root unwritable.
	if err := os.RemoveAll(e.relFolder); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e.v.ReleaseDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(e.v.ReleaseDir, 0o755) })
	text, err := guardRunBash(t, e, guardRender(t, e, "release_copy", nil))
	t.Logf("output:\n%s", text)
	guardAssertFailedClosed(t, "release_copy", text, err)
	if !strings.Contains(text, "failed to create release folder") {
		t.Errorf("mkdir failure must be reported explicitly at the mkdir guard:\n%s", text)
	}
}

// TestReleaseCopyBashHappyPath: normal copy publishes the file.
func TestReleaseCopyBashHappyPath(t *testing.T) {
	e := guardSetup(t)
	content := strings.Repeat("V", 300)
	if err := os.WriteFile(filepath.Join(e.epDir, "Test Series - 01 [1080p].mkv"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	text, err := guardRunBash(t, e, guardRender(t, e, "release_copy", nil))
	t.Logf("output:\n%s", text)
	guardAssertSucceeded(t, "release_copy", text, err)
	b, readErr := os.ReadFile(filepath.Join(e.relFolder, "Test Series - 01 [1080p].mkv"))
	if readErr != nil || string(b) != content {
		t.Errorf("release copy content mismatch: %q err=%v", b, readErr)
	}
}

// --- W6: chapter_extract ----------------------------------------------------

// TestChapterExtractBashStaleDirFailsClosed: a DIRECTORY named chapters.xml
// with zero chapters in the source — rm -f cannot remove it; the old
// unchecked rm reported 100 + JOB_DONE AND logged a false 'removed stale
// chapters.xml' (reviewer probe P8).
func TestChapterExtractBashStaleDirFailsClosed(t *testing.T) {
	e := guardSetup(t)
	if err := os.WriteFile(filepath.Join(e.epDir, "src.mkv"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(e.epDir, "chapters.xml"), 0o755); err != nil {
		t.Fatal(err)
	}
	guardWriteStub(t, e, "mkvextract", "#!/usr/bin/env bash\nexit 0\n")
	text, err := guardRunBash(t, e, guardRender(t, e, "chapter_extract", nil))
	t.Logf("output:\n%s", text)
	guardAssertFailedClosed(t, "chapter_extract", text, err)
	if strings.Contains(text, "removed stale chapters.xml") {
		t.Error("false 'removed' log for a stale artifact that could not be removed")
	}
}

// TestChapterExtractBashHappyPathNoChapters: zero chapters and no stale
// artifact is a legitimate skip — the guard must not over-reject.
func TestChapterExtractBashHappyPathNoChapters(t *testing.T) {
	e := guardSetup(t)
	if err := os.WriteFile(filepath.Join(e.epDir, "src.mkv"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	guardWriteStub(t, e, "mkvextract", "#!/usr/bin/env bash\nexit 0\n")
	text, err := guardRunBash(t, e, guardRender(t, e, "chapter_extract", nil))
	t.Logf("output:\n%s", text)
	guardAssertSucceeded(t, "chapter_extract", text, err)
}
