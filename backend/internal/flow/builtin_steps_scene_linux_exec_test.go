//go:build linux

package flow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Dedicated execution tests for the scene_detect bash port. The ffprobe
// stub emits the exact JSON shape the step parses; the ffmpeg stub writes
// a contract-shaped scdet.log (format frozen against ffmpeg 9.x: frame
// lines + lavfi.scd.score metadata). Sidecars force the failure edges.

// sceneFfprobeStub: default 24fps, 720 frames, 30s. Sidecars:
//
//	ffprobe_fail        — exit 1
//	ffprobe_no_frames   — nb_frames "N/A" (duration*fps fallback)
//	ffprobe_bad_rate    — r_frame_rate "garbage"
//	ffprobe_no_duration — durations "N/A"
const sceneFfprobeStub = `#!/usr/bin/env bash
dir="$(cd "$(dirname "$0")" && pwd)"
printf '%s\n' "$@" > "$dir/ffprobe_args.txt"
if [ -f "$dir/ffprobe_fail" ]; then echo "stub: probe failure" >&2; exit 1; fi
nb='"720"'; rate='24/1'; sdur='30.000000'; fdur='30.000000'
[ -f "$dir/ffprobe_no_frames" ] && nb='"N/A"'
[ -f "$dir/ffprobe_bad_rate" ] && rate='garbage'
if [ -f "$dir/ffprobe_no_duration" ]; then sdur='"N/A"'; fdur='"N/A"'; fi
printf '{"streams":[{"r_frame_rate":"%s","nb_frames":"%s","duration":"%s"}],"format":{"duration":"%s"}}\n' "$rate" "$nb" "$sdur" "$fdur"
`

// sceneFfmpegStub: writes scdet.log with boundaries at frames 240 (score
// 25.0) and 480 (score 30.0) unless sidecars say otherwise:
//
//	ffmpeg_fail      — exit 1 without a log
//	ffmpeg_empty_log — touches an empty log
//	ffmpeg_no_cuts   — all scores below threshold
//	ffmpeg_bad_log   — garbage (no frame lines)
const sceneFfmpegStub = `#!/usr/bin/env bash
dir="$(cd "$(dirname "$0")" && pwd)"
printf '%s\n' "$@" > "$dir/ffmpeg_args.txt"
log=''
prev=''
for a in "$@"; do
  case "$prev" in -vf) log="${a#*metadata=print:file=}";; esac
  prev="$a"
done
if [ -f "$dir/ffmpeg_fail" ]; then echo "stub: ffmpeg failure" >&2; exit 1; fi
if [ -f "$dir/ffmpeg_empty_log" ]; then : > "$log"; exit 0; fi
if [ -f "$dir/ffmpeg_bad_log" ]; then printf 'garbage\nmore garbage\n' > "$log"; exit 0; fi
{
  for f in 0 1 239 240 241 479 480 481 719; do
    t=$(awk -v f="$f" 'BEGIN{printf "%.6f", f/24}')
    printf 'frame:%d    pts:%d       pts_time:%s\n' "$f" "$((f * 512))" "$t"
    printf 'lavfi.scd.mafd=1.000\n'
    if [ -f "$dir/ffmpeg_no_cuts" ]; then
      printf 'lavfi.scd.score=1.000\n'
    elif [ "$f" = 240 ]; then
      printf 'lavfi.scd.score=25.000\n'
    elif [ "$f" = 480 ]; then
      printf 'lavfi.scd.score=30.000\n'
    else
      printf 'lavfi.scd.score=1.000\n'
    fi
  done
} > "$log"
echo "stub: scdet pass complete"
`

