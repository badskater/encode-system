// Package update manages the agent auto-update manifest: which agent binary
// and EncodeLib.ps1 version the controller wants deployed, plus serving the
// payloads to agents.
//
// The store is per-platform for agent binaries: a Windows payload
// (encode-agent.exe) and a Linux payload (encode-agent-linux-amd64) are kept
// in independent slots with their own version/sha/prev. Publishing to one
// platform never disturbs the other's manifest fields. The model carries
// both sets of fields; the controller scopes the manifest to the requesting
// node's platform so the agent-side comparison logic is unchanged across
// platforms.
package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/badskater/encode-system/backend/internal/model"
)

// Platform keys identifying which agent slot a publish/rollback/payload
// operation targets. The empty string and any unknown value are treated as
// Windows (the historical fleet default) so a legacy caller that omits the
// platform behaves exactly as before.
const (
	PlatformWindows = "windows"
	PlatformLinux   = "linux"
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

// NormalizePlatform coerces a platform key to one of the known slots. The
// empty string (legacy callers, old manifest rows, form field absent) maps
// to Windows — the historical fleet default — so a publish without a
// platform field behaves exactly as the single-slot code did.
func NormalizePlatform(platform string) string {
	switch strings.ToLower(platform) {
	case PlatformLinux:
		return PlatformLinux
	default:
		return PlatformWindows
	}
}

// agentPath returns the on-disk path for the CURRENT payload of the given
// platform. Windows keeps the historical encode-agent.exe name (existing
// Windows agents and the provisioner's win_copy staging depend on it); Linux
// uses encode-agent-linux-amd64 so the two platforms never collide.
func (s *Store) agentPath(platform string) string {
	switch NormalizePlatform(platform) {
	case PlatformLinux:
		return filepath.Join(s.dir, "encode-agent-linux-amd64")
	default:
		return filepath.Join(s.dir, "encode-agent.exe")
	}
}

// prevAgentPath is the rollback slot: the payload that was current before
// the most recent publish. Rotated on every PublishAgent; promoted back by
// RollbackAgent (which demotes the rolled-back release into this slot, so
// a rollback is itself reversible). Per-platform so each slot has its own
// one-release history.
func (s *Store) prevAgentPath(platform string) string {
	return s.agentPath(platform) + ".prev"
}

// Dir returns the payload directory (used by the provisioner to stage
// published artifacts for ansible win_copy).
func (s *Store) Dir() string     { return s.dir }
func (s *Store) libPath() string { return filepath.Join(s.dir, "EncodeLib.ps1") }
func (s *Store) binPath() string { return filepath.Join(s.dir, "bin-package.zip") }
func (s *Store) versionPath() string {
	return filepath.Join(s.dir, "manifest.json")
}

// loadFromDisk rebuilds the manifest from previously stored payloads. Each
// payload type is restored INDEPENDENTLY: a fleet may publish only some of
// them (e.g. agent + bin but never EncodeLib, or windows agent but never
// linux), and a missing payload must only invalidate ITS OWN entry — not the
// whole manifest. Discarding everything because one payload was never
// published is what orphans the others on every restart.
//
// Back-compat: a manifest.json written by the OLD single-slot code carries
// only the windows agent fields (no linux_* keys). Those unmarshal to zero
// values (empty linux version/sha), which is correct — a fleet that never
// published a linux payload has an empty linux slot. The recovery below then
// recomputes each platform's sha from its on-disk payload independently.
func (s *Store) loadFromDisk() (model.UpdateManifest, bool) {
	b, err := os.ReadFile(s.versionPath())
	if err != nil {
		return model.UpdateManifest{}, false
	}
	var m model.UpdateManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return model.UpdateManifest{}, false
	}
	// Windows agent payload: recompute the hash from what is actually on
	// disk; a vanished payload clears its entry.
	if _, err := os.Stat(s.agentPath(PlatformWindows)); err == nil {
		if h, err := fileSHA256(s.agentPath(PlatformWindows)); err == nil {
			m.AgentSHA256 = h
		}
	} else {
		m.AgentVersion = ""
		m.AgentSHA256 = ""
	}
	// Windows rollback slot: recover prev identity the same way — a vanished
	// prev payload clears the entry so RollbackAgent errors cleanly instead
	// of promoting a missing file.
	if _, err := os.Stat(s.prevAgentPath(PlatformWindows)); err == nil {
		if h, err := fileSHA256(s.prevAgentPath(PlatformWindows)); err == nil {
			m.PrevAgentSHA256 = h
		}
	} else {
		m.PrevAgentVersion = ""
		m.PrevAgentSHA256 = ""
	}
	// Linux agent payload: recover independently. A missing linux payload
	// (Windows-only fleet, or a legacy manifest with no linux_* fields)
	// just leaves the linux fields empty — it must NOT invalidate the
	// windows entry above.
	if _, err := os.Stat(s.agentPath(PlatformLinux)); err == nil {
		if h, err := fileSHA256(s.agentPath(PlatformLinux)); err == nil {
			m.LinuxAgentSHA256 = h
		}
	} else {
		m.LinuxAgentVersion = ""
		m.LinuxAgentSHA256 = ""
	}
	// Linux rollback slot.
	if _, err := os.Stat(s.prevAgentPath(PlatformLinux)); err == nil {
		if h, err := fileSHA256(s.prevAgentPath(PlatformLinux)); err == nil {
			m.PrevLinuxAgentSHA256 = h
		}
	} else {
		m.PrevLinuxAgentVersion = ""
		m.PrevLinuxAgentSHA256 = ""
	}
	// EncodeLib payload (often never published — that is fine).
	if _, err := os.Stat(s.libPath()); err == nil {
		if h, err := fileSHA256(s.libPath()); err == nil {
			m.LibSHA256 = h
		}
	} else {
		m.LibVersion = 0
		m.LibSHA256 = ""
	}
	// Bin package.
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

// Manifest returns the current desired versions (both platforms' fields).
func (s *Store) Manifest() model.UpdateManifest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.manifest
}

