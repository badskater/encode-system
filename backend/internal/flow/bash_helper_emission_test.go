package flow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBashRenderEmitsFindSourceFileHelper is the regression test for the
// CRITICAL find_source_file linkage gap: rendered bash output must contain
// the `find_source_file () {` definition (emitted by BashDialect.Preamble,
// mirroring the PS `. $LibPath` dot-source of EncodeLib.ps1), and the
// definition MUST appear BEFORE the first call site so the helper is
// available wherever a step body invokes it.
//
// Before the fix, BashDialect.Preamble did not emit the helper at all; the
// exec test (TestBuiltinBashExecAllPortedTemplates) masked the gap by
// manually prepending findSourceFileBash. This test asserts the helper is
// present in a plain render with NO manual injection.
func TestBashRenderEmitsFindSourceFileHelper(t *testing.T) {
	// audio is one of 8 templates that call find_source_file; it is the
	// simplest body that invokes the helper unconditionally.
	script, err := renderBuiltinBash(t, "audio")
	if err != nil {
		t.Fatalf("render audio via BashDialect: %v", err)
	}
	def := "find_source_file () {"
	if !strings.Contains(script, def) {
		t.Fatalf("rendered bash script missing the shared %q helper definition "+
			"(BashDialect.Preamble must emit findSourceFileBash, mirroring PS "+
			"`. $LibPath`):\n%s", def, script)
	}
	// The first call site is `find_source_file "$EpisodeDir"` (or similar).
	// Definition must precede the first call so the helper is defined before
	// any step body runs. bash resolves function names at call time, but
	// definition-precedes-use keeps the script robust under set -euo pipefail
	// and is the natural invariant the PS dot-source guarantees.
	defIdx := strings.Index(script, def)
	callIdx := strings.Index(script, "find_source_file \"$EpisodeDir\"")
	if callIdx < 0 {
		t.Fatalf("audio template should call find_source_file; cannot locate call site:\n%s", script)
	}
	if defIdx >= callIdx {
		t.Fatalf("find_source_file definition (idx %d) must appear BEFORE its first "+
			"call site (idx %d):\n%s", defIdx, callIdx, script)
	}
	t.Logf("helper def at %d, first call at %d (definition precedes use ✓)", defIdx, callIdx)
}

// TestBashRenderEmitsFindSourceFileHelperAcrossCallers sweeps every template
// that calls find_source_file and asserts the helper definition is present in
// each rendered script WITHOUT any manual injection. This guards against a
// future regression where the Preamble emission is dropped.
func TestBashRenderEmitsFindSourceFileHelperAcrossCallers(t *testing.T) {
	// Confirmed callers of find_source_file (authoritative audit).
	callers := []string{
		"media_probe", "hdr_probe", "audio", "audio_branch", "audio_lang",
		"flac_audio", "encode_4k", "verify_output",
	}
	def := "find_source_file () {"
	for _, key := range callers {
		key := key
		t.Run(key, func(t *testing.T) {
			script, err := renderBuiltinBash(t, key)
			if err != nil {
				t.Fatalf("render %q: %v", key, err)
			}
			if !strings.Contains(script, def) {
				t.Errorf("rendered %q missing %q helper — Preamble must emit it", key, def)
			}
		})
	}
}

// TestBashRenderExecutesNoFindSourceFileInjection renders an audio flow via
// BashDialect, writes it to disk with NO manual findSourceFileBash prepend,
// and executes it under bash with stubbed tools. The script must NOT produce
// "command not found" / exit 127 — the helper is now linked naturally.
func TestBashRenderExecutesNoFindSourceFileInjection(t *testing.T) {
	bin, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed; skipping execution test")
	}
	binDir, path := stubBinDir(t)
	v, root := execTestEnv(t, binDir)
	// renderExecScript produces the script with NO manual injection.
	script := renderExecScript(t, "audio", v)
	audioBodyIdx := strings.Index(script, "audio () {")
	if audioBodyIdx < 0 {
		t.Fatal("audio body not found in rendered script")
	}
	if !strings.Contains(script[:audioBodyIdx], "find_source_file () {") {
		t.Fatal("helper should be in the preamble before the audio body")
	}
	scriptPath := filepath.Join(root, "audio_exec.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+path)
	out, err := cmd.CombinedOutput()
	text := string(out)
	if strings.Contains(text, "command not found") {
		t.Fatalf("audio script produced 'command not found' (find_source_file not linked):\n%s", text)
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 127 {
		t.Fatalf("audio script exited 127 (command not found):\n%s", text)
	}
	// Non-127 failure is acceptable (stub may not fully satisfy parsing);
	// the linkage guarantee is the absence of command-not-found.
	t.Logf("audio exec: out=\n%s", text)
}
