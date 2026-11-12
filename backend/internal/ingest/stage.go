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
// refused as well. Containment is checked BEFORE open (cheap early
// refusal) AND re-verified AFTER open against the opened file's final
// path (Q2: a swap between check and open must not leak foreign files;
// a swap after open holds the original inode and is safe). Destination
// names are flattened to their base name, prefixed with the parent
// directories when the relative path has depth (Q10: multi-CD releases
// must not silently overwrite each other). Samples are never staged
// (Q3: a 'sample' directory segment or a basename starting with
// 'sample').
//
// Single-file torrents (Q5) save their one file DIRECTLY at the client
// save root — both providers report the root as the save path — so when
// the basename-mapped torrent directory is absent and the single file
// sits at downloadRoot/<filename>, that file is staged from the root.
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
	singleFile := false
	info, err := os.Stat(torrentDir)
	if err != nil || !info.IsDir() {
		// Single-file fallback (Q5): the torrent's one file sits directly
		// at the save root, so there is no torrent directory to map.
		if singleFileAtRoot(downloadRoot, t) {
			torrentDir = downloadRoot
			singleFile = true
		} else {
			return nil, fmt.Errorf("torrent %s: directory %s not visible under download_root %s (check the mount): %v",
				t.Hash, base, downloadRoot, err)
		}
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
	// In single-file mode the torrent dir IS the root (equality).
	realRoot, err := filepath.EvalSymlinks(downloadRoot)
	if err != nil {
		return nil, fmt.Errorf("torrent %s: resolve download_root: %w", t.Hash, err)
	}
	if !singleFile && !strings.HasPrefix(realTorrentDir, realRoot+string(filepath.Separator)) {
		return nil, fmt.Errorf("torrent %s: directory %q resolves outside download_root (symlink escape refused)", t.Hash, base)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("torrent %s: create episode dir: %w", t.Hash, err)
	}

	var staged []string
	sawVideo := false
	for _, f := range t.Files {
		if isSamplePath(f.Name) {
			continue // samples are never encode sources (Q3)
		}
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
		// Pre-open containment: the resolved source must stay inside the
		// resolved torrent directory (symlink escape guard, P6 lesson).
		// This is an EARLY refusal only — the authoritative check happens
		// after open (Q2 TOCTOU).
		realSrc, err := filepath.EvalSymlinks(src)
		if err != nil {
			return staged, fmt.Errorf("torrent %s: source %s unreadable: %w", t.Hash, f.Name, err)
		}
		if !strings.HasPrefix(realSrc, realTorrentDir+string(filepath.Separator)) {
			return staged, fmt.Errorf("torrent %s: file %q escapes the torrent directory", t.Hash, f.Name)
		}
		// Destination naming (Q10): flatten to the base name, but prefix
		// the parent directories when the relative path has depth so
		// same-named files from different subdirs (cd1/a.mkv, cd2/a.mkv)
		// cannot silently overwrite each other.
		destBase := filepath.Base(rel)
		if dir := filepath.Dir(rel); dir != "." {
			destBase = strings.ReplaceAll(dir, string(filepath.Separator), "_") + "_" + destBase
		}
		dest := filepath.Join(destDir, destBase)
		if _, err := os.Stat(dest); err == nil {
			continue // existing file: skip (resume-safe)
		}
		// Open the source, then re-verify containment against the OPENED
		// file's final path (Q2). A path-component swap between the
		// pre-open check and this open would otherwise leak foreign
		// content; the handle's resolved path is authoritative. A swap
		// AFTER open holds the original inode and stays safe.
		in, err := os.Open(src)
		if err != nil {
			return staged, fmt.Errorf("torrent %s: source %s unreadable: %w", t.Hash, f.Name, err)
		}
		finalPath, err := openedFilePath(in)
		if err != nil {
			in.Close()
			return staged, fmt.Errorf("torrent %s: verify %s: %w", t.Hash, f.Name, err)
		}
		if !strings.HasPrefix(finalPath, realTorrentDir+string(filepath.Separator)) {
			in.Close()
			return staged, fmt.Errorf("torrent %s: file %q escapes the torrent directory (post-open verification)", t.Hash, f.Name)
		}
		if err := copyFileAtomic(ctx, in, dest); err != nil {
			return staged, fmt.Errorf("torrent %s: stage %s: %w", t.Hash, f.Name, err)
		}
		staged = append(staged, destBase)
	}
	if !sawVideo {
		return nil, fmt.Errorf("torrent %s: no source video files", t.Hash)
	}
	return staged, nil
}

// isSamplePath reports whether a torrent-relative file path is a sample:
// any DIRECTORY segment named "sample" (case-insensitive) or a basename
// starting with "sample" (case-insensitive). Samples are never encode
// sources — staging one lets the scanner's alphabetical source fallback
// pick it over the episode (Q3).
func isSamplePath(name string) bool {
	segs := strings.Split(filepath.ToSlash(name), "/")
	base := segs[len(segs)-1]
	if strings.HasPrefix(strings.ToLower(base), "sample") {
		return true
	}
	for _, seg := range segs[:len(segs)-1] {
		if strings.EqualFold(seg, "sample") {
			return true
		}
	}
	return false
}

// singleFileAtRoot reports whether t is a single-file torrent whose one
// file sits DIRECTLY under the download root (Q5). Both providers report
// the save ROOT for single-file torrents (qBit save_path=root; ruTorrent
// savePath=Dir(basePath)), so the basename mapping must fall back to
// downloadRoot/<filename> instead of a nonexistent torrent directory.
// Conservative: exactly one file, a plain base name, present as a
// regular file — anything else keeps the clear mount error.
func singleFileAtRoot(root string, t Torrent) bool {
	if len(t.Files) != 1 {
		return false
	}
	name := t.Files[0].Name
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return false
	}
	fi, err := os.Stat(filepath.Join(root, name))
	return err == nil && fi.Mode().IsRegular()
}

// copyFileAtomic copies the ALREADY-OPENED source to dest via a unique
// temp file + rename, then closes the source. The temp name carries a
// random suffix so concurrent stagers (and retries) never collide on a
// fixed name (P6 lesson). The temp file is removed on any failure; a
// successful rename leaves no temp residue. The source is opened by the
// caller so its final path can be containment-verified post-open (Q2).
func copyFileAtomic(ctx context.Context, in *os.File, dest string) error {
	defer in.Close()
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	tmp := dest + ".ingest-" + hex.EncodeToString(suffix) + ".tmp"
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