// PublishAgent stores a new agent binary for the given platform and bumps
// that platform's manifest version. The whole write-hash-rename-persist
// sequence runs under the lock so two concurrent publishes cannot interleave
// on the same temp path.
//
// Before installing the new payload, the CURRENT one is rotated into the
// rollback slot (encode-agent.exe.prev / encode-agent-linux-amd64.prev) and
// its identity recorded in the manifest — one release of history, enough to
// undo a bad publish without re-uploading. The rotation is best-effort: a
// failure to copy the old payload aside (e.g. first publish, or a vanished
// file) must not block the new release.
//
// Publishing to one platform never touches the other platform's fields:
// a Windows publish leaves LinuxVersion/SHA/Prev unchanged and vice-versa.
func (s *Store) PublishAgent(platform, version string, r io.Reader) error {
	platform = NormalizePlatform(platform)
	s.mu.Lock()
	defer s.mu.Unlock()
	curVersion := s.currentAgentVersion(platform)
	if version == curVersion {
		return fmt.Errorf("agent version %s is already published", version)
	}
	// Rotate the outgoing release into the rollback slot BEFORE overwriting
	// it. Only when the current payload actually exists on disk (first
	// publish has nothing to keep) and a version is recorded.
	prevVersion, prevSHA := "", ""
	if curVersion != "" {
		if cur, err := os.Open(s.agentPath(platform)); err == nil {
			if err := writeFileHashing(s.prevAgentPath(platform), cur); err == nil {
				if h, err := fileSHA256(s.prevAgentPath(platform)); err == nil {
					prevVersion, prevSHA = curVersion, h
				}
			}
			cur.Close()
		}
	}
	hash, err := installPayload(s.agentPath(platform), r)
	if err != nil {
		return fmt.Errorf("publish agent: %w", err)
	}
	s.setAgentVersion(platform, version, hash)
	// Always overwrite the prev entry (even with empties) so the manifest
	// never advertises a rollback slot that this publish didn't produce.
	s.setPrevAgent(platform, prevVersion, prevSHA)
	if prevVersion == "" {
		os.Remove(s.prevAgentPath(platform)) // stale slot from an older release
	}
	return s.persistLocked()
}

// ErrNoRollback is returned by RollbackAgent when no previous release is
// available (single publish so far, or the prev payload vanished).
var ErrNoRollback = fmt.Errorf("no previous agent release to roll back to")

