//go:build linux

package flow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Dedicated execution tests for the bd_merge/bd_split bash ports. The
// generic all-templates harness proves linkage; these prove the ARTIFACT
// CONTRACTS (src.m2ts + chapters.xml, chapter-NN parts + sidecars, rebased
// range chapters), the guard paths, the BinDir+tsMuxeR-lib resolution and
// both input layouts (loose .mpls, full BDMV).
//
// The tsMuxeR stub is contract-shaped: detection mode emits the exact
// listing format the steps parse (verified against real tsMuxeR 3.1.0);
// mux mode honours --split-chapters in the meta. Sidecar files in the stub
// dir force the failure edges (same pattern as the keyframes stubs).

// bdStubBody is the configurable tsMuxeR stub. Sidecars:
//
//	tsmuxer_detect_fail  — detection exits 1
//	tsmuxer_mux_fail     — mux exits 1
//	tsmuxer_no_marks     — listing without a Marks line
//	tsmuxer_part_count   — split produces N parts instead of 3
const bdStubBody = `#!/usr/bin/env bash
dir="$(cd "$(dirname "$0")" && pwd)"
printf '%s\n' "$@" > "$dir/tsmuxer_args.txt"
env | grep '^LD_LIBRARY_PATH=' > "$dir/tsmuxer_env.txt" || true
if [ "$#" -eq 1 ]; then
  if [ -f "$dir/tsmuxer_detect_fail" ]; then
    echo "stub: detection failure" >&2
    exit 1
  fi
  cat << 'EOF'
tsMuxeR version 3.1.0 (stub). github.com/teaching-droid/tsMuxer
Track ID:    4113
Stream type: H.264
Stream ID:   V_MPEG4/ISO/AVC
Stream info: stub
Stream lang: und

Track ID:    4352
Stream type: AC3
Stream ID:   A_AC3
Stream info: stub
Stream lang: und

File #00000 name=stub.m2ts
Duration: 00:00:30.000
EOF
  if [ ! -f "$dir/tsmuxer_no_marks" ]; then
    if [ -f "$dir/tsmuxer_bad_marks" ]; then
      echo 'Marks: * ?'
    elif [ -f "$dir/tsmuxer_glob_marks" ]; then
      echo 'Marks: 00:00:1?.000'
    elif [ -f "$dir/tsmuxer_first_mark_nonzero" ]; then
      echo 'Marks: 00:00:05.000 00:00:10.000 00:00:20.000'
    elif [ -f "$dir/tsmuxer_marks_100" ]; then
      m='Marks:'
      i=0
      while [ "$i" -lt 100 ]; do
        m="$m $(printf '%02d:%02d:%02d.000' $((i / 3600)) $((i % 3600 / 60)) $((i % 60)))"
        i=$((i + 1))
      done
      echo "$m"
    else
      echo 'Marks: 00:00:00.000 00:00:10.000 00:00:20.000'
    fi
  fi
  if [ -f "$dir/tsmuxer_multiclip" ]; then
    echo 'File #00001 name=stub2.m2ts'
    echo 'Duration: 00:00:30.000'
  fi
  exit 0
fi
meta="$1"; out="$2"
if [ -f "$dir/tsmuxer_mux_fail" ]; then
  echo "stub: mux failure" >&2
  exit 1
fi
# Syntax fidelity (real tsMuxeR rejects the space form): --cut-pictures
# must carry =cover or =inside; a bare flag fails the mux.
if grep -q -- '--cut-pictures' "$meta" && ! grep -qE -- '--cut-pictures=(cover|inside)' "$meta"; then
  echo "stub: --cut-pictures takes cover or inside, not \"\"" >&2
  exit 1
fi
if grep -q -- '--split-chapters' "$meta"; then
  count=3
  [ -f "$dir/tsmuxer_first_mark_nonzero" ] && count=4
  [ -f "$dir/tsmuxer_marks_100" ] && count=100
  [ -f "$dir/tsmuxer_part_count" ] && count=$(cat "$dir/tsmuxer_part_count")
  base="${out%.*}"
  p=1
  while [ "$p" -le "$count" ]; do
    head -c 256 /dev/zero > "$base.split.$p.m2ts"
    p=$((p + 1))
  done
else
  head -c 256 /dev/zero > "$out"
fi
echo "tsMuxeR stub: muxed $out"
`

