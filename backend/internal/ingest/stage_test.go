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
	if len(staged) != 2 {
		t.Fatalf("expected 2 video files staged, got %v", staged)
	}
	got, err := os.ReadFile(filepath.Join(dest, "show.s01e01.mkv"))
	if err != nil || string(got) != "video-data" {
		t.Fatalf("staged content: %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "extra.srt")); !os.IsNotExist(err) {
		t.Fatal("non-video file must not be staged")
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
