package agent

import (
	"strings"
	"testing"
)

// Regression (backlog cleanup): the swap sidecar must render a valid batch
// script — literal %tries% in the output (not mangled by Go's Sprintf), the
// move-retry loop present, and a restart that happens ONLY after a
// successful move.
func TestWindowsSwapScript(t *testing.T) {
	script := windowsSwapScript(
		"encode-agent.exe",
		`C:\encode-agent\encode-agent.exe.new`,
		`C:\encode-agent\encode-agent.exe`,
		`C:\encode-agent`,
		`"C:\encode-agent\encode-agent.exe" -foreground`,
	)

	// Sprintf escaping must produce the literal batch variable.
	if !strings.Contains(script, "if %tries% geq 15") {
		t.Errorf("tries guard mangled:\n%s", script)
	}
	// Waits for the old process before moving.
	if !strings.Contains(script, `tasklist /FI "IMAGENAME eq encode-agent.exe"`) {
		t.Error("script must wait for the old process to exit")
	}
	// Retry loop with bounded attempts.
	if !strings.Contains(script, ":try_move") || !strings.Contains(script, "goto try_move") {
		t.Error("script must retry the move")
	}
	// Restart only after :swapped — never on failure.
	swapped := strings.Index(script, ":swapped")
	netStart := strings.Index(script, "net start encode-agent")
	relaunch := strings.Index(script, ":relaunch")
	exitFailed := strings.Index(script, "exit /b 1")
	if swapped < 0 || netStart < swapped {
		t.Error("service restart must follow the :swapped label")
	}
	if relaunch < netStart {
		t.Error("relaunch fallback must follow the service path")
	}
	if exitFailed < 0 || exitFailed > swapped {
		t.Error("failure exit must precede the swap-success path")
	}
	// Service detection gates the net stop/start path.
	if !strings.Contains(script, "sc query encode-agent") {
		t.Error("script must detect service deployments before net stop/start")
	}
	// Non-service deployments relaunch with the original arguments.
	if !strings.Contains(script, `"C:\encode-agent\encode-agent.exe" -foreground`) {
		t.Error("script must relaunch with the original arguments for task deployments")
	}
	// Failure leaves a log behind for post-mortems.
	if !strings.Contains(script, `swap-update.log`) {
		t.Error("failed swaps must be logged")
	}
}

func TestRelaunchCommand(t *testing.T) {
	cmd := relaunchCommand(`C:\encode-agent\encode-agent.exe`)
	if !strings.HasPrefix(cmd, `"C:\encode-agent\encode-agent.exe"`) {
		t.Errorf("relaunch must quote the exe path: %q", cmd)
	}
}
