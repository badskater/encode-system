//go:build linux

package flow

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Execution tests for zone_build (bash) and the encode use_zones injection.
// Fixtures use the scene_detect scenes.json contract (one scene per line).

const zoneScenesFixture = `{
  "source": "src.m2ts",
  "fps": 24,
  "frame_count": 216,
  "duration": 9,
  "threshold": 10,
  "min_scene_len": 0,
  "scenes": [
    {"index": 1, "start_frame": 0, "end_frame": 71, "start_time": 0, "end_time": 3, "score": 0},
    {"index": 2, "start_frame": 72, "end_frame": 143, "start_time": 3, "end_time": 6, "score": 25},
    {"index": 3, "start_frame": 144, "end_frame": 215, "start_time": 6, "end_time": 9, "score": 30}
  ]
}`

const zoneChaptersFixture = `<?xml version="1.0" encoding="UTF-8"?>
<Chapters>
  <EditionEntry>
    <ChapterAtom>
      <ChapterTimeStart>00:00:00.000000000</ChapterTimeStart>
      <ChapterDisplay><ChapterString>Main</ChapterString><ChapterLanguage>eng</ChapterLanguage></ChapterDisplay>
    </ChapterAtom>
    <ChapterAtom>
      <ChapterTimeStart>00:00:03.000000000</ChapterTimeStart>
      <ChapterDisplay><ChapterString>ED</ChapterString><ChapterLanguage>eng</ChapterLanguage></ChapterDisplay>
    </ChapterAtom>
    <ChapterAtom>
      <ChapterTimeStart>00:00:06.000000000</ChapterTimeStart>
      <ChapterDisplay><ChapterString>Preview</ChapterString><ChapterLanguage>eng</ChapterLanguage></ChapterDisplay>
    </ChapterAtom>
  </EditionEntry>
</Chapters>`

func zoneTestEnv(t *testing.T, withChapters bool) (Vars, string) {
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
	os.WriteFile(filepath.Join(epDir, "scenes.json"), []byte(zoneScenesFixture), 0o644)
	os.WriteFile(filepath.Join(epDir, "1080.vpy"), []byte("# stub"), 0o644)
	if withChapters {
		os.WriteFile(filepath.Join(epDir, "chapters.xml"), []byte(zoneChaptersFixture), 0o644)
	}
	v := Vars{BinDir: binDir, ScriptsDir: scriptsDir, ReleaseDir: filepath.Join(root, "release"), Group: "TestGroup", Tag: "1080p"}
	return v, epDir
}

type zoneDoc struct {
	FrameCount int64   `json:"frame_count"`
	FPS        float64 `json:"fps"`
	Zones      []struct {
		Start  int64   `json:"start"`
		End    int64   `json:"end"`
		Mode   string  `json:"mode"`
		Value  float64 `json:"value"`
		Reason string  `json:"reason"`
	} `json:"zones"`
	X265 string `json:"x265"`
}

func readZones(t *testing.T, epDir string) zoneDoc {
	t.Helper()
	var d zoneDoc
	if err := json.Unmarshal([]byte(bdRead(t, filepath.Join(epDir, "zones.json"))), &d); err != nil {
		t.Fatalf("zones.json invalid: %v", err)
	}
	return d
}

func TestZoneBuildBashFullRules(t *testing.T) {
	v, epDir := zoneTestEnv(t, true)
	out := bdMustRun(t, bdRender(t, "zone_build", nil, v), v)
	for _, marker := range []string{"ENCODE_STEP zone_build 0", "ENCODE_STEP zone_build 30", "ENCODE_STEP zone_build 60", "ENCODE_STEP zone_build 100"} {
		if !strings.Contains(out, marker) {
			t.Errorf("missing marker %q:\n%s", marker, out)
		}
	}
	d := readZones(t, epDir)
	// Precedence order: heuristic first/last, then chapter ED + Preview.
	want := []struct {
		s, e   int64
		mode   string
		val    float64
		reason string
	}{
		{0, 71, "b", 0.6, "credits:first-scene"},
		{144, 215, "b", 0.6, "credits:last-scene"},
		{72, 143, "b", 0.8, "chapter:ED"},
		{144, 215, "b", 0.8, "chapter:Preview"},
	}
	if len(d.Zones) != len(want) {
		t.Fatalf("want %d zones, got %+v", len(want), d.Zones)
	}
	for i, w := range want {
		z := d.Zones[i]
		if z.Start != w.s || z.End != w.e || z.Mode != w.mode || z.Value != w.val || z.Reason != w.reason {
			t.Errorf("zone %d = %+v, want %+v", i, z, w)
		}
	}
	if d.X265 != "0,71,b=0.6/144,215,b=0.6/72,143,b=0.8/144,215,b=0.8" {
		t.Errorf("x265 string wrong: %q", d.X265)
	}
}

