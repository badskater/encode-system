//go:build linux

package agent

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the job script into its own process group so the
// timeout kill can reach the WHOLE tree: bash → vspipe/x265/ffmpeg children
// all inherit the group. Killing only the script pid would orphan a wedged
// x265 — the exact failure mode the timeout exists to fix.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree SIGKILLs the job's process group (negative pid). The group
// was created by setProcessGroup at spawn time; every descendant that did
// not detach into its own session dies with it.
func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
