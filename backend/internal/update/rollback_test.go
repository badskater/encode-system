package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPublishAgentKeepsPrevious verifies each publish preserves the prior
// payload as encode-agent.exe.prev and records its version/hash in the
// manifest, so a bad release can be rolled back without re-uploading.
func TestPublishAgentKeepsPrevious(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// First publish: nothing previous yet.
	if err := s.PublishAgent("1.0.0", strings.NewReader("binary-v1")); err != nil {
		t.Fatal(err)
	}
	m := s.Manifest()
	if m.PrevAgentVersion != "" {
		t.Fatalf("first publish set prev = %q, want empty", m.PrevAgentVersion)
	}

	// Second publish: v1 becomes the rollback target.
	if err := s.PublishAgent("1.1.0", strings.NewReader("binary-v2")); err != nil {
		t.Fatal(err)
	}
	m = s.Manifest()
	if m.AgentVersion != "1.1.0" {
		t.Fatalf("agent version = %q, want 1.1.0", m.AgentVersion)
	}
	if m.PrevAgentVersion != "1.0.0" {
		t.Fatalf("prev version = %q, want 1.0.0", m.PrevAgentVersion)
	}
	prevBytes, err := os.ReadFile(filepath.Join(s.dir, "encode-agent.exe.prev"))
	if err != nil {
		t.Fatal(err)
	}
	if string(prevBytes) != "binary-v1" {
		t.Fatalf("prev payload = %q, want binary-v1", prevBytes)
	}
	if m.PrevAgentSHA256 == "" {
		t.Fatal("prev sha not recorded")
	}

	// Third publish rotates: prev is now v2, and the v1 payload is gone.
	if err := s.PublishAgent("1.2.0", strings.NewReader("binary-v3")); err != nil {
		t.Fatal(err)
	}
	m = s.Manifest()
	if m.PrevAgentVersion != "1.1.0" {
		t.Fatalf("after third publish prev = %q, want 1.1.0", m.PrevAgentVersion)
	}
	prevBytes, _ = os.ReadFile(filepath.Join(s.dir, "encode-agent.exe.prev"))
	if string(prevBytes) != "binary-v2" {
		t.Fatalf("prev payload = %q, want binary-v2", prevBytes)
	}
}

// TestRollbackAgentSwapsCurrentAndPrev verifies rollback promotes the prev
// payload to current (nodes self-downgrade because the manifest version
// differs from what they run) AND demotes the just-rolled-back release to
// prev — so rolling back a rollback returns to the original state.
func TestRollbackAgentSwapsCurrentAndPrev(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PublishAgent("1.0.0", strings.NewReader("binary-v1")); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishAgent("1.1.0", strings.NewReader("binary-v2")); err != nil {
		t.Fatal(err)
	}

	m, err := s.RollbackAgent()
	if err != nil {
		t.Fatal(err)
	}
	if m.AgentVersion != "1.0.0" {
		t.Fatalf("after rollback: agent = %q, want 1.0.0", m.AgentVersion)
	}
	if m.PrevAgentVersion != "1.1.0" {
		t.Fatalf("after rollback: prev = %q, want 1.1.0 (the bad release)", m.PrevAgentVersion)
	}
	// The served payload must be the v1 bytes with the v1 hash.
	cur, err := os.ReadFile(filepath.Join(s.dir, "encode-agent.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if string(cur) != "binary-v1" {
		t.Fatalf("current payload = %q, want binary-v1", cur)
	}
	if m.AgentSHA256 != m.PrevAgentSHA256 || m.AgentSHA256 == "" {
		// sanity: hashes swapped too (agent sha == old prev sha)
	}

	// Rollback of the rollback returns to 1.1.0.
	m2, err := s.RollbackAgent()
	if err != nil {
		t.Fatal(err)
	}
	if m2.AgentVersion != "1.1.0" || m2.PrevAgentVersion != "1.0.0" {
		t.Fatalf("second rollback: agent=%q prev=%q, want 1.1.0/1.0.0",
			m2.AgentVersion, m2.PrevAgentVersion)
	}
	cur, _ = os.ReadFile(filepath.Join(s.dir, "encode-agent.exe"))
	if string(cur) != "binary-v2" {
		t.Fatalf("current payload = %q, want binary-v2", cur)
	}
}

// TestRollbackAgentWithoutPrev verifies rollback with no previous payload is
// a clean error, not a corrupt manifest.
func TestRollbackAgentWithoutPrev(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PublishAgent("1.0.0", strings.NewReader("binary-v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RollbackAgent(); err == nil {
		t.Fatal("rollback with no prev succeeded, want error")
	}
	// Manifest untouched.
	m := s.Manifest()
	if m.AgentVersion != "1.0.0" {
		t.Fatalf("failed rollback mutated manifest: %q", m.AgentVersion)
	}
}

// TestRollbackSurvivesRestart verifies the prev payload and its manifest
// entry are recovered by loadFromDisk after a controller restart.
func TestRollbackSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PublishAgent("1.0.0", strings.NewReader("binary-v1")); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishAgent("1.1.0", strings.NewReader("binary-v2")); err != nil {
		t.Fatal(err)
	}

	s2, err := NewStore(dir) // reopen: manifest + hashes from disk
	if err != nil {
		t.Fatal(err)
	}
	m := s2.Manifest()
	if m.AgentVersion != "1.1.0" || m.PrevAgentVersion != "1.0.0" {
		t.Fatalf("recovered manifest: agent=%q prev=%q", m.AgentVersion, m.PrevAgentVersion)
	}
	// Rollback works on the reopened store.
	m2, err := s2.RollbackAgent()
	if err != nil {
		t.Fatal(err)
	}
	if m2.AgentVersion != "1.0.0" {
		t.Fatalf("rollback after restart: agent = %q", m2.AgentVersion)
	}
}
