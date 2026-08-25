package scanner

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mkEp creates <root>/<series>/<ep>/ with the given files.
func mkEp(t *testing.T, root, series, ep string, files ...string) {
	t.Helper()
	dir := filepath.Join(root, series, ep)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScanFindsReadyEpisode(t *testing.T) {
	root := t.TempDir()
	mkEp(t, root, "Ookami-san", "Ep 01", "src.m2ts", "1080.vpy")

	cands, _, err := Scan(root, 0)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(cands), cands)
	}
	c := cands[0]
	if c.Series != "Ookami-san" || c.EpisodeDir != "Ookami-san/Ep 01" {
		t.Errorf("wrong identity: %+v", c)
	}
	if c.ScriptType != "vpy" || c.ScriptFile != "1080.vpy" || c.SourceFile != "src.m2ts" {
		t.Errorf("wrong detection: %+v", c)
	}
}

func TestScanSkipsFolderMissingScript(t *testing.T) {
	root := t.TempDir()
	mkEp(t, root, "S", "Ep 01", "src.m2ts") // no filter script yet (mid-copy)

	cands, _, err := Scan(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("folder without script must be skipped: %+v", cands)
	}
}

func TestScanSkipsFolderMissingSource(t *testing.T) {
	root := t.TempDir()
	mkEp(t, root, "S", "Ep 01", "1080.avs") // script only, source not copied

	cands, _, _ := Scan(root, 0)
	if len(cands) != 0 {
		t.Fatalf("folder without source must be skipped: %+v", cands)
	}
}

func TestScanPrefersCanonical1080Script(t *testing.T) {
	root := t.TempDir()
	mkEp(t, root, "S", "Ep 01", "src.mkv", "backup.avs", "1080.avs")

	cands, _, _ := Scan(root, 0)
	if len(cands) != 1 {
		t.Fatalf("want 1 candidate: %+v", cands)
	}
	if cands[0].ScriptFile != "1080.avs" {
		t.Errorf("expected 1080.avs preferred, got %s", cands[0].ScriptFile)
	}
}

func TestScanMultipleSeriesSorted(t *testing.T) {
	root := t.TempDir()
	mkEp(t, root, "Zebra", "Ep 02", "src.ts", "1080.vpy")
	mkEp(t, root, "Alpha", "Ep 01", "src.m2ts", "1080.avs")

	cands, _, _ := Scan(root, 0)
	if len(cands) != 2 {
		t.Fatalf("want 2 candidates: %+v", cands)
	}
	if cands[0].Series != "Alpha" || cands[1].Series != "Zebra" {
		t.Errorf("expected sorted order: %+v", cands)
	}
	if cands[1].ScriptType != "vpy" {
		t.Errorf("vpy detection failed: %+v", cands[1])
	}
}

func TestScanIgnoresTopLevelFiles(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "stray.m2ts"), []byte("x"), 0o644)
	mkEp(t, root, "S", "Ep 01", "src.m2ts", "1080.vpy")

	cands, _, _ := Scan(root, 0)
	if len(cands) != 1 {
		t.Fatalf("top-level files must be ignored: %+v", cands)
	}
}

func TestScanEmptyRoot(t *testing.T) {
	root := t.TempDir()
	cands, _, err := Scan(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("empty root: %+v", cands)
	}
}

func TestScanMissingRootErrors(t *testing.T) {
	if _, _, err := Scan(filepath.Join(t.TempDir(), "nope"), 0); err == nil {
		t.Fatal("expected error for missing root")
	}
}

// Regression: a source modified within the stability window is deferred so
// jobs are never created against a file still copying over NFS.
func TestScanDefersUnstableSource(t *testing.T) {
	root := t.TempDir()
	mkEp(t, root, "S", "Ep 01", "src.m2ts", "1080.vpy") // mtime = now

	cands, _, err := Scan(root, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("fresh source must be deferred: %+v", cands)
	}

	// Age the source beyond the window -> becomes ready.
	past := time.Now().Add(-10 * time.Minute)
	os.Chtimes(filepath.Join(root, "S", "Ep 01", "src.m2ts"), past, past)
	cands, _, err = Scan(root, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("aged source must be ready: %+v", cands)
	}
}