// sceneTestEnv: episode with a flat source + stub ffmpeg/ffprobe in BinDir.
func sceneTestEnv(t *testing.T) (Vars, string, string) {
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
	os.WriteFile(filepath.Join(epDir, "src.m2ts"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(epDir, "1080.vpy"), []byte("# stub"), 0o644)
	os.WriteFile(filepath.Join(binDir, "ffprobe"), []byte(sceneFfprobeStub), 0o755)
	os.WriteFile(filepath.Join(binDir, "ffmpeg"), []byte(sceneFfmpegStub), 0o755)
	v := Vars{BinDir: binDir, ScriptsDir: scriptsDir, ReleaseDir: filepath.Join(root, "release"), Group: "TestGroup", Tag: "1080p"}
	return v, epDir, binDir
}

type sceneDoc struct {
	Source      string  `json:"source"`
	FPS         float64 `json:"fps"`
	FrameCount  int64   `json:"frame_count"`
	Duration    float64 `json:"duration"`
	Threshold   float64 `json:"threshold"`
	MinSceneLen int     `json:"min_scene_len"`
	Scenes      []struct {
		Index      int     `json:"index"`
		StartFrame int64   `json:"start_frame"`
		EndFrame   int64   `json:"end_frame"`
		StartTime  float64 `json:"start_time"`
		EndTime    float64 `json:"end_time"`
		Score      float64 `json:"score"`
	} `json:"scenes"`
}

func readScenes(t *testing.T, epDir string) sceneDoc {
	t.Helper()
	var d sceneDoc
	if err := json.Unmarshal([]byte(bdRead(t, filepath.Join(epDir, "scenes.json"))), &d); err != nil {
		t.Fatalf("scenes.json invalid: %v", err)
	}
	return d
}

func TestSceneDetectBashArtifacts(t *testing.T) {
	v, epDir, _ := sceneTestEnv(t)
	out := bdMustRun(t, bdRender(t, "scene_detect", nil, v), v)
	for _, marker := range []string{"ENCODE_STEP scene_detect 0", "ENCODE_STEP scene_detect 30", "ENCODE_STEP scene_detect 60", "ENCODE_STEP scene_detect 100"} {
		if !strings.Contains(out, marker) {
			t.Errorf("missing marker %q:\n%s", marker, out)
		}
	}
	d := readScenes(t, epDir)
	if d.Source != "src.m2ts" || d.FPS != 24 || d.FrameCount != 720 || d.Duration != 30 {
		t.Fatalf("probe fields wrong: %+v", d)
	}
	if len(d.Scenes) != 3 {
		t.Fatalf("want 3 scenes, got %+v", d.Scenes)
	}
	want := []struct{ start, end int64 }{{0, 239}, {240, 479}, {480, 719}}
	for i, w := range want {
		s := d.Scenes[i]
		if s.Index != i+1 || s.StartFrame != w.start || s.EndFrame != w.end {
			t.Errorf("scene %d wrong: %+v (want %v)", i+1, s, w)
		}
	}
	if d.Scenes[1].StartTime != 10 || d.Scenes[2].StartTime != 20 {
		t.Errorf("boundary times wrong: %+v", d.Scenes)
	}
	if d.Scenes[1].Score != 25 || d.Scenes[2].Score != 30 {
		t.Errorf("boundary scores wrong: %+v", d.Scenes)
	}
	if _, err := os.Stat(filepath.Join(epDir, "scdet.log")); err != nil {
		t.Errorf("scdet.log evidence missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(epDir, ".scenes.json.tmp")); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}
}

func TestSceneDetectBashSkipIfFresh(t *testing.T) {
	v, epDir, binDir := sceneTestEnv(t)
	os.WriteFile(filepath.Join(epDir, "scenes.json"), []byte(`{"scenes":[]}`), 0o644)
	// Make scenes.json newer than the source.
	future := time.Now().Add(time.Minute)
	os.Chtimes(filepath.Join(epDir, "scenes.json"), future, future)
	out := bdMustRun(t, bdRender(t, "scene_detect", nil, v), v)
	if !strings.Contains(out, "reusing") {
		t.Fatalf("skip-if-fresh not honored:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(binDir, "ffmpeg_args.txt")); !os.IsNotExist(err) {
		t.Fatal("ffmpeg ran despite fresh scenes.json")
	}
}

func TestSceneDetectBashMinSceneLenMerge(t *testing.T) {
	v, epDir, _ := sceneTestEnv(t)
	bdMustRun(t, bdRender(t, "scene_detect", map[string]string{"min_scene_len": "300"}, v), v)
	d := readScenes(t, epDir)
	// Boundary 240 is < 300 frames from start -> merged; 480 kept.
	if len(d.Scenes) != 2 || d.Scenes[0].StartFrame != 0 || d.Scenes[1].StartFrame != 480 {
		t.Fatalf("min_scene_len merge wrong: %+v", d.Scenes)
	}
	if d.MinSceneLen != 300 {
		t.Errorf("min_scene_len not recorded: %+v", d)
	}
}

func TestSceneDetectBashThresholdParam(t *testing.T) {
	v, epDir, _ := sceneTestEnv(t)
	bdMustRun(t, bdRender(t, "scene_detect", map[string]string{"threshold": "28"}, v), v)
	d := readScenes(t, epDir)
	if len(d.Scenes) != 2 || d.Scenes[1].StartFrame != 480 || d.Threshold != 28 {
		t.Fatalf("threshold filtering wrong: %+v", d)
	}
}

func TestSceneDetectBashNoCutsSingleScene(t *testing.T) {
	v, epDir, binDir := sceneTestEnv(t)
	os.WriteFile(filepath.Join(binDir, "ffmpeg_no_cuts"), []byte("1"), 0o644)
	bdMustRun(t, bdRender(t, "scene_detect", nil, v), v)
	d := readScenes(t, epDir)
	if len(d.Scenes) != 1 || d.Scenes[0].StartFrame != 0 || d.Scenes[0].EndFrame != 719 {
		t.Fatalf("no-cut video must be one scene: %+v", d.Scenes)
	}
}

func TestSceneDetectBashNbFramesFallback(t *testing.T) {
	v, epDir, binDir := sceneTestEnv(t)
	os.WriteFile(filepath.Join(binDir, "ffprobe_no_frames"), []byte("1"), 0o644)
	bdMustRun(t, bdRender(t, "scene_detect", nil, v), v)
	d := readScenes(t, epDir)
	if d.FrameCount != 720 { // round(30 * 24)
		t.Fatalf("duration*fps fallback wrong: %+v", d)
	}
}

func TestSceneDetectBashFailClosed(t *testing.T) {
	cases := []struct {
		name    string
		sidecar string
		bin     string
		params  map[string]string
		wantMsg string
	}{
		{"probe fails", "ffprobe_fail", "ffprobe", nil, "ffprobe failed"},
		{"bad rate", "ffprobe_bad_rate", "ffprobe", nil, "r_frame_rate"},
		{"no duration", "ffprobe_no_duration", "ffprobe", nil, "duration"},
		{"ffmpeg fails", "ffmpeg_fail", "ffmpeg", nil, "scdet pass failed"},
		{"empty log", "ffmpeg_empty_log", "ffmpeg", nil, "scdet.log"},
		{"garbage log", "ffmpeg_bad_log", "ffmpeg", nil, "frame lines"},
		{"bad threshold", "", "", map[string]string{"threshold": "abc"}, "invalid threshold"},
		{"threshold range", "", "", map[string]string{"threshold": "101"}, "0..100"},
		{"bad min len", "", "", map[string]string{"min_scene_len": "-1"}, "invalid min_scene_len"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, _, binDir := sceneTestEnv(t)
			if tc.sidecar != "" {
				os.WriteFile(filepath.Join(binDir, tc.sidecar), []byte("1"), 0o644)
			}
			bdMustFail(t, bdRender(t, "scene_detect", tc.params, v), v, tc.wantMsg)
		})
	}
}
