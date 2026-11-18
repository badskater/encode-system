//go:build linux

package flow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// REAL-BINARY end-to-end proof for the bd steps on Linux (the user's hard
// requirement). Gated like the Postgres tests: set ENCODE_TEST_TSMUXER_BIN
// to a real tsMuxeR CLI (teaching-droid/tsMuxer v3.1.0 verified) and have
// ffmpeg/ffprobe available (ENCODE_TEST_FFMPEG / ENCODE_TEST_FFPROBE or
// PATH). Optional ENCODE_TEST_TSMUXER_LIB points at a directory of shared
// libraries (libfreetype/libpng/libbrotli) which is copied into the
// BinDir/tsMuxeR-lib bundled-libs layout the steps support.
//
// The test AUTHORS a synthetic Blu-ray (ffmpeg testsrc2 with 1s GOPs +
// AC-3, tsMuxeR --blu-ray --custom-chapters at 0/10/20s), then executes
// the RENDERED bash job scripts against it with the real binary:
//
//	bd_merge        → src.m2ts (~30s) + chapters.xml (3 marks)
//	bd_split        → chapter-01..03.m2ts (~10s each), all decode clean
//	bd_split range  → src.m2ts (~10s, chapter 2 only) + rebased chapters.xml
//
// Run on the dev host:
//
//	ENCODE_TEST_TSMUXER_BIN=<probe>/squashfs-root/usr/bin/tsMuxeR \
//	ENCODE_TEST_TSMUXER_LIB=<probe>/libs/usr/lib/x86_64-linux-gnu \
//	ENCODE_TEST_FFMPEG=<ffmpeg-dir>/ffmpeg ENCODE_TEST_FFPROBE=<ffmpeg-dir>/ffprobe \
//	go test ./internal/flow/ -run TestBdRealBinary -v
func TestBdRealBinaryLinuxEndToEnd(t *testing.T) {
	tsBin := os.Getenv("ENCODE_TEST_TSMUXER_BIN")
	if tsBin == "" {
		t.Skip("ENCODE_TEST_TSMUXER_BIN not set; skipping real-binary bd test")
	}
	ffmpeg := os.Getenv("ENCODE_TEST_FFMPEG")
	if ffmpeg == "" {
		var err error
		ffmpeg, err = exec.LookPath("ffmpeg")
		if err != nil {
			t.Skip("ffmpeg not available (set ENCODE_TEST_FFMPEG); skipping")
		}
	}
	ffprobe := os.Getenv("ENCODE_TEST_FFPROBE")
	if ffprobe == "" {
		cand := filepath.Join(filepath.Dir(ffmpeg), "ffprobe")
		if _, err := os.Stat(cand); err == nil {
			ffprobe = cand
		} else {
			var err error
			ffprobe, err = exec.LookPath("ffprobe")
			if err != nil {
				t.Skip("ffprobe not available (set ENCODE_TEST_FFPROBE); skipping")
			}
		}
	}
	if _, err := os.Stat(tsBin); err != nil {
		t.Fatalf("ENCODE_TEST_TSMUXER_BIN %s: %v", tsBin, err)
	}

	root := t.TempDir()
	scriptsDir := filepath.Join(root, "scripts")
	epDir := filepath.Join(scriptsDir, "Test Series", "Ep 01")
	binDir := filepath.Join(root, "bin")
	work := filepath.Join(root, "work")
	for _, d := range []string{epDir, binDir, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// BinDir layout: real binary + optional bundled libs (the step prepends
	// tsMuxeR-lib to LD_LIBRARY_PATH for BinDir invocations).
	copyFile(t, tsBin, filepath.Join(binDir, "tsMuxeR"), 0o755)
	libSrc := os.Getenv("ENCODE_TEST_TSMUXER_LIB")
	if libSrc != "" {
		libDst := filepath.Join(binDir, "tsMuxeR-lib")
		if err := os.MkdirAll(libDst, 0o755); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(libSrc)
		if err != nil {
			t.Fatalf("ENCODE_TEST_TSMUXER_LIB: %v", err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.Contains(e.Name(), ".so") {
				continue
			}
			copyFile(t, filepath.Join(libSrc, e.Name()), filepath.Join(libDst, e.Name()), 0o755)
		}
	}

	// --- Author the synthetic disc (real tsMuxeR, --blu-ray) ---
	mp4 := filepath.Join(work, "test.mp4")
	run(t, work, nil, ffmpeg, "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=30",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=30",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "24", "-keyint_min", "24",
		"-sc_threshold", "0", "-profile:v", "high", "-level", "4.1", "-pix_fmt", "yuv420p",
		"-c:a", "ac3", "-b:a", "192k", mp4)
	meta := filepath.Join(work, "author.meta")
	os.WriteFile(meta, []byte("MUXOPT --blu-ray --custom-chapters=\"00:00:00.000;00:00:10.000;00:00:20.000\"\n"+
		"V_MPEG4/ISO/AVC, \"test.mp4\", track=1\n"+
		"A_AC3, \"test.mp4\", track=2\n"), 0o644)
	var authorEnv []string
	if libSrc != "" {
		authorEnv = append(os.Environ(), "LD_LIBRARY_PATH="+libSrc)
	}
	run(t, work, authorEnv, filepath.Join(binDir, "tsMuxeR"), meta, epDir)
	if _, err := os.Stat(filepath.Join(epDir, "BDMV", "PLAYLIST", "00000.mpls")); err != nil {
		t.Fatalf("synthetic disc not authored: %v", err)
	}
	os.WriteFile(filepath.Join(epDir, "1080.vpy"), []byte("# stub filter script\n"), 0o644)

	v := Vars{BinDir: binDir, ScriptsDir: scriptsDir, ReleaseDir: filepath.Join(root, "release"), Group: "TestGroup", Tag: "1080p"}

	// --- bd_merge: playlist → one src.m2ts + chapters sidecar ---
	out := bdMustRun(t, bdRender(t, "bd_merge", nil, v), v)
	if !strings.Contains(out, "ENCODE_STEP bd_merge 100") {
		t.Fatalf("bd_merge did not complete:\n%s", out)
	}
	merged := filepath.Join(epDir, "src.m2ts")
	assertDuration(t, ffprobe, merged, 29.5, 31.0)
	xml := bdRead(t, filepath.Join(epDir, "chapters.xml"))
	for _, want := range []string{"00:00:00.000000000", "00:00:10.000000000", "00:00:20.000000000"} {
		if !strings.Contains(xml, want) {
			t.Errorf("chapters.xml missing mark %s:\n%s", want, xml)
		}
	}
	assertDecodesClean(t, ffmpeg, merged)

	// --- bd_split chapters: one m2ts per chapter, all decode clean ---
	out = bdMustRun(t, bdRender(t, "bd_split", nil, v), v)
	if !strings.Contains(out, "ENCODE_STEP bd_split 100") {
		t.Fatalf("bd_split did not complete:\n%s", out)
	}
	for _, nn := range []string{"01", "02", "03"} {
		part := filepath.Join(epDir, "chapter-"+nn+".m2ts")
		assertDuration(t, ffprobe, part, 9.5, 10.7)
		assertDecodesClean(t, ffmpeg, part)
		if _, err := os.Stat(filepath.Join(epDir, "chapter-"+nn+".xml")); err != nil {
			t.Errorf("chapter-%s.xml sidecar missing: %v", nn, err)
		}
	}

	// --- bd_split range 2-2: chapter 2 only, rebased sidecar ---
	out = bdMustRun(t, bdRender(t, "bd_split", map[string]string{"mode": "range", "range": "2-2"}, v), v)
	if !strings.Contains(out, "ENCODE_STEP bd_split 100") {
		t.Fatalf("bd_split range did not complete:\n%s", out)
	}
	assertDuration(t, ffprobe, merged, 9.5, 10.7)
	assertDecodesClean(t, ffmpeg, merged)
	xml = bdRead(t, filepath.Join(epDir, "chapters.xml"))
	if !strings.Contains(xml, "Chapter 02") || strings.Contains(xml, "Chapter 01") || strings.Contains(xml, "Chapter 03") {
		t.Errorf("range sidecar must carry only Chapter 02:\n%s", xml)
	}
}

func copyFile(t *testing.T, src, dst string, mode os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("copy %s: %v", src, err)
	}
	if err := os.WriteFile(dst, b, mode); err != nil {
		t.Fatalf("copy → %s: %v", dst, err)
	}
}

func run(t *testing.T, dir string, env []string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func assertDuration(t *testing.T, ffprobe, path string, lo, hi float64) {
	t.Helper()
	cmd := exec.Command(ffprobe, "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("ffprobe duration %q: %v", out, err)
	}
	if d < lo || d > hi {
		t.Errorf("%s duration %.3fs outside [%.1f, %.1f]", filepath.Base(path), d, lo, hi)
	}
}

func assertDecodesClean(t *testing.T, ffmpeg, path string) {
	t.Helper()
	cmd := exec.Command(ffmpeg, "-v", "error", "-i", path, "-f", "null", "-")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("decode %s: %v\n%s", filepath.Base(path), err, out)
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		t.Errorf("decode errors in %s:\n%s", filepath.Base(path), out)
	}
}
