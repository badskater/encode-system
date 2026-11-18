package scanner

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mkBdmvEp creates <root>/<series>/<ep>/ with a Blu-ray disc layout
// (BDMV/PLAYLIST + BDMV/STREAM) plus the given flat files (filter script).
// Playlist and stream names are paired: 00000.mpls ↔ 00000.m2ts.
func mkBdmvEp(t *testing.T, root, series, ep string, clips int, flat ...string) string {
	t.Helper()
	dir := filepath.Join(root, series, ep)
	for _, sub := range []string{"BDMV/PLAYLIST", "BDMV/STREAM"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < clips; i++ {
		pl := filepath.Join(dir, "BDMV", "PLAYLIST", fmt.Sprintf("%05d.mpls", i))
		st := filepath.Join(dir, "BDMV", "STREAM", fmt.Sprintf("%05d.m2ts", i))
		if err := os.WriteFile(pl, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(st, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range flat {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestScanBDMVEpisode: a disc-layout episode (no flat media file) is a
// candidate once it has a filter script; SourceFile points at the playlist.
func TestScanBDMVEpisode(t *testing.T) {
	root := t.TempDir()
	mkBdmvEp(t, root, "Show", "Ep 01", 1, "1080.vpy")

	cands, _, err := Scan(root, 0)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(cands), cands)
	}
	c := cands[0]
	if c.Series != "Show" || c.EpisodeDir != "Show/Ep 01" {
		t.Errorf("wrong identity: %+v", c)
	}
	if c.ScriptType != "vpy" || c.ScriptFile != "1080.vpy" {
		t.Errorf("wrong script detection: %+v", c)
	}
	if c.SourceFile != "BDMV/PLAYLIST/00000.mpls" {
		t.Errorf("SourceFile must be the playlist rel path, got %q", c.SourceFile)
	}
}

// TestScanFlatSourceBeatsBDMV: an explicit flat source wins over the disc
// layout (the flat file is the already-extracted episode source).
func TestScanFlatSourceBeatsBDMV(t *testing.T) {
	root := t.TempDir()
	mkBdmvEp(t, root, "Show", "Ep 01", 1, "src.m2ts", "1080.vpy")

	cands, _, err := Scan(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].SourceFile != "src.m2ts" {
		t.Fatalf("flat source must outrank BDMV: %+v", cands)
	}
}

// TestScanBDMVNeedsStream: playlists without any stream file are a partial
// copy — not a candidate.
func TestScanBDMVNeedsStream(t *testing.T) {
	root := t.TempDir()
	dir := mkBdmvEp(t, root, "Show", "Ep 01", 1, "1080.vpy")
	if err := os.Remove(filepath.Join(dir, "BDMV", "STREAM", "00000.m2ts")); err != nil {
		t.Fatal(err)
	}

	cands, _, _ := Scan(root, 0)
	if len(cands) != 0 {
		t.Fatalf("BDMV without streams must be skipped: %+v", cands)
	}
}

// TestScanBDMVNeedsScript: same mid-copy rule as flat sources.
func TestScanBDMVNeedsScript(t *testing.T) {
	root := t.TempDir()
	mkBdmvEp(t, root, "Show", "Ep 01", 1)

	cands, _, _ := Scan(root, 0)
	if len(cands) != 0 {
		t.Fatalf("BDMV without script must be skipped: %+v", cands)
	}
}

// TestScanBDMVStabilityGate: a stream file still being copied (fresh mtime)
// defers the whole disc episode, like a fresh flat source.
func TestScanBDMVStabilityGate(t *testing.T) {
	root := t.TempDir()
	dir := mkBdmvEp(t, root, "Show", "Ep 01", 2, "1080.vpy")
	fresh := filepath.Join(dir, "BDMV", "STREAM", "00001.m2ts")
	now := time.Now()
	if err := os.Chtimes(fresh, now, now); err != nil {
		t.Fatal(err)
	}

	cands, _, _ := Scan(root, time.Hour)
	if len(cands) != 0 {
		t.Fatalf("fresh BDMV stream must defer the episode: %+v", cands)
	}
	// Once stable, the same layout is picked up.
	old := now.Add(-2 * time.Hour)
	for _, f := range []string{
		filepath.Join(dir, "BDMV", "STREAM", "00000.m2ts"),
		filepath.Join(dir, "BDMV", "STREAM", "00001.m2ts"),
		filepath.Join(dir, "BDMV", "PLAYLIST", "00000.mpls"),
		filepath.Join(dir, "BDMV", "PLAYLIST", "00001.mpls"),
	} {
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}
	cands, _, _ = Scan(root, time.Hour)
	if len(cands) != 1 {
		t.Fatalf("stable BDMV episode must be found: %+v", cands)
	}
}

// TestScanBDMVMultiClipPlaylistPick: several playlists → deterministic
// first (sorted) playlist is reported; the bd steps resolve the actual
// playlist at runtime (param or single-candidate rule).
func TestScanBDMVMultiClipPlaylistPick(t *testing.T) {
	root := t.TempDir()
	mkBdmvEp(t, root, "Show", "Ep 01", 3, "1080.vpy")

	cands, _, _ := Scan(root, 0)
	if len(cands) != 1 || cands[0].SourceFile != "BDMV/PLAYLIST/00000.mpls" {
		t.Fatalf("want first sorted playlist: %+v", cands)
	}
}