// bdFfprobeStub serves the range-mode rebase query: first video PTS of
// the cut output. Default 610.0 = pinned 600s base + 10.0s keyframe
// (keyframe-aligned cut at mark 00:00:10.000). Sidecar ffprobe_pts
// overrides the value (off-keyframe probes).
const bdFfprobeStub = `#!/usr/bin/env bash
dir="$(cd "$(dirname "$0")" && pwd)"
printf '%s\n' "$@" > "$dir/ffprobe_args.txt"
if [ -f "$dir/ffprobe_pts" ]; then cat "$dir/ffprobe_pts"; else echo '610.000000'; fi
`

// bdTestEnv builds an episode tree + a stub bin dir. layout controls the
// playlist arrangement: "loose" (one flat .mpls), "bdmv" (BDMV/PLAYLIST +
// BDMV/STREAM), "multi" (two flat .mpls), "none". Returns Vars, episode dir
// and stub dir.
func bdTestEnv(t *testing.T, layout string) (Vars, string, string) {
	t.Helper()
	root := t.TempDir()
	scriptsDir := filepath.Join(root, "scripts")
	epDir := filepath.Join(scriptsDir, "Test Series", "Ep 01")
	binDir := filepath.Join(root, "bin")
	for _, d := range []string{epDir, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(epDir, "1080.vpy"), []byte("# stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	switch layout {
	case "loose":
		os.WriteFile(filepath.Join(epDir, "00000.mpls"), []byte("x"), 0o644)
		os.WriteFile(filepath.Join(epDir, "00000.m2ts"), []byte("x"), 0o644)
	case "bdmv":
		os.MkdirAll(filepath.Join(epDir, "BDMV", "PLAYLIST"), 0o755)
		os.MkdirAll(filepath.Join(epDir, "BDMV", "STREAM"), 0o755)
		os.WriteFile(filepath.Join(epDir, "BDMV", "PLAYLIST", "00000.mpls"), []byte("x"), 0o644)
		os.WriteFile(filepath.Join(epDir, "BDMV", "STREAM", "00000.m2ts"), []byte("x"), 0o644)
	case "multi":
		os.WriteFile(filepath.Join(epDir, "00000.mpls"), []byte("x"), 0o644)
		os.WriteFile(filepath.Join(epDir, "00001.mpls"), []byte("x"), 0o644)
	case "none":
		// no playlist at all
	}
	if err := os.WriteFile(filepath.Join(binDir, "tsMuxeR"), []byte(bdStubBody), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "ffprobe"), []byte(bdFfprobeStub), 0o755); err != nil {
		t.Fatal(err)
	}
	v := Vars{BinDir: binDir, ScriptsDir: scriptsDir, ReleaseDir: filepath.Join(root, "release"), Group: "TestGroup", Tag: "1080p"}
	return v, epDir, binDir
}

// bdRender renders a single bd step with params via BashDialect.
func bdRender(t *testing.T, key string, params map[string]string, v Vars) string {
	t.Helper()
	f := singleStepFlow(key)
	f.Steps[0].Params = params
	script, err := RenderDialect(f, linuxTestJob(), v, BuiltinResolver(), BashDialect{})
	if err != nil {
		t.Fatalf("render %s: %v", key, err)
	}
	return script
}

// bdRun executes the rendered script with ONLY coreutils on PATH (the
// tsMuxeR/ffprobe stubs live in BinDir — exercising the BinDir resolution
// path, not command -v) in a fresh empty working directory. Returns
// combined output + exit error.
func bdRun(t *testing.T, script string, v Vars) (string, error) {
	t.Helper()
	return bdRunInDir(t, script, v, t.TempDir())
}

// bdRunInDir is bdRun with a controlled cwd — glob-expansion probes plant
// mark-shaped filenames there (a bare word-splitting Marks loop would
// pathname-expand against the process cwd).
func bdRunInDir(t *testing.T, script string, v Vars, cwd string) (string, error) {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "bd.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", scriptPath)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "PATH=/usr/bin:/bin:/usr/local/bin")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func bdMustRun(t *testing.T, script string, v Vars) string {
	t.Helper()
	out, err := bdRun(t, script, v)
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "command not found") || strings.Contains(out, "exit 127") {
		t.Fatalf("linkage broken (127):\n%s", out)
	}
	return out
}

