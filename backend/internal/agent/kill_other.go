//go:build !linux && !windows

package agent

import (
	"os/exec"
)

// setProcessGroup is a no-op on platforms without a dedicated tree-kill
// implementation (the fleet runs Windows + Linux only).
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessTree falls back to killing the script process alone. Descendant
// processes may survive on these platforms — acceptable for an unsupported
// test host, never used in production.
func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
