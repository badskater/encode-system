package agent

import (
	"strings"
	"testing"
)

// Regression (backlog cleanup): the swap sidecar must render a valid batch
// script — literal %tries% in the output (not mangled by Go's Sprintf), the
// move-retry loop present, and a service restart that happens ONLY after a
// successful move.
func TestWindowsSwapScript(t *testing.T) {
	script := windowsSwapScript(
		"encode-agent.exe",
		`C:\encode-agent\encode-agent.exe.new`,
		`C:\encode-agent\encode-agent.exe`,
		`C:\encode-agent`,
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
	exitFailed := strings.Index(script, "exit /b 1")
	if swapped < 0 || netStart < swapped {
		t.Error("service restart must follow the :swapped label")
	}
	if exitFailed < 0 || exitFailed > swapped {
		t.Error("failure exit must precede the swap-success path")
	}
	// Failure leaves a log behind for post-mortems.
	if !strings.Contains(script, `swap-update.log`) {
		t.Error("failed swaps must be logged")
	}
}