func bdMustFail(t *testing.T, script string, v Vars, wantMsg string) string {
	t.Helper()
	out, err := bdRun(t, script, v)
	if err == nil {
		t.Fatalf("expected failure, got success:\n%s", out)
	}
	if !strings.Contains(out, wantMsg) {
		t.Fatalf("failure output missing %q:\n%s", wantMsg, out)
	}
	if !strings.Contains(out, "ENCODE_STEP_FAILED") {
		t.Fatalf("failure without ENCODE_STEP_FAILED marker:\n%s", out)
	}
	return out
}

func bdRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// --- bd_merge -----------------------------------------------------------

func TestBdBashMergeArtifacts(t *testing.T) {
	v, epDir, binDir := bdTestEnv(t, "loose")
	out := bdMustRun(t, bdRender(t, "bd_merge", nil, v), v)

	for _, marker := range []string{"ENCODE_STEP bd_merge 0", "ENCODE_STEP bd_merge 30", "ENCODE_STEP bd_merge 60", "ENCODE_STEP bd_merge 100"} {
		if !strings.Contains(out, marker) {
			t.Errorf("missing marker %q:\n%s", marker, out)
		}
	}
	if _, err := os.Stat(filepath.Join(epDir, "src.m2ts")); err != nil {
		t.Errorf("src.m2ts not created: %v", err)
	}
	xml := bdRead(t, filepath.Join(epDir, "chapters.xml"))
	for _, want := range []string{
		"<ChapterTimeStart>00:00:00.000000000</ChapterTimeStart>",
		"<ChapterTimeStart>00:00:10.000000000</ChapterTimeStart>",
		"<ChapterTimeStart>00:00:20.000000000</ChapterTimeStart>",
		"<ChapterString>Chapter 01</ChapterString>",
		"<ChapterString>Chapter 03</ChapterString>",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("chapters.xml missing %q:\n%s", want, xml)
		}
	}
	// Meta references the PLAYLIST (not the raw m2ts) with both tracks.
	meta := bdRead(t, filepath.Join(epDir, "bd-merge.meta"))
	if !strings.HasPrefix(meta, "MUXOPT\n") {
		t.Errorf("meta must start with plain MUXOPT:\n%s", meta)
	}
	if !strings.Contains(meta, `V_MPEG4/ISO/AVC, "`) || !strings.Contains(meta, `A_AC3, "`) {
		t.Errorf("meta missing track lines:\n%s", meta)
	}
	if !strings.Contains(meta, "00000.mpls\", track=4113") || !strings.Contains(meta, "track=4352") {
		t.Errorf("meta must reference the playlist with detected track ids:\n%s", meta)
	}
	if strings.Contains(meta, "--split-chapters") || strings.Contains(meta, "--cut-") {
		t.Errorf("merge meta must not carry split/cut options:\n%s", meta)
	}
	// BinDir resolution was used (no tsMuxeR on PATH).
	args := bdRead(t, filepath.Join(binDir, "tsmuxer_args.txt"))
	if !strings.Contains(args, "bd-merge.meta") {
		t.Errorf("stub did not record the mux invocation:\n%s", args)
	}
}

func TestBdBashMergeBDMVLayout(t *testing.T) {
	v, epDir, _ := bdTestEnv(t, "bdmv")
	bdMustRun(t, bdRender(t, "bd_merge", nil, v), v)
	meta := bdRead(t, filepath.Join(epDir, "bd-merge.meta"))
	if !strings.Contains(meta, "BDMV/PLAYLIST/00000.mpls") {
		t.Errorf("BDMV playlist not resolved:\n%s", meta)
	}
}

func TestBdBashMergeExplicitPlaylistParam(t *testing.T) {
	v, epDir, _ := bdTestEnv(t, "multi")
	bdMustRun(t, bdRender(t, "bd_merge", map[string]string{"playlist": "00001.mpls"}, v), v)
	meta := bdRead(t, filepath.Join(epDir, "bd-merge.meta"))
	if !strings.Contains(meta, "00001.mpls") || strings.Contains(meta, "00000.mpls") {
		t.Errorf("explicit playlist param not honoured:\n%s", meta)
	}
}

