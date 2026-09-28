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
	objs, err := store.List(ctx, bucket, "")
	if err != nil {
		return nil, err
	}

	// Group objects by their depth-2 directory ("series/episode").
	byDir := map[string][]s3Entry{}
	for _, o := range objs {
		dir, name := path.Split(o.Key)
		trimmed := strings.TrimSuffix(dir, "/")
		// Depth-2 keys are "series/episode/file": the dir part holds exactly
		// one slash. Root/top-level files and deeper nesting are ignored,
		// mirroring the filesystem scan's depth rule.
		if name == "" || strings.Count(trimmed, "/") != 1 {
			continue
		}
		key := trimmed
		byDir[key] = append(byDir[key], s3Entry{name: name, obj: o})
	}

	dirs := make([]string, 0, len(byDir))
	for d := range byDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	var out []Candidate
	for _, d := range dirs {
		c, ok := inspectS3Episode(d, byDir[d], minStableAge, time.Now())
		if ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// s3Entry is one object in an episode directory listing.
type s3Entry struct {
	name string // base name within the episode dir
	obj  model.S3Object
}

// inspectS3Episode applies the same source/script selection and stability
// rules as inspectEpisode to one episode's object listing.
func inspectS3Episode(epDir string, entries []s3Entry, minStableAge time.Duration, now time.Time) (Candidate, bool) {
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
	if source == "" || script == "" {
		return Candidate{}, false
	}
	if minStableAge > 0 && !sourceMtime.IsZero() && now.Sub(sourceMtime) < minStableAge {
		return Candidate{}, false // still uploading
	}
	scriptType := "avs"
	if strings.HasSuffix(strings.ToLower(script), ".vpy") {
		scriptType = "vpy"
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
