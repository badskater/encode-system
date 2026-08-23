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

// Scan walks the scripts root and returns ready episode candidates.
//
// Layout convention (legacy-compatible): <root>/<Series>/<EpisodeFolder>/ with
// the episode folder containing one source media file and one filter script.
// Only depth-2 folders are considered; deeper nesting and files at other
// levels are ignored so partial copies never trigger jobs.
//
// A candidate needs BOTH a source and a filter script — a folder mid-copy
// (source uploaded, script not yet) is skipped until the next scan.
func Scan(root string) ([]Candidate, error) {
	fsys := os.DirFS(root)

	seriesDirs, err := sortedDirs(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read scripts root %s: %w", root, err)
	}

	var out []Candidate
	for _, series := range seriesDirs {
		epDirs, err := sortedDirs(fsys, series)
		if err != nil {
			continue // unreadable series dir: skip, keep scanning others
		}
		for _, ep := range epDirs {
			c, ok, err := inspectEpisode(fsys, series, ep)
			if err != nil || !ok {
				continue
			}
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EpisodeDir < out[j].EpisodeDir })
	return out, nil
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
// The canonical "1080.*" script wins when several filter scripts exist.
func inspectEpisode(fsys fs.FS, series, ep string) (Candidate, bool, error) {
	dir := path.Join(series, ep)
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return Candidate{}, false, err
	}
	var source, script string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(path.Ext(name))
		if source == "" && isSourceExt(ext) {
			source = name
		}
		if ext != ".avs" && ext != ".vpy" {
			continue
		}
		if script == "" || strings.HasPrefix(name, "1080.") {
			script = name
		}
	}
	if source == "" || script == "" {
		return Candidate{}, false, nil
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