func TestBdBashMergeNoMarksRemovesStaleChapters(t *testing.T) {
	v, epDir, binDir := bdTestEnv(t, "loose")
	os.WriteFile(filepath.Join(binDir, "tsmuxer_no_marks"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(epDir, "chapters.xml"), []byte("stale"), 0o644)
	out := bdMustRun(t, bdRender(t, "bd_merge", nil, v), v)
	if !strings.Contains(out, "no chapter marks") {
		t.Errorf("no-marks path not logged:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(epDir, "chapters.xml")); !os.IsNotExist(err) {
		t.Errorf("stale chapters.xml must be removed when the playlist has no marks")
	}
	if _, err := os.Stat(filepath.Join(epDir, "src.m2ts")); err != nil {
		t.Errorf("merge must still succeed without marks: %v", err)
	}
}

func TestBdBashMergeGuards(t *testing.T) {
	t.Run("missing tool", func(t *testing.T) {
		v, _, binDir := bdTestEnv(t, "loose")
		os.Remove(filepath.Join(binDir, "tsMuxeR"))
		bdMustFail(t, bdRender(t, "bd_merge", nil, v), v, "tsMuxeR not found")
	})
	t.Run("no playlist", func(t *testing.T) {
		v, _, _ := bdTestEnv(t, "none")
		bdMustFail(t, bdRender(t, "bd_merge", nil, v), v, "no Blu-ray playlist")
	})
	t.Run("multiple playlists without param", func(t *testing.T) {
		v, _, _ := bdTestEnv(t, "multi")
		bdMustFail(t, bdRender(t, "bd_merge", nil, v), v, "multiple playlists")
	})
	t.Run("traversal param", func(t *testing.T) {
		v, _, _ := bdTestEnv(t, "loose")
		bdMustFail(t, bdRender(t, "bd_merge", map[string]string{"playlist": "../evil.mpls"}, v), v, "unsafe playlist parameter")
	})
	t.Run("absolute param", func(t *testing.T) {
		v, _, _ := bdTestEnv(t, "loose")
		bdMustFail(t, bdRender(t, "bd_merge", map[string]string{"playlist": "/etc/passwd"}, v), v, "unsafe playlist parameter")
	})
	t.Run("quote param", func(t *testing.T) {
		v, _, _ := bdTestEnv(t, "loose")
		bdMustFail(t, bdRender(t, "bd_merge", map[string]string{"playlist": `a".mpls`}, v), v, "unsafe playlist parameter")
	})
	t.Run("missing param file", func(t *testing.T) {
		v, _, _ := bdTestEnv(t, "loose")
		bdMustFail(t, bdRender(t, "bd_merge", map[string]string{"playlist": "99999.mpls"}, v), v, "playlist not found")
	})
	t.Run("detection failure", func(t *testing.T) {
		v, _, binDir := bdTestEnv(t, "loose")
		os.WriteFile(filepath.Join(binDir, "tsmuxer_detect_fail"), []byte("1"), 0o644)
		bdMustFail(t, bdRender(t, "bd_merge", nil, v), v, "detection failed")
	})
	t.Run("mux failure", func(t *testing.T) {
		v, _, binDir := bdTestEnv(t, "loose")
		os.WriteFile(filepath.Join(binDir, "tsmuxer_mux_fail"), []byte("1"), 0o644)
		bdMustFail(t, bdRender(t, "bd_merge", nil, v), v, "mux failed")
	})
}

// The bundled-libs layout: BinDir/tsMuxeR + BinDir/tsMuxeR-lib must prepend
// the lib dir to LD_LIBRARY_PATH for tsMuxeR invocations only.
func TestBdBashMergeBundledLibs(t *testing.T) {
	v, _, binDir := bdTestEnv(t, "loose")
	libDir := filepath.Join(binDir, "tsMuxeR-lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bdMustRun(t, bdRender(t, "bd_merge", nil, v), v)
	envRec := bdRead(t, filepath.Join(binDir, "tsmuxer_env.txt"))
	if !strings.Contains(envRec, libDir) {
		t.Errorf("tsMuxeR-lib not prepended to LD_LIBRARY_PATH: %q", envRec)
	}
}

// --- bd_split -----------------------------------------------------------

func TestBdBashSplitChaptersArtifacts(t *testing.T) {
	v, epDir, _ := bdTestEnv(t, "loose")
	// Stale artifacts from an earlier run must be cleaned.
	os.WriteFile(filepath.Join(epDir, "chapter-99.m2ts"), []byte("stale"), 0o644)
	os.WriteFile(filepath.Join(epDir, "chapter-99.xml"), []byte("stale"), 0o644)
	os.WriteFile(filepath.Join(epDir, "chapters.xml"), []byte("stale"), 0o644)
	os.WriteFile(filepath.Join(epDir, "bd-split.split.7.m2ts"), []byte("stale"), 0o644)

	out := bdMustRun(t, bdRender(t, "bd_split", nil, v), v)
	for _, marker := range []string{"ENCODE_STEP bd_split 0", "ENCODE_STEP bd_split 30", "ENCODE_STEP bd_split 60", "ENCODE_STEP bd_split 100"} {
		if !strings.Contains(out, marker) {
			t.Errorf("missing marker %q:\n%s", marker, out)
		}
	}
	for n := 1; n <= 3; n++ {
		nn := []string{"01", "02", "03"}[n-1]
		if _, err := os.Stat(filepath.Join(epDir, "chapter-"+nn+".m2ts")); err != nil {
			t.Errorf("chapter-%s.m2ts missing: %v", nn, err)
		}
		xml := bdRead(t, filepath.Join(epDir, "chapter-"+nn+".xml"))
		if !strings.Contains(xml, "<ChapterTimeStart>00:00:00.000000000</ChapterTimeStart>") {
			t.Errorf("chapter-%s.xml must carry a single mark at 0:\n%s", nn, xml)
		}
		if !strings.Contains(xml, "<ChapterString>Chapter "+nn+"</ChapterString>") {
			t.Errorf("chapter-%s.xml must keep the original chapter number:\n%s", nn, xml)
		}
	}
	for _, stale := range []string{"chapter-99.m2ts", "chapter-99.xml", "chapters.xml", "bd-split.split.7.m2ts"} {
		if _, err := os.Stat(filepath.Join(epDir, stale)); !os.IsNotExist(err) {
			t.Errorf("stale artifact %s not cleaned", stale)
		}
	}
	// No leftover split parts or base file.
	entries, _ := os.ReadDir(epDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "bd-split.split.") {
			t.Errorf("leftover split part: %s", e.Name())
		}
	}
	meta := bdRead(t, filepath.Join(epDir, "bd-split.meta"))
	if !strings.HasPrefix(meta, "MUXOPT --split-chapters\n") {
		t.Errorf("split meta must carry --split-chapters:\n%s", meta)
	}
}

func TestBdBashSplitRangeArtifacts(t *testing.T) {
	v, epDir, binDir := bdTestEnv(t, "loose")
	// Range 2-2 keeps ONLY chapter 2: cut from mark 2 (10s) to the start
	// of chapter 3 (20s). (A range ending at the LAST chapter omits
	// --cut-end — covered by TestBdBashSplitRangeToEndOmitsCutEnd.)
	bdMustRun(t, bdRender(t, "bd_split", map[string]string{"mode": "range", "range": "2-2"}, v), v)

	meta := bdRead(t, filepath.Join(epDir, "bd-split.meta"))
	if !strings.Contains(meta, "MUXOPT --cut-start=00:00:10.000 --cut-pictures=inside --start-time=27000000 --cut-end=00:00:20.000") {
		t.Errorf("range 2-2 must cut inside from mark 2 to mark 3 start with pinned base:\n%s", meta)
	}
	if _, err := os.Stat(filepath.Join(epDir, "src.m2ts")); err != nil {
		t.Errorf("range mode must produce src.m2ts: %v", err)
	}
	xml := bdRead(t, filepath.Join(epDir, "chapters.xml"))
	// Rebased: chapter 2 at 0; original number kept; no other chapters.
	if !strings.Contains(xml, "<ChapterTimeStart>00:00:00.000000000</ChapterTimeStart>") {
		t.Errorf("range chapters not rebased:\n%s", xml)
	}
	if !strings.Contains(xml, "<ChapterString>Chapter 02</ChapterString>") ||
		strings.Contains(xml, "Chapter 01") || strings.Contains(xml, "Chapter 03") {
		t.Errorf("range 2-2 must keep only Chapter 02:\n%s", xml)
	}
	_ = binDir
}

func TestBdBashSplitRangeToEndOmitsCutEnd(t *testing.T) {
	v, epDir, _ := bdTestEnv(t, "loose")
	bdMustRun(t, bdRender(t, "bd_split", map[string]string{"mode": "range", "range": "3"}, v), v)
	meta := bdRead(t, filepath.Join(epDir, "bd-split.meta"))
	if !strings.Contains(meta, "--cut-start=00:00:20.000") {
		t.Errorf("single-chapter range must cut from its mark:\n%s", meta)
	}
	if strings.Contains(meta, "--cut-end") {
		t.Errorf("range to the last chapter must omit --cut-end:\n%s", meta)
	}
}

func TestBdBashSplitGuards(t *testing.T) {
	t.Run("no marks", func(t *testing.T) {
		v, _, binDir := bdTestEnv(t, "loose")
		os.WriteFile(filepath.Join(binDir, "tsmuxer_no_marks"), []byte("1"), 0o644)
		bdMustFail(t, bdRender(t, "bd_split", nil, v), v, "no chapter marks")
	})
	t.Run("bad mode", func(t *testing.T) {
		v, _, _ := bdTestEnv(t, "loose")
		bdMustFail(t, bdRender(t, "bd_split", map[string]string{"mode": "explode"}, v), v, "mode must be")
	})
	t.Run("bad range syntax", func(t *testing.T) {
		v, _, _ := bdTestEnv(t, "loose")
		bdMustFail(t, bdRender(t, "bd_split", map[string]string{"mode": "range", "range": "two-five"}, v), v, "range must be N or N-M")
	})
	t.Run("range out of bounds", func(t *testing.T) {
		v, _, _ := bdTestEnv(t, "loose")
		bdMustFail(t, bdRender(t, "bd_split", map[string]string{"mode": "range", "range": "2-9"}, v), v, "outside the playlist")
	})
	t.Run("range reversed", func(t *testing.T) {
		v, _, _ := bdTestEnv(t, "loose")
		bdMustFail(t, bdRender(t, "bd_split", map[string]string{"mode": "range", "range": "3-2"}, v), v, "outside the playlist")
	})
	t.Run("range zero", func(t *testing.T) {
		v, _, _ := bdTestEnv(t, "loose")
		bdMustFail(t, bdRender(t, "bd_split", map[string]string{"mode": "range", "range": "0-2"}, v), v, "outside the playlist")
	})
	t.Run("part count mismatch", func(t *testing.T) {
		v, _, binDir := bdTestEnv(t, "loose")
		os.WriteFile(filepath.Join(binDir, "tsmuxer_part_count"), []byte("2"), 0o644)
		bdMustFail(t, bdRender(t, "bd_split", nil, v), v, "split parts but expected")
	})
	t.Run("missing tool", func(t *testing.T) {
		v, _, binDir := bdTestEnv(t, "loose")
		os.Remove(filepath.Join(binDir, "tsMuxeR"))
		bdMustFail(t, bdRender(t, "bd_split", nil, v), v, "tsMuxeR not found")
	})
}

// --- hardening regressions (adversarial-review attack patterns) ---------

// A playlist filename containing a newline must not forge log/markers or
// split into phantom candidates: discovery is null-delimited, echoes are
// flattened, and the meta-quote guard fails closed on control chars.
func TestBdBashNewlineFilenameFailsClosed(t *testing.T) {
	v, epDir, _ := bdTestEnv(t, "none")
	os.WriteFile(filepath.Join(epDir, "bad\nENCODE_STEP bd_merge 100.mpls"), []byte("x"), 0o644)
	out := bdMustFail(t, bdRender(t, "bd_merge", nil, v), v, "newlines")
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "ENCODE_STEP bd_merge 100") {
			t.Fatalf("forged completion marker from newline filename:\n%s", out)
		}
	}
}

