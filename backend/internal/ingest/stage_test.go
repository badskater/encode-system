package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedTorrent creates a download-root layout with one torrent directory
// containing the given files (name → content). Returns the root.
func seedTorrent(t *testing.T, torrentName string, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, torrentName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestStageCopiesVideoFilesOnly proves staging copies source videos,
// flattens subdirectory paths to base names, and leaves extras behind.
// Samples are EXCLUDED (Q3): staging one lets the scanner's alphabetical
// fallback pick it over the episode.
func TestStageCopiesVideoFilesOnly(t *testing.T) {
	root := seedTorrent(t, "Show.S01E01", map[string]string{
		"show.s01e01.mkv":   "video-data",
		"subs/extra.srt":    "subtitle",
		"show.nfo":          "nfo",
		"sample/sample.mkv": "sample-video",
	})
	dest := filepath.Join(t.TempDir(), "Show", "Ep 01")
	tor := Torrent{
		Hash: "h1", Name: "Show.S01E01", SavePath: "/client/downloads/Show.S01E01",
		Files: []File{
			{Name: "show.s01e01.mkv", Size: 9},
			{Name: "subs/extra.srt", Size: 8},
			{Name: "show.nfo", Size: 3},
			{Name: "sample/sample.mkv", Size: 12},
		},
	}
	staged, err := Stage(context.Background(), root, tor, dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(staged) != 1 || staged[0] != "show.s01e01.mkv" {
		t.Fatalf("expected only the episode staged (sample excluded), got %v", staged)
	}
	got, err := os.ReadFile(filepath.Join(dest, "show.s01e01.mkv"))
	if err != nil || string(got) != "video-data" {
		t.Fatalf("staged content: %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "extra.srt")); !os.IsNotExist(err) {
		t.Fatal("non-video file must not be staged")
	}
	if _, err := os.Stat(filepath.Join(dest, "sample.mkv")); !os.IsNotExist(err) {
		t.Fatal("sample must not be staged")
	}
	// No temp residue.
	entries, _ := os.ReadDir(dest)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("temp residue: %s", e.Name())
		}
	}
}

// TestStageSkipsExisting proves a re-stage resumes: existing destination
// files are untouched and not re-copied.
func TestStageSkipsExisting(t *testing.T) {
	root := seedTorrent(t, "Show.S01E01", map[string]string{"a.mkv": "new-content"})
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "a.mkv"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	tor := Torrent{Hash: "h", SavePath: "/d/Show.S01E01", Files: []File{{Name: "a.mkv"}}}
	staged, err := Stage(context.Background(), root, tor, dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(staged) != 0 {
		t.Fatalf("existing file must be skipped, staged=%v", staged)
	}
	got, _ := os.ReadFile(filepath.Join(dest, "a.mkv"))
	if string(got) != "original" {
		t.Fatalf("existing file overwritten: %q", got)
	}
}

// TestStageMissingTorrentDir proves a download root that does not
// contain the torrent folder is a clear error (the mount-requirement
// failure mode), never a silent success.
func TestStageMissingTorrentDir(t *testing.T) {
	root := t.TempDir() // empty: torrent dir absent
	dest := t.TempDir()
	tor := Torrent{Hash: "h", SavePath: "/d/Show.S01E01", Files: []File{{Name: "a.mkv"}}}
	_, err := Stage(context.Background(), root, tor, dest)
	if err == nil || !strings.Contains(err.Error(), "not visible under download_root") {
		t.Fatalf("missing dir must error clearly, got %v", err)
	}
}

// TestStageNoVideoFiles proves a torrent with only extras is refused.
func TestStageNoVideoFiles(t *testing.T) {
	root := seedTorrent(t, "Pack", map[string]string{"a.nfo": "x"})
	tor := Torrent{Hash: "h", SavePath: "/d/Pack", Files: []File{{Name: "a.nfo"}}}
	_, err := Stage(context.Background(), root, tor, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no source video files") {
		t.Fatalf("no-video torrent must error, got %v", err)
	}
}

// TestStagePartialFailureCleanup proves a mid-batch failure removes the
// in-flight temp file, keeps already-renamed files (resume-safe), and
// returns the error.
func TestStagePartialFailureCleanup(t *testing.T) {
	root := seedTorrent(t, "Show", map[string]string{"a.mkv": "aaa"})
	// b.mkv is listed but absent on disk → second copy fails.
	dest := t.TempDir()
	tor := Torrent{Hash: "h", SavePath: "/d/Show", Files: []File{
		{Name: "a.mkv"}, {Name: "b.mkv"},
	}}
	staged, err := Stage(context.Background(), root, tor, dest)
	if err == nil {
		t.Fatal("missing second source must fail the stage")
	}
	if len(staged) != 1 || staged[0] != "a.mkv" {
		t.Fatalf("first file must stay staged for resume, got %v", staged)
	}
	if _, err := os.Stat(filepath.Join(dest, "a.mkv")); err != nil {
		t.Fatal("first file must survive the partial failure")
	}
	entries, _ := os.ReadDir(dest)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("failed copy left temp residue: %s", e.Name())
		}
	}
	// Resume: add b.mkv, re-stage → only b.mkv is copied.
	if err := os.WriteFile(filepath.Join(root, "Show", "b.mkv"), []byte("bbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	staged2, err := Stage(context.Background(), root, tor, dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(staged2) != 1 || staged2[0] != "b.mkv" {
		t.Fatalf("resume must stage only the missing file, got %v", staged2)
	}
}

// TestStageTraversalRefused proves untrusted torrent file names cannot
// escape the torrent directory or write outside the episode folder.
func TestStageTraversalRefused(t *testing.T) {
	root := seedTorrent(t, "Show", map[string]string{"a.mkv": "x"})
	// A secret outside the torrent dir that a traversal would reach.
	outside := filepath.Join(root, "secret.mkv")
	if err := os.WriteFile(outside, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../secret.mkv", "/etc/passwd.mkv", "a/../../secret.mkv"} {
		dest := t.TempDir()
		tor := Torrent{Hash: "h", SavePath: "/d/Show", Files: []File{{Name: name}}}
		_, err := Stage(context.Background(), root, tor, dest)
		if err == nil {
			t.Fatalf("traversal %q must be refused", name)
		}
		entries, _ := os.ReadDir(dest)
		if len(entries) != 0 {
			t.Fatalf("traversal %q wrote files: %v", name, entries)
		}
	}
}

// TestStageSymlinkEscapeRefused proves a symlink inside the torrent
// pointing outside is refused (real-path containment, P6 lesson).
func TestStageSymlinkEscapeRefused(t *testing.T) {
	root := seedTorrent(t, "Show", map[string]string{"real.mkv": "x"})
	outside := filepath.Join(root, "outside.mkv")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "Show", "link.mkv")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	dest := t.TempDir()
	tor := Torrent{Hash: "h", SavePath: "/d/Show", Files: []File{{Name: "link.mkv"}}}
	_, err := Stage(context.Background(), root, tor, dest)
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("symlink escape must be refused, got %v", err)
	}
}

