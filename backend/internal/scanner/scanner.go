// Package scanner walks the NFS-mounted scripts share and finds episode
// folders ready to encode: a source media file plus a VapourSynth/AviSynth
// filter script.
package scanner

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// SourceExtensions are media files accepted as encode sources.
var SourceExtensions = []string{".m2ts", ".ts", ".mkv", ".mp4"}

// Candidate is one episode folder ready for job creation.
type Candidate struct {
	Series     string // top-level folder name
	EpisodeDir string // path relative to the scripts root, forward slashes
	ScriptType string // "avs" or "vpy"
	ScriptFile string // filter script file name, e.g. "1080.vpy"
	SourceFile string // detected source file name
}

// Scan walks the scripts root and returns ready episode candidates plus the
// number of unreadable directories skipped (a degraded NFS subtree then shows
// up in logs instead of silently shrinking the candidate list).
//
// Layout convention (legacy-compatible): <root>/<Series>/<EpisodeFolder>/ with
// the episode folder containing one source media file and one filter script.
// Only depth-2 folders are considered; deeper nesting and files at other
// levels are ignored so partial copies never trigger jobs.
//
// A candidate needs BOTH a source and a filter script — a folder mid-copy
// (source uploaded, script not yet) is skipped until the next scan. Sources
// whose mtime is newer than minStableAge are considered still copying and are
// likewise deferred (NFS copies of large .m2ts files take a while).
func Scan(root string, minStableAge time.Duration) ([]Candidate, int, error) {
	fsys := os.DirFS(root)

	seriesDirs, err := sortedDirs(fsys, ".")
	if err != nil {
		return nil, 0, fmt.Errorf("read scripts root %s: %w", root, err)
	}

	var out []Candidate
	var skippedDirs int
	for _, series := range seriesDirs {
		epDirs, err := sortedDirs(fsys, series)
		if err != nil {
			skippedDirs++
			continue // unreadable series dir: skip, keep scanning others
		}
		for _, ep := range epDirs {
			c, ok, err := inspectEpisode(fsys, series, ep, minStableAge)
			if err != nil {
				skippedDirs++
				continue
			}
			if ok {
				out = append(out, c)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EpisodeDir < out[j].EpisodeDir })
	return out, skippedDirs, nil
}

// sortedDirs lists subdirectory names of dir, sorted for stable ordering.
func sortedDirs(fsys fs.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

// inspectEpisode checks one episode folder for source + filter script.
// Source selection prefers raw container extensions (.m2ts/.ts/.mp4) over
// .mkv so a leftover mux OUTPUT ("Series - 01 [1080p].mkv") never
// masquerades as a source. The canonical "1080.vpy" wins over other filter
// scripts, then "1080.avs", for a deterministic pick.
func inspectEpisode(fsys fs.FS, series, ep string, minStableAge time.Duration) (Candidate, bool, error) {
	dir := path.Join(series, ep)
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return Candidate{}, false, err
	}
	var source, script string
	var mkvFallback string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(path.Ext(name))
		if isSourceExt(ext) && ext != ".mkv" && source == "" {
			source = name
		} else if ext == ".mkv" && mkvFallback == "" {
			mkvFallback = name
		}
		if ext != ".avs" && ext != ".vpy" {
			continue
		}
		switch {
		case strings.HasPrefix(name, "1080.vpy"):
			script = name
		case strings.HasPrefix(name, "1080.avs") && !strings.HasPrefix(script, "1080.vpy"):
			script = name
		case script == "":
			script = name
		}
	}
	if source == "" {
		source = mkvFallback
	}
	if source == "" || script == "" {
		return Candidate{}, false, nil
	}
	// Stability gate: a source still being copied over NFS has a fresh mtime.
	if minStableAge > 0 {
		info, err := fs.Stat(fsys, path.Join(dir, source))
		if err != nil {
			return Candidate{}, false, err
		}
		if time.Since(info.ModTime()) < minStableAge {
			return Candidate{}, false, nil
		}
	}
	return Candidate{
		Series:     series,
		EpisodeDir: path.Join(series, ep),
		ScriptType: strings.TrimPrefix(path.Ext(script), "."),
		ScriptFile: script,
		SourceFile: source,
	}, true, nil
}

func isSourceExt(ext string) bool {
	for _, s := range SourceExtensions {
		if ext == s {
			return true
		}
	}
	return false
}
