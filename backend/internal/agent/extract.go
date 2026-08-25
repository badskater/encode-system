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
// absolute paths, and symlinks before anything touches the disk. Files that
// cannot be replaced in place (a tool currently locked by a running process)
// are staged as <name>.new so the NEXT sync can swap them, instead of
// failing the whole update.
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
		if err := safeRelPath(name); err != nil {
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
		n, err := io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			os.Remove(tmp)
			return fmt.Errorf("write %q: %w", target, err)
		}
		written += n
		if written > maxExtractBytes {
			os.Remove(tmp)
			return fmt.Errorf("extracted size exceeds %d byte cap", maxExtractBytes)
		}
		if err := os.Rename(tmp, target); err != nil {
			// In-use file (e.g. a locked dll): keep the staged copy so a
			// later sync retries the swap; don't fail the whole package.
			staged := target + ".new"
			os.Remove(staged)
			if rerr := os.Rename(tmp, staged); rerr != nil {
				os.Remove(tmp)
				return fmt.Errorf("swap %q (locked, staging also failed): %w", target, err)
			}
			continue
		}
	}
	return nil
}

// safeRelPath rejects names that could escape the extraction root: absolute
// paths, drive letters, UNC roots, or any ".." segment.
func safeRelPath(name string) error {
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
