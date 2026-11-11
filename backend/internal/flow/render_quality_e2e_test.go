package flow

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// Stub-exec end-to-end tests for quality_gate (both dialects).
//
// ffmpeg is stubbed (it is not a test dependency): a bash stub named
// ffmpeg / ffmpeg.exe answers the -filters probe from a sidecar file and
// fabricates the comparison artifacts — a libvmaf JSON log (real shape:
// pooled_metrics.vmaf.mean + frames[]) or an SSIM stderr summary + per-frame
// stats file. Sidecar files configure the score, frame count and exit code
// per subtest, exactly like the mkvmerge.json sidecar pattern in
// render_verify_e2e_test.go.
//
// jq and bc are ALSO stubbed (python3-backed, faithful to the exact queries
// the template issues) because neither is a test-host dependency; the
// generic exec suite already stubs them the same way. The JSON shapes the
// stubs emit were validated against a real ffmpeg 9 build with libvmaf.
//
// The bash suite always runs (bash is a test dependency); the PowerShell
// suite runs when pwsh is installed and skips otherwise (same convention as
// render_verify_e2e_test.go).

const qgFiltersWithLibvmaf = ` .. libvmaf           VV->V      Calculate the VMAF between two video streams.
 .. ssim              VV->V      Calculate the SSIM between two video streams.
`

const qgFiltersWithoutLibvmaf = ` .. ssim              VV->V      Calculate the SSIM between two video streams.
`

// qgFFmpegStub is the fake ffmpeg. Probe mode (-filters) cats the sidecar;
// comparison mode inspects the -lavfi argument and writes the artifacts the
// real filters would produce, into the CURRENT directory (the step runs
// ffmpeg with cwd = episode dir, so relative artifact paths land there).
const qgFFmpegStub = `#!/usr/bin/env bash
dir="$(cd "$(dirname "$0")" && pwd)"
for a in "$@"; do
  if [ "$a" = "-filters" ]; then
    cat "$dir/filters.txt" 2>/dev/null
    exit 0
  fi
done
filter=""
prev=""
for a in "$@"; do
  [ "$prev" = "-lavfi" ] && filter="$a"
  prev="$a"
done
rc=0
[ -f "$dir/ffmpeg_exit" ] && rc=$(cat "$dir/ffmpeg_exit")
case "$filter" in
  *libvmaf=*)
    if [ -f "$dir/vmaf.json" ]; then
      cp "$dir/vmaf.json" quality_vmaf.json
      echo "[Parsed_libvmaf @ 0x1] VMAF score: stub" >&2
    fi
    ;;
  *ssim=*)
    score="0.99"
    [ -f "$dir/ssim_score" ] && score=$(cat "$dir/ssim_score")
    n=13
    [ -f "$dir/ssim_frames" ] && n=$(cat "$dir/ssim_frames")
    : > quality_ssim.log
    i=1
    while [ "$i" -le "$n" ]; do
      echo "n:$i Y:0.99 U:0.99 V:0.99 All:$score (30.0)" >> quality_ssim.log
      i=$((i+1))
    done
    if [ ! -f "$dir/ssim_no_summary" ]; then
      echo "[Parsed_ssim_0 @ 0x1] SSIM Y:0.99 (30.0) U:0.99 (30.0) V:0.99 (30.0) All:$score (30.0)" >&2
    fi
    ;;
esac
echo "ffmpeg stub: comparison done" >&2
exit "$rc"
`

// qgJQStub implements EXACTLY the three jq invocations the bash template
// issues: -r '.pooled_metrics.vmaf.mean // empty' FILE, -r '.frames |
// length' FILE, and -n --arg/--argjson object construction. python3 is a
// test-host dependency (the generic exec suite's bc stub already uses it).
const qgJQStub = `#!/usr/bin/env python3
import sys, json
args = sys.argv[1:]
if '-n' in args:
    out = {}
    i = 0
    while i < len(args):
        if args[i] == '--arg':
            out[args[i+1]] = args[i+2]; i += 3
        elif args[i] == '--argjson':
            out[args[i+1]] = json.loads(args[i+2]); i += 3
        else:
            i += 1
    print(json.dumps(out))
    sys.exit(0)
q = None; path = None
for a in args:
    if a.startswith('-'):
        continue
    if q is None:
        q = a
    else:
        path = a
try:
    with open(path) as f:
        data = json.load(f)
except Exception:
    sys.exit(1)
if 'pooled_metrics' in q:
    try:
        print(data['pooled_metrics']['vmaf']['mean'])
    except (KeyError, TypeError):
        pass  # '// empty' -> no output
elif 'length' in q:
    frames = data.get('frames')
    print(len(frames) if frames is not None else 0)
`

