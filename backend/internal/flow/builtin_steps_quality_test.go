package flow

import (
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestQualityGateRenderBothDialects asserts the script-content contract of
// the quality_gate builtin in BOTH dialects: libvmaf probe, loud SSIM
// fallback, sampled comparison, threshold failure, metric emission, the
// quality.json artifact, param validation, and the distinct tool-error
// messages. The generic parity suites (marker parity, no-PS-cmdlets,
// linkage) cover quality_gate automatically via allBuiltinKeys(); this
// test pins the feature-specific semantics.
func TestQualityGateRenderBothDialects(t *testing.T) {
	ps := renderBuiltinPS(t, "quality_gate")
	bash, err := renderBuiltinBash(t, "quality_gate")
	if err != nil {
		t.Fatalf("bash render quality_gate: %v", err)
	}

	// Content contract per dialect: each entry lists substrings that must
	// appear in the rendered script. PS and bash use different mechanics
	// for the same semantics, so some entries are dialect-specific.
	common := []string{
		// Sampling: select every Nth frame on BOTH inputs + re-timestamp +
		// 10-bit 4:2:0 normalization + scale source to encode dimensions.
		`select='not(mod(n\`,
		`,setpts=N/FRAME_RATE/TB,format=yuv420p10le`,
		"[0:v]", "[1:v]", "scale2ref",
		// EXACT scale2ref operand order (Q3): reference FIRST ([ref][dist])
		// so the SOURCE is scaled to the encode's dimensions, and the metric
		// filter takes [dists][refs] (distorted first). Substring presence of
		// "scale2ref" alone let a swapped graph pass review while real ffmpeg
		// flipped SSIM 0.979 -> 0.930 across the 0.95 threshold.
		"[ref][dist]scale2ref[refs][dists];[dists][refs]",
		// Default sampling factor 12 (drift guard vs the Go constant).
		QualityGateDefaultSampling,
		// libvmaf probe via ffmpeg -filters.
		"-filters", "libvmaf",
		// Loud fallback warning + fallback tracking.
		"WARNING: ffmpeg has no libvmaf support",
		"fallback",
		// Per-metric defaults (drift guard vs the Go constants).
		QualityGateVMAFThreshold, QualityGateSSIMThreshold,
		// Comparison filters + artifact paths (relative, cwd = episode dir).
		"libvmaf=log_fmt=json:log_path=quality_vmaf.json",
		"ssim=stats_file=quality_ssim.log",
		// Score parsing shapes.
		"pooled_metrics", "vmaf", "mean",
		"quality.json",
		"sampled_frames", "fallback_used",
		// Metric emission (quality_metric is numeric: metrics_json is float-only).
		"ENCODE_METRIC quality_score=",
		"ENCODE_METRIC quality_metric=",
		// Below-threshold verdict (distinct from tool failures).
		"is below threshold",
		"encode quality rejected",
		// Tool-failure classes, each with its own message.
		"muxed MKV not found",
		"no source media file found",
		"ffmpeg comparison failed with exit code",
		"tool failure, not a quality verdict",
		"could not parse VMAF score",
		"could not parse SSIM score",
		"no sampled frames",
		// Param validation.
		"invalid metric",
		"sampling must be an integer 1-1000",
		"threshold must be numeric",
		"out of range for",
		"VMAF-scale but libvmaf is unavailable",
		// Fail-closed score validation (Q1): non-finite / out-of-range
		// scores are tool errors, never a passing verdict.
		"failing closed",
		// Frame-count parity pre-check (Q5) + graceful ffprobe-missing path.
		"frame-count mismatch",
		"ffprobe not available",
	}
	psOnly := []string{
		"function Invoke-QualityGate",
		"Resolve-Tool $Job.BinDir 'ffmpeg.exe'",
		"Find-SourceFile $Job.EpisodeDir",
		// File-IDENTITY self-compare guard (Q2): path-string equality is
		// bypassed by symlinks AND hardlinks.
		"Test-SameFile",
		"IsFinite",
		"Push-Location -LiteralPath $Job.EpisodeDir",
		"ConvertFrom-Json",
		"ConvertTo-Json",
		"InvariantCulture",
	}
	bashOnly := []string{
		"quality_gate () {",
		"find_source_file \"$EpisodeDir\"",
		"command -v bc",
		"command -v jq",
		"jq -r '.pooled_metrics.vmaf.mean | select(type == \"number\") // empty'",
		"jq -n --arg metric",
		"cd \"$EpisodeDir\"",
	}

	for _, want := range common {
		if !strings.Contains(ps, want) {
			t.Errorf("PS render missing %q", want)
		}
		if !strings.Contains(bash, want) {
			t.Errorf("bash render missing %q", want)
		}
	}
	for _, want := range psOnly {
		if !strings.Contains(ps, want) {
			t.Errorf("PS render missing %q", want)
		}
	}
	for _, want := range bashOnly {
		if !strings.Contains(bash, want) {
			t.Errorf("bash render missing %q", want)
		}
	}

	// Negative graph contract (Q3): the SWAPPED operand order must not
	// appear in either dialect — it scales the encode to the source dims
	// and flips the scores (real ffmpeg: SSIM 0.979 -> 0.930).
	for _, d := range []struct{ name, body string }{{"PS", ps}, {"bash", bash}} {
		if strings.Contains(d.body, "[dist][ref]scale2ref") {
			t.Errorf("%s render carries SWAPPED scale2ref operands ([dist][ref])", d.name)
		}
		if strings.Contains(d.body, "[refs][dists]libvmaf") || strings.Contains(d.body, "[refs][dists]ssim") {
			t.Errorf("%s render feeds the metric filter swapped inputs ([refs][dists])", d.name)
		}
	}

	// The ffmpeg-missing error is dialect-specific (Resolve-Tool throws
	// its own message on PS); both must name ffmpeg and fail the step.
	if !strings.Contains(ps, "ffmpeg.exe") {
		t.Error("PS render must resolve ffmpeg.exe (missing-tool error names it)")
	}
	if !strings.Contains(bash, "ffmpeg not found") {
		t.Error("bash render missing 'ffmpeg not found' error")
	}

	// Progress markers: identical fixed sequence in both dialects (the
	// generic TestBuiltinBashMarkerParity also enforces this).
	for _, marker := range []string{
		"ENCODE_STEP quality_gate 0",
		"ENCODE_STEP quality_gate 20",
		"ENCODE_STEP quality_gate 80",
		"ENCODE_STEP quality_gate 100",
	} {
		if !strings.Contains(ps, marker) {
			t.Errorf("PS render missing marker %q", marker)
		}
		if !strings.Contains(bash, marker) {
			t.Errorf("bash render missing marker %q", marker)
		}
	}
}

// TestQualityGateParamsDeclared: the template must declare exactly the
// metric/threshold/sampling params with the documented defaults so the
// FlowBuilder surfaces them (the UI renders params purely from ParamDef).
func TestQualityGateParamsDeclared(t *testing.T) {
	tpl := QualityGateTemplate()
	if tpl.Key != "quality_gate" || !tpl.Builtin {
		t.Fatalf("unexpected template identity: %+v", tpl)
	}
	want := map[string]model.ParamDef{
		"metric":    {Key: "metric", Default: "vmaf"},
		"threshold": {Key: "threshold", Type: "number"},
		"sampling":  {Key: "sampling", Type: "number", Default: "12"},
	}
	if len(tpl.Params) != len(want) {
		t.Fatalf("want %d params, got %d: %+v", len(want), len(tpl.Params), tpl.Params)
	}
	for _, p := range tpl.Params {
		w, ok := want[p.Key]
		if !ok {
			t.Errorf("unexpected param %q", p.Key)
			continue
		}
		if p.Default != w.Default || p.Type != w.Type {
			t.Errorf("param %q: got default=%q type=%q, want default=%q type=%q",
				p.Key, p.Default, p.Type, w.Default, w.Type)
		}
		if p.Label == "" {
			t.Errorf("param %q has no label", p.Key)
		}
	}
	// threshold has NO static default: it is metric-dependent (90 VMAF /
	// 0.95 SSIM) and resolved script-side. A static default here would
	// prefill the wrong scale in the builder.
	for _, p := range tpl.Params {
		if p.Key == "threshold" && p.Default != "" {
			t.Errorf("threshold must not carry a static default (metric-dependent), got %q", p.Default)
		}
	}
}

// TestQualityGateRegisteredAfterVerifyOutput: the builtin inventory must
// list quality_gate immediately after verify_output (boot seeding order).
func TestQualityGateRegisteredAfterVerifyOutput(t *testing.T) {
	keys := allBuiltinKeys()
	for i, k := range keys {
		if k == "quality_gate" {
			if i == 0 || keys[i-1] != "verify_output" {
				t.Fatalf("quality_gate must follow verify_output in BuiltinStepTemplates (got %v)", keys)
			}
			return
		}
	}
	t.Fatal("quality_gate not registered in BuiltinStepTemplates")
}

// TestDefaultFlowsSeedQualityGateAfterVerifyOutput is the flow-package half
// of the factory guard: every NEW factory flow carries quality_gate
// immediately after verify_output (and before release_copy). The API-level
// half (existing stored flows are never modified) lives in
// api/seed_quality_gate_test.go.
func TestDefaultFlowsSeedQualityGateAfterVerifyOutput(t *testing.T) {
	for _, f := range []*model.Flow{DefaultFlow(), Default4kFlow(), Default4kCPUFlow()} {
		idx := -1
		for i, st := range f.Steps {
			if st.TemplateKey() == "quality_gate" {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Errorf("flow %q: quality_gate missing from the factory steps", f.Name)
			continue
		}
		if idx == 0 || f.Steps[idx-1].TemplateKey() != "verify_output" {
			t.Errorf("flow %q: quality_gate must come immediately after verify_output", f.Name)
		}
		if idx+1 >= len(f.Steps) || f.Steps[idx+1].TemplateKey() != "release_copy" {
			t.Errorf("flow %q: quality_gate must come before release_copy", f.Name)
		}
	}
}