func TestZoneBuildBashOverridesLast(t *testing.T) {
	v, epDir := zoneTestEnv(t, true)
	bdMustRun(t, bdRender(t, "zone_build", map[string]string{"overrides": "0,50,q=18/100,120,b=1.5"}, v), v)
	d := readZones(t, epDir)
	n := len(d.Zones)
	if n < 2 {
		t.Fatalf("overrides missing: %+v", d.Zones)
	}
	q := d.Zones[n-2]
	b := d.Zones[n-1]
	if q.Start != 0 || q.End != 50 || q.Mode != "q" || q.Value != 18 || q.Reason != "override" {
		t.Errorf("q override wrong: %+v", q)
	}
	if b.Start != 100 || b.End != 120 || b.Mode != "b" || b.Value != 1.5 {
		t.Errorf("b override wrong: %+v", b)
	}
	if !strings.HasSuffix(d.X265, "/0,50,q=18/100,120,b=1.5") {
		t.Errorf("overrides must come last in x265 string: %q", d.X265)
	}
}

func TestZoneBuildBashHeuristicOnly(t *testing.T) {
	v, epDir := zoneTestEnv(t, false)
	bdMustRun(t, bdRender(t, "zone_build", nil, v), v)
	d := readZones(t, epDir)
	if len(d.Zones) != 2 || d.Zones[0].Reason != "credits:first-scene" || d.Zones[1].Reason != "credits:last-scene" {
		t.Fatalf("without chapters only the heuristic applies: %+v", d.Zones)
	}
}

func TestZoneBuildBashSingleSceneNoHeuristic(t *testing.T) {
	v, epDir := zoneTestEnv(t, false)
	os.WriteFile(filepath.Join(epDir, "scenes.json"), []byte(`{
  "fps": 24, "frame_count": 216,
  "scenes": [
    {"index": 1, "start_frame": 0, "end_frame": 215, "start_time": 0, "end_time": 9, "score": 0}
  ]
}`), 0o644)
	bdMustRun(t, bdRender(t, "zone_build", nil, v), v)
	d := readZones(t, epDir)
	if len(d.Zones) != 0 || d.X265 != "" {
		t.Fatalf("single scene must not get credits zones: %+v", d)
	}
}

func TestZoneBuildBashCustomPatterns(t *testing.T) {
	v, epDir := zoneTestEnv(t, true)
	bdMustRun(t, bdRender(t, "zone_build", map[string]string{"name_patterns": "Preview"}, v), v)
	d := readZones(t, epDir)
	for _, z := range d.Zones {
		if strings.HasPrefix(z.Reason, "chapter:") && z.Reason != "chapter:Preview" {
			t.Errorf("custom pattern leaked a match: %+v", z)
		}
	}
}

func TestZoneBuildBashSkipIfFresh(t *testing.T) {
	v, epDir := zoneTestEnv(t, true)
	os.WriteFile(filepath.Join(epDir, "zones.json"), []byte(`{"zones":[],"x265":""}`), 0o644)
	future := time.Now().Add(time.Minute)
	os.Chtimes(filepath.Join(epDir, "zones.json"), future, future)
	out := bdMustRun(t, bdRender(t, "zone_build", nil, v), v)
	if !strings.Contains(out, "reusing") {
		t.Fatalf("skip-if-fresh not honored:\n%s", out)
	}
}

func TestZoneBuildBashFailClosed(t *testing.T) {
	cases := []struct {
		name     string
		params   map[string]string
		noScenes bool
		wantMsg  string
	}{
		{"missing scenes", nil, true, "scenes.json missing"},
		{"bad override token", map[string]string{"overrides": "0-50:b=0.5"}, false, "invalid override token"},
		{"override out of range", map[string]string{"overrides": "0,300,b=0.5"}, false, "outside"},
		{"override start>=end", map[string]string{"overrides": "50,50,b=0.5"}, false, "start >= end"},
		{"override b zero", map[string]string{"overrides": "0,50,b=0"}, false, "(0, 10]"},
		{"override q too big", map[string]string{"overrides": "0,50,q=52"}, false, "0..51"},
		{"bad credits_b", map[string]string{"credits_b": "abc"}, false, "(0, 10]"},
		{"bad credits_max", map[string]string{"credits_max_seconds": "-5"}, false, "0..36000"},
		{"bad regex", map[string]string{"name_patterns": "["}, false, "name_patterns"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, epDir := zoneTestEnv(t, false)
			if tc.noScenes {
				os.Remove(filepath.Join(epDir, "scenes.json"))
			}
			bdMustFail(t, bdRender(t, "zone_build", tc.params, v), v, tc.wantMsg)
		})
	}
}

// --- encode use_zones injection -----------------------------------------

