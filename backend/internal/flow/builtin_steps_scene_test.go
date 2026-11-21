package flow

import (
	"strings"
	"testing"
)

// scene_detect (phase 2 of series-scripts-scene-zones): ffmpeg scdet over
// the episode source publishes scenes.json for the zone builder. These
// contract tests pin the registration, params, markers and artifact names
// in BOTH dialects; behavior is covered by the exec suite.

func TestSceneDetectTemplateRegistered(t *testing.T) {
	tmpl, err := BuiltinResolver()("scene_detect")
	if err != nil {
		t.Fatalf("scene_detect must be a registered builtin: %v", err)
	}
	if !tmpl.Builtin {
		t.Fatal("scene_detect must be builtin")
	}
	params := map[string]string{}
	for _, p := range tmpl.Params {
		params[p.Key] = p.Default
	}
	if params["threshold"] != "10" {
		t.Errorf("threshold default must be 10, got %q", params["threshold"])
	}
	if params["min_scene_len"] != "0" {
		t.Errorf("min_scene_len default must be 0, got %q", params["min_scene_len"])
	}
}

func TestSceneDetectPSContract(t *testing.T) {
	tmpl, err := BuiltinResolver()("scene_detect")
	if err != nil {
		t.Fatal(err)
	}
	ps := tmpl.PowerShell
	for _, want := range []string{
		"function Invoke-SceneDetect",
		"ENCODE_STEP scene_detect 0",
		"ENCODE_STEP scene_detect 30",
		"ENCODE_STEP scene_detect 60",
		"ENCODE_STEP scene_detect 100",
		"Find-SourceFile",
		"Resolve-Tool $Job.BinDir 'ffmpeg.exe'",
		"Resolve-Tool $Job.BinDir 'ffprobe.exe'",
		"scdet=threshold=",
		"metadata=print:file=",
		"scenes.json",
		"scdet.log",
	} {
		if !strings.Contains(ps, want) {
			t.Errorf("PS template missing contract element %q", want)
		}
	}
}

func TestSceneDetectBashRegistered(t *testing.T) {
	src, ok := builtinBashSources["scene_detect"]
	if !ok || src == "" {
		t.Fatal("scene_detect must have a bash port")
	}
	for _, want := range []string{
		"scene_detect ()",
		"ENCODE_STEP scene_detect 0",
		"ENCODE_STEP scene_detect 100",
		"find_source_file",
		"scdet=threshold=",
		"metadata=print:file=",
		"scenes.json",
		"ffprobe",
		"ffmpeg",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("bash port missing contract element %q", want)
		}
	}
	tools := builtinLinuxToolMap["scene_detect"]
	if tools != "ffmpeg" {
		t.Errorf("scene_detect primary tool must be ffmpeg, got %q", tools)
	}
}

// Live-farm regression (v1.25.0 job 30): PS 5.1 under Set-StrictMode Latest
// THROWS on missing JSON properties — m2ts ffprobe output has no nb_frames
// key (and often no stream duration). The template must use null-safe
// PSObject.Properties guards, matching the bash absent-key fallback.
func TestSceneDetectPSNullSafeProbeAccess(t *testing.T) {
	tpl := sceneDetectTemplate().PowerShell
	for _, key := range []string{"nb_frames", "r_frame_rate", "duration", "streams", "format"} {
		if !strings.Contains(tpl, "PSObject.Properties['"+key+"']") {
			t.Errorf("PS template must null-guard %q (StrictMode throws on missing keys)", key)
		}
	}
	if strings.Contains(tpl, "$vs.nb_frames") && !strings.Contains(tpl, "$nbRaw") {
		t.Error("frame count must use the captured nb_frames value")
	}
}

// Live-farm regression (v1.25.0 job 32): the ffmpeg filtergraph parser treats
// \ : ' [ ] , ; and space as special — the metadata log path MUST be escaped
// at the option-value level in BOTH dialects (Windows C:\ paths with spaces).
func TestSceneDetectFiltergraphEscaping(t *testing.T) {
	ps := sceneDetectTemplate().PowerShell
	if !strings.Contains(ps, `.Replace('\', '\\')`) || !strings.Contains(ps, `.Replace(':', '\:')`) || !strings.Contains(ps, `.Replace(' ', '\ ')`) {
		t.Error("PS template must escape backslash/colon/space in the filtergraph log path")
	}
	if !strings.Contains(ps, "metadata=print:file=' + $lf") {
		t.Error("PS vf must use the escaped path variable")
	}
	bash := bashSourceSceneDetect
	if !strings.Contains(bash, `s/\\/\\\\/g`) || !strings.Contains(bash, `s/:/\\:/g`) || !strings.Contains(bash, `s/ /\\ /g`) {
		t.Error("bash template must escape backslash/colon/space in the filtergraph log path")
	}
	if !strings.Contains(bash, "metadata=print:file=$lf") {
		t.Error("bash vf must use the escaped path variable")
	}
}

// The factory snapshots must stay byte-exact copies of the shipped versions
// (the upgrade chain matches byte-for-byte; a drifted snapshot silently
// stops upgrading untouched installs).
func TestSceneDetectFactorySnapshots(t *testing.T) {
	if !strings.HasPrefix(SceneDetectFactoryV1, "function Invoke-SceneDetect {") ||
		!strings.HasPrefix(SceneDetectFactoryV1a, "function Invoke-SceneDetect {") {
		t.Fatal("factory snapshots must be complete Invoke-SceneDetect scripts")
	}
	if strings.Contains(SceneDetectFactoryV1, "PSObject.Properties['nb_frames']") {
		t.Error("V1 snapshot must be the PRE-StrictMode-fix script")
	}
	if !strings.Contains(SceneDetectFactoryV1a, "PSObject.Properties['nb_frames']") {
		t.Error("V1a snapshot must be the StrictMode-fixed script")
	}
	if strings.Contains(SceneDetectFactoryV1a, ".Replace(") {
		t.Error("V1a snapshot must be PRE-filtergraph-escaping")
	}
	cur := sceneDetectTemplate().PowerShell
	if cur == SceneDetectFactoryV1 || cur == SceneDetectFactoryV1a {
		t.Error("current factory must differ from both snapshots")
	}
}
