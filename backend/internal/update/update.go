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
func (s *Store) binPath() string   { return filepath.Join(s.dir, "bin-package.zip") }
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
	// Recompute hashes from what is actually on disk: if a payload was
	// replaced or tampered with after publish, agents must receive the real
	// hash, not a stale recorded one.
	if h, err := fileSHA256(s.agentPath()); err == nil {
		m.AgentSHA256 = h
	}
	if h, err := fileSHA256(s.libPath()); err == nil {
		m.LibSHA256 = h
	}
	// Bin package is optional (a fleet can pre-install tools manually).
	if fi, err := os.Stat(s.binPath()); err == nil {
		if h, err := fileSHA256(s.binPath()); err == nil {
			m.BinSHA256 = h
			m.BinSize = fi.Size()
		}
	} else {
		m.BinVersion = 0 // payload vanished: stop advertising it
		m.BinSHA256 = ""
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
// The whole write-hash-rename-persist sequence runs under the lock so two
// concurrent publishes cannot interleave on the same temp path.
func (s *Store) PublishAgent(version string, r io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if version == s.manifest.AgentVersion {
		return fmt.Errorf("agent version %s is already published", version)
	}
	hash, err := installPayload(s.agentPath(), r)
	if err != nil {
		return fmt.Errorf("publish agent: %w", err)
	}
	s.manifest.AgentVersion = version
	s.manifest.AgentSHA256 = hash
	return s.persistLocked()
}

// PublishLib stores a new EncodeLib.ps1 and bumps its version counter.
func (s *Store) PublishLib(version int64, r io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if version <= s.manifest.LibVersion {
		return fmt.Errorf("lib version must exceed %d", s.manifest.LibVersion)
	}
	hash, err := installPayload(s.libPath(), r)
	if err != nil {
		return fmt.Errorf("publish lib: %w", err)
	}
	s.manifest.LibVersion = version
	s.manifest.LibSHA256 = hash
	return s.persistLocked()
}

// installPayload writes r to a unique temp file next to dest, hashes it, and
// atomically renames it into place. The temp file is always cleaned up.
func installPayload(dest string, r io.Reader) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temp payload: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write payload: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	hash, err := fileSHA256(tmpName)
	if err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return "", fmt.Errorf("install payload: %w", err)
	}
	return hash, nil
}

// PublishBin stores a new tools-folder zip and bumps its version counter.
// Nodes extract it over their bin dir when their bin_version differs.
func (s *Store) PublishBin(version int64, r io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if version <= s.manifest.BinVersion {
		return fmt.Errorf("bin version must exceed %d", s.manifest.BinVersion)
	}
	hash, err := installPayload(s.binPath(), r)
	if err != nil {
		return fmt.Errorf("publish bin: %w", err)
	}
	fi, err := os.Stat(s.binPath())
	if err != nil {
		return fmt.Errorf("stat bin payload: %w", err)
	}
	s.manifest.BinVersion = version
	s.manifest.BinSHA256 = hash
	s.manifest.BinSize = fi.Size()
	return s.persistLocked()
}

// AgentPayload opens the stored agent binary for serving.
func (s *Store) AgentPayload() (*os.File, error) { return os.Open(s.agentPath()) }

// LibPayload opens the stored EncodeLib.ps1 for serving.
func (s *Store) LibPayload() (*os.File, error) { return os.Open(s.libPath()) }

// BinPayload opens the stored bin-folder zip for serving.
func (s *Store) BinPayload() (*os.File, error) { return os.Open(s.binPath()) }

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

// persistLocked writes manifest.json atomically (tmp + rename). Caller holds s.mu.
func (s *Store) persistLocked() error {
	b, err := json.Marshal(s.manifest)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, "manifest.json.tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.versionPath())
}