// zoneEncodeEnv adds x265/vspipe stubs; the runner puts the stub dir on
// PATH (bash encode calls vspipe from PATH).
func zoneEncodeEnv(t *testing.T, zonesJSON string) (Vars, string, string) {
	t.Helper()
	v, epDir := zoneTestEnv(t, false)
	binDir := v.BinDir
	if zonesJSON != "" {
		os.WriteFile(filepath.Join(epDir, "zones.json"), []byte(zonesJSON), 0o644)
	}
	x265Stub := `#!/usr/bin/env bash
dir="$(cd "$(dirname "$0")" && pwd)"
printf '%s\n' "$@" > "$dir/x265_args.txt"
out=''; prev=''
for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; prev="$a"; done
[ -n "$out" ] && : > "$out"
cat > /dev/null
echo "x265 stub: encoded"
`
	vspipeStub := "#!/usr/bin/env bash\nexit 0\n"
	os.WriteFile(filepath.Join(binDir, "x265"), []byte(x265Stub), 0o755)
	os.WriteFile(filepath.Join(binDir, "vspipe"), []byte(vspipeStub), 0o755)
	return v, epDir, binDir
}

func zoneEncodeRun(t *testing.T, script string, v Vars, binDir string) (string, error) {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "enc.sh")
	os.WriteFile(scriptPath, []byte(script), 0o755)
	cmd := exec.Command("bash", scriptPath)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "PATH="+binDir+":/usr/bin:/bin:/usr/local/bin")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestEncodeZonesInjection(t *testing.T) {
	v, _, binDir := zoneEncodeEnv(t, `{"zones":[{"start":0,"end":71,"mode":"b","value":0.6,"reason":"credits:first-scene"}],"x265":"0,71,b=0.6"}`)
	out, err := zoneEncodeRun(t, bdRender(t, "encode", map[string]string{"use_zones": "true"}, v), v, binDir)
	if err != nil {
		t.Fatalf("encode with zones failed: %v\n%s", err, out)
	}
	args := bdRead(t, filepath.Join(binDir, "x265_args.txt"))
	if !strings.Contains(args, "--zones\n0,71,b=0.6\n") {
		t.Fatalf("--zones not injected:\n%s", args)
	}
	// --zones must precede the -o tail.
	zi := strings.Index(args, "--zones")
	oi := strings.Index(args, "\n-o\n")
	if zi < 0 || oi < 0 || zi > oi {
		t.Fatalf("--zones must come before -o:\n%s", args)
	}
}

func TestEncodeZonesDefaultOff(t *testing.T) {
	v, _, binDir := zoneEncodeEnv(t, `{"zones":[],"x265":"0,71,b=0.6"}`)
	out, err := zoneEncodeRun(t, bdRender(t, "encode", nil, v), v, binDir)
	if err != nil {
		t.Fatalf("encode failed: %v\n%s", err, out)
	}
	args := bdRead(t, filepath.Join(binDir, "x265_args.txt"))
	if strings.Contains(args, "--zones") {
		t.Fatalf("zones must be off by default:\n%s", args)
	}
}

func TestEncodeZonesRawArgsMode(t *testing.T) {
	v, _, binDir := zoneEncodeEnv(t, `{"zones":[],"x265":"10,20,q=18"}`)
	out, err := zoneEncodeRun(t, bdRender(t, "encode", map[string]string{"use_zones": "true", "x265_args": "--preset fast --crf 18"}, v), v, binDir)
	if err != nil {
		t.Fatalf("raw-mode encode failed: %v\n%s", err, out)
	}
	args := bdRead(t, filepath.Join(binDir, "x265_args.txt"))
	if !strings.Contains(args, "--preset\nfast\n") || !strings.Contains(args, "--zones\n10,20,q=18\n") {
		t.Fatalf("raw args + zones must coexist:\n%s", args)
	}
}

func TestEncodeZonesEmptyString(t *testing.T) {
	v, _, binDir := zoneEncodeEnv(t, `{"zones":[],"x265":""}`)
	out, err := zoneEncodeRun(t, bdRender(t, "encode", map[string]string{"use_zones": "true"}, v), v, binDir)
	if err != nil {
		t.Fatalf("empty zones must succeed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no zones applied") {
		t.Fatalf("empty-zones log missing:\n%s", out)
	}
	args := bdRead(t, filepath.Join(binDir, "x265_args.txt"))
	if strings.Contains(args, "--zones") {
		t.Fatalf("empty string must not inject --zones:\n%s", args)
	}
}

func TestEncodeZonesFailClosed(t *testing.T) {
	t.Run("missing zones.json", func(t *testing.T) {
		v, _, binDir := zoneEncodeEnv(t, "")
		out, err := zoneEncodeRun(t, bdRender(t, "encode", map[string]string{"use_zones": "true"}, v), v, binDir)
		if err == nil || !strings.Contains(out, "zones.json is missing") || !strings.Contains(out, "ENCODE_STEP_FAILED") {
			t.Fatalf("must fail closed:\n%s", out)
		}
	})
	t.Run("malformed string", func(t *testing.T) {
		v, _, binDir := zoneEncodeEnv(t, `{"zones":[],"x265":"0,71;rm -rf"}`)
		out, err := zoneEncodeRun(t, bdRender(t, "encode", map[string]string{"use_zones": "true"}, v), v, binDir)
		if err == nil || !strings.Contains(out, "malformed") || !strings.Contains(out, "ENCODE_STEP_FAILED") {
			t.Fatalf("must fail closed:\n%s", out)
		}
	})
}
