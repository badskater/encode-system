package flow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestDefaultFlowLinuxExecNoCommandNotFound is the review's original proof
// method: render a DefaultFlow (the 1080p production pipeline) via BashDialect
// and execute the full multi-step script under bash with stubbed binaries on
// a temp PATH. The script MUST NOT exit 127 or print "command not found" —
// the find_source_file helper (and every step function) must resolve.
//
// DefaultFlow includes the dgindex step which is REFUSED on Linux, so this
// test builds a Linux-shaped DefaultFlow that swaps the refused steps out
// for their available siblings, exercising the same step functions a real
// Linux encode job would run: source_rename, audio, encode, mux,
// verify_output, release_copy, crc32_rename, discord_notify.
func TestDefaultFlowLinuxExecNoCommandNotFound(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed; skipping execution test")
	}
	binDir, path := stubBinDir(t)
	v, root := execTestEnv(t, binDir)

	// Build a Linux-shaped default flow (no dgindex/keyframes — refused on
	// Linux). The steps here are exactly the ones a production Linux encode
	// job dispatches: source_rename → audio → encode → mux → verify_output
	// → release_copy → crc32_rename → discord_notify.
	f := &model.Flow{
		Name: "default-1080-linux",
		Steps: []model.Step{
			{Type: model.StepSourceRename, Params: map[string]string{"source_name": "src"}},
			{Type: model.StepAudio, Params: map[string]string{"track": "2", "bitrate": "320"}},
			{Type: model.StepEncode, Params: map[string]string{}},
			{Type: model.StepMux},
			{Type: model.StepType("verify_output")},
			{Type: model.StepReleaseCopy},
			{Type: model.StepType("crc32_rename")},
			{Type: model.StepType("discord_notify")},
		},
	}
	j := linuxTestJob()
	script, err := RenderDialect(f, j, v, BuiltinResolver(), BashDialect{})
	if err != nil {
		t.Fatalf("render DefaultFlow-for-linux: %v", err)
	}

	// No manual injection — the helper must be present naturally.
	if !strings.Contains(script, "find_source_file () {") {
		t.Fatalf("rendered script missing find_source_file helper definition")
	}

	scriptPath := filepath.Join(root, "default_linux.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+path, "DiscordWebhook=")
	out, err := cmd.CombinedOutput()
	text := string(out)

	// PRIMARY assertion: no "command not found" / exit 127.
	if strings.Contains(text, "command not found") {
		t.Fatalf("DefaultFlow-for-linux produced 'command not found' (find_source_file or a step function not linked):\n%s", text)
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 127 {
		t.Fatalf("DefaultFlow-for-linux exited 127 (command not found):\n%s", text)
	}
	t.Logf("DefaultFlow-for-linux exit=%v (non-127 = linkage OK; stubs may not fully satisfy):\n%s", err, text)
}
