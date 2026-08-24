package api

import (
	"net/http"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestStepTemplateEditsSurviveBoot verifies the seeding contract: UI edits to
// a built-in template persist when the server is constructed again against
// the same store (what happens on every controller restart).
func TestStepTemplateEditsSurviveBoot(t *testing.T) {
	e := newTestEnv(t)

	templates, err := e.server.Store.ListStepTemplates(ctxBg())
	if err != nil || len(templates) == 0 {
		t.Fatal("no templates seeded")
	}
	var audio *model.StepTemplate
	for _, tpl := range templates {
		if tpl.Key == "audio" {
			audio = tpl
		}
	}
	if audio == nil {
		t.Fatal("audio template not seeded")
	}

	// Simulate a UI edit: change the script.
	edited := *audio
	edited.PowerShell = "function Invoke-AudioExtract {\n    Write-Output \"user-edited\"\n}"
	edited.Label = "Audio (user customized)"
	if _, err := e.server.Store.UpsertStepTemplate(ctxBg(), &edited); err != nil {
		t.Fatal(err)
	}

	// Re-run seeding as the next boot would.
	if err := e.server.seedStepTemplates(); err != nil {
		t.Fatal(err)
	}

	after, err := e.server.Store.StepTemplateByKey(ctxBg(), "audio")
	if err != nil {
		t.Fatal(err)
	}
	if after.PowerShell != edited.PowerShell || after.Label != edited.Label {
		t.Fatalf("boot seeding overwrote the UI edit:\nlabel=%q\nps=%q", after.Label, after.PowerShell)
	}
}

// TestStepTemplateResetRestoresFactory verifies POST
// /api/step-templates/{id}/reset undoes a built-in edit.
func TestStepTemplateResetRestoresFactory(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	tpl, err := e.server.Store.StepTemplateByKey(ctxBg(), "audio")
	if err != nil {
		t.Fatal(err)
	}

	// Edit it.
	edited := *tpl
	edited.PowerShell = "function Invoke-AudioExtract {\n    Write-Output \"broken by user\"\n}"
	upserted, err := e.server.Store.UpsertStepTemplate(ctxBg(), &edited)
	if err != nil {
		t.Fatal(err)
	}

	// Reset via API.
	resp, body := doJSON(t, "POST", ts.URL+"/api/step-templates/"+itoa(upserted.ID)+"/reset", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset failed: %d %s", resp.StatusCode, body)
	}

	after, err := e.server.Store.StepTemplateByKey(ctxBg(), "audio")
	if err != nil {
		t.Fatal(err)
	}
	if after.PowerShell == edited.PowerShell {
		t.Fatal("reset did not restore the factory script")
	}
	if !after.Builtin {
		t.Fatal("reset must keep the template builtin")
	}
}

// TestStepTemplateResetRejectsCustom ensures custom templates have no factory
// source to reset to.
func TestStepTemplateResetRejectsCustom(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	created, err := e.server.Store.UpsertStepTemplate(ctxBg(), &model.StepTemplate{
		Key:        "my_custom",
		Label:      "Custom",
		Builtin:    false,
		PowerShell: "function Invoke-MyCustom {\n    Write-Output \"x\"\n}",
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, _ := doJSON(t, "POST", ts.URL+"/api/step-templates/"+itoa(created.ID)+"/reset", adminTok, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409 for custom reset, got %d", resp.StatusCode)
	}
}

// TestRenderedEncodeUsesStructuredFields checks the rendered job script
// assembles x265 args from the named params (preset visible, legacy defaults
// present) rather than requiring a raw argument string.
func TestRenderedEncodeUsesStructuredFields(t *testing.T) {
	// Rendered through the flow package's E2E machinery is covered in
	// render tests; here we assert the template exposes the structured
	// params the UI builds from.
	e := newTestEnv(t)
	tpl, err := e.server.Store.StepTemplateByKey(ctxBg(), "encode")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"preset": false, "crf": false, "no_sao": false, "x265_args": false}
	for _, p := range tpl.Params {
		if _, ok := want[p.Key]; ok {
			want[p.Key] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Fatalf("encode template missing structured param %q", k)
		}
	}
}

// TestNewPluginStepsSeeded verifies the three FileFlows-inspired steps exist.
func TestNewPluginStepsSeeded(t *testing.T) {
	e := newTestEnv(t)
	for _, key := range []string{"media_probe", "audio_branch", "crc32_rename"} {
		tpl, err := e.server.Store.StepTemplateByKey(ctxBg(), key)
		if err != nil || tpl == nil {
			t.Fatalf("plugin step %q not seeded (err=%v)", key, err)
		}
		if !tpl.Builtin {
			t.Fatalf("plugin step %q should be builtin", key)
		}
	}
}