// Hostile glob tokens in the Marks line must be validated, not
// pathname-expanded. NON-VACUOUS: the cwd contains a mark-shaped file, so
// a bare word-splitting loop would expand '00:00:1?.000' into a VALID
// mark and complete — the test only passes with read -ra in place.
// Both dialects' steps must fail closed identically (parity).
func TestBdBashGlobMarksFailClosed(t *testing.T) {
	for _, key := range []string{"bd_merge", "bd_split"} {
		t.Run(key, func(t *testing.T) {
			v, _, binDir := bdTestEnv(t, "loose")
			os.WriteFile(filepath.Join(binDir, "tsmuxer_glob_marks"), []byte("1"), 0o644)
			cwd := t.TempDir()
			// The expansion target: a legal filename that IS a valid mark.
			os.WriteFile(filepath.Join(cwd, "00:00:15.000"), []byte("x"), 0o644)
			script := bdRender(t, key, nil, v)
			out, err := bdRunInDir(t, script, v, cwd)
			if err == nil {
				t.Fatalf("%s: glob token expanded into a valid mark (defense missing):\n%s", key, out)
			}
			if !strings.Contains(out, "invalid chapter mark") {
				t.Fatalf("%s: expected mark validation failure:\n%s", key, out)
			}
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "ENCODE_STEP "+key+" 100") {
					t.Fatalf("%s: forged completion marker:\n%s", key, out)
				}
			}
		})
	}
}

