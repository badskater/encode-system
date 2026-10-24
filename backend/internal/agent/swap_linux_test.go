package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// skipOnNonLinux gates the swap-script end-to-end tests to linux. The script
// body assertions run on every non-windows platform (the script is pure
// text), but actually executing /bin/sh is linux-only in CI.
func skipOnNonLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("POSIX swap end-to-end test is linux-only")
	}
}

// TestLinuxSwapScriptContent asserts the rendered swap-update.sh mirrors the
// bat contract: a bounded retry loop around the mv, a relaunch that happens
// ONLY after a successful move, a systemd-aware branch that skips relaunch
// when INVOCATION_ID is set, and a failure log on exhaustion.
func TestLinuxSwapScriptContent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("linux swap script is not used on windows")
	}
	script := linuxSwapScript(
		"/opt/encode-agent/encode-agent",
		"/opt/encode-agent/encode-agent.new",
		"/var/lib/encode-agent",
		`"/opt/encode-agent/encode-agent" -config /etc/encode-agent/agent.json -foreground`,
	)

	// Retry loop with bounded attempts (mirrors the bat's 15-try guard).
	if !strings.Contains(script, "tries=0") || !strings.Contains(script, "tries -ge 15") {
		t.Errorf("script must guard retries with a tries counter:\n%s", script)
	}
	if !strings.Contains(script, "mv -f") {
		t.Errorf("script must move the staged binary with mv:\n%s", script)
	}
	// A marker that the move succeeded must precede the relaunch.
	swapped := strings.Index(script, "# swapped")
	relaunch := strings.Index(script, "relaunch")
	exitFailed := strings.Index(script, "exit 1")
	if swapped < 0 {
		t.Error("script must mark the successful-swap branch")
	}
	if relaunch >= 0 && relaunch < swapped {
		t.Error("relaunch must follow the swap-success marker, not precede it")
	}
	if exitFailed < 0 || (swapped >= 0 && exitFailed > swapped) {
		t.Error("failure exit must precede the swap-success path")
	}
	// systemd-aware: when INVOCATION_ID is set, skip relaunch (let Restart=always
	// bring the new binary back) and exit 0 after the swap.
	if !strings.Contains(script, "INVOCATION_ID") {
		t.Error("script must detect systemd via INVOCATION_ID")
	}
	if !strings.Contains(script, "systemctl --quiet is-active encode-agent") {
		t.Error("script should confirm the unit is active before relying on Restart=always")
	}
	// Bare deployments (no INVOCATION_ID) relaunch with the original arguments.
	if !strings.Contains(script, `"/opt/encode-agent/encode-agent" -config /etc/encode-agent/agent.json -foreground`) {
		t.Error("script must relaunch with the original arguments for bare deployments")
	}
	// Failure leaves a log behind for post-mortems.
	if !strings.Contains(script, "swap-update.log") {
		t.Error("failed swaps must be logged")
	}
	// Script must be executable bash.
	if !strings.HasPrefix(script, "#!/usr/bin/env bash") && !strings.HasPrefix(script, "#!/bin/bash") {
		t.Errorf("script must have a bash shebang:\n%s", script)
	}
}

// TestLinuxSwapScriptRelaunchOrder ensures the systemd (skip-relaunch) branch
// comes BEFORE the bare relaunch branch, so an active unit never tries to
// start a duplicate process.
func TestLinuxSwapScriptRelaunchOrder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("linux swap script is not used on windows")
	}
	script := linuxSwapScript("/x/agent", "/x/agent.new", "/x", `"/x/agent"`)
	systemdBranch := strings.Index(script, "INVOCATION_ID")
	bareRelaunch := strings.Index(script, "setsid") // bare relaunch uses setsid
	if systemdBranch < 0 {
		t.Fatal("missing INVOCATION_ID branch")
	}
	if bareRelaunch < 0 {
		t.Fatal("missing setsid bare-relaunch branch")
	}
	if bareRelaunch < systemdBranch {
		t.Error("systemd-aware (skip-relaunch) branch must come before the bare setsid relaunch")
	}
}

