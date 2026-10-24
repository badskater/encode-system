package flow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestBashRenderFullFlow: a simple flow rendered via BashDialect must
// contain the bash preamble (shebang + set -euo pipefail), ENCODE_STEP
// markers the agent can parse, and no PowerShell cmdlets. This is the
// smoke test that the bash dialect produces a runnable bash script.
func TestBashRenderFullFlow(t *testing.T) {
	// A minimal pure-external-command template the bash dialect can
	// render (no PS cmdlets in the body).
	tpl := &model.StepTemplate{
		Key:   "noop",
		Label: "No-op",
		Params: []model.ParamDef{
			{Key: "msg", Label: "Message"},
		},
		PowerShell: "function Invoke-Noop {\n    param($Job,$Params)\n    Write-Output \"ENCODE_STEP noop 0\"\n    Write-Output \"hello from noop\"\n    Write-Output \"ENCODE_STEP noop 100\"\n}",
	}
	resolve := func(key string) (*model.StepTemplate, error) {
		if key == "noop" {
			return tpl, nil
		}
		return nil, errNoTemplate
	}
	f := &model.Flow{Name: "bash-e2e", Steps: []model.Step{
		{Type: "noop", Params: map[string]string{"msg": "world"}},
	}}
	j := &model.Job{ID: 9, Series: "Test Series", Episode: "01",
		EpisodeDir: "Test Series/Ep 01", ScriptType: "vpy"}

	script, err := RenderDialect(f, j, testVars(), resolve, BashDialect{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	// Shebang + strict mode.
	if !strings.HasPrefix(script, "#!/usr/bin/env bash") {
		t.Errorf("missing shebang:\n%s", script)
	}
	if !strings.Contains(script, "set -euo pipefail") {
		t.Errorf("missing set -euo pipefail:\n%s", script)
	}

	// ENCODE_STEP markers the agent parses.
	for _, want := range []string{
		"ENCODE_STEP noop start",
		"ENCODE_STEP_FAILED noop",
		"ENCODE_JOB_DONE",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q", want)
		}
	}

	// No PowerShell cmdlets leaked into the bash output.
	for _, bad := range []string{
		"function Invoke",
		"param($Job",
		"Write-Output",
		"pscustomobject",
		"Set-StrictMode",
		"$ErrorActionPreference",
		"Join-Path",
	} {
		if strings.Contains(script, bad) {
			t.Errorf("bash script leaked PowerShell token %q:\n%s", bad, script)
		}
	}

	// Job context values are quoted bash assignments.
	if !strings.Contains(script, "'Test Series'") {
		t.Errorf("script missing quoted series value:\n%s", script)
	}
}

// TestBashRenderedScriptExecutes renders a simple flow via the bash
// dialect and ACTUALLY runs it under /bin/bash, asserting the ENCODE_STEP
// markers and ENCODE_JOB_DONE appear on stdout in order. This is the
// strongest proof the dialect produces a syntactically valid, runnable
// script — not just a string that passes shape assertions. Skipped when
// bash is absent (non-Linux CI without bash).
func TestBashRenderedScriptExecutes(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed; skipping bash execution test")
	}
	tpl := &model.StepTemplate{
		Key:   "hello",
		Label: "Hello",
		Params: []model.ParamDef{
			{Key: "msg", Label: "Message"},
		},
		PowerShell: "function Invoke-Hello {\n    param($Job,$Params)\n" +
			"    Write-Output \"ENCODE_STEP hello 0\"\n" +
			"    Write-Output \"hello world\"\n" +
			"    Write-Output \"ENCODE_STEP hello 100\"\n}",
	}
	resolve := func(key string) (*model.StepTemplate, error) { return tpl, nil }
	f := &model.Flow{Name: "bash-run", Steps: []model.Step{
		{Type: "hello", Params: map[string]string{"msg": "world"}},
	}}
	j := &model.Job{ID: 1, Series: "Test", Episode: "01",
		EpisodeDir: "Test/Ep 01", ScriptType: "vpy"}

	script, err := RenderDialect(f, j, testVars(), resolve, BashDialect{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "job.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bash, scriptPath).CombinedOutput()
	t.Logf("bash output:\n%s", out)
	if err != nil {
		t.Fatalf("rendered bash script failed: %v\n---script---\n%s", err, script)
	}
	text := string(out)
	for _, want := range []string{
		"ENCODE_STEP hello start",
		"ENCODE_STEP hello 0",
		"ENCODE_STEP hello 100",
		"ENCODE_JOB_DONE",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q", want)
		}
	}
}

// TestBashRenderParamInjectionNeutralized: a hostile param value must be
// quoted inert in the rendered script.
func TestBashRenderParamInjectionNeutralized(t *testing.T) {
	tpl := &model.StepTemplate{
		Key:        "hostile",
		Label:      "Hostile",
		Params:     []model.ParamDef{{Key: "msg", Label: "Message"}},
		PowerShell: "function Invoke-Hostile {\n    param($Job,$Params)\n    Write-Output \"ENCODE_STEP hostile 0\"\n    Write-Output \"ENCODE_STEP hostile 100\"\n}",
	}
	resolve := func(key string) (*model.StepTemplate, error) { return tpl, nil }
	f := &model.Flow{Name: "pinj", Steps: []model.Step{
		{Type: "hostile", Params: map[string]string{"msg": `'; rm -rf /; '`}},
	}}
	script, err := RenderDialect(f, testJob(), testVars(), resolve, BashDialect{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// The raw injection must not appear bare; it must be quoted.
	if strings.Contains(script, `rm -rf /`) {
		// Only acceptable inside a single-quoted region.
		idx := strings.Index(script, "rm -rf /")
		if !quoteStateAt(script, idx, false) {
			t.Errorf("bare injection survived:\n%s", script)
		}
	}
}
