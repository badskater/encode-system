package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPublishWindowsAndLinuxIndependent verifies each platform slot has its
// own version/sha/prev, and publishing to one platform never disturbs the
// other's manifest fields.
func TestPublishWindowsAndLinuxIndependent(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PublishAgent(PlatformWindows, "1.0.0", strings.NewReader("MZ-windows-v1")); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishAgent(PlatformLinux, "1.0.0", strings.NewReader("\x7fELF-linux-v1")); err != nil {
		t.Fatal(err)
	}
	m := s.Manifest()
	if m.AgentVersion != "1.0.0" || m.AgentSHA256 == "" {
		t.Fatalf("windows agent wrong: %+v", m)
	}
	if m.LinuxAgentVersion != "1.0.0" || m.LinuxAgentSHA256 == "" {
		t.Fatalf("linux agent wrong: %+v", m)
	}

	// Bump windows; linux must be untouched.
	if err := s.PublishAgent(PlatformWindows, "1.1.0", strings.NewReader("MZ-windows-v2")); err != nil {
		t.Fatal(err)
	}
	m = s.Manifest()
	if m.AgentVersion != "1.1.0" {
		t.Fatalf("windows agent = %q, want 1.1.0", m.AgentVersion)
	}
	if m.PrevAgentVersion != "1.0.0" {
		t.Fatalf("windows prev = %q, want 1.0.0", m.PrevAgentVersion)
	}
	if m.LinuxAgentVersion != "1.0.0" || m.LinuxAgentSHA256 == "" {
		t.Fatalf("linux agent disturbed by windows publish: %+v", m)
	}
	if m.PrevLinuxAgentVersion != "" {
		t.Fatalf("linux prev disturbed by windows publish: %+v", m)
	}

	// Bump linux; windows must be untouched.
	if err := s.PublishAgent(PlatformLinux, "1.1.0", strings.NewReader("\x7fELF-linux-v2")); err != nil {
		t.Fatal(err)
	}
	m = s.Manifest()
	if m.LinuxAgentVersion != "1.1.0" {
		t.Fatalf("linux agent = %q, want 1.1.0", m.LinuxAgentVersion)
	}
	if m.PrevLinuxAgentVersion != "1.0.0" {
		t.Fatalf("linux prev = %q, want 1.0.0", m.PrevLinuxAgentVersion)
	}
	if m.AgentVersion != "1.1.0" || m.PrevAgentVersion != "1.0.0" {
		t.Fatalf("windows agent disturbed by linux publish: %+v", m)
	}
}

// TestAgentPayloadPerPlatform verifies AgentPayload(platform) returns the
// right bytes for each platform, and that a missing linux payload errors
// cleanly (not the windows binary).
func TestAgentPayloadPerPlatform(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PublishAgent(PlatformWindows, "1.0.0", strings.NewReader("MZ-windows")); err != nil {
		t.Fatal(err)
	}
	// No linux payload published yet.
	if _, err := s.AgentPayload(PlatformLinux); err == nil {
		t.Fatal("AgentPayload(linux) succeeded with no linux publish, want error")
	}

	if err := s.PublishAgent(PlatformLinux, "1.0.0", strings.NewReader("\x7fELF-linux")); err != nil {
		t.Fatal(err)
	}
	wf, err := s.AgentPayload(PlatformWindows)
	if err != nil {
		t.Fatal(err)
	}
	wb := make([]byte, 32)
	n, _ := wf.Read(wb)
	wf.Close()
	if string(wb[:n]) != "MZ-windows" {
		t.Fatalf("windows payload = %q, want MZ-windows", wb[:n])
	}
	lf, err := s.AgentPayload(PlatformLinux)
	if err != nil {
		t.Fatal(err)
	}
	lb := make([]byte, 32)
	n, _ = lf.Read(lb)
	lf.Close()
	if string(lb[:n]) != "\x7fELF-linux" {
		t.Fatalf("linux payload = %q, want \\x7fELF-linux", lb[:n])
	}
}

