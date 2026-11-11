// Package offsite copies DB snapshots to remote destinations (S3 + SFTP)
// and enforces per-destination retention. It runs after every successful
// local snapshot (scheduled AND manual); failures here never fail the local
// backup — the caller logs/audits/notifies per Result.
package offsite

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// KeySuffix is appended to the snapshot name for the shares-key companion
// object. Mirrors the local convention (<db file>.shares-key) so an offsite
// restore is "download both, put the key beside the restored DB".
const KeySuffix = ".shares-key"

// DefaultKeepN is the retention used when settings keep_n is 0 (unset).
const DefaultKeepN = 30

// Destination is one offsite target. Implementations bake in their root
// (S3 bucket+prefix / SFTP directory); names are plain snapshot file names
// with the UTC timestamp embedded (encode-20060102-150405-…).
type Destination interface {
	// Name identifies the destination in results/logs ("s3", "sftp").
	Name() string
	// Put uploads one local file under the given remote name.
	Put(ctx context.Context, name, localPath string) error
	// List returns the object names under the destination root.
	List(ctx context.Context) ([]string, error)
	// Delete removes one object by name.
	Delete(ctx context.Context, name string) error
}

// Result reports one destination's outcome for audit/notify. Err covers
// build/upload failures (the snapshot did not fully land); PruneErr covers
// retention failures AFTER a successful upload (best-effort, never undoes
// the upload). Error strings are rendered by the caller through
// notify.RedactURL before logging/auditing.
type Result struct {
	Destination string
	Uploaded    []string // object names successfully uploaded
	Pruned      []string // object names deleted by retention
	Err         string
	PruneErr    string
}

// Run uploads one snapshot (and, when includeKey is set and the key file is
// readable, the shares-key companion) to every destination, then prunes each
// to keepN generations. It never returns an error: per-destination failures
// land in Result so one broken target cannot block the others — the local
// backup is already durable when this runs. keyBundled reports whether the
// shares key actually traveled with at least one snapshot copy.
func Run(ctx context.Context, log *slog.Logger, dests []Destination, snapshotPath, keyPath string, includeKey bool, keepN int) (results []Result, keyBundled bool) {
	if log == nil {
		log = slog.Default()
	}
	if keepN < 1 {
		keepN = DefaultKeepN
	}
	name := filepath.Base(snapshotPath)

	// Read the key once for all destinations. A missing key file is normal
	// on a fresh install that has not stored any secret yet (the cipher
	// creates it lazily) — snapshot-only copies are still valid there.
	var keyData []byte
	if includeKey && keyPath != "" {
		b, err := os.ReadFile(keyPath)
		switch {
		case err == nil:
			keyData = b
		case os.IsNotExist(err):
			log.Info("offsite: shares key not present yet, uploading snapshot only", "key_path", keyPath)
		default:
			log.Warn("offsite: shares key unreadable, uploading snapshot only", "key_path", keyPath, "err", err)
		}
	}
	// S3's Put is file-based, so stage the key bytes in a 0600 temp file
	// once per run (removed on return — plaintext key never lingers).
	keyTmp := stageKey(log, keyData)
	if keyTmp != "" {
		defer os.Remove(keyTmp)
	}

	for _, d := range dests {
		r := Result{Destination: d.Name()}
		if ctx.Err() != nil {
			r.Err = ctx.Err().Error()
			results = append(results, r)
			continue
		}
		// Bound hangs: S3 honors ctx natively; SFTP does not, so closing
		// the connection on ctx cancellation aborts blocked transfers.
		// Defers run LIFO: Close first, then stop the AfterFunc.
		if c, ok := d.(io.Closer); ok {
			stop := context.AfterFunc(ctx, func() { _ = c.Close() })
			defer stop()
			defer c.Close()
		}
		if err := d.Put(ctx, name, snapshotPath); err != nil {
			r.Err = err.Error()
			results = append(results, r)
			continue
		}
		r.Uploaded = append(r.Uploaded, name)
		if keyTmp != "" {
			kname := name + KeySuffix
			if err := d.Put(ctx, kname, keyTmp); err != nil {
				r.Err = "shares key upload: " + err.Error()
				results = append(results, r)
				continue
			}
			r.Uploaded = append(r.Uploaded, kname)
			keyBundled = true
		}
		// Retention: best-effort after a successful upload. A listing or
		// delete failure is reported but never marks the upload failed.
		names, err := d.List(ctx)
		if err != nil {
			r.PruneErr = err.Error()
		} else {
			for _, del := range pruneList(names, keepN) {
				if err := d.Delete(ctx, del); err != nil {
					if r.PruneErr == "" {
						r.PruneErr = err.Error()
					}
					continue
				}
				r.Pruned = append(r.Pruned, del)
			}
		}
		results = append(results, r)
	}
	return results, keyBundled
}

// stageKey writes key bytes to a 0600 temp file for file-based uploads.
// Returns "" (and logs) when staging fails — the run continues snapshot-only.
func stageKey(log *slog.Logger, keyData []byte) string {
	if len(keyData) == 0 {
		return ""
	}
	f, err := os.CreateTemp("", "encode-offsite-key-*")
	if err != nil {
		log.Warn("offsite: stage shares key", "err", err)
		return ""
	}
	_, werr := f.Write(keyData)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		log.Warn("offsite: stage shares key", "write_err", werr, "close_err", cerr)
		os.Remove(f.Name())
		return ""
	}
	return f.Name()
}

// pruneList returns the object names to delete so at most keepN snapshot
// generations remain (newest kept). A generation is the snapshot object plus
// its optional key companion — both go together. Foreign objects under the
// same root are never touched. Snapshot names embed a fixed-width UTC
// timestamp (encode-20060102-150405-…), so lexicographic order IS
// chronological order; the collision counter suffix (-1, -2) sorts after
// the plain name within the same second, which is harmless.
func pruneList(names []string, keepN int) []string {
	present := make(map[string]bool, len(names))
	bases := make(map[string]bool)
	for _, n := range names {
		present[n] = true
		base := strings.TrimSuffix(n, KeySuffix)
		if isSnapshotName(base) {
			bases[base] = true
		}
	}
	if len(bases) <= keepN {
		return nil
	}
	sorted := make([]string, 0, len(bases))
	for b := range bases {
		sorted = append(sorted, b)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(sorted)))
	var del []string
	for _, b := range sorted[keepN:] {
		del = append(del, b)
		if present[b+KeySuffix] {
			del = append(del, b+KeySuffix)
		}
	}
	return del
}

// isSnapshotName recognizes the controller's snapshot naming on both
// backends: encode-<UTC timestamp>-<kind>[-counter].db (SQLite) / .dump (PG).
func isSnapshotName(name string) bool {
	return strings.HasPrefix(name, "encode-") &&
		(strings.HasSuffix(name, ".db") || strings.HasSuffix(name, ".dump"))
}

// validName rejects traversal in remote object names (defense in depth —
// names come from our own snapshot naming and destination listings, never
// directly from user input, but a hostile listing must not escape the root).
func validName(name string) error {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return fmt.Errorf("invalid remote object name %q", name)
	}
	return nil
}
