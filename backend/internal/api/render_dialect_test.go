package api

import (
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/flow"
	"github.com/badskater/encode-system/backend/internal/model"
)

// TestRenderJobLinuxPlatformGetsBash verifies the controller renders a bash
// script (shebang present, no PowerShell cmdlets) when the target node's
// platform is "linux". This is the mixed-farm correctness criterion: the
// same job rendered for a Linux node produces a runnable bash script.
//
// The default-1080 flow uses dgindex (Windows-only, refused by the bash
// dialect), so the test creates a custom bash-renderable step template
// (pure Write-Output body — the 1.2 minimal subset) and a flow referencing
// it. This isolates the controller's per-platform dialect SELECTION, which is
// the unit under test, from the flow-package's step-template mapping work.
func TestRenderJobLinuxPlatformGetsBash(t *testing.T) {
	e := newTestEnv(t)
	ctx := ctxBg()

	// A custom step template whose body is pure Write-Output — the minimal
	// subset the bash dialect accepts and translates to echo.
	tpl, err := e.server.Store.UpsertStepTemplate(ctx, &model.StepTemplate{
		Key:    "bash_noop",
		Label:  "Bash Noop",
		Params: []model.ParamDef{{Key: "msg", Label: "Message", Default: "hello"}},
		PowerShell: "function Invoke-BashNoop {\n" +
			"param($Job, $Params)\n" +
			"Write-Output \"ENCODE_STEP noop 1.0\"\n" +
			"Write-Output \"ENCODE_JOB_DONE\"\n" +
			"}\n",
	})
	if err != nil {
		t.Fatalf("create step template: %v", err)
	}
	fl, err := e.server.Store.CreateFlow(ctx, &model.Flow{
		Name: "bash-test-flow", Steps: []model.Step{{Type: model.StepType(tpl.Key)}},
	})
	if err != nil {
		t.Fatalf("create flow: %v", err)
	}
	job := &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID}

	payload, err := e.server.renderJob(ctx, job, "linux")
	if err != nil {
		t.Fatalf("render linux: %v", err)
	}
	if !strings.HasPrefix(payload.Script, "#!/usr/bin/env bash") {
		t.Fatalf("linux script must start with bash shebang:\n%s", payload.Script)
	}
	if strings.Contains(payload.Script, "pscustomobject") || strings.Contains(payload.Script, "Write-Output") {
		t.Fatalf("linux script must not contain PS cmdlets/objects:\n%s", payload.Script)
	}
	if strings.Contains(payload.Script, "$LibPath") {
		t.Fatalf("linux script must not reference the PS-only $LibPath param:\n%s", payload.Script)
	}
	if !strings.Contains(payload.Script, "set -euo pipefail") {
		t.Fatalf("linux script must carry bash strict mode:\n%s", payload.Script)
	}
	// The ENCODE_STEP marker must survive the dialect switch — the agent
	// parser is dialect-agnostic by contract.
	if !strings.Contains(payload.Script, "ENCODE_STEP noop 1.0") {
		t.Fatalf("linux script must carry the ENCODE_STEP marker:\n%s", payload.Script)
	}
}

// TestRenderJobWindowsPlatformGetsPowerShell verifies the controller renders
// a PowerShell script (no shebang, PS preamble present) when the target
// node's platform is "windows".
func TestRenderJobWindowsPlatformGetsPowerShell(t *testing.T) {
	e := newTestEnv(t)
	ctx := ctxBg()

	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	job := &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID}

	payload, err := e.server.renderJob(ctx, job, "windows")
	if err != nil {
		t.Fatalf("render windows: %v", err)
	}
	if strings.HasPrefix(payload.Script, "#!") {
		t.Fatalf("windows script must not start with a shebang:\n%s", payload.Script)
	}
	if !strings.Contains(payload.Script, "$LibPath") {
		t.Fatalf("windows script must carry the PS $LibPath param:\n%s", payload.Script)
	}
	if !strings.Contains(payload.Script, "pscustomobject") {
		t.Fatalf("windows script must build a PS $Job object:\n%s", payload.Script)
	}
}