// TestStageSymlinkedDirEscapeRefused proves a torrent directory that is
// a symlink resolving OUTSIDE download_root is refused: containment must
// be checked against the resolved download root, not only against the
// resolved torrent dir (which trivially contains its own escaped files).
func TestStageSymlinkedDirEscapeRefused(t *testing.T) {
	root := t.TempDir()    // download_root
	outside := t.TempDir() // separate tree outside the root
	if err := os.WriteFile(filepath.Join(outside, "a.mkv"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "Show")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	dest := t.TempDir()
	tor := Torrent{Hash: "h", SavePath: "/d/Show", Files: []File{{Name: "a.mkv"}}}
	_, err := Stage(context.Background(), root, tor, dest)
	if err == nil || !strings.Contains(err.Error(), "outside download_root") {
		t.Fatalf("symlinked dir escaping download_root must be refused, got %v", err)
	}
	entries, _ := os.ReadDir(dest)
	if len(entries) != 0 {
		t.Fatalf("escaped stage wrote files: %v", entries)
	}
}

// TestStageSymlinkedDirInsideRootAllowed is the positive control: a
// torrent dir symlinked to another location INSIDE download_root still
// stages (the containment fix must not break legitimate symlinks).
func TestStageSymlinkedDirInsideRootAllowed(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real", "Show")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "a.mkv"), []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(root, "Show")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	dest := t.TempDir()
	tor := Torrent{Hash: "h", SavePath: "/d/Show", Files: []File{{Name: "a.mkv"}}}
	staged, err := Stage(context.Background(), root, tor, dest)
	if err != nil {
		t.Fatalf("in-root symlinked dir must stage: %v", err)
	}
	if len(staged) != 1 || staged[0] != "a.mkv" {
		t.Fatalf("staged = %v", staged)
	}
	got, err := os.ReadFile(filepath.Join(dest, "a.mkv"))
	if err != nil || string(got) != "video" {
		t.Fatalf("staged content: %q err=%v", got, err)
	}
}

// TestStageCancelledContext proves a cancelled ctx stops staging.
func TestStageCancelledContext(t *testing.T) {
	root := seedTorrent(t, "Show", map[string]string{"a.mkv": "x"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tor := Torrent{Hash: "h", SavePath: "/d/Show", Files: []File{{Name: "a.mkv"}}}
	_, err := Stage(ctx, root, tor, t.TempDir())
	if err == nil {
		t.Fatal("cancelled ctx must abort staging")
	}
}

// TestStageEmptySavePath proves an empty/unusable save path is refused.
func TestStageEmptySavePath(t *testing.T) {
	for _, sp := range []string{"", "/", "."} {
		tor := Torrent{Hash: "h", SavePath: sp, Files: []File{{Name: "a.mkv"}}}
		if _, err := Stage(context.Background(), t.TempDir(), tor, t.TempDir()); err == nil {
			t.Fatalf("save path %q must be refused", sp)
		}
	}
}

// TestStageTOCTOUSwapRig reproduces the reviewer's containment race (Q2):
// a goroutine swaps a RESOLVED path component (real dir → symlink outside
// download_root) between the pre-open EvalSymlinks check and os.Open.
// Post-open verification of the OPENED file's final path must hold the
// boundary: 400 iterations, ZERO leaks of outside content. A swap AFTER
// open holds the original inode and is safe.
func TestStageTOCTOUSwapRig(t *testing.T) {
	root := t.TempDir()    // download_root
	outside := t.TempDir() // attacker-controlled tree OUTSIDE root
	if err := os.WriteFile(filepath.Join(outside, "a.mkv"), []byte("SECRET-OUTSIDE"), 0o644); err != nil {
		t.Fatal(err)
	}
	show := filepath.Join(root, "Show")
	real := filepath.Join(show, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "a.mkv"), []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Symlink support probe (the rig is meaningless without it).
	probe := filepath.Join(t.TempDir(), "probe-link")
	if err := os.Symlink(outside, probe); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		bak := real + ".bak"
		for {
			select {
			case <-stop:
				return
			default:
			}
			if os.Rename(real, bak) == nil {
				if os.Symlink(outside, real) == nil {
					os.Remove(real)
				}
				os.Rename(bak, real)
			}
		}
	}()

	tor := Torrent{Hash: "h", SavePath: "/d/Show", Files: []File{{Name: "real/a.mkv"}}}
	leaked := 0
	for i := 0; i < 400; i++ {
		dest := filepath.Join(t.TempDir(), "ep")
		staged, err := Stage(context.Background(), root, tor, dest)
		if err != nil {
			continue // refused swap window — correct outcome
		}
		if len(staged) != 1 {
			t.Fatalf("iteration %d: staged=%v", i, staged)
		}
		b, err := os.ReadFile(filepath.Join(dest, staged[0]))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "video" {
			leaked++
		}
	}
	if leaked > 0 {
		t.Fatalf("TOCTOU: %d/400 iterations leaked OUTSIDE content into the episode folder", leaked)
	}
}

// TestStageSampleExclusion pins Q3: samples are never staged — a 'sample'
// directory segment (any case) or a basename starting with 'sample' (any
// case) is skipped, so the scanner's alphabetical source fallback can
// never pick a sample over the episode.
func TestStageSampleExclusion(t *testing.T) {
	root := seedTorrent(t, "Show.S01E01", map[string]string{
		"episode.mkv":        "episode",
		"sample/sample.mkv":  "sample-dir",
		"Sample.mkv":         "sample-base",
		"sample-show.mkv":    "sample-prefix",
		"SAMPLE/Sample2.mkv": "sample-case",
		"subs/extra.srt":     "subtitle",
	})
	dest := t.TempDir()
	tor := Torrent{Hash: "h", Name: "Show.S01E01", SavePath: "/d/Show.S01E01", Files: []File{
		{Name: "episode.mkv"}, {Name: "sample/sample.mkv"}, {Name: "Sample.mkv"},
		{Name: "sample-show.mkv"}, {Name: "SAMPLE/Sample2.mkv"}, {Name: "subs/extra.srt"},
	}}
	staged, err := Stage(context.Background(), root, tor, dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(staged) != 1 || staged[0] != "episode.mkv" {
		t.Fatalf("only the episode must stage, got %v", staged)
	}
	entries, _ := os.ReadDir(dest)
	if len(entries) != 1 {
		t.Fatalf("dest must hold exactly the episode: %v", entries)
	}
}

// TestStageSingleFileAtRoot pins Q5: a single-file torrent saves its file
// DIRECTLY at the client save root (qBit save_path=root; ruTorrent
// savePath=Dir(basePath)). The basename mapping must fall back to
// downloadRoot/<filename> instead of looking for a nonexistent
// downloadRoot/<root-basename> directory.
func TestStageSingleFileAtRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "movie.mkv"), []byte("movie"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	tor := Torrent{Hash: "h", Name: "Movie.2024.1080p", SavePath: "/downloads",
		Files: []File{{Name: "movie.mkv", Size: 5}}}
	staged, err := Stage(context.Background(), root, tor, dest)
	if err != nil {
		t.Fatalf("single-file torrent must stage under the flat mount: %v", err)
	}
	if len(staged) != 1 || staged[0] != "movie.mkv" {
		t.Fatalf("staged=%v", staged)
	}
	b, _ := os.ReadFile(filepath.Join(dest, "movie.mkv"))
	if string(b) != "movie" {
		t.Fatalf("content=%q", b)
	}
	// Negative control: the file is NOT at the root → clear mount error.
	tor2 := Torrent{Hash: "h2", SavePath: "/downloads", Files: []File{{Name: "absent.mkv"}}}
	if _, err := Stage(context.Background(), root, tor2, t.TempDir()); err == nil {
		t.Fatal("missing single-file source must error")
	}
}

// TestStageMultiCDDistinctNames pins Q10: flattening must not silently
// drop same-named files from different subdirectories (multi-CD
// releases). When the relative path has depth, the destination base name
// is prefixed with its parent directories (cd1_a.mkv, cd2_a.mkv).
func TestStageMultiCDDistinctNames(t *testing.T) {
	root := seedTorrent(t, "Show", map[string]string{
		"cd1/a.mkv":       "CD1",
		"cd2/a.mkv":       "CD2",
		"disc3/sub/b.mkv": "DEEP",
	})
	dest := t.TempDir()
	tor := Torrent{Hash: "h", SavePath: "/d/Show", Files: []File{
		{Name: "cd1/a.mkv"}, {Name: "cd2/a.mkv"}, {Name: "disc3/sub/b.mkv"},
	}}
	staged, err := Stage(context.Background(), root, tor, dest)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"cd1_a.mkv": "CD1", "cd2_a.mkv": "CD2", "disc3_sub_b.mkv": "DEEP"}
	if len(staged) != len(want) {
		t.Fatalf("staged=%v want %v", staged, want)
	}
	for name, content := range want {
		b, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || string(b) != content {
			t.Fatalf("%s: %q err=%v", name, b, err)
		}
	}
}
