//go:build linux

package flow

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSceneDetectRealBinaryLinuxEndToEnd runs the RENDERED bash step against
// the real ffmpeg/ffprobe on a synthetic video with authored hard cuts.
// Gated: ENCODE_TEST_FFMPEG + ENCODE_TEST_FFPROBE must point at real
// binaries (the dev host keeps them under the Hermes tools dir; CI skips).
//
// Fixture: three 3s segments @24fps (testsrc2 / red color / testsrc)
// concatenated -> hard cuts at frames 72 and 144, 216 frames total.
func TestSceneDetectRealBinaryLinuxEndToEnd(t *testing.T) {
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
	v := Vars{BinDir: binDir, ScriptsDir: scriptsDir, ReleaseDir: filepath.Join(root, "release"), Group: "TestGroup", Tag: "1080p"}

	bdMustRun(t, bdRender(t, "scene_detect", nil, v), v)
	d := readScenes(t, epDir)
	if d.FPS != 24 || d.FrameCount != 216 {
		t.Fatalf("probe fields wrong: fps=%v frames=%d", d.FPS, d.FrameCount)
	}
	if len(d.Scenes) != 3 {
		t.Fatalf("want 3 scenes from the authored cuts, got %+v", d.Scenes)
	}
	// Hard cuts at 72/144; scdet may land ±1 frame on the boundary.
	for i, want := range []int64{72, 144} {
		got := d.Scenes[i+1].StartFrame
		if got < want-1 || got > want+1 {
			t.Errorf("scene %d boundary %d outside ±1 of %d", i+2, got, want)
		}
	}
	if d.Scenes[0].StartFrame != 0 || d.Scenes[2].EndFrame != 215 {
		t.Errorf("outer bounds wrong: %+v", d.Scenes)
	}
	if _, err := os.Stat(filepath.Join(epDir, "scdet.log")); err != nil {
		t.Errorf("scdet.log evidence missing: %v", err)
	}

	// min_scene_len=100 merges the first short scene (72 < 100), keeps 144.
	os.Remove(filepath.Join(epDir, "scenes.json"))
	bdMustRun(t, bdRender(t, "scene_detect", map[string]string{"min_scene_len": "100"}, v), v)
	d2 := readScenes(t, epDir)
	if len(d2.Scenes) != 2 || d2.Scenes[1].StartFrame < 143 || d2.Scenes[1].StartFrame > 145 {
		t.Fatalf("min_scene_len merge on real footage wrong: %+v", d2.Scenes)
	}
}