// A newline in the untrusted range/mode params must not forge line-start
// progress markers through the error echo (agent live tracker parses
// ^ENCODE_STEP unanchored at the end).
func TestBdBashParamMarkerForgeryFailsClosed(t *testing.T) {
	for _, tc := range []struct{ name, param, value string }{
		{"range", "range", "x\nENCODE_STEP bd_split 100"},
		{"range cross-step", "range", "x\nENCODE_STEP encode 100"},
		{"mode", "mode", "x\nENCODE_STEP bd_split 100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, _, _ := bdTestEnv(t, "loose")
			params := map[string]string{tc.param: tc.value}
			if tc.param == "range" {
				params["mode"] = "range"
			}
			out := bdMustFail(t, bdRender(t, "bd_split", params, v), v, "ENCODE_STEP_FAILED")
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "ENCODE_STEP bd_split 100") || strings.HasPrefix(line, "ENCODE_STEP encode 100") {
					t.Fatalf("forged marker from %s param:\n%s", tc.param, out)
				}
			}
		})
	}
}

// bash arithmetic wraps mod 2^64: an uncapped digit string would validate
// 2^64+2 as chapter 2. The {1,9} cap must reject it (PS [int] overflow
// parity).
func TestBdBashRangeOverflowFailsClosed(t *testing.T) {
	v, _, _ := bdTestEnv(t, "loose")
	bdMustFail(t, bdRender(t, "bd_split", map[string]string{"mode": "range", "range": "18446744073709551618"}, v), v, "range must be N or N-M")
}