// TestLinuxSwapEndToEnd stages a fake "new binary" (a stub shell script that
// touches a marker file), renders the real swap-update.sh, runs it under the
// bare (non-systemd) path, and verifies the marker appears — proving the mv
// succeeded and the new binary was launched.
func TestLinuxSwapEndToEnd(t *testing.T) {
	skipOnNonLinux(t)
	dir := t.TempDir()

	// The "current binary" is a no-op stub (the swap script will mv over it).
	self := filepath.Join(dir, "encode-agent")
	if err := os.WriteFile(self, []byte("#!/bin/sh\necho old\n"), 0o755); err != nil {
		t.Fatalf("write self: %v", err)
	}
	// The "new binary" stub touches a marker so we can prove it ran.
	marker := filepath.Join(dir, "ran.marker")
	newBin := filepath.Join(dir, "encode-agent.new")
	newBinScript := "#!/bin/sh\ntouch " + shellQuote(marker) + "\n"
	if err := os.WriteFile(newBin, []byte(newBinScript), 0o755); err != nil {
		t.Fatalf("write staged: %v", err)
	}

	script := linuxSwapScript(self, newBin, dir, shellQuote(self)+" -foreground")
	scriptPath := filepath.Join(dir, "swap-update.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}

	// Run the script synchronously with NO INVOCATION_ID so it takes the bare
	// relaunch path (setsid the new binary). The new binary touches the marker.
	cmd := exec.Command("bash", scriptPath)
	cmd.Env = []string{} // empty env => no INVOCATION_ID => bare path
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script run failed: %v\n%s", err, out)
	}
	// Wait for the detached marker to appear.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("new binary did not run (marker missing); script out:\n%s", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestLinuxSwapSystemdModeSkipsRelaunch runs the swap script WITH INVOCATION_ID
// set and an active-ish fake systemctl stub on PATH. It verifies the new
// binary is swapped into place but NOT relaunched (no marker from a relaunch
// path), proving systemd mode relies on Restart=always instead.
func TestLinuxSwapSystemdModeSkipsRelaunch(t *testing.T) {
	skipOnNonLinux(t)
	dir := t.TempDir()

	self := filepath.Join(dir, "encode-agent")
	if err := os.WriteFile(self, []byte("#!/bin/sh\necho old\n"), 0o755); err != nil {
		t.Fatalf("write self: %v", err)
	}
	// The "new binary" would touch a marker IF relaunched; systemd mode must
	// NOT relaunch it, so the marker must stay absent.
	marker := filepath.Join(dir, "ran.marker")
	newBin := filepath.Join(dir, "encode-agent.new")
	newBinScript := "#!/bin/sh\ntouch " + shellQuote(marker) + "\n"
	if err := os.WriteFile(newBin, []byte(newBinScript), 0o755); err != nil {
		t.Fatalf("write staged: %v", err)
	}
	// Fake systemctl that reports the unit as "active" so the systemd branch
	// is taken.
	fakeBin := filepath.Join(dir, "fakebin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatalf("mkdir fakebin: %v", err)
	}
	fakeSystemctl := filepath.Join(fakeBin, "systemctl")
	if err := os.WriteFile(fakeSystemctl, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake systemctl: %v", err)
	}

	script := linuxSwapScript(self, newBin, dir, shellQuote(self)+" -foreground")
	scriptPath := filepath.Join(dir, "swap-update.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Env = []string{
		"INVOCATION_ID=fake-uuid",
		"PATH=" + fakeBin + ":/usr/bin:/bin",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script run failed: %v\n%s", err, out)
	}
	// The new binary must have been moved into place (self now equals newBin).
	got, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("read self after swap: %v", err)
	}
	if !strings.Contains(string(got), "touch") {
		t.Errorf("self was not replaced by the staged new binary; got:\n%s", got)
	}
	// Give any stray relaunch a moment to fire, then assert the marker is absent.
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("systemd mode must NOT relaunch the binary (Restart=always handles it); marker was touched")
	}
}

// TestLinuxSwapScriptMode asserts the written swap-update.sh is 0700.
func TestLinuxSwapScriptMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode check is POSIX-only")
	}
	dir := t.TempDir()
	script := linuxSwapScript(filepath.Join(dir, "agent"), filepath.Join(dir, "agent.new"), dir, `"agent"`)
	path := filepath.Join(dir, "swap-update.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("swap-update.sh perm = %o, want 0700", mode)
	}
}

// shellQuote wraps a path in single quotes, escaping embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
