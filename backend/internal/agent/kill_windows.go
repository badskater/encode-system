//go:build windows

package agent

import (
	"os/exec"
	"strings"
)

// setProcessGroup is a no-op on Windows: the kill mechanism (taskkill /T)
// walks the parent→child tree by PID, so no group setup is needed at spawn.
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessTree force-kills the job process AND its children via
// taskkill /T /F (the Windows analogue of the Linux process-group kill).
// PowerShell → vspipe/x265/ffmpeg children are all reached through the
// tree walk; killing only the script pid would orphan a wedged encoder.
// A "not found" result means the process already exited — "nothing to
// kill", not a kill failure (mirrors the Linux ESRCH normalization).
func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	args := windowsKillArgs(cmd.Process.Pid)
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil && strings.Contains(strings.ToLower(string(out)), "not found") {
		return nil
	}
	return err
}
