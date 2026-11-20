package scanner

import (
	"context"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/s3"
)

// ScanS3 discovers ready episode folders by listing the scripts bucket —
// the S3 equivalent of walking the mounted share. Layout convention is
// identical to the filesystem scan: <series>/<episode>/<files> at depth 2;
// deeper nesting is ignored. A candidate needs one source media file plus
// one filter script, with the same selection priority as Scan (raw
// containers beat .mkv; 2160.vpy > 2160.avs > 1080.vpy > 1080.avs).
//
// Stability gate: object LastModified newer than minStableAge means the
// source is still uploading — deferred to the next scan, exactly like a
// fresh mtime on NFS. minStableAge <= 0 disables the gate.
func ScanS3(ctx context.Context, store s3.ObjectStore, bucket string, minStableAge time.Duration) ([]Candidate, error) {
	return scanS3(ctx, store, bucket, minStableAge, true)
}

// ScanS3SourceOnly is the bucket parity of ScanSourceOnly: stable-source
// episodes without a filter script (materialization candidates).
func ScanS3SourceOnly(ctx context.Context, store s3.ObjectStore, bucket string, minStableAge time.Duration) ([]Candidate, error) {
	cands, err := scanS3(ctx, store, bucket, minStableAge, false)
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for _, c := range cands {
		if c.ScriptFile == "" {
			out = append(out, c)
		}
	}
	return out, nil
}

func scanS3(ctx context.Context, store s3.ObjectStore, bucket string, minStableAge time.Duration, requireScript bool) ([]Candidate, error) {
	objs, err := store.List(ctx, bucket, "")
	if err != nil {
		return nil, err
	}

	// Group objects by their depth-2 directory ("series/episode").
	// BDMV subtree keys (series/episode/BDMV/PLAYLIST|STREAM/…) are deeper
	// than the flat layout rule, but they are evidence of a disc-layout
	// episode — collect them per episode dir instead of dropping them.
	byDir := map[string][]s3Entry{}
	bdmvByDir := map[string][]s3Entry{}
	for _, o := range objs {
		dir, name := path.Split(o.Key)
		trimmed := strings.TrimSuffix(dir, "/")
		// Depth-2 keys are "series/episode/file": the dir part holds exactly
		// one slash. Root/top-level files and deeper nesting are ignored,
		// mirroring the filesystem scan's depth rule.
		if name == "" {
			continue
		}
		if strings.HasPrefix(o.Key, TemplateDirName+"/") {
			continue // reserved: per-series script templates, not a series
		}
		if strings.Count(trimmed, "/") == 1 {
			byDir[trimmed] = append(byDir[trimmed], s3Entry{name: name, obj: o})
			continue
		}
		if rel, epDir, ok := bdmvKey(o.Key); ok {
			bdmvByDir[epDir] = append(bdmvByDir[epDir], s3Entry{name: rel, obj: o})
		}
	}

	dirs := make([]string, 0, len(byDir)+len(bdmvByDir))
	seen := map[string]bool{}
	for d := range byDir {
		dirs = append(dirs, d)
		seen[d] = true
	}
	for d := range bdmvByDir {
		if !seen[d] {
			dirs = append(dirs, d)
		}
	}
	sort.Strings(dirs)

	var out []Candidate
	for _, d := range dirs {
		c, ok := inspectS3Episode(d, byDir[d], bdmvByDir[d], minStableAge, time.Now(), requireScript)
		if ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// bdmvKey recognizes a Blu-ray playlist/stream object key inside an episode
// prefix: <series>/<episode>/BDMV/PLAYLIST/<name>.mpls or
// <series>/<episode>/BDMV/STREAM/<name>.m2ts (spec-uppercase directories,
// case-insensitive extensions — parity with the filesystem bdmvSource).
// Returns the path relative to the episode dir and the depth-2 episode dir.
func bdmvKey(key string) (rel, epDir string, ok bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 5 || parts[2] != "BDMV" {
		return "", "", false
	}
	ext := strings.ToLower(path.Ext(parts[4]))
	switch {
	case parts[3] == "PLAYLIST" && ext == ".mpls":
	case parts[3] == "STREAM" && ext == ".m2ts":
	default:
		return "", "", false
	}
	return strings.Join(parts[2:], "/"), parts[0] + "/" + parts[1], true
}

// s3Entry is one object in an episode directory listing.
type s3Entry struct {
	name string // base name within the episode dir
	obj  model.S3Object
}

// inspectS3Episode applies the same source/script selection and stability
// rules as inspectEpisode to one episode's object listing. bdmvEntries
// carries the disc-layout evidence (BDMV/PLAYLIST + BDMV/STREAM objects)
// used when no flat source exists — parity with the filesystem fallback.
func inspectS3Episode(epDir string, entries, bdmvEntries []s3Entry, minStableAge time.Duration, now time.Time, requireScript bool) (Candidate, bool) {
	var source, script string
	var mkvFallback string
	var sourceMtime time.Time
	for _, e := range entries {
		name := e.name
		ext := strings.ToLower(path.Ext(name))
		if isSourceExt(ext) && ext != ".mkv" && source == "" {
			source = name
			sourceMtime = e.obj.LastModified
		} else if ext == ".mkv" && mkvFallback == "" {
			mkvFallback = name
			sourceMtime = e.obj.LastModified
		}
		if ext != ".avs" && ext != ".vpy" {
			continue
		}
		switch {
		case strings.HasPrefix(name, "2160.vpy"):
			script = name
		case strings.HasPrefix(name, "2160.avs") && !strings.HasPrefix(script, "2160.vpy"):
			script = name
		case strings.HasPrefix(name, "1080.vpy") && !strings.HasPrefix(script, "2160"):
			script = name
		case strings.HasPrefix(name, "1080.avs") && !strings.HasPrefix(script, "2160") && !strings.HasPrefix(script, "1080.vpy"):
			script = name
		case script == "":
			script = name
		}
	}
	if source == "" {
		source = mkvFallback
	}
	// Disc-layout fallback (parity with inspectEpisode): playlists AND
	// streams must both be present; the newest BDMV object is the
	// stability signal.
	if source == "" && len(bdmvEntries) > 0 {
		var playlists []string
		hasStream := false
		var newest time.Time
		for _, e := range bdmvEntries {
			if strings.HasPrefix(e.name, "BDMV/PLAYLIST/") {
				playlists = append(playlists, e.name)
			} else if strings.HasPrefix(e.name, "BDMV/STREAM/") {
				hasStream = true
			}
			if e.obj.LastModified.After(newest) {
				newest = e.obj.LastModified
			}
		}
		if len(playlists) > 0 && hasStream {
			sort.Strings(playlists)
			source = playlists[0]
			sourceMtime = newest
		}
	}
	if source == "" || (requireScript && script == "") {
		return Candidate{}, false
	}
	if minStableAge > 0 && !sourceMtime.IsZero() && now.Sub(sourceMtime) < minStableAge {
		return Candidate{}, false // still uploading
	}
	scriptType := ""
	if script != "" {
		scriptType = "avs"
		if strings.HasSuffix(strings.ToLower(script), ".vpy") {
			scriptType = "vpy"
		}
	}
	parts := strings.SplitN(epDir, "/", 2)
	return Candidate{
		Series:     parts[0],
		EpisodeDir: epDir,
		ScriptType: scriptType,
		ScriptFile: script,
		SourceFile: source,
	}, true
}
