package scanner

import (
	"os"
	"path/filepath"
	"testing"
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

	cands, err := Scan(root)
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

	cands, err := Scan(root)
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

	cands, _ := Scan(root)
	if len(cands) != 0 {
		t.Fatalf("folder without source must be skipped: %+v", cands)
	}
}

func TestScanPrefersCanonical1080Script(t *testing.T) {
	root := t.TempDir()
	mkEp(t, root, "S", "Ep 01", "src.mkv", "backup.avs", "1080.avs")

	cands, _ := Scan(root)
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

	cands, _ := Scan(root)
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

	cands, _ := Scan(root)
	if len(cands) != 1 {
		t.Fatalf("top-level files must be ignored: %+v", cands)
	}
}

func TestScanEmptyRoot(t *testing.T) {
	root := t.TempDir()
	cands, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("empty root: %+v", cands)
	}
}

func TestScanMissingRootErrors(t *testing.T) {
	if _, err := Scan(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error for missing root")
	}
}
