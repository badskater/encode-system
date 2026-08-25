package flow

import (
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// Regression (adversarial review): a hostile step type must not be able to
// break out of the ENCODE_STEP marker string and inject PowerShell.
func TestRenderRejectsUnsafeStepType(t *testing.T) {
	f := &model.Flow{
		Name: "hostile",
		Steps: []model.Step{
			{Type: model.StepType(`x"; Remove-Item C:\ -Recurse; "`), Params: map[string]string{}},
		},
	}
	// The template resolver would need a matching key; the unsafe-type check
	// must fire regardless — sanitize strips everything but [a-z0-9_], so this
	// type has no resolvable template and must be refused, not rendered.
	_, err := Render(f, testJob(), Vars{}, nil)
	if err == nil {
		t.Fatal("hostile step type must be refused")
	}
}

// Regression: param keys with hashtable-breaking characters are sanitized,
// never rendered verbatim into the params literal.
func TestParamsLiteralSanitizesKeys(t *testing.T) {
	tpl := &model.StepTemplate{
		Key:   "demo",
		Label: "Demo",
		Params: []model.ParamDef{
			{Key: "evil} ; Bad-Cmd ; $x = @{y", Label: "Evil"},
		},
		PowerShell: "function Invoke-Demo {\n    param($Job, $Params)\n}",
	}
	f := &model.Flow{
		Name:  "paramtest",
		Steps: []model.Step{{Type: "demo", Params: map[string]string{}}},
	}
	resolve := func(key string) (*model.StepTemplate, error) {
		if key == "demo" {
			return tpl, nil
		}
		return nil, errNoTemplate
	}
	script, err := Render(f, testJob(), Vars{}, resolve)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(script, "Bad-Cmd") {
		t.Fatal("param key injection survived into the script")
	}
}

var errNoTemplate = errTemplateNotFound{}

type errTemplateNotFound struct{}

func (errTemplateNotFound) Error() string { return "template not found" }

func testJob() *model.Job {
	return &model.Job{ID: 1, Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy"}
}

// Regression: two templates defining the same function name must be refused
// instead of silently overriding each other.
func TestRenderRejectsDuplicateFunctionNames(t *testing.T) {
	tplA := &model.StepTemplate{Key: "a_step", Label: "A", PowerShell: "function Invoke-Same {\n    param($Job,$Params)\n}"}
	tplB := &model.StepTemplate{Key: "b_step", Label: "B", PowerShell: "function Invoke-Same {\n    param($Job,$Params)\n}"}
	resolve := func(key string) (*model.StepTemplate, error) {
		switch key {
		case "a_step":
			return tplA, nil
		case "b_step":
			return tplB, nil
		}
		return nil, errNoTemplate
	}
	f := &model.Flow{
		Name:  "dup",
		Steps: []model.Step{{Type: "a_step"}, {Type: "b_step"}},
	}
	if _, err := Render(f, testJob(), Vars{}, resolve); err == nil {
		t.Fatal("duplicate function names must be refused")
	}
}

// Regression: a template without a function definition must fail validation
// (and never panic the renderer).
func TestValidateRejectsFunctionlessTemplate(t *testing.T) {
	tpl := &model.StepTemplate{Key: "nofn", Label: "NoFn", PowerShell: "Write-Output 'hi'"}
	resolve := func(key string) (*model.StepTemplate, error) { return tpl, nil }
	f := &model.Flow{Name: "x", Steps: []model.Step{{Type: "nofn"}}}
	if err := ValidateForRender(f, resolve); err == nil {
		t.Fatal("function-less template must fail validation")
	}
}

// Regression: newline injection through template labels/keys is neutralized
// in comment lines.
func TestRenderStripsNewlinesFromComments(t *testing.T) {
	tpl := &model.StepTemplate{
		Key:        "n_step",
		Label:      "Nice\nInvoke-EvilCmd",
		PowerShell: "function Invoke-N {\n    param($Job,$Params)\n}",
	}
	resolve := func(key string) (*model.StepTemplate, error) { return tpl, nil }
	f := &model.Flow{Name: "x\nInvoke-Evil2", Steps: []model.Step{{Type: "n_step"}}}
	script, err := Render(f, testJob(), Vars{}, resolve)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	lines := strings.Split(script, "\n")
	for _, l := range lines {
		if strings.HasPrefix(l, "#") && strings.Contains(l, "Invoke-Evil") {
			// Must only ever appear inside the comment, newline-flattened.
			continue
		}
		if strings.Contains(l, "Invoke-EvilCmd") && !strings.HasPrefix(l, "#") {
			t.Fatalf("executable injection line found outside a comment: %q", l)
		}
	}
}

// Regression: nil job returns an error instead of panicking.
func TestRenderNilJob(t *testing.T) {
	f := DefaultFlow()
	if _, err := Render(f, nil, Vars{}, nil); err == nil {
		t.Fatal("nil job must error")
	}
}

// Regression: backslash episode dirs still yield a suffix.
func TestEpisodeNumberBackslash(t *testing.T) {
	// The "Ep NN" regex fires first regardless of separator.
	if got := EpisodeNumber(`S\Ep 05`); got != "05" {
		t.Errorf("backslash Ep dir = %q, want 05", got)
	}
	// Fallback must split on backslashes too.
	if got := EpisodeNumber(`C:\shares\S\Finale`); got != "Finale" {
		t.Errorf("deep backslash dir suffix = %q", got)
	}
}