// TestRenderJobEmptyPlatformDefaultsToPowerShell verifies a legacy/old node
// with an empty platform field gets the PowerShell script (historical fleet
// default), so a rolling upgrade never breaks old nodes.
func TestRenderJobEmptyPlatformDefaultsToPowerShell(t *testing.T) {
	e := newTestEnv(t)
	ctx := ctxBg()

	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	job := &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID}

	payload, err := e.server.renderJob(ctx, job, "")
	if err != nil {
		t.Fatalf("render empty platform: %v", err)
	}
	if !strings.Contains(payload.Script, "$LibPath") {
		t.Fatalf("empty platform must default to PowerShell ($LibPath present):\n%s", payload.Script)
	}
}

// TestRenderJobMixedPlatformIdenticalJob verifies the mixed-farm acceptance
// criterion: the SAME job rendered for two different platforms produces two
// different scripts — bash for linux, PowerShell for windows — with the same
// step markers (ENCODE_STEP text is dialect-agnostic).
func TestRenderJobMixedPlatformIdenticalJob(t *testing.T) {
	e := newTestEnv(t)
	ctx := ctxBg()

	// Use a bash-renderable custom step template (pure Write-Output body)
	// so the Linux render does not fail on Windows-only built-ins.
	tpl, err := e.server.Store.UpsertStepTemplate(ctx, &model.StepTemplate{
		Key:    "mix_noop",
		Label:  "Mix Noop",
		Params: []model.ParamDef{{Key: "msg", Label: "Message", Default: "hello"}},
		PowerShell: "function Invoke-MixNoop {\n" +
			"param($Job, $Params)\n" +
			"Write-Output \"ENCODE_STEP mix 1.0\"\n" +
			"Write-Output \"ENCODE_JOB_DONE\"\n" +
			"}\n",
	})
	if err != nil {
		t.Fatalf("create step template: %v", err)
	}
	fl, err := e.server.Store.CreateFlow(ctx, &model.Flow{
		Name: "mix-test-flow", Steps: []model.Step{{Type: model.StepType(tpl.Key)}},
	})
	if err != nil {
		t.Fatalf("create flow: %v", err)
	}
	job := &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID}

	linuxPayload, err := e.server.renderJob(ctx, job, "linux")
	if err != nil {
		t.Fatalf("render linux: %v", err)
	}
	windowsPayload, err := e.server.renderJob(ctx, job, "windows")
	if err != nil {
		t.Fatalf("render windows: %v", err)
	}
	// Both scripts must emit the ENCODE_STEP marker (the protocol text is
	// identical across dialects — this is the agent parser's contract).
	if !strings.Contains(linuxPayload.Script, "ENCODE_STEP mix 1.0") {
		t.Fatalf("linux script missing ENCODE_STEP marker:\n%s", linuxPayload.Script)
	}
	if !strings.Contains(windowsPayload.Script, "ENCODE_STEP mix 1.0") {
		t.Fatalf("windows script missing ENCODE_STEP marker:\n%s", windowsPayload.Script)
	}
	// The two scripts must be DIFFERENT (different dialects).
	if linuxPayload.Script == windowsPayload.Script {
		t.Fatal("identical job rendered for linux and windows must NOT produce identical scripts")
	}
}

// TestDialectForPlatform is a unit test for the flow package's platform →
// dialect selector, exercising all three branches (linux, windows, empty).
func TestDialectForPlatform(t *testing.T) {
	if _, ok := flow.DialectForPlatform("linux").(flow.BashDialect); !ok {
		t.Fatal("linux must select BashDialect")
	}
	if _, ok := flow.DialectForPlatform("windows").(flow.PowerShellDialect); !ok {
		t.Fatal("windows must select PowerShellDialect")
	}
	if _, ok := flow.DialectForPlatform("").(flow.PowerShellDialect); !ok {
		t.Fatal("empty platform must default to PowerShellDialect")
	}
	if _, ok := flow.DialectForPlatform("darwin").(flow.PowerShellDialect); !ok {
		t.Fatal("unknown platform must default to PowerShellDialect")
	}
}
