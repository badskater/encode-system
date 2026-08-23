// Package update manages the agent auto-update manifest: which agent binary
// and EncodeLib.ps1 version the controller wants deployed, plus serving the
// payloads to agents.
package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/badskater/encode-system/backend/internal/model"
)

// Store tracks the current manifest and stores payloads on disk.
type Store struct {
	dir string

	mu       sync.RWMutex
	manifest model.UpdateManifest
}

// NewStore creates an update store rooted at dir (created if missing).
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create update dir: %w", err)
	}
	s := &Store{dir: dir}
	// Recover manifest from disk if payloads exist from a previous run.
	if m, ok := s.loadFromDisk(); ok {
		s.manifest = m
	}
	return s, nil
}

func (s *Store) agentPath() string { return filepath.Join(s.dir, "encode-agent.exe") }
func (s *Store) libPath() string   { return filepath.Join(s.dir, "EncodeLib.ps1") }
func (s *Store) versionPath() string {
	return filepath.Join(s.dir, "manifest.json")
}

// loadFromDisk rebuilds the manifest from previously stored payloads.
func (s *Store) loadFromDisk() (model.UpdateManifest, bool) {
	b, err := os.ReadFile(s.versionPath())
	if err != nil {
		return model.UpdateManifest{}, false
	}
	var m model.UpdateManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return model.UpdateManifest{}, false
	}
	// Payloads must still exist, otherwise the manifest is stale.
	if _, err := os.Stat(s.agentPath()); err != nil {
		return model.UpdateManifest{}, false
	}
	if _, err := os.Stat(s.libPath()); err != nil {
		return model.UpdateManifest{}, false
	}
	return m, true
}

// Manifest returns the current desired versions.
func (s *Store) Manifest() model.UpdateManifest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.manifest
}

// PublishAgent stores a new agent binary and bumps the manifest version.
func (s *Store) PublishAgent(version string, r io.Reader) error {
	tmp := s.agentPath() + ".tmp"
	if err := writeFileHashing(tmp, r); err != nil {
		return err
	}
	hash, err := fileSHA256(tmp)
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.agentPath()); err != nil {
		return fmt.Errorf("install agent payload: %w", err)
	}
	s.mu.Lock()
	s.manifest.AgentVersion = version
	s.manifest.AgentSHA256 = hash
	m := s.manifest
	s.mu.Unlock()
	return s.persist(m)
}

// PublishLib stores a new EncodeLib.ps1 and bumps its version counter.
func (s *Store) PublishLib(version int64, r io.Reader) error {
	tmp := s.libPath() + ".tmp"
	if err := writeFileHashing(tmp, r); err != nil {
		return err
	}
	hash, err := fileSHA256(tmp)
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.libPath()); err != nil {
		return fmt.Errorf("install lib payload: %w", err)
	}
	s.mu.Lock()
	s.manifest.LibVersion = version
	s.manifest.LibSHA256 = hash
	m := s.manifest
	s.mu.Unlock()
	return s.persist(m)
}

// AgentPayload opens the stored agent binary for serving.
func (s *Store) AgentPayload() (*os.File, error) { return os.Open(s.agentPath()) }

// LibPayload opens the stored EncodeLib.ps1 for serving.
func (s *Store) LibPayload() (*os.File, error) { return os.Open(s.libPath()) }

func writeFileHashing(path string, r io.Reader) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create payload file: %w", err)
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("write payload: %w", err)
	}
	return f.Close()
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Store) persist(m model.UpdateManifest) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(s.versionPath(), b, 0o644)
}