// RollbackAgent promotes the rollback slot to current for the given platform
// and demotes the just-rolled-back release into the slot, so the operation is
// symmetric: rolling back twice returns to the original state. Nodes pick
// the change up through the normal sync — syncAgent triggers on ANY version
// difference, so a downgrade propagates exactly like an upgrade (staged
// as <binary>.new + swap on restart — on Windows that is encode-agent.exe.new,
// on Linux encode-agent.new). Returns the updated manifest.
func (s *Store) RollbackAgent(platform string) (model.UpdateManifest, error) {
	platform = NormalizePlatform(platform)
	s.mu.Lock()
	defer s.mu.Unlock()
	prevVer := s.prevAgentVersion(platform)
	if prevVer == "" {
		return s.manifest, ErrNoRollback
	}
	if _, err := os.Stat(s.prevAgentPath(platform)); err != nil {
		return s.manifest, ErrNoRollback
	}
	// Swap payloads: current -> prev slot, prev slot -> current. Use a
	// temp name because os.Rename onto an existing file is fine on Unix
	// but the two-step swap keeps both files intact if either rename
	// fails halfway (the manifest only flips after both succeed).
	swap := s.prevAgentPath(platform) + ".swap"
	if err := os.Rename(s.agentPath(platform), swap); err != nil {
		return s.manifest, fmt.Errorf("rollback agent: %w", err)
	}
	if err := os.Rename(s.prevAgentPath(platform), s.agentPath(platform)); err != nil {
		os.Rename(swap, s.agentPath(platform)) // restore on failure
		return s.manifest, fmt.Errorf("rollback agent: %w", err)
	}
	if err := os.Rename(swap, s.prevAgentPath(platform)); err != nil {
		return s.manifest, fmt.Errorf("rollback agent: %w", err)
	}
	// Flip the manifest identities to match the swapped payloads.
	curVer, prevVer2 := s.currentAgentVersion(platform), s.prevAgentVersion(platform)
	s.setAgentVersion(platform, prevVer2, s.prevAgentSHA(platform))
	s.setPrevAgent(platform, curVer, s.currentAgentSHA(platform))
	if err := s.persistLocked(); err != nil {
		return s.manifest, err
	}
	return s.manifest, nil
}

// currentAgentVersion returns the manifest's current agent version for the
// platform. Caller holds s.mu.
func (s *Store) currentAgentVersion(platform string) string {
	if NormalizePlatform(platform) == PlatformLinux {
		return s.manifest.LinuxAgentVersion
	}
	return s.manifest.AgentVersion
}

// currentAgentSHA returns the manifest's current agent sha for the platform.
// Caller holds s.mu.
func (s *Store) currentAgentSHA(platform string) string {
	if NormalizePlatform(platform) == PlatformLinux {
		return s.manifest.LinuxAgentSHA256
	}
	return s.manifest.AgentSHA256
}

// prevAgentVersion returns the manifest's prev agent version for the
// platform. Caller holds s.mu.
func (s *Store) prevAgentVersion(platform string) string {
	if NormalizePlatform(platform) == PlatformLinux {
		return s.manifest.PrevLinuxAgentVersion
	}
	return s.manifest.PrevAgentVersion
}

// prevAgentSHA returns the manifest's prev agent sha for the platform.
// Caller holds s.mu.
func (s *Store) prevAgentSHA(platform string) string {
	if NormalizePlatform(platform) == PlatformLinux {
		return s.manifest.PrevLinuxAgentSHA256
	}
	return s.manifest.PrevAgentSHA256
}

// setAgentVersion stamps the current version+sha for the platform. Caller
// holds s.mu.
func (s *Store) setAgentVersion(platform, version, sha string) {
	if NormalizePlatform(platform) == PlatformLinux {
		s.manifest.LinuxAgentVersion = version
		s.manifest.LinuxAgentSHA256 = sha
		return
	}
	s.manifest.AgentVersion = version
	s.manifest.AgentSHA256 = sha
}

// setPrevAgent stamps the prev version+sha for the platform. Caller holds
// s.mu.
func (s *Store) setPrevAgent(platform, version, sha string) {
	if NormalizePlatform(platform) == PlatformLinux {
		s.manifest.PrevLinuxAgentVersion = version
		s.manifest.PrevLinuxAgentSHA256 = sha
		return
	}
	s.manifest.PrevAgentVersion = version
	s.manifest.PrevAgentSHA256 = sha
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

// AgentPayload opens the stored agent binary for the given platform for
// serving. Returns an error wrapping the open failure when no payload for
// that platform has been published — the caller (handleDownloadAgent)
// surfaces a clean 404 so a Linux node on a Windows-only controller gets a
// "no linux agent payload published" 404 instead of a Windows PE binary.
func (s *Store) AgentPayload(platform string) (*os.File, error) {
	return os.Open(s.agentPath(platform))
}

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