// Regression: a leftover mux output .mkv must not be mistaken for a source
// when the real source is gone... but a lone .mkv is still accepted as a
// fallback source (some workflows encode from MKV containers).
func TestScanSourceSelectionPriority(t *testing.T) {
	root := t.TempDir()
	// Folder with both a real .m2ts source and a mux-output .mkv: the .m2ts
	// must win as the source.
	mkEp(t, root, "S", "Ep 01", "src.m2ts", "S - 01 [1080p].mkv", "1080.vpy")
	cands, _, _ := Scan(root, 0)
	if len(cands) != 1 || cands[0].SourceFile != "src.m2ts" {
		t.Fatalf(".m2ts must outrank mux output .mkv: %+v", cands)
	}

	// Folder with only an .mkv and a script: accepted as fallback source.
	mkEp(t, root, "S", "Ep 02", "source.mkv", "1080.vpy")
	cands, _, _ = Scan(root, 0)
	if len(cands) != 2 {
		t.Fatalf("want 2 candidates: %+v", cands)
	}
}

// Regression: 1080.vpy beats other scripts deterministically.
func TestScanScriptPreference(t *testing.T) {
	root := t.TempDir()
	mkEp(t, root, "S", "Ep 01", "src.m2ts", "1080.avs", "1080.vpy", "extra.avs")
	cands, _, _ := Scan(root, 0)
	if len(cands) != 1 || cands[0].ScriptFile != "1080.vpy" || cands[0].ScriptType != "vpy" {
		t.Fatalf("1080.vpy must win: %+v", cands)
	}
}

// 4K support: 2160 scripts are recognized and outrank 1080 ones; within the
// same resolution VapourSynth wins over AviSynth.
func TestScanScriptPreference2160(t *testing.T) {
	root := t.TempDir()
	// 2160.vpy present alongside everything else -> wins outright.
	mkEp(t, root, "S", "Ep 01", "src.m2ts", "1080.vpy", "1080.avs", "2160.avs", "2160.vpy")
	cands, _, _ := Scan(root, 0)
	if len(cands) != 1 || cands[0].ScriptFile != "2160.vpy" || cands[0].ScriptType != "vpy" {
		t.Fatalf("2160.vpy must win: %+v", cands)
	}

	// 2160.avs with 1080 candidates -> the 4K script wins.
	mkEp(t, root, "S", "Ep 02", "src.m2ts", "1080.vpy", "2160.avs")
	cands, _, _ = Scan(root, 0)
	if len(cands) != 2 || cands[1].ScriptFile != "2160.avs" || cands[1].ScriptType != "avs" {
		t.Fatalf("2160.avs must win over 1080.vpy: %+v", cands)
	}

	// No 2160 script -> legacy behavior (1080.vpy wins).
	mkEp(t, root, "S", "Ep 03", "src.m2ts", "1080.vpy", "1080.avs")
	cands, _, _ = Scan(root, 0)
	if len(cands) != 3 || cands[2].ScriptFile != "1080.vpy" {
		t.Fatalf("1080.vpy must stay the 1080 pick: %+v", cands)
	}
}

// Regression: skipped/unreadable dirs are reported, not swallowed silently.
func TestScanReportsSkippedDirs(t *testing.T) {
	root := t.TempDir()
	mkEp(t, root, "S", "Ep 01", "src.m2ts", "1080.vpy")
	bad := filepath.Join(root, "Broken")
	os.MkdirAll(filepath.Join(bad, "Ep 01"), 0o755)
	os.Chmod(bad, 0o000)
	t.Cleanup(func() { os.Chmod(bad, 0o755) })

	cands, skipped, err := Scan(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("good series must survive: %+v", cands)
	}
	if skipped == 0 {
		t.Fatal("unreadable series dir must be counted as skipped")
	}
}
