//go:build linux

package flow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestZoneBuildRealBinaryChain runs scene_detect -> zone_build against the
// real ffmpeg on the authored hard-cut fixture, with a chapters.xml naming
// the middle segment "ED". Gated like the scene_detect real test.
func TestZoneBuildRealBinaryChain(t *testing.T) {
	ffmpeg := os.Getenv("ENCODE_TEST_FFMPEG")
	ffprobe := os.Getenv("ENCODE_TEST_FFPROBE")
	if ffmpeg == "" || ffprobe == "" {
		t.Skip("ENCODE_TEST_FFMPEG / ENCODE_TEST_FFPROBE not set")
	}
	root := t.TempDir()
	scriptsDir := filepath.Join(root, "scripts")
	epDir := filepath.Join(scriptsDir, "Test Series", "Ep 01")
	binDir := filepath.Join(root, "bin")
	for _, d := range []string{epDir, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range map[string]string{"ffmpeg": ffmpeg, "ffprobe": ffprobe} {
		if err := os.Symlink(target, filepath.Join(binDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	src := filepath.Join(epDir, "cuts.mp4")
	cmd := exec.Command(ffmpeg, "-hide_banner", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24:duration=3",
		"-f", "lavfi", "-i", "color=c=red:size=320x240:rate=24:duration=3",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=24:duration=3",
		"-filter_complex", "[0:v][1:v][2:v]concat=n=3:v=1:a=0",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "24", src, "-y")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("author fixture: %v\n%s", err, out)
	}
	os.WriteFile(filepath.Join(epDir, "1080.vpy"), []byte("# stub"), 0o644)
	os.WriteFile(filepath.Join(epDir, "chapters.xml"), []byte(`<?xml version="1.0" encoding="UTF-8"?>
<Chapters>
  <EditionEntry>
    <ChapterAtom>
      <ChapterTimeStart>00:00:00.000000000</ChapterTimeStart>
      <ChapterDisplay><ChapterString>Main</ChapterString></ChapterDisplay>
    </ChapterAtom>
    <ChapterAtom>
      <ChapterTimeStart>00:00:03.000000000</ChapterTimeStart>
      <ChapterDisplay><ChapterString>ED</ChapterString></ChapterDisplay>
    </ChapterAtom>
    <ChapterAtom>
      <ChapterTimeStart>00:00:06.000000000</ChapterTimeStart>
      <ChapterDisplay><ChapterString>Preview</ChapterString></ChapterDisplay>
    </ChapterAtom>
  </EditionEntry>
</Chapters>`), 0o644)
	v := Vars{BinDir: binDir, ScriptsDir: scriptsDir, ReleaseDir: filepath.Join(root, "release"), Group: "TestGroup", Tag: "1080p"}

	bdMustRun(t, bdRender(t, "scene_detect", nil, v), v)
	bdMustRun(t, bdRender(t, "zone_build", nil, v), v)
	d := readZones(t, epDir)
	// Expect: credits first/last (3s scenes), chapter ED ~[72,143],
	// chapter Preview ~[144,215]. Boundaries ±1 frame (scdet snap).
	var ed, preview, first, last bool
	for _, z := range d.Zones {
		switch {
		case z.Reason == "credits:first-scene" && z.Start == 0:
			first = true
		case z.Reason == "credits:last-scene" && z.End == 215:
			last = true
		case z.Reason == "chapter:ED":
			ed = true
			if z.Start < 71 || z.Start > 73 || z.End < 142 || z.End > 144 || z.Mode != "b" || z.Value != 0.8 {
				t.Errorf("ED zone wrong: %+v", z)
			}
		case z.Reason == "chapter:Preview":
			preview = true
			if z.Start < 143 || z.Start > 145 || z.End != 215 {
				t.Errorf("Preview zone wrong: %+v", z)
			}
		}
	}
	if !first || !last || !ed || !preview {
		t.Fatalf("missing zones (first=%v last=%v ed=%v preview=%v): %+v", first, last, ed, preview, d.Zones)
	}
	if d.X265 == "" || !strings.Contains(d.X265, "b=0.8") || !strings.Contains(d.X265, "b=0.6") {
		t.Errorf("x265 string wrong: %q", d.X265)
	}
}
