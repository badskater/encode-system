package update

import (
	"os"
	"strings"
	"testing"
)

func TestPublishAndManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.PublishAgent("0.2.0", strings.NewReader("MZ-binary-bytes")); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishLib(3, strings.NewReader("Write-Output hi")); err != nil {
		t.Fatal(err)
	}

	m := s.Manifest()
	if m.AgentVersion != "0.2.0" || m.AgentSHA256 == "" {
		t.Fatalf("agent manifest wrong: %+v", m)
	}
	if m.LibVersion != 3 || m.LibSHA256 == "" {
		t.Fatalf("lib manifest wrong: %+v", m)
	}

	// Payloads must be servable.
	f, err := s.AgentPayload()
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	f2, err := s.LibPayload()
	if err != nil {
		t.Fatal(err)
	}
	f2.Close()
}

func TestManifestRecoversAfterRestart(t *testing.T) {
	dir := t.TempDir()
	s1, _ := NewStore(dir)
	s1.PublishAgent("0.5.0", strings.NewReader("bytes"))
	s1.PublishLib(7, strings.NewReader("lib"))

	// Simulate controller restart: new store over the same dir.
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := s2.Manifest()
	if m.AgentVersion != "0.5.0" || m.LibVersion != 7 {
		t.Fatalf("manifest not recovered: %+v", m)
	}
}

func TestEmptyManifestWhenNoPayloads(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	m := s.Manifest()
	if m.AgentVersion != "" || m.LibVersion != 0 {
		t.Fatalf("expected empty manifest: %+v", m)
	}
}

// Regression (live deployment): publishing agent + bin but NEVER EncodeLib
// used to make loadFromDisk discard the WHOLE manifest on restart (it
// required every payload to exist), orphaning the published payloads and
// resetting fleet-deployed versions. Payload recovery must be independent.
func TestManifestSurvivesRestartWithPartialPayloads(t *testing.T) {
	dir := t.TempDir()
	s1, _ := NewStore(dir)
	s1.PublishAgent("0.8.1", strings.NewReader("agent-bytes"))
	s1.PublishBin(1, strings.NewReader("zip-bytes"))
	// Deliberately NO PublishLib.

	s2, err := NewStore(dir) // simulated controller restart
	if err != nil {
		t.Fatal(err)
	}
	m := s2.Manifest()
	if m.AgentVersion != "0.8.1" {
		t.Errorf("agent entry lost on restart: %+v", m)
	}
	if m.BinVersion != 1 || m.BinSHA256 == "" {
		t.Errorf("bin entry lost on restart: %+v", m)
	}
	if m.LibVersion != 0 {
		t.Errorf("lib should stay unpublished: %+v", m)
	}
}

// A payload deleted from disk must clear only ITS OWN entry.
func TestManifestClearsOnlyVanishedPayload(t *testing.T) {
	dir := t.TempDir()
	s1, _ := NewStore(dir)
	s1.PublishAgent("0.8.1", strings.NewReader("agent-bytes"))
	s1.PublishBin(1, strings.NewReader("zip-bytes"))

	if err := os.Remove(dir + "/bin-package.zip"); err != nil {
		t.Fatal(err)
	}
	s2, _ := NewStore(dir)
	m := s2.Manifest()
	if m.AgentVersion != "0.8.1" {
		t.Errorf("agent entry must survive bin removal: %+v", m)
	}
	if m.BinVersion != 0 || m.BinSHA256 != "" {
		t.Errorf("vanished bin must be cleared: %+v", m)
	}
}
