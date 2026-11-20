// Package scanner walks the NFS-mounted scripts share and finds episode
// folders ready to encode: a source media file plus a VapourSynth/AviSynth
// filter script.
//
// Series can assign a default filter script (script_type/script_template).
// The scan loop materializes _templates/<Series>/<template> into each
// detected episode that lacks its own script (placeholder substitution;
// an existing per-episode script always wins). _templates is a reserved
// top-level folder, never scanned as a series.
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
	return scanFs(root, minStableAge, true)
}

// ScanSourceOnly returns episodes with a stable source but NO filter
// script — the materialization candidates for series with a script
// assignment. Episodes that already carry a script are excluded (the
// normal Scan owns them). Same layout/stability rules as Scan.
func ScanSourceOnly(root string, minStableAge time.Duration) ([]Candidate, int, error) {
	cands, skipped, err := scanFs(root, minStableAge, false)
	if err != nil {
		return nil, skipped, err
	}
	var out []Candidate
	for _, c := range cands {
		if c.ScriptFile == "" {
			out = append(out, c)
		}
	}
	return out, skipped, nil
}

func scanFs(root string, minStableAge time.Duration, requireScript bool) ([]Candidate, int, error) {
	fsys := os.DirFS(root)

	seriesDirs, err := sortedDirs(fsys, ".")
	if err != nil {
		return nil, 0, fmt.Errorf("read scripts root %s: %w", root, err)
	}

	var out []Candidate
	var skippedDirs int
	for _, series := range seriesDirs {
		if series == TemplateDirName {
			continue // reserved: per-series script templates, not a series
		}
		epDirs, err := sortedDirs(fsys, series)
		if err != nil {
			skippedDirs++
			continue // unreadable series dir: skip, keep scanning others
		}
		for _, ep := range epDirs {
			c, ok, err := inspectEpisodeOpts(fsys, series, ep, minStableAge, requireScript)
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
// masquerades as a source. Filter script priority (deterministic pick):
// 2160.vpy > 2160.avs > 1080.vpy > 1080.avs > any other .avs/.vpy, so an
// explicitly-authored 4K script wins over a legacy 1080 one, and VapourSynth
// wins over AviSynth at the same resolution.
func inspectEpisode(fsys fs.FS, series, ep string, minStableAge time.Duration) (Candidate, bool, error) {
	return inspectEpisodeOpts(fsys, series, ep, minStableAge, true)
}

// inspectEpisodeOpts is inspectEpisode with a script-requirement switch.
// requireScript=false (ScanSourceOnly) accepts scriptless episodes and
// lets the BDMV disc-layout fallback qualify without a script — the series
// template materializes later, and bd_merge produces the flat source.
func inspectEpisodeOpts(fsys fs.FS, series, ep string, minStableAge time.Duration, requireScript bool) (Candidate, bool, error) {
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
	// Blu-ray disc layout fallback: no flat media file, but a BDMV folder
	// with playlists AND streams. The bd_merge/bd_split steps resolve the
	// actual playlist at runtime; SourceFile reports the first (sorted)
	// playlist for display. A flat source always wins over the disc layout.
	var bdmvNewest time.Time
	if source == "" && (script != "" || !requireScript) {
		pl, newest, ok, err := bdmvSource(fsys, dir)
		if err != nil {
			return Candidate{}, false, err
		}
		if ok {
			source = pl
			bdmvNewest = newest
		}
	}
	if source == "" || (requireScript && script == "") {
		return Candidate{}, false, nil
	}
	// Stability gate: a source still being copied over NFS has a fresh mtime.
	if minStableAge > 0 {
		if !bdmvNewest.IsZero() {
			// Disc layout: the newest playlist/stream mtime represents the
			// copy progress (the big stream files land last).
			if time.Since(bdmvNewest) < minStableAge {
				return Candidate{}, false, nil
			}
		} else {
			info, err := fs.Stat(fsys, path.Join(dir, source))
			if err != nil {
				return Candidate{}, false, err
			}
			if time.Since(info.ModTime()) < minStableAge {
				return Candidate{}, false, nil
			}
		}
	}
	scriptType := strings.TrimPrefix(path.Ext(script), ".")
	return Candidate{
		Series:     series,
		EpisodeDir: path.Join(series, ep),
		ScriptType: scriptType,
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

// bdmvSource detects a Blu-ray disc layout inside an episode folder:
// BDMV/PLAYLIST/*.mpls plus BDMV/STREAM/*.m2ts (spec-uppercase directories,
// case-insensitive extensions). Returns the first sorted playlist's path
// relative to the episode dir and the newest mtime across all playlist and
// stream files (the stability signal — a disc mid-copy has fresh streams).
// A missing BDMV folder is not an error: ok=false lets the caller skip.
func bdmvSource(fsys fs.FS, dir string) (string, time.Time, bool, error) {
	plEntries, err := fs.ReadDir(fsys, path.Join(dir, "BDMV", "PLAYLIST"))
	if err != nil {
		return "", time.Time{}, false, nil
	}
	stEntries, err := fs.ReadDir(fsys, path.Join(dir, "BDMV", "STREAM"))
	if err != nil {
		return "", time.Time{}, false, nil
	}
	var playlists, streams []string
	for _, e := range plEntries {
		if !e.IsDir() && strings.EqualFold(path.Ext(e.Name()), ".mpls") {
			playlists = append(playlists, e.Name())
		}
	}
	for _, e := range stEntries {
		if !e.IsDir() && strings.EqualFold(path.Ext(e.Name()), ".m2ts") {
			streams = append(streams, e.Name())
		}
	}
	if len(playlists) == 0 || len(streams) == 0 {
		return "", time.Time{}, false, nil
	}
	sort.Strings(playlists)
	sort.Strings(streams)
	var newest time.Time
	for _, rel := range append(
		[]string{path.Join("BDMV", "PLAYLIST", playlists[0])},
		streamFiles(streams)...,
	) {
		info, err := fs.Stat(fsys, path.Join(dir, rel))
		if err != nil {
			return "", time.Time{}, false, err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	// All playlists matter for the stability signal too (a fresh playlist
	// rewrite means the disc is still being authored/copied).
	for _, pl := range playlists[1:] {
		info, err := fs.Stat(fsys, path.Join(dir, "BDMV", "PLAYLIST", pl))
		if err != nil {
			return "", time.Time{}, false, err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return path.Join("BDMV", "PLAYLIST", playlists[0]), newest, true, nil
}

// streamFiles maps stream base names to their BDMV/STREAM relative paths.
func streamFiles(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, path.Join("BDMV", "STREAM", n))
	}
	return out
}
