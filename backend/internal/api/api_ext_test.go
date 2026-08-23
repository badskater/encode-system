package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// ---------- Series API ----------

func TestSeriesAutoRegisteredAndConfigurable(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Simulate the scanner registering a series (the loop does this in prod).
	sr, err := e.server.Store.UpsertSeriesByName(ctx, "Test Series")
	if err != nil {
		t.Fatal(err)
	}

	// Listed with job counts.
	resp, body := doJSON(t, "GET", ts.URL+"/api/series", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("list series: %d", resp.StatusCode)
	}
	var series []struct {
		model.Series
		Jobs int `json:"jobs"`
	}
	json.Unmarshal(body, &series)
	if len(series) != 1 || series[0].Name != "Test Series" {
		t.Fatalf("series list wrong: %s", body)
	}

	// Flow selection via PATCH.
	flows, _ := e.server.Store.ListFlows(ctx)
	resp2, body2 := doJSON(t, "PATCH", ts.URL+"/api/series/"+itoa(sr.ID), adminTok,
		map[string]any{"flow_id": flows[0].ID, "enabled": false})
	if resp2.StatusCode != 200 {
		t.Fatalf("patch series: %d %s", resp2.StatusCode, body2)
	}
	var patched model.Series
	json.Unmarshal(body2, &patched)
	if patched.FlowID != flows[0].ID || patched.Enabled {
		t.Fatalf("patch not applied: %+v", patched)
	}

	// Invalid flow id refused.
	resp3, _ := doJSON(t, "PATCH", ts.URL+"/api/series/"+itoa(sr.ID), adminTok,
		map[string]any{"flow_id": 9999})
	if resp3.StatusCode != 400 {
		t.Fatalf("want 400 for unknown flow, got %d", resp3.StatusCode)
	}
}

// ---------- Default flow ----------

