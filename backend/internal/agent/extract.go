package agent

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxExtractBytes caps the total decompressed size of a bin package (a
// zip-bomb guard: the download cap bounds the COMPRESSED size, not the
// expanded one). 4 GiB covers any sane tools folder.
const maxExtractBytes = 4 << 30

// extractBinZip unpacks a tools-folder zip over destDir. Defense in depth:
// the controller validates the archive at publish time, but the agent is the
// last line of defense — every entry is re-checked for path traversal,
// absolute paths, and symlinks before anything touches the disk.
//
// Failure semantics: ANY file that cannot be placed (locked by a running
// process, disk error) fails the WHOLE extraction with an error. The caller
// does not bump the bin version in that case, so the controller re-offers the
// same package on the next heartbeat and the extraction retries from scratch
// — extraction is idempotent (existing files are harmlessly rewritten), so
// the file eventually lands once the lock clears. No half-applied package is
// ever reported as synced.
func extractBinZip(data []byte, destDir string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create bin dir: %w", err)
	}
	var written int64
	for _, zf := range zr.File {
		name := strings.ReplaceAll(zf.Name, `\`, "/")
		if err := SafeRelPath(name); err != nil {
			return fmt.Errorf("entry %q: %w", zf.Name, err)
		}
		target := filepath.Join(destDir, filepath.FromSlash(name))
		// Re-verify the resolved path stays inside destDir (the canonical
		// zip-slip check, after joining with the real destination).
		if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) &&
			target != filepath.Clean(destDir) {
			return fmt.Errorf("entry %q escapes bin dir", zf.Name)
		}

		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mkdir %q: %w", target, err)
			}
			continue
		}
		if zf.FileInfo().Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("entry %q: symlinks not allowed", zf.Name)
		}

		rc, err := zf.Open()
		if err != nil {
			return fmt.Errorf("open entry %q: %w", zf.Name, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			rc.Close()
			return fmt.Errorf("mkdir parent for %q: %w", target, err)
		}
		tmp := target + ".tmp"
		out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			rc.Close()
			return fmt.Errorf("create temp for %q: %w", target, err)
		}
		// Stream with the remaining budget so a single entry cannot fill the
		// disk before the cumulative check sees it.
		limit := maxExtractBytes - written
		n, err := io.Copy(out, io.LimitReader(rc, limit+1))
		rc.Close()
		out.Close()
		if err != nil {
			os.Remove(tmp)
			return fmt.Errorf("write %q: %w", target, err)
		}
		if n > limit {
			os.Remove(tmp)
			return fmt.Errorf("extracted size exceeds %d byte cap", maxExtractBytes)
		}
		written += n
		if err := os.Rename(tmp, target); err != nil {
			os.Remove(tmp)
			// Locked file (a running tool holds it): fail the sync; the
			// controller re-offers the package and we retry next heartbeat.
			return fmt.Errorf("swap %q (file in use?): %w", target, err)
		}
	}
	return nil
}

// SafeRelPath rejects names that could escape the extraction root: absolute
// paths, drive letters, UNC roots, or any ".." segment. Exported so the
// publish handler can apply the identical guard server-side at upload time.
func SafeRelPath(name string) error {
	if name == "" {
		return fmt.Errorf("empty entry name")
	}
	if filepath.IsAbs(name) || name[0] == '/' || name[0] == '\\' {
		return fmt.Errorf("absolute path")
	}
	if len(name) >= 2 && name[1] == ':' {
		return fmt.Errorf("drive-letter path")
	}
	if strings.HasPrefix(name, `\\`) || strings.HasPrefix(name, `//`) {
		return fmt.Errorf("UNC path")
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return fmt.Errorf("parent-traversal segment")
		}
	}
	return nil
}
