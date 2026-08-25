package agent

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func mkTestZip(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractBinZipHappyPath(t *testing.T) {
	dir := t.TempDir()
	data := mkTestZip(t, map[string][]byte{
		"x265_x64.exe":    []byte("x265-bytes"),
		"subdir/tool.dll": []byte("dll-bytes"),
	})
	if err := extractBinZip(data, dir); err != nil {
		t.Fatalf("extract: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "x265_x64.exe"))
	if err != nil || string(b) != "x265-bytes" {
		t.Fatalf("x265 content wrong: %v %q", err, b)
	}
	b, err = os.ReadFile(filepath.Join(dir, "subdir", "tool.dll"))
	if err != nil || string(b) != "dll-bytes" {
		t.Fatalf("nested content wrong: %v %q", err, b)
	}
}

func TestExtractBinZipRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	for _, evil := range []string{"../escape.txt", `..\escape2.txt`, "a/../../up.txt", "/abs.txt"} {
		data := mkTestZip(t, map[string][]byte{evil: []byte("payload")})
		if err := extractBinZip(data, dir); err == nil {
			t.Errorf("entry %q: extraction should have failed", evil)
		}
		// Nothing may have landed outside the dest dir.
		if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.txt")); err == nil {
			t.Errorf("entry %q escaped the bin dir", evil)
		}
	}
}

func TestExtractBinZipOverwrites(t *testing.T) {
	dir := t.TempDir()
	// Pre-existing file with different content gets replaced.
	if err := os.WriteFile(filepath.Join(dir, "tool.exe"), []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := mkTestZip(t, map[string][]byte{"tool.exe": []byte("new")})
	if err := extractBinZip(data, dir); err != nil {
		t.Fatalf("extract: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "tool.exe"))
	if string(b) != "new" {
		t.Fatalf("overwrite failed: %q", b)
	}
}

func TestSafeRelPath(t *testing.T) {
	bad := []string{"", "/x", `C:\x`, `\\server\x`, "a/../b", ".."}
	for _, p := range bad {
		if err := safeRelPath(p); err == nil {
			t.Errorf("%q should be rejected", p)
		}
	}
	good := []string{"tool.exe", "a/b/c.dll", "a.exe.tmp"}
	for _, p := range good {
		if err := safeRelPath(p); err != nil {
			t.Errorf("%q wrongly rejected: %v", p, err)
		}
	}
}