// A stale DIRECTORY at a chapter artifact path must fail the step, not
// silently swallow the renamed part (mv into a dir exits 0).
func TestBdBashSplitStaleTargetDirFailsClosed(t *testing.T) {
	v, epDir, _ := bdTestEnv(t, "loose")
	if err := os.MkdirAll(filepath.Join(epDir, "chapter-01.m2ts"), 0o755); err != nil {
		t.Fatal(err)
	}
	bdMustFail(t, bdRender(t, "bd_split", nil, v), v, "not a regular file after rename")
}

// First mark non-zero → tsMuxeR emits N+1 parts (pre-chapter segment
// 0 -> first mark). It must map to chapter-00, not fail the count check.
func TestBdBashSplitNonZeroFirstMark(t *testing.T) {
	v, epDir, binDir := bdTestEnv(t, "loose")
	os.WriteFile(filepath.Join(binDir, "tsmuxer_first_mark_nonzero"), []byte("1"), 0o644)
	bdMustRun(t, bdRender(t, "bd_split", nil, v), v)
	for _, nn := range []string{"00", "01", "02", "03"} {
		if _, err := os.Stat(filepath.Join(epDir, "chapter-"+nn+".m2ts")); err != nil {
			t.Errorf("chapter-%s.m2ts missing: %v", nn, err)
		}
		xml := bdRead(t, filepath.Join(epDir, "chapter-"+nn+".xml"))
		if !strings.Contains(xml, "<ChapterString>Chapter "+nn+"</ChapterString>") {
			t.Errorf("chapter-%s.xml name wrong:\n%s", nn, xml)
		}
	}
}

