package ingest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Stage copies a torrent's source video files from the controller-visible
// download root into destDir (the scripts-share episode folder). It is
// atomic-ish PER FILE: each copy lands in a unique temp file inside
// destDir and is renamed over the destination only when complete, so a
// crash or partial copy never leaves a half-written source that the
// scanner could pick up. Existing destination files are skipped (a
// re-stage after a ledger-write failure resumes instead of duplicating).
//
// Path safety: torrent file names are untrusted input. Absolute paths,
// ".." traversal and symlink escapes out of the torrent directory are
// refused; a symlinked torrent dir resolving outside download_root is
// refused as well; destination names are flattened to their base name
// (the scanner reads flat episode folders).
//
// On the first failure the in-flight temp file is removed and the error
// returned; files already renamed stay (the caller writes the dedupe
// ledger only on FULL success, so the next poll resumes and skips them).
// Returns the base names of newly staged files.
func Stage(ctx context.Context, downloadRoot string, t Torrent, destDir string) ([]string, error) {
	if strings.TrimSpace(t.SavePath) == "" {
		return nil, fmt.Errorf("torrent %s: empty save path", t.Hash)
	}
	// The client save path is absolute on the CLIENT host; the controller
	// sees the same directory under downloadRoot. Resolve by base name —
	// the documented mount requirement is that the client saves completed
	// torrents DIRECTLY under its download root (no per-category nesting).
	base := filepath.Base(filepath.FromSlash(t.SavePath))
	if base == "" || base == "." || base == ".." || base == string(filepath.Separator) {
		return nil, fmt.Errorf("torrent %s: unusable save path %q", t.Hash, t.SavePath)
	}
	torrentDir := filepath.Join(downloadRoot, base)
	info, err := os.Stat(torrentDir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("torrent %s: directory %s not visible under download_root %s (check the mount): %v",
			t.Hash, base, downloadRoot, err)
	}
	// Resolve symlinks on the torrent dir itself so the containment check
	// below compares real paths (a symlinked torrent dir must not widen
	// the boundary).
	realTorrentDir, err := filepath.EvalSymlinks(torrentDir)
	if err != nil {
		return nil, fmt.Errorf("torrent %s: resolve dir: %w", t.Hash, err)
	}
	// The resolved torrent dir must still live under the resolved
	// download root: a symlinked torrent dir pointing outside the root
	// would otherwise pass the per-file check below (which compares
	// against the torrent dir itself) and stage foreign files. The
	// mount is operator-trusted, but the containment claim must hold.
	realRoot, err := filepath.EvalSymlinks(downloadRoot)
	if err != nil {
		return nil, fmt.Errorf("torrent %s: resolve download_root: %w", t.Hash, err)
	}
	if !strings.HasPrefix(realTorrentDir, realRoot+string(filepath.Separator)) {
		return nil, fmt.Errorf("torrent %s: directory %q resolves outside download_root (symlink escape refused)", t.Hash, base)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("torrent %s: create episode dir: %w", t.Hash, err)
	}

	var staged []string
	sawVideo := false
	for _, f := range t.Files {
		if !IsVideoFile(f.Name) {
			continue // only encode sources are staged
		}
		sawVideo = true
		if err := ctx.Err(); err != nil {
			return staged, err
		}
		rel := filepath.FromSlash(f.Name)
		if filepath.IsAbs(rel) || strings.Contains(f.Name, "..") {
			return staged, fmt.Errorf("torrent %s: unsafe file path %q", t.Hash, f.Name)
		}
		src := filepath.Join(torrentDir, rel)
		// Containment: the resolved source must stay inside the resolved
		// torrent directory (symlink escape guard, P6 lesson).
		realSrc, err := filepath.EvalSymlinks(src)
		if err != nil {
			return staged, fmt.Errorf("torrent %s: source %s unreadable: %w", t.Hash, f.Name, err)
		}
		if !strings.HasPrefix(realSrc, realTorrentDir+string(filepath.Separator)) {
			return staged, fmt.Errorf("torrent %s: file %q escapes the torrent directory", t.Hash, f.Name)
		}
		dest := filepath.Join(destDir, filepath.Base(rel))
		if _, err := os.Stat(dest); err == nil {
			continue // existing file: skip (resume-safe)
		}
		if err := copyFileAtomic(ctx, realSrc, dest); err != nil {
			return staged, fmt.Errorf("torrent %s: stage %s: %w", t.Hash, f.Name, err)
		}
		staged = append(staged, filepath.Base(rel))
	}
	if !sawVideo {
		return nil, fmt.Errorf("torrent %s: no source video files", t.Hash)
	}
	return staged, nil
}

// copyFileAtomic copies src to dest via a unique temp file + rename. The
// temp name carries random suffix so concurrent stagers (and retries)
// never collide on a fixed name (P6 lesson). The temp file is removed on
// any failure; a successful rename leaves no temp residue.
func copyFileAtomic(ctx context.Context, src, dest string) error {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	tmp := dest + ".ingest-" + hex.EncodeToString(suffix) + ".tmp"
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	// Every read checks ctx so a cancelled poll stops a large copy
	// promptly instead of finishing the whole file first.
	if _, err := io.Copy(out, &ctxReader{ctx: ctx, r: in}); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ctxReader aborts a copy when the context is cancelled (checked on every
// Read — io.Copy uses a 32 KiB buffer, so the check is cheap and frequent).
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
