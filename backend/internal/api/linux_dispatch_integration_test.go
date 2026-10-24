package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestLinuxDispatchIntegrationBashRender exercises the FULL dispatch path end
// to end: a job is created in the pending queue, a heartbeat carrying
// platform=linux arrives, the controller assigns the job and renders it in the
// bash dialect, and the heartbeat reply carries a script that starts with the
// bash shebang and contains no PowerShell cmdlets.
//
// This is the mixed-farm integration criterion: the heartbeat→claim→render
// pipeline must produce a runnable bash script for a Linux node, not the
// default PowerShell script. The unit tests in render_dialect_test.go cover
// the renderJob() selection in isolation; this test covers the wiring through
// the HTTP heartbeat handler, the job-claim loop, and the reply payload.
func TestLinuxDispatchIntegrationBashRender(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// A custom step template whose body is pure Write-Output — the minimal
	// subset the bash dialect accepts and translates to echo. The default-1080
	// flow uses dgindex (Windows-only, refused by the bash dialect), so we
	// isolate the dispatch wiring from the flow-package's step-template
	// mapping by using a bash-renderable step.
	tpl, err := e.server.Store.UpsertStepTemplate(ctx, &model.StepTemplate{
		Key:    "integ_bash_noop",
		Label:  "Integration Bash Noop",
		Params: []model.ParamDef{{Key: "msg", Label: "Message", Default: "hello"}},
		PowerShell: "function Invoke-IntegBashNoop {\n" +
			"param($Job, $Params)\n" +
			"Write-Output \"ENCODE_STEP integ 1.0\"\n" +
			"Write-Output \"ENCODE_JOB_DONE\"\n" +
			"}\n",
	})
	if err != nil {
		t.Fatalf("create step template: %v", err)
	}
	fl, err := e.server.Store.CreateFlow(ctx, &model.Flow{
		Name: "integ-bash-flow", Steps: []model.Step{{Type: model.StepType(tpl.Key)}},
	})
	if err != nil {
		t.Fatalf("create flow: %v", err)
	}

	// Seed a pending job (no node assignment yet).
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	// Heartbeat with platform=linux — this persists the platform onto the node
	// row AND triggers the job-claim loop, which renders the job in the node's
	// dialect (bash for linux) and returns it in the reply.
	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token,
		platformHeartbeat("enc-01", 1, "linux"))
	if resp.StatusCode != 200 {
		t.Fatalf("heartbeat: status %d: %s", resp.StatusCode, body)
	}
	var reply model.HeartbeatReply
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("unmarshal heartbeat reply: %v (%s)", err, body)
	}
	if reply.Instruction != "job" {
		t.Fatalf("want job instruction, got %q (%s)", reply.Instruction, body)
	}
	if reply.Job == nil || reply.Job.ID != job.ID {
		t.Fatalf("wrong job payload: %+v", reply.Job)
	}

	// The script must be a bash script: shebang present, no PS cmdlets.
	if !strings.HasPrefix(reply.Job.Script, "#!/usr/bin/env bash") {
		t.Fatalf("linux dispatch script must start with bash shebang:\n%s", reply.Job.Script)
	}
	if strings.Contains(reply.Job.Script, "Write-Output") {
		t.Fatalf("linux dispatch script must not contain Write-Output (PS cmdlet leaked):\n%s", reply.Job.Script)
	}
	// The ENCODE_STEP marker must survive the dialect switch (agent parser
	// contract — dialect-agnostic).
	if !strings.Contains(reply.Job.Script, "ENCODE_STEP integ 1.0") {
		t.Fatalf("linux dispatch script must carry the ENCODE_STEP marker:\n%s", reply.Job.Script)
	}
	// Bash strict mode is a dispatch invariant for Linux scripts.
	if !strings.Contains(reply.Job.Script, "set -euo pipefail") {
		t.Fatalf("linux dispatch script must carry bash strict mode:\n%s", reply.Job.Script)
	}

	// The job must now be assigned to this node.
	got, _ := e.server.Store.GetJob(ctx, job.ID)
	if got.Status != model.JobAssigned || got.NodeID != e.node.ID {
		t.Fatalf("job not assigned to linux node: %+v", got)
	}
}

// TestWindowsDispatchIntegrationPowerShellRender is the Windows counterpart:
// the same heartbeat→claim→render pipeline produces a PowerShell script (no
// shebang, $LibPath param present) for platform=windows. An empty platform
// (legacy node) defaults to PowerShell too — covered by the unit test
// TestRenderJobEmptyPlatformDefaultsToPowerShell; this test covers the dispatch
// wiring for an explicit windows platform.
func TestWindowsDispatchIntegrationPowerShellRender(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Use the seeded default-1080 flow (Windows-renderable, the historical
	// fleet default).
	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token,
		platformHeartbeat("enc-01", 1, "windows"))
	if resp.StatusCode != 200 {
		t.Fatalf("heartbeat: status %d: %s", resp.StatusCode, body)
	}
	var reply model.HeartbeatReply
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("unmarshal heartbeat reply: %v (%s)", err, body)
	}
	if reply.Instruction != "job" {
		t.Fatalf("want job instruction, got %q (%s)", reply.Instruction, body)
	}
	if reply.Job == nil || reply.Job.ID != job.ID {
		t.Fatalf("wrong job payload: %+v", reply.Job)
	}

	// The script must be PowerShell: no shebang, $LibPath present.
	if strings.HasPrefix(reply.Job.Script, "#!") {
		t.Fatalf("windows dispatch script must not start with a shebang:\n%s", reply.Job.Script)
	}
	if !strings.Contains(reply.Job.Script, "$LibPath") {
		t.Fatalf("windows dispatch script must carry the PS $LibPath param:\n%s", reply.Job.Script)
	}

	// The job must be assigned to this node.
	got, _ := e.server.Store.GetJob(ctx, job.ID)
	if got.Status != model.JobAssigned || got.NodeID != e.node.ID {
		t.Fatalf("job not assigned to windows node: %+v", got)
	}
}