func TestDefaultFlowEndpoints(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	flows, _ := e.server.Store.ListFlows(ctx)
	if len(flows) == 0 {
		t.Fatal("seeded flow missing")
	}
	// Seeded default flow carries the flag after boot.
	def, err := e.server.Store.DefaultFlow(ctx)
	if err != nil {
		t.Fatalf("no default flow after seed: %v", err)
	}

	// Create a second flow and promote it.
	second, err := e.server.Store.CreateFlow(ctx, &model.Flow{
		Name:  "alt",
		Steps: []model.Step{{Type: model.StepDGIndex}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, body := doJSON(t, "POST", ts.URL+"/api/flows/"+itoa(second.ID)+"/default", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("set default: %d %s", resp.StatusCode, body)
	}
	def2, _ := e.server.Store.DefaultFlow(ctx)
	if def2.ID != second.ID {
		t.Fatalf("default not switched: %+v", def2)
	}
	if def2.ID == def.ID {
		t.Fatal("default must have changed")
	}
}

// ---------- Step templates ----------

func TestStepTemplateEndpoints(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// Built-ins seeded at boot.
	resp, body := doJSON(t, "GET", ts.URL+"/api/step-templates", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("list templates: %d", resp.StatusCode)
	}
	var templates []model.StepTemplate
	json.Unmarshal(body, &templates)
	if len(templates) < 7 {
		t.Fatalf("want >=7 built-in templates, got %d", len(templates))
	}

	// Custom template with valid PowerShell accepted.
	custom := model.StepTemplate{
		Key:   "custom_notify",
		Label: "Notify",
		Params: []model.ParamDef{
			{Key: "message", Label: "Message", Placeholder: "done"},
		},
		PowerShell: "function Invoke-CustomNotify {\n" +
			"    param([Parameter(Mandatory=$true)] [pscustomobject] $Job, [pscustomobject] $Params)\n" +
			"    Write-Output \"ENCODE_STEP custom_notify 100\"\n" +
			"}",
	}
	resp2, body2 := doJSON(t, "POST", ts.URL+"/api/step-templates", adminTok, custom)
	if resp2.StatusCode != 201 {
		t.Fatalf("create custom template: %d %s", resp2.StatusCode, body2)
	}

	// Bad key refused.
	bad := custom
	bad.Key = "Bad Key!"
	resp3, _ := doJSON(t, "POST", ts.URL+"/api/step-templates", adminTok, bad)
	if resp3.StatusCode != 400 {
		t.Fatalf("want 400 for bad key, got %d", resp3.StatusCode)
	}

	// PowerShell without a function refused.
	nofn := custom
	nofn.Key = "nofn_step"
	nofn.PowerShell = "Write-Output 'not a function'"
	resp4, _ := doJSON(t, "POST", ts.URL+"/api/step-templates", adminTok, nofn)
	if resp4.StatusCode != 400 {
		t.Fatalf("want 400 for function-less script, got %d", resp4.StatusCode)
	}

	// Built-in delete refused.
	var builtinID int64
	for _, tpl := range templates {
		if tpl.Builtin {
			builtinID = tpl.ID
			break
		}
	}
	resp5, _ := doJSON(t, "DELETE", ts.URL+"/api/step-templates/"+itoa(builtinID), adminTok, nil)
	if resp5.StatusCode != 409 {
		t.Fatalf("built-in delete must be 409, got %d", resp5.StatusCode)
	}
}

// ---------- Flow import/export ----------

func TestFlowExportImportRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Create a custom template + a flow using it.
	custom := model.StepTemplate{
		Key:        "custom_check",
		Label:      "Custom check",
		PowerShell: "function Invoke-CustomCheck {\n    param($Job, $Params)\n    Write-Output \"ENCODE_STEP custom_check 100\"\n}",
		Params:     []model.ParamDef{},
	}
	tpl, err := e.server.Store.UpsertStepTemplate(ctx, &custom)
	if err != nil {
		t.Fatal(err)
	}
	fl, err := e.server.Store.CreateFlow(ctx, &model.Flow{
		Name: "with-custom",
		Steps: []model.Step{
			{Type: model.StepDGIndex},
			{Type: model.StepType(tpl.Key)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Export embeds the custom template but not built-ins.
	resp, body := doJSON(t, "GET", ts.URL+"/api/flows/"+itoa(fl.ID)+"/export", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("export: %d", resp.StatusCode)
	}
	var exp struct {
		Flow      model.Flow            `json:"flow"`
		Templates []*model.StepTemplate `json:"templates"`
	}
	json.Unmarshal(body, &exp)
	if exp.Flow.Name != "with-custom" || len(exp.Templates) != 1 {
		t.Fatalf("export shape wrong: %s", body)
	}
	if exp.Templates[0].Key != "custom_check" {
		t.Fatalf("export must embed the custom template only: %+v", exp.Templates)
	}

	// Delete the template and the flow, then import — templates come back.
	e.server.Store.DeleteFlow(ctx, fl.ID)
	if err := e.server.Store.DeleteStepTemplate(ctx, tpl.ID); err != nil {
		t.Fatal(err)
	}
	resp2, body2 := doJSON(t, "POST", ts.URL+"/api/flows/import", adminTok, exp)
	if resp2.StatusCode != 201 {
		t.Fatalf("import: %d %s", resp2.StatusCode, body2)
	}
	var imported model.Flow
	json.Unmarshal(body2, &imported)
	if imported.Name != "with-custom" || len(imported.Steps) != 2 {
		t.Fatalf("imported flow wrong: %+v", imported)
	}
	if _, err := e.server.Store.StepTemplateByKey(ctx, "custom_check"); err != nil {
		t.Fatalf("imported template missing: %v", err)
	}

	// Import with an unknown template reference is refused.
	exp.Flow.Name = "broken"
	exp.Templates = nil
	exp.Flow.Steps = []model.Step{{Type: model.StepType("never_existed_step")}}
	resp3, _ := doJSON(t, "POST", ts.URL+"/api/flows/import", adminTok, exp)
	if resp3.StatusCode != 400 {
		t.Fatalf("import with missing template must be 400, got %d", resp3.StatusCode)
	}
}

// ---------- Pairing ----------

func TestPairingCodeFlow(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// Issue a code.
	resp, body := doJSON(t, "POST", ts.URL+"/api/pairing", adminTok,
		map[string]any{"name_hint": "enc-77", "ttl_hours": 1})
	if resp.StatusCode != 201 {
		t.Fatalf("issue code: %d %s", resp.StatusCode, body)
	}
	var issued struct {
		Code string `json:"code"`
	}
	json.Unmarshal(body, &issued)
	if len(issued.Code) < 32 {
		t.Fatalf("code too short: %q", issued.Code)
	}

	// Active codes listed (hash only — plaintext must NOT appear).
	resp2, body2 := doJSON(t, "GET", ts.URL+"/api/pairing", adminTok, nil)
	if resp2.StatusCode != 200 {
		t.Fatal(resp2.StatusCode)
	}
	if strings.Contains(string(body2), issued.Code) {
		t.Fatal("plaintext code leaked in list endpoint")
	}

	// Pair a node with the code — no bearer needed on this endpoint.
	pairReq := map[string]string{"code": issued.Code, "name": "enc-77"}
	resp3, body3 := doJSON(t, "POST", ts.URL+"/api/agent/pair", "", pairReq)
	if resp3.StatusCode != 201 {
		t.Fatalf("pair: %d %s", resp3.StatusCode, body3)
	}
	var paired struct {
		Node  model.Node `json:"node"`
		Token string     `json:"token"`
	}
	json.Unmarshal(body3, &paired)
	if paired.Node.Name != "enc-77" || paired.Token == "" {
		t.Fatalf("pair result wrong: %s", body3)
	}

	// The paired node can heartbeat with its new bearer.
	resp4, body4 := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", paired.Token,
		heartbeat("enc-77", 0, 0))
	if resp4.StatusCode != 200 {
		t.Fatalf("paired node heartbeat: %d %s", resp4.StatusCode, body4)
	}
	var reply model.HeartbeatReply
	json.Unmarshal(body4, &reply)
	if reply.Instruction != "none" {
		t.Fatalf("paired node should idle, got %q", reply.Instruction)
	}

	// The code is consumed: reusing it fails and leaves no residue.
	resp5, _ := doJSON(t, "POST", ts.URL+"/api/agent/pair", "",
		map[string]string{"code": issued.Code, "name": "enc-78"})
	if resp5.StatusCode != 401 {
		t.Fatalf("reused code must be 401, got %d", resp5.StatusCode)
	}
	if _, err := e.server.Store.NodeByName(ctxBg(), "enc-78"); err == nil {
		t.Fatal("failed pairing must not leave a node behind")
	}

	// Duplicate node name at pair time refused.
	resp6, body6 := doJSON(t, "POST", ts.URL+"/api/pairing", adminTok,
		map[string]any{"name_hint": "dup"})
	var issued2 struct {
		Code string `json:"code"`
	}
	json.Unmarshal(body6, &issued2)
	resp7, _ := doJSON(t, "POST", ts.URL+"/api/agent/pair", "",
		map[string]string{"code": issued2.Code, "name": "enc-77"})
	if resp7.StatusCode != 409 {
		t.Fatalf("duplicate name must be 409, got %d", resp7.StatusCode)
	}
	_ = resp6
}

// ---------- Custom step rendering through the API ----------

func TestCustomStepRendersIntoJobScript(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	custom := model.StepTemplate{
		Key:   "custom_tag",
		Label: "Tag",
		Params: []model.ParamDef{
			{Key: "marker", Label: "Marker", Placeholder: "encode-system"},
		},
		PowerShell: "function Invoke-CustomTag {\n" +
			"    param([Parameter(Mandatory=$true)] [pscustomobject] $Job, [pscustomobject] $Params)\n" +
			"    Write-Output \"ENCODE_STEP custom_tag 50\"\n" +
			"    Write-Output \"marker=$($Params.marker)\"\n" +
			"    Write-Output \"ENCODE_STEP custom_tag 100\"\n" +
			"}",
	}
	tpl, err := e.server.Store.UpsertStepTemplate(ctx, &custom)
	if err != nil {
		t.Fatal(err)
	}
	fl, err := e.server.Store.CreateFlow(ctx, &model.Flow{
		Name: "custom-first",
		Steps: []model.Step{
			{Type: model.StepType(tpl.Key), Params: map[string]string{"marker": "hello world"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "S", Episode: "01", EpisodeDir: "S/Ep 01",
		ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Heartbeat assigns the job and returns the rendered script.
	_, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 1, 0))
	var reply model.HeartbeatReply
	json.Unmarshal(body, &reply)
	if reply.Instruction != "job" || reply.Job == nil {
		t.Fatalf("expected job assignment, got: %s", body)
	}
	if reply.Job.ID != job.ID {
		t.Fatalf("wrong job: %+v", reply.Job)
	}
	script := reply.Job.Script
	// The custom function must be linked into the final script...
	if !strings.Contains(script, "function Invoke-CustomTag") {
		t.Fatal("custom step PowerShell not linked into rendered script")
	}
	// ...with its params from the flow...
	if !strings.Contains(script, "marker = 'hello world'") {
		t.Fatal("custom step params not rendered")
	}
	// ...and called in the pipeline.
	if !strings.Contains(script, "Invoke-CustomTag -Job $Job -Params $stepParams") {
		t.Fatal("custom step not invoked in pipeline")
	}
	// The shared job context must be present.
	if !strings.Contains(script, "$Job = [pscustomobject]@{") {
		t.Fatal("job context object missing")
	}
}