// TestRollbackPerPlatform verifies rollback is per-platform: rolling back
// windows does not affect linux and vice-versa.
func TestRollbackPerPlatform(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.PublishAgent(PlatformWindows, "1.0.0", strings.NewReader("win-v1"))
	s.PublishAgent(PlatformWindows, "1.1.0", strings.NewReader("win-v2"))
	s.PublishAgent(PlatformLinux, "1.0.0", strings.NewReader("linux-v1"))
	s.PublishAgent(PlatformLinux, "1.1.0", strings.NewReader("linux-v2"))

	// Roll back windows only.
	m, err := s.RollbackAgent(PlatformWindows)
	if err != nil {
		t.Fatal(err)
	}
	if m.AgentVersion != "1.0.0" || m.PrevAgentVersion != "1.1.0" {
		t.Fatalf("windows after rollback: agent=%q prev=%q", m.AgentVersion, m.PrevAgentVersion)
	}
	// Linux untouched.
	if m.LinuxAgentVersion != "1.1.0" || m.PrevLinuxAgentVersion != "1.0.0" {
		t.Fatalf("linux disturbed by windows rollback: %+v", m)
	}
	// Windows payload is v1.
	cur, _ := os.ReadFile(filepath.Join(s.dir, "encode-agent.exe"))
	if string(cur) != "win-v1" {
		t.Fatalf("windows payload after rollback = %q, want win-v1", cur)
	}

	// Roll back linux only.
	m, err = s.RollbackAgent(PlatformLinux)
	if err != nil {
		t.Fatal(err)
	}
	if m.LinuxAgentVersion != "1.0.0" || m.PrevLinuxAgentVersion != "1.1.0" {
		t.Fatalf("linux after rollback: agent=%q prev=%q", m.LinuxAgentVersion, m.PrevLinuxAgentVersion)
	}
	// Windows untouched.
	if m.AgentVersion != "1.0.0" || m.PrevAgentVersion != "1.1.0" {
		t.Fatalf("windows disturbed by linux rollback: %+v", m)
	}
	// Linux payload is v1.
	cur, _ = os.ReadFile(filepath.Join(s.dir, "encode-agent-linux-amd64"))
	if string(cur) != "linux-v1" {
		t.Fatalf("linux payload after rollback = %q, want linux-v1", cur)
	}
}

// TestLoadFromDiskBothPlatforms verifies loadFromDisk recovers BOTH platforms'
// payloads independently after a restart.
func TestLoadFromDiskBothPlatforms(t *testing.T) {
	dir := t.TempDir()
	s1, _ := NewStore(dir)
	s1.PublishAgent(PlatformWindows, "2.0.0", strings.NewReader("MZ-win"))
	s1.PublishAgent(PlatformLinux, "2.0.0", strings.NewReader("\x7fELF-linux"))
	s1.PublishAgent(PlatformWindows, "2.1.0", strings.NewReader("MZ-win-v2"))
	s1.PublishAgent(PlatformLinux, "2.1.0", strings.NewReader("\x7fELF-linux-v2"))

	s2, err := NewStore(dir) // simulated restart
	if err != nil {
		t.Fatal(err)
	}
	m := s2.Manifest()
	if m.AgentVersion != "2.1.0" || m.PrevAgentVersion != "2.0.0" {
		t.Fatalf("windows not recovered: %+v", m)
	}
	if m.LinuxAgentVersion != "2.1.0" || m.PrevLinuxAgentVersion != "2.0.0" {
		t.Fatalf("linux not recovered: %+v", m)
	}
}

// TestLoadFromDiskLegacyWindowsManifest verifies back-compat: a manifest.json
// written by the OLD single-slot code (only windows fields, no linux_* keys)
// must still load. The linux fields come back empty (zero value) — no error,
// and the windows entry is intact. This is the critical back-compat test:
// a controller upgrading from the single-platform build must not lose its
// published windows payload.
func TestLoadFromDiskLegacyWindowsManifest(t *testing.T) {
	dir := t.TempDir()
	// Simulate the OLD single-slot code: write a legacy manifest.json that
	// has only the windows agent fields, and a windows payload on disk.
	legacyManifest := `{"agent_version":"0.9.0","agent_sha256":"windows-sha-placeholder","lib_version":0,"lib_sha256":"","bin_version":0,"bin_sha256":"","bin_size":0}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(legacyManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "encode-agent.exe"), []byte("MZ-legacy"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("legacy manifest must load without error: %v", err)
	}
	m := s.Manifest()
	// Windows entry intact (sha recomputed from the on-disk payload).
	if m.AgentVersion != "0.9.0" {
		t.Fatalf("windows version lost from legacy manifest: %q", m.AgentVersion)
	}
	if m.AgentSHA256 == "" || m.AgentSHA256 == "windows-sha-placeholder" {
		t.Fatalf("windows sha not recomputed from disk: %q", m.AgentSHA256)
	}
	// Linux fields empty — no linux payload was published.
	if m.LinuxAgentVersion != "" || m.LinuxAgentSHA256 != "" {
		t.Fatalf("linux fields must be empty on legacy manifest: %+v", m)
	}
	if m.PrevLinuxAgentVersion != "" || m.PrevLinuxAgentSHA256 != "" {
		t.Fatalf("linux prev fields must be empty on legacy manifest: %+v", m)
	}
}

// TestPublishDefaultPlatformWindows verifies the empty/absent platform
// defaults to Windows via NormalizePlatform — preserving the legacy publish
// path where no platform field is sent.
func TestPublishDefaultPlatformWindows(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	// Empty string -> Windows.
	if err := s.PublishAgent("", "1.0.0", strings.NewReader("MZ-bytes")); err != nil {
		t.Fatal(err)
	}
	m := s.Manifest()
	if m.AgentVersion != "1.0.0" {
		t.Fatalf("default-platform publish did not hit windows slot: %+v", m)
	}
	// The payload landed on the windows path.
	if _, err := os.Stat(filepath.Join(s.dir, "encode-agent.exe")); err != nil {
		t.Fatalf("windows payload file missing: %v", err)
	}
}
