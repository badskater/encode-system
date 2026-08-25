package provision

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/badskater/encode-system/backend/internal/update"
)

// UpdateStoreAdapter implements Payloads on top of the controller's update
// store: whatever has been published (agent binary, EncodeLib.ps1, bin zip)
// is copied into the run's staging directory for ansible win_copy.
type UpdateStoreAdapter struct {
	Up *update.Store
}

// stage copies src file to dest, creating parent dirs. Returns false when
// src does not exist (payload never published) instead of failing — callers
// decide whether that payload is mandatory.
func stage(dest, src string) (bool, error) {
	in, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return false, err
	}
	out, err := os.Create(dest)
	if err != nil {
		return false, err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return false, fmt.Errorf("copy %s: %w", filepath.Base(src), err)
	}
	return true, nil
}

// StageAgentBinary stages encode-agent.exe.
func (a *UpdateStoreAdapter) StageAgentBinary(dest string) (bool, error) {
	return stage(dest, filepath.Join(a.dir(), "encode-agent.exe"))
}

// StageLib stages EncodeLib.ps1.
func (a *UpdateStoreAdapter) StageLib(dest string) (bool, error) {
	return stage(dest, filepath.Join(a.dir(), "EncodeLib.ps1"))
}

// StageBinZip stages the bin-package.zip.
func (a *UpdateStoreAdapter) StageBinZip(dest string) (bool, error) {
	return stage(dest, filepath.Join(a.dir(), "bin-package.zip"))
}

// dir exposes the update store's payload directory.
func (a *UpdateStoreAdapter) dir() string { return a.Up.Dir() }