// qgBCStub evaluates bc -l expressions (comparisons joined by || / &&).
const qgBCStub = `#!/usr/bin/env bash
expr=$(cat)
python3 - "$expr" <<'PY'
import sys
e = sys.argv[1].strip().replace('||', ' or ').replace('&&', ' and ')
try:
    r = eval(e, {'__builtins__': {}}, {})
    print(1 if r else 0)
except Exception:
    print(0)
PY
`

// qgVMAFLog builds a libvmaf JSON log in the REAL shape (validated against
// ffmpeg 9 / libvmaf 2.x): pooled_metrics.vmaf.mean + one frames[] entry
// per compared frame.
func qgVMAFLog(mean float64, frames int) string {
	var b strings.Builder
	b.WriteString(`{"version":"2.3.1","fps":24.0,"frames":[`)
	for i := 0; i < frames; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"frameNum":%d,"metrics":{"vmaf":%g}}`, i, mean)
	}
	fmt.Fprintf(&b, `],"pooled_metrics":{"vmaf":{"min":%g,"max":%g,"mean":%g,"harmonic_mean":%g}}}`,
		mean, mean, mean, mean)
	return b.String()
}

// qgCase is one stub-exec scenario. Zero values mean "default": ssim score
// 0.99, 13 sampled frames, ffmpeg exit 0, source + MKV present.
type qgCase struct {
	name   string
	params map[string]string

	filters        string  // -filters sidecar ("" = file absent -> no libvmaf)
	vmafMean       float64 // >0: write a vmaf.json sidecar with this mean
	vmafFrames     int     // frames[] length (default 13 when vmafMean>0)
	vmafBadJSON    bool    // write garbage instead of a valid log
	noVmafLog      bool    // ffmpeg exits 0 but writes no log at all
	ssimScore      string  // All: value (default "0.99")
	ssimFrames     int     // stats-file lines (default 13)
	ssimZeroFrames bool    // explicit zero-frame stats file (empty comparison)
	ssimNoSummary  bool    // stats file but no stderr summary line
	ffmpegExit     int     // comparison-run exit code
	noFFmpeg       bool    // no ffmpeg on PATH or in BinDir
	noMKV          bool    // muxed output absent
	noSource       bool    // source media absent
	wantFail       bool
	wantSubstr     []string // output must contain (both dialects)
	wantPS         []string // PS-only output substrings
	wantBash       []string // bash-only output substrings
	wantAbsent     []string // output must NOT contain

	// quality.json expectations (only checked when wantArtifact is true).
	wantArtifact  bool
	artMetric     string
	artScore      float64
	artThreshold  float64
	artFrames     int
	artFallback   bool
	wantMetricOut []string // exact ENCODE_METRIC lines expected in output
}

func qgCases() []qgCase {
	return []qgCase{
		{
			name: "vmaf_pass", filters: qgFiltersWithLibvmaf, vmafMean: 95.5,
			wantSubstr:   []string{"ENCODE_STEP quality_gate 100", "metric: vmaf"},
			wantArtifact: true, artMetric: "vmaf", artScore: 95.5, artThreshold: 90, artFrames: 13,
			wantMetricOut: []string{"ENCODE_METRIC quality_score=95.5000", "ENCODE_METRIC quality_metric=1"},
		},
		{
			name: "vmaf_below_threshold", filters: qgFiltersWithLibvmaf, vmafMean: 82.1,
			wantFail: true,
			wantSubstr: []string{
				"ENCODE_STEP_FAILED quality_gate",
				"is below threshold", "82.1000", "90", "encode quality rejected",
				// Metrics + artifact are emitted BEFORE the verdict so a
				// rejected encode still reports its score.
				"ENCODE_METRIC quality_score=82.1000",
			},
			wantArtifact: true, artMetric: "vmaf", artScore: 82.1, artThreshold: 90, artFrames: 13,
		},
		{
			name: "vmaf_custom_threshold_pass", filters: qgFiltersWithLibvmaf, vmafMean: 82.1,
			params:       map[string]string{"threshold": "80"},
			wantSubstr:   []string{"ENCODE_STEP quality_gate 100"},
			wantArtifact: true, artMetric: "vmaf", artScore: 82.1, artThreshold: 80, artFrames: 13,
		},
		{
			name: "ssim_explicit_pass", filters: qgFiltersWithLibvmaf, ssimScore: "0.97",
			params:       map[string]string{"metric": "ssim"},
			wantSubstr:   []string{"ENCODE_STEP quality_gate 100", "metric: ssim"},
			wantAbsent:   []string{"WARNING"},
			wantArtifact: true, artMetric: "ssim", artScore: 0.97, artThreshold: 0.95, artFrames: 13,
			wantMetricOut: []string{"ENCODE_METRIC quality_score=0.970000", "ENCODE_METRIC quality_metric=0"},
		},
		{
			name: "ssim_below_threshold", filters: qgFiltersWithLibvmaf, ssimScore: "0.90",
			params:       map[string]string{"metric": "ssim"},
			wantFail:     true,
			wantSubstr:   []string{"ENCODE_STEP_FAILED quality_gate", "is below threshold", "0.900000", "0.95"},
			wantArtifact: true, artMetric: "ssim", artScore: 0.90, artThreshold: 0.95, artFrames: 13,
		},
		{
			name: "fallback_pass", filters: qgFiltersWithoutLibvmaf, ssimScore: "0.97",
			wantSubstr: []string{
				"WARNING: ffmpeg has no libvmaf support",
				"ENCODE_STEP quality_gate 100",
				"ENCODE_METRIC quality_metric=0",
			},
			wantArtifact: true, artMetric: "ssim", artScore: 0.97, artThreshold: 0.95, artFrames: 13, artFallback: true,
		},
		{
			name: "fallback_vmaf_scale_threshold_rejected", filters: qgFiltersWithoutLibvmaf,
			params:     map[string]string{"threshold": "90"},
			wantFail:   true,
			wantSubstr: []string{"ENCODE_STEP_FAILED quality_gate", "VMAF-scale but libvmaf is unavailable"},
		},
		{
			name: "fallback_explicit_ssim_threshold", filters: qgFiltersWithoutLibvmaf, ssimScore: "0.97",
			params:       map[string]string{"threshold": "0.9"},
			wantSubstr:   []string{"ENCODE_STEP quality_gate 100"},
			wantArtifact: true, artMetric: "ssim", artScore: 0.97, artThreshold: 0.9, artFrames: 13, artFallback: true,
		},
		{
			name: "ffmpeg_missing", noFFmpeg: true,
			wantFail:   true,
			wantSubstr: []string{"ENCODE_STEP_FAILED quality_gate"},
			wantBash:   []string{"ffmpeg not found"},
			wantPS:     []string{"required tool not found"},
		},
		{
			name: "ffmpeg_crash", filters: qgFiltersWithLibvmaf, ffmpegExit: 1,
			wantFail: true,
			wantSubstr: []string{
				"ENCODE_STEP_FAILED quality_gate",
				"ffmpeg comparison failed with exit code 1",
				"tool failure, not a quality verdict",
			},
			wantAbsent: []string{"is below threshold"},
		},
		{
			name: "vmaf_no_log", filters: qgFiltersWithLibvmaf, noVmafLog: true,
			wantFail:   true,
			wantSubstr: []string{"ENCODE_STEP_FAILED quality_gate", "wrote no VMAF log"},
		},
		{
			name: "vmaf_bad_json", filters: qgFiltersWithLibvmaf, vmafBadJSON: true,
			wantFail:   true,
			wantSubstr: []string{"ENCODE_STEP_FAILED quality_gate", "could not parse VMAF score"},
		},
		{
			name: "ssim_no_summary", filters: qgFiltersWithLibvmaf, ssimNoSummary: true,
			params:     map[string]string{"metric": "ssim"},
			wantFail:   true,
			wantSubstr: []string{"ENCODE_STEP_FAILED quality_gate", "could not parse SSIM score"},
		},
		{
			name: "ssim_zero_frames", filters: qgFiltersWithLibvmaf, ssimZeroFrames: true,
			params:     map[string]string{"metric": "ssim"},
			wantFail:   true,
			wantSubstr: []string{"ENCODE_STEP_FAILED quality_gate", "no sampled frames"},
		},
		{
			name: "missing_mkv", filters: qgFiltersWithLibvmaf, noMKV: true,
			wantFail:   true,
			wantSubstr: []string{"ENCODE_STEP_FAILED quality_gate", "muxed MKV not found"},
		},
		{
			name: "missing_source", filters: qgFiltersWithLibvmaf, noSource: true,
			wantFail:   true,
			wantSubstr: []string{"ENCODE_STEP_FAILED quality_gate", "no source media file found"},
		},
		{
			name: "sampling_param_reaches_ffmpeg", filters: qgFiltersWithLibvmaf, vmafMean: 95.5,
			params:       map[string]string{"sampling": "4"},
			wantSubstr:   []string{`mod(n\,4)`, "ENCODE_STEP quality_gate 100"},
			wantArtifact: true, artMetric: "vmaf", artScore: 95.5, artThreshold: 90, artFrames: 13,
		},
		{
			name: "metric_case_insensitive", filters: qgFiltersWithLibvmaf, vmafMean: 95.5,
			params:     map[string]string{"metric": "VMAF"},
			wantSubstr: []string{"ENCODE_STEP quality_gate 100", "metric: vmaf"},
		},
	}
}

// qgParamValidationCases is the invalid-param table: every row must fail
// the step with the validation message (never run ffmpeg).
func qgParamValidationCases() []qgCase {
	return []qgCase{
		{name: "bad_metric", params: map[string]string{"metric": "psnr"},
			wantFail: true, wantSubstr: []string{"invalid metric", "ENCODE_STEP_FAILED quality_gate"}},
		{name: "sampling_zero", params: map[string]string{"sampling": "0"},
			wantFail: true, wantSubstr: []string{"sampling must be an integer 1-1000"}},
		{name: "sampling_non_numeric", params: map[string]string{"sampling": "abc"},
			wantFail: true, wantSubstr: []string{"sampling must be an integer 1-1000"}},
		{name: "sampling_over_max", params: map[string]string{"sampling": "1001"},
			wantFail: true, wantSubstr: []string{"sampling must be an integer 1-1000"}},
		{name: "sampling_overflow", params: map[string]string{"sampling": "99999"},
			wantFail: true, wantSubstr: []string{"sampling must be an integer 1-1000"}},
		{name: "threshold_non_numeric", params: map[string]string{"threshold": "abc"},
			wantFail: true, wantSubstr: []string{"threshold must be numeric"}},
		{name: "threshold_negative", params: map[string]string{"threshold": "-1"},
			wantFail: true, wantSubstr: []string{"threshold must be numeric"}},
		{name: "threshold_vmaf_over_100", params: map[string]string{"threshold": "101"},
			wantFail: true, wantSubstr: []string{"out of range for vmaf"}},
		{name: "threshold_ssim_over_1", params: map[string]string{"metric": "ssim", "threshold": "1.5"},
			wantFail: true, wantSubstr: []string{"out of range for ssim"}},
		{name: "threshold_overflow", params: map[string]string{"threshold": "9999999999999"},
			wantFail: true, wantSubstr: []string{"threshold must be numeric"}},
	}
}

// qgSetup builds the stub toolchain + episode tree for one case and returns
// the render Vars, the episode dir, and the PATH to run with.
func qgSetup(t *testing.T, tc qgCase, exeSuffix string) (Vars, string, string) {
	t.Helper()
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	scriptsDir := filepath.Join(root, "scripts")
	releaseDir := filepath.Join(root, "release")
	for _, d := range []string{binDir, scriptsDir, releaseDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	writeStub := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// ffmpeg stub + sidecars (skipped entirely for the missing-ffmpeg case).
	if !tc.noFFmpeg {
		writeStub(binDir, "ffmpeg"+exeSuffix, qgFFmpegStub)
		filters := tc.filters
		if filters == "" {
			filters = qgFiltersWithoutLibvmaf
		}
		writeStub(binDir, "filters.txt", filters)
		switch {
		case tc.vmafBadJSON:
			writeStub(binDir, "vmaf.json", "{not valid json")
		case tc.vmafMean > 0:
			n := tc.vmafFrames
			if n == 0 {
				n = 13
			}
			writeStub(binDir, "vmaf.json", qgVMAFLog(tc.vmafMean, n))
		case tc.noVmafLog:
			// no sidecar -> stub writes no log
		}
		if tc.ssimScore != "" {
			writeStub(binDir, "ssim_score", tc.ssimScore)
		}
		// Frame-count sidecar: only when deviating from the stub default
		// (13). ssimZeroFrames writes an explicit 0 (empty stats file).
		if tc.ssimZeroFrames {
			writeStub(binDir, "ssim_frames", "0")
		} else if tc.ssimFrames != 0 && tc.ssimFrames != 13 {
			writeStub(binDir, "ssim_frames", fmt.Sprintf("%d", tc.ssimFrames))
		}
		if tc.ssimNoSummary {
			writeStub(binDir, "ssim_no_summary", "1")
		}
		if tc.ffmpegExit != 0 {
			writeStub(binDir, "ffmpeg_exit", fmt.Sprintf("%d", tc.ffmpegExit))
		}
	}

	// jq + bc stubs (bash only needs them, but harmless for PS runs).
	writeStub(binDir, "jq", qgJQStub)
	writeStub(binDir, "bc", qgBCStub)

	// Episode tree: source + muxed MKV (the artifacts mux/verify produced).
	epDir := filepath.Join(scriptsDir, "Quality Series", "Ep 01")
	if err := os.MkdirAll(epDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !tc.noSource {
		if err := os.WriteFile(filepath.Join(epDir, "src.mkv"), []byte("fake-source"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if !tc.noMKV {
		mkv := filepath.Join(epDir, OutputName("Quality Series", "01", "1080p"))
		if err := os.WriteFile(mkv, []byte("fake-mkv"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	v := Vars{BinDir: binDir, ScriptsDir: scriptsDir, ReleaseDir: releaseDir, Group: "TestGroup", Tag: "1080p"}

	// PATH: stub dir first, real coreutils behind. For the missing-ffmpeg
	// case build a MINIMAL path (coreutils symlinks + jq/bc/python3, no
	// ffmpeg) so a host-installed ffmpeg can never satisfy the lookup.
	path := binDir + ":/usr/bin:/bin:/usr/local/bin"
	if tc.noFFmpeg {
		minDir := filepath.Join(root, "minbin")
		if err := os.MkdirAll(minDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, tool := range []string{"bash", "find", "sort", "head", "tr", "grep", "sed", "wc", "cut", "tail", "python3"} {
			p, err := exec.LookPath(tool)
			if err != nil {
				t.Skipf("coreutils tool %q not available: %v", tool, err)
			}
			if err := os.Symlink(p, filepath.Join(minDir, tool)); err != nil {
				t.Fatal(err)
			}
		}
		// jq/bc stubs must be reachable from the minimal PATH too.
		for _, tool := range []string{"jq", "bc"} {
			if err := os.Symlink(filepath.Join(binDir, tool), filepath.Join(minDir, tool)); err != nil {
				t.Fatal(err)
			}
		}
		path = minDir
	}
	return v, epDir, path
}

// qgRender renders a single-step quality_gate flow in the given dialect.
func qgRender(t *testing.T, tc qgCase, v Vars, d Dialect) string {
	t.Helper()
	f := &model.Flow{Name: "quality-e2e", Steps: []model.Step{
		{Type: model.StepType("quality_gate"), Params: tc.params},
	}}
	j := &model.Job{ID: 70, Series: "Quality Series", Episode: "01",
		EpisodeDir: "Quality Series/Ep 01", ScriptType: "vpy"}
	script, err := RenderDialect(f, j, v, BuiltinResolver(), d)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return script
}

// qgAssert checks one executed case's output + quality.json artifact.
func qgAssert(t *testing.T, tc qgCase, text string, runErr error, epDir string, ps bool) {
	t.Helper()
	if tc.wantFail {
		if runErr == nil {
			t.Fatalf("expected failure but script exited 0\n%s", text)
		}
		if !strings.Contains(text, "ENCODE_STEP_FAILED quality_gate") {
			t.Errorf("output missing ENCODE_STEP_FAILED quality_gate\n%s", text)
		}
		if strings.Contains(text, "ENCODE_STEP quality_gate 100") {
			t.Errorf("failing case emitted the pass marker\n%s", text)
		}
	} else {
		if runErr != nil {
			t.Fatalf("expected pass but script failed: %v\n%s", runErr, text)
		}
		if strings.Contains(text, "ENCODE_STEP_FAILED") {
			t.Errorf("unexpected ENCODE_STEP_FAILED in passing case\n%s", text)
		}
	}
	for _, want := range tc.wantSubstr {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q\n%s", want, text)
		}
	}
	for _, want := range tc.wantBash {
		if !ps && !strings.Contains(text, want) {
			t.Errorf("bash output missing %q\n%s", want, text)
		}
	}
	for _, want := range tc.wantPS {
		if ps && !strings.Contains(text, want) {
			t.Errorf("PS output missing %q\n%s", want, text)
		}
	}
	for _, absent := range tc.wantAbsent {
		if strings.Contains(text, absent) {
			t.Errorf("output must NOT contain %q\n%s", absent, text)
		}
	}
	for _, line := range tc.wantMetricOut {
		if !strings.Contains(text, line) {
			t.Errorf("output missing metric line %q\n%s", line, text)
		}
	}
	if tc.wantArtifact {
		raw, err := os.ReadFile(filepath.Join(epDir, "quality.json"))
		if err != nil {
			t.Fatalf("quality.json not written: %v\n%s", err, text)
		}
		var art struct {
			Metric        string  `json:"metric"`
			Score         float64 `json:"score"`
			Threshold     float64 `json:"threshold"`
			SampledFrames int     `json:"sampled_frames"`
			FallbackUsed  bool    `json:"fallback_used"`
		}
		if err := json.Unmarshal(raw, &art); err != nil {
			t.Fatalf("quality.json unparseable: %v\n%s", err, raw)
		}
		if art.Metric != tc.artMetric {
			t.Errorf("quality.json metric = %q, want %q", art.Metric, tc.artMetric)
		}
		if diff := art.Score - tc.artScore; diff > 1e-6 || diff < -1e-6 {
			t.Errorf("quality.json score = %v, want %v", art.Score, tc.artScore)
		}
		if diff := art.Threshold - tc.artThreshold; diff > 1e-6 || diff < -1e-6 {
			t.Errorf("quality.json threshold = %v, want %v", art.Threshold, tc.artThreshold)
		}
		if art.SampledFrames != tc.artFrames {
			t.Errorf("quality.json sampled_frames = %d, want %d", art.SampledFrames, tc.artFrames)
		}
		if art.FallbackUsed != tc.artFallback {
			t.Errorf("quality.json fallback_used = %v, want %v", art.FallbackUsed, tc.artFallback)
		}
	}
}

// TestQualityGateBashStubExec runs every scenario under /bin/bash with the
// stubbed ffmpeg/jq/bc toolchain. This is the primary executed proof: score
// parsing, below-threshold failure, the loud fallback path, tool-error
// classes, param validation, and the quality.json artifact.
func TestQualityGateBashStubExec(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed; skipping bash execution test")
	}
	cases := append(qgCases(), qgParamValidationCases()...)
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			v, epDir, path := qgSetup(t, tc, "")
			script := qgRender(t, tc, v, BashDialect{})
			scriptPath := filepath.Join(epDir, "job.sh")
			if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bash, scriptPath)
			cmd.Env = append(os.Environ(), "PATH="+path)
			out, runErr := cmd.CombinedOutput()
			text := string(out)
			t.Logf("bash output:\n%s", text)
			if strings.Contains(text, "command not found") {
				t.Fatalf("command not found in output (linkage broken):\n%s", text)
			}
			qgAssert(t, tc, text, runErr, epDir, false)
		})
	}
}

// TestQualityGatePowerShellStubExec is the PS parity of the bash stub-exec
// suite (same scenarios, ffmpeg.exe stub executed via shebang). Skipped when
// pwsh is absent, exactly like render_verify_e2e_test.go.
func TestQualityGatePowerShellStubExec(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not installed; skipping PowerShell integration test")
	}
	libPath, err := filepath.Abs("../../../powershell/EncodeLib.ps1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(libPath); err != nil {
		t.Fatalf("EncodeLib.ps1 not found at %s", libPath)
	}
	cases := append(qgCases(), qgParamValidationCases()...)
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			v, epDir, path := qgSetup(t, tc, ".exe")
			script := qgRender(t, tc, v, PowerShellDialect{})
			scriptPath := filepath.Join(epDir, "job.ps1")
			if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(pwsh, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", scriptPath, "-LibPath", libPath)
			cmd.Env = append(os.Environ(), "PATH="+path, "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1")
			out, runErr := cmd.CombinedOutput()
			text := string(out)
			t.Logf("pwsh output:\n%s", text)
			qgAssert(t, tc, text, runErr, epDir, true)
		})
	}
}
