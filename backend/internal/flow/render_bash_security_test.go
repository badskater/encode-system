package flow

import (
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// These tests mirror render_security_test.go's threat model for the bash
// dialect: hostile series/episode/path values must render inert (quoted),
// never executable, and the bash script must never let a hostile value
// break out of a quote or comment.

// hostileJob returns a job whose every interpolated string is hostile.
func hostileJob() *model.Job {
	return &model.Job{
		ID:         1,
		Series:     `'; rm -rf /'`,
		Episode:    "01\nInvoke-EvilCmd",
		EpisodeDir: `S/Ep 01'; echo pwned; '`,
		ScriptType: "vpy",
	}
}

func hostileVars() Vars {
	return Vars{
		BinDir:         "/bin'; rm -rf /; '",
		ScriptsDir:     "/scripts'; echo pwned; '",
		ReleaseDir:     "/release`whoami`",
		Group:          "Grp$(curl evil.example)",
		Tag:            "1080p",
		DiscordWebhook: "https://evil.example/webhook",
	}
}

// benignBashTemplate is a minimal pure-external-command template the bash
// dialect can render (only Write-Output, which it translates to echo).
// Hostile VALUES ride in via the job context and params; the template
// body itself is benign so the dialect does not refuse it.
func benignBashTemplate(key string) *model.StepTemplate {
	return &model.StepTemplate{
		Key:   key,
		Label: "Benign " + key,
		Params: []model.ParamDef{
			{Key: "msg", Label: "Message"},
		},
		PowerShell: "function Invoke-Benign {\n    param($Job,$Params)\n" +
			"    Write-Output \"ENCODE_STEP " + key + " 0\"\n" +
			"    Write-Output \"ENCODE_STEP " + key + " 100\"\n}",
	}
}

// mustBashRender renders the given flow with the bash dialect and a
// resolver serving benign bash-renderable templates, failing t on error.
func mustBashRender(t *testing.T, f *model.Flow) string {
	t.Helper()
	// Build a resolver that serves benign templates for any step type
	// present in the flow.
	byKey := map[string]*model.StepTemplate{}
	for _, st := range f.Steps {
		k := st.TemplateKey()
		if _, ok := byKey[k]; !ok {
			byKey[k] = benignBashTemplate(k)
		}
	}
	resolve := func(key string) (*model.StepTemplate, error) {
		if t, ok := byKey[key]; ok {
			return t, nil
		}
		return nil, errNoTemplate
	}
	script, err := RenderDialect(f, hostileJob(), hostileVars(), resolve, BashDialect{})
	if err != nil {
		t.Fatalf("bash render: %v", err)
	}
	return script
}

// TestBashRenderQuoteEscapesInjection: every interpolated hostile value
// must appear inside a single-quoted bash string, never bare. A bare
// metacharacter sequence (`;`, `$(...)`, backticks) outside of quotes
// would be executable — assert none survive unquoted.
//
// Because bash single-quoted strings can span newlines, this check tracks
// quote state ACROSS lines (a value with an embedded newline opens a
// quote on one line and closes it on a later one — both lines are safe).
func TestBashRenderQuoteEscapesInjection(t *testing.T) {
	f := &model.Flow{Name: "inj", Steps: []model.Step{
		{Type: "noop", Params: map[string]string{"msg": `'; rm -rf /; '`}},
	}}
	script := mustBashRender(t, f)

	dangerous := []string{
		"rm -rf /",
		"echo pwned",
		"whoami",
		"curl evil.example",
		"Invoke-EvilCmd",
	}
	lines := strings.Split(script, "\n")
	quoteOpen := false // carries across lines for multi-line single-quoted strings
	for i, line := range lines {
		// Process the line, tracking whether we end inside an open quote.
		lineQuoteOpen, checked := scanLineForBareTokens(line, dangerous, quoteOpen, i)
		if !checked {
			t.Errorf("line %d: a dangerous token was bare/unquoted: %q", i, line)
		}
		quoteOpen = lineQuoteOpen
	}
}

// scanLineForBareTokens walks one line the way bash parses single quotes
// (carrying inQuote from the previous line for multi-line strings). For
// each dangerous token found on the line, it verifies the token falls
// inside a quoted region OR the line is a comment. Returns the end-of-line
// quote state and whether all dangerous tokens on the line were safe.
func scanLineForBareTokens(line string, dangerous []string, inQuoteIn bool, lineNo int) (inQuoteOut bool, ok bool) {
	inQuote := inQuoteIn
	ok = true
	for _, tok := range dangerous {
		searchFrom := 0
		for {
			idx := strings.Index(line[searchFrom:], tok)
			if idx < 0 {
				break
			}
			absIdx := searchFrom + idx
			// Compute quote state at absIdx by walking from the start
			// of the line with the carried-in quote state.
			quoted := quoteStateAt(line, absIdx, inQuoteIn)
			comment := !inQuote && strings.HasPrefix(strings.TrimSpace(line), "#")
			if !quoted && !comment {
				ok = false
			}
			searchFrom = absIdx + len(tok)
		}
	}
	// Compute the end-of-line quote state for the next line.
	inQuoteOut = quoteStateAt(line, len(line), inQuoteIn)
	return inQuoteOut, ok
}

// quoteStateAt walks the line the way bash parses single quotes (with
// the '\” escape) starting from the carried-in inQuote state, returning
// the quote state at position idx.
func quoteStateAt(line string, idx int, inQuoteIn bool) bool {
	inQuote := inQuoteIn
	i := 0
	for i < idx {
		c := line[i]
		if c == '\'' {
			if !inQuote {
				inQuote = true
				i++
				continue
			}
			if i+2 < len(line) && line[i+1] == '\\' && line[i+2] == '\'' {
				inQuote = false
				i += 3
				continue
			}
			inQuote = false
			i++
			continue
		}
		i++
	}
	return inQuote
}

// TestBashRenderStripsNewlinesFromComments mirrors the PS
// TestRenderStripsNewlinesFromComments: a newline in a label must not
// break out of a comment line.
func TestBashRenderStripsNewlinesFromComments(t *testing.T) {
	tpl := &model.StepTemplate{
		Key:        "n_step",
		Label:      "Nice\nInvoke-EvilCmd",
		PowerShell: "function Invoke-N {\n    param($Job,$Params)\n    Write-Output \"ENCODE_STEP n_step 0\"\n}",
	}
	resolve := func(key string) (*model.StepTemplate, error) { return tpl, nil }
	f := &model.Flow{Name: "x\nInvoke-Evil2", Steps: []model.Step{{Type: "n_step"}}}
	script, err := RenderDialect(f, testJob(), hostileVars(), resolve, BashDialect{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	lines := strings.Split(script, "\n")
	for _, l := range lines {
		// "Invoke-EvilCmd" may appear inside a comment (newline-flattened)
		// but never as an executable line outside one.
		if strings.Contains(l, "Invoke-EvilCmd") && !strings.HasPrefix(strings.TrimSpace(l), "#") {
			t.Fatalf("executable injection line found outside a comment: %q", l)
		}
	}
}

// TestBashRenderRejectsUnsafeStepType mirrors the PS hostile-step-type
// test: a step type with metacharacters must sanitize to no resolvable
// key and be refused, not rendered.
func TestBashRenderRejectsUnsafeStepType(t *testing.T) {
	f := &model.Flow{
		Name: "hostile",
		Steps: []model.Step{
			{Type: model.StepType(`x"; rm -rf /; "`), Params: map[string]string{}},
		},
	}
	_, err := RenderDialect(f, testJob(), hostileVars(), nil, BashDialect{})
	if err == nil {
		t.Fatal("hostile step type must be refused")
	}
}

// TestBashRenderRejectsPowerShellCmdlet: for Task 1.2, the bash dialect
// must return a clear render error when a template's body is inherently
// PowerShell-only (cmdlets). Task 1.3 will add Linux tool mappings.
func TestBashRenderRejectsPowerShellCmdlet(t *testing.T) {
	tpl := &model.StepTemplate{
		Key:        "ps_only",
		Label:      "PS-only",
		PowerShell: "function Invoke-PsOnly {\n    param($Job,$Params)\n    $h = Get-FileHash -Path 'x'\n}",
	}
	resolve := func(key string) (*model.StepTemplate, error) { return tpl, nil }
	f := &model.Flow{Name: "psonly", Steps: []model.Step{{Type: "ps_only"}}}
	_, err := RenderDialect(f, testJob(), hostileVars(), resolve, BashDialect{})
	if err == nil {
		t.Fatal("bash dialect must refuse PowerShell-only templates for now")
	}
	if !strings.Contains(err.Error(), "bash") && !strings.Contains(err.Error(), "PowerShell") {
		t.Errorf("error must explain it is a bash/PowerShell mismatch: %v", err)
	}
}

// TestBashRenderNoPowerShellCmdletsInOutput: a renderable bash script must
// not contain any PowerShell-only cmdlet token — proves the dialect did
// not pass PS through verbatim.
func TestBashRenderNoPowerShellCmdletsInOutput(t *testing.T) {
	// A pure-external-command template (only Write-Output) renders fine.
	tpl := &model.StepTemplate{
		Key:   "bash_ok",
		Label: "Bash-ok",
		PowerShell: "function Invoke-BashOk {\n    param($Job,$Params)\n" +
			"    Write-Output \"ENCODE_STEP bash_ok 0\"\n" +
			"    Write-Output \"done\"\n" +
			"    Write-Output \"ENCODE_STEP bash_ok 100\"\n}",
	}
	resolve := func(key string) (*model.StepTemplate, error) { return tpl, nil }
	f := &model.Flow{Name: "ok", Steps: []model.Step{{Type: "bash_ok"}}}
	script, err := RenderDialect(f, testJob(), hostileVars(), resolve, BashDialect{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	psCmdlets := []string{"function Invoke", "param($Job", "Write-Output", "pscustomobject",
		"Join-Path", "Resolve-Tool", "Set-StrictMode", "$ErrorActionPreference"}
	for _, tok := range psCmdlets {
		if strings.Contains(script, tok) {
			t.Errorf("bash output leaked PowerShell token %q", tok)
		}
	}
}