// >99 chapters: %02d is MINIMUM width — cleanup must match 3-digit names
// and the rename must produce chapter-100 (not collide with stale files).
func TestBdBashSplitHundredChapters(t *testing.T) {
	v, epDir, binDir := bdTestEnv(t, "loose")
	os.WriteFile(filepath.Join(binDir, "tsmuxer_marks_100"), []byte("1"), 0o644)
	// Stale artifacts beyond the 2-digit pattern.
	os.WriteFile(filepath.Join(epDir, "chapter-100.m2ts"), []byte("stale"), 0o644)
	os.WriteFile(filepath.Join(epDir, "chapter-101.m2ts"), []byte("stale"), 0o644)
	os.WriteFile(filepath.Join(epDir, "chapter-101.xml"), []byte("stale"), 0o644)
	bdMustRun(t, bdRender(t, "bd_split", nil, v), v)
	if _, err := os.Stat(filepath.Join(epDir, "chapter-100.m2ts")); err != nil {
		t.Fatalf("chapter-100.m2ts missing: %v", err)
	}
	if b := bdRead(t, filepath.Join(epDir, "chapter-100.m2ts")); len(b) != 256 {
		t.Errorf("chapter-100.m2ts must be the fresh part, not the stale file (%d bytes)", len(b))
	}
	if _, err := os.Stat(filepath.Join(epDir, "chapter-101.m2ts")); !os.IsNotExist(err) {
		t.Errorf("stale chapter-101.m2ts not cleaned")
	}
	if _, err := os.Stat(filepath.Join(epDir, "chapter-101.xml")); !os.IsNotExist(err) {
		t.Errorf("stale chapter-101.xml not cleaned")
	}
}

// Mode switches must not leave orphans: chapters→range, chapters→merge.
func TestBdBashModeSwitchCleansStaleChapters(t *testing.T) {
	for _, next := range []struct {
		key    string
		params map[string]string
	}{{"bd_split", map[string]string{"mode": "range", "range": "2-2"}}, {"bd_merge", nil}} {
		t.Run(next.key, func(t *testing.T) {
			v, epDir, _ := bdTestEnv(t, "loose")
			bdMustRun(t, bdRender(t, "bd_split", nil, v), v)
			bdMustRun(t, bdRender(t, next.key, next.params, v), v)
			entries, _ := os.ReadDir(epDir)
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "chapter-") || strings.HasPrefix(e.Name(), "bd-split.split.") {
					t.Errorf("stale split artifact survived switch to %s: %s", next.key, e.Name())
				}
			}
		})
	}
}

// Range mode requires ffprobe for the measured rebase — fail fast with an
// actionable message when it is absent.
func TestBdBashRangeRequiresFfprobe(t *testing.T) {
	v, _, binDir := bdTestEnv(t, "loose")
	os.Remove(filepath.Join(binDir, "ffprobe"))
	bdMustFail(t, bdRender(t, "bd_split", map[string]string{"mode": "range", "range": "2-2"}, v), v, "requires ffprobe")
}

// Multi-clip playlist listing (two File # entries): tracks are listed
// once per stream, meta must carry exactly one line per track referencing
// the playlist (tsMuxeR expands PlayItems internally).
func TestBdBashMergeMultiClipListing(t *testing.T) {
	v, epDir, binDir := bdTestEnv(t, "loose")
	os.WriteFile(filepath.Join(binDir, "tsmuxer_multiclip"), []byte("1"), 0o644)
	bdMustRun(t, bdRender(t, "bd_merge", nil, v), v)
	meta := bdRead(t, filepath.Join(epDir, "bd-merge.meta"))
	lines := strings.Split(strings.TrimSpace(meta), "\n")
	if len(lines) != 3 { // MUXOPT + 2 tracks
		t.Fatalf("multi-clip listing must not duplicate track lines:\n%s", meta)
	}
	for _, l := range lines[1:] {
		if !strings.Contains(l, "00000.mpls\", track=") {
			t.Errorf("track line must reference the playlist: %s", l)
		}
	}
}

// Parity sanity: the PS templates declare the same artifact contracts.
// (Static — the PS behaviour is exercised on Windows nodes; the bash exec
// tests above are the behavioural ground truth on Linux.)
func TestBdPSContractParity(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want []string
	}{
		{"bd_merge", []string{"ENCODE_STEP bd_merge 0", "ENCODE_STEP bd_merge 100", "src.m2ts", "chapters.xml", "bd-merge.meta", "tsMuxeR.exe", "MUXOPT"}},
		{"bd_split", []string{"ENCODE_STEP bd_split 0", "ENCODE_STEP bd_split 100", "--split-chapters", "--cut-start=", "chapter-", "bd-split.meta", "tsMuxeR.exe"}},
	} {
		tmpl, err := BuiltinResolver()(tc.key)
		if err != nil || tmpl == nil {
			t.Fatalf("template %s not registered: %v", tc.key, err)
		}
		for _, w := range tc.want {
			if !strings.Contains(tmpl.PowerShell, w) {
				t.Errorf("%s PS body missing %q", tc.key, w)
			}
		}
	}
}
