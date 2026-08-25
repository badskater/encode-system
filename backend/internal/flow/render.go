// Package flow renders flow definitions (ordered pipeline steps) into the
// PowerShell job scripts executed by Windows agents.
//
// Architecture (phase 2): every pipeline SECTION owns its PowerShell. Step
// templates (built-in or custom) carry the function source; the renderer
// links each referenced function into the final job script and calls it with
// a shared $Job context object plus the step's $Params. EncodeLib.ps1 on the
// node supplies only the helpers (Resolve-Tool, Invoke-Tool, Find-SourceFile,
// Assert-SafeName).
package flow

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/badskater/encode-system/backend/internal/model"
)

// Vars carries the node-side paths and release metadata shared by all jobs.
type Vars struct {
	BinDir     string // tools directory on the node, e.g. C:\bin
	ScriptsDir string // NFS scripts mount on the node, e.g. C:\Encodes\scripts
	ReleaseDir string // NFS release mount on the node, e.g. C:\Encodes\ReleaseFolders
	Group      string // release group tag, e.g. OldFartsSubs
	Tag        string // quality tag, e.g. 1080p
	// DiscordWebhook is the controller's configured webhook (env). It is
	// injected into the $Job context so the discord_notify step can fall
	// back to it when a flow does not set its own webhook param. Empty =
	// no global webhook; the step then relies on its param (or no-ops).
	DiscordWebhook string
}

// TemplateResolver looks up a step template by key. Controllers resolve from
// the store; tests/offline rendering use BuiltinResolver.
type TemplateResolver func(key string) (*model.StepTemplate, error)

// BuiltinResolver serves the built-in templates (no store needed).
func BuiltinResolver() TemplateResolver {
	byKey := map[string]*model.StepTemplate{}
	for _, t := range BuiltinStepTemplates() {
		byKey[t.Key] = t
	}
	return func(key string) (*model.StepTemplate, error) {
		t, ok := byKey[key]
		if !ok {
			return nil, fmt.Errorf("unknown step template %q", key)
		}
		return t, nil
	}
}

// DefaultX265Args mirrors the legacy batch script's x265 invocation so flows
// start with the proven parameter set.
const DefaultX265Args = "--frame-threads 1 --lookahead-slices 8 --input-depth 10 --output-depth 10 " +
	"--videoformat ntsc --range limited --colorprim bt709 --transfer bt709 --colormatrix bt709 " +
	"--allow-non-conformance --preset slow --crf 15 --deblock=-2:-2 --min-keyint 23 --keyint 240 " +
	"--ref 6 --bframes 12 --b-adapt 2 --b-pyramid --b-intra --fades --aq-mode 5 --aq-strength 0.80 " +
	"--aq-strength-edge 0.90 --aq-bias-strength 0.95 --aq-bias-strength-edge 1.00 --subme 7 --me star " +
	"--merange 24 --qcomp 0.82 --rc-lookahead 160 --rd 6 --rdoq-level 2 --psy-rd 2.0 --psy-rdoq 2.0 " +
	"--open-gop --cbqpoffs -2 --crqpoffs -2 --qpstep 2 --weightb --rect --amp --tu-intra-depth 2 " +
	"--tu-inter-depth 2 --tskip --ctu 32 --max-tu-size 16 --rskip 0 --no-strong-intra-smoothing " +
	"--no-sao --no-sao-non-deblock"

// DefaultFlow is the standard 1080p pipeline matching the legacy batch scripts.
func DefaultFlow() *model.Flow {
	return &model.Flow{
		Name: "default-1080",
		Steps: []model.Step{
			{Type: model.StepSourceRename, Params: map[string]string{"source_name": "src"}},
			{Type: model.StepDGIndex},
			{Type: model.StepAudio, Params: map[string]string{"track": "2", "bitrate": "320"}},
			{Type: model.StepEncode, Params: map[string]string{}},
			{Type: model.StepMux},
			{Type: model.StepReleaseCopy},
			{Type: model.StepKeyframes},
		},
	}
}



// psQuote renders a PowerShell single-quoted string literal, escaping embedded
// single quotes by doubling them. All rendered values pass through here, which
// keeps generated scripts injection-safe for arbitrary series names.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// psSafeIdent restricts identifiers (template keys, param keys, step types)
// to a safe alphabet before they are interpolated as bare PowerShell tokens.
// Anything outside [a-z0-9_] is dropped — keys are validated at the API
// boundary too, so this is defense in depth.
func psSafeIdent(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// psComment strips newline/CR so a value interpolated into a '#' comment can
// never terminate the comment and smuggle executable lines.
func psComment(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

var episodeRe = regexp.MustCompile(`Ep\s*(\d+)`)

// EpisodeNumber extracts the episode number from a folder name like "Ep 01",
// preserving the folder's zero-padding so output names match the legacy
// convention ("- 01"). Falls back to the raw dir suffix when no pattern matches.
func EpisodeNumber(dir string) string {
	if m := episodeRe.FindStringSubmatch(dir); m != nil {
		return m[1]
	}
	dir = strings.TrimRight(dir, `/\`)
	// Episode dirs may be slash- or backslash-separated; handle both.
	for i := len(dir) - 1; i >= 0; i-- {
		if dir[i] == '/' || dir[i] == '\\' {
			return dir[i+1:]
		}
	}
	return dir
}

// OutputName computes the final MKV name: "<Series> - <Episode> [<Tag>].mkv".
func OutputName(series, episode, tag string) string {
	return fmt.Sprintf("%s - %s [%s].mkv", series, episode, tag)
}

// ReleaseFolderName computes "[<Group>] <Series> - Raws [<Tag>]".
func ReleaseFolderName(group, series, tag string) string {
	return fmt.Sprintf("[%s] %s - Raws [%s]", group, series, tag)
}

// psFuncName extracts the function name a template defines.
var psFuncName = regexp.MustCompile(`(?m)^\s*function\s+([A-Za-z][A-Za-z0-9_-]*)`)

// Render produces the complete PowerShell script for one job. The script
// dot-sources EncodeLib.ps1 (-LibPath), builds the shared $Job context,
// embeds every referenced step function, and runs the steps in flow order.
func Render(f *model.Flow, j *model.Job, v Vars, resolve TemplateResolver) (string, error) {
	if f == nil || len(f.Steps) == 0 {
		return "", fmt.Errorf("flow %q has no steps", name(f))
	}
	if resolve == nil {
		resolve = BuiltinResolver()
	}
	if j == nil || j.Series == "" || j.EpisodeDir == "" {
		return "", fmt.Errorf("job missing series/episode dir")
	}

	// Resolve all templates up front so a missing custom template fails the
	// whole render instead of dying mid-job on the node.
	templates := make([]*model.StepTemplate, len(f.Steps))
	for i, st := range f.Steps {
		t, err := resolve(st.TemplateKey())
		if err != nil {
			return "", fmt.Errorf("flow %q step %d: %w", f.Name, i, err)
		}
		if psFuncName.FindStringSubmatch(t.PowerShell) == nil {
			return "", fmt.Errorf("flow %q step %d (%s): template defines no function", f.Name, i, t.Key)
		}
		templates[i] = t
	}

	episode := j.Episode
	if episode == "" {
		episode = EpisodeNumber(j.EpisodeDir)
	}
	scriptName := j.ScriptFile
	if scriptName == "" {
		scriptName = "1080." + j.ScriptType
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by encode-system controller - job %d, flow %s\n", j.ID, psComment(f.Name))
	b.WriteString("param([Parameter(Mandatory=$true)][string]$LibPath)\n")
	b.WriteString("Set-StrictMode -Version Latest\n")
	b.WriteString("$ErrorActionPreference = 'Stop'\n")
	b.WriteString(". $LibPath\n\n")

	b.WriteString("# --- Job context (shared by every step) ---\n")
	fmt.Fprintf(&b, "$BinDir     = %s\n", psQuote(v.BinDir))
	fmt.Fprintf(&b, "$ScriptsDir = %s\n", psQuote(v.ScriptsDir))
	fmt.Fprintf(&b, "$ReleaseDir = %s\n", psQuote(v.ReleaseDir))
	fmt.Fprintf(&b, "$Series     = %s\n", psQuote(j.Series))
	fmt.Fprintf(&b, "$Episode    = %s\n", psQuote(episode))
	fmt.Fprintf(&b, "$EpisodeDir = Join-Path $ScriptsDir %s\n", psQuote(j.EpisodeDir))
	fmt.Fprintf(&b, "$ScriptFile = Join-Path $EpisodeDir %s\n", psQuote(scriptName))
	fmt.Fprintf(&b, "$HevcFile   = Join-Path $EpisodeDir %s\n", psQuote(EncodeHevcName))
	fmt.Fprintf(&b, "$AudioFile  = Join-Path $EpisodeDir %s\n", psQuote(AudioWavName))
	fmt.Fprintf(&b, "$OutputName = %s\n", psQuote(OutputName(j.Series, episode, v.Tag)))
	fmt.Fprintf(&b, "$ReleaseFolder = %s\n", psQuote(ReleaseFolderName(v.Group, j.Series, v.Tag)))
	fmt.Fprintf(&b, "$DefaultX265Args = %s\n\n", psQuote(DefaultX265Args))

	b.WriteString("$Job = [pscustomobject]@{\n")
	b.WriteString("    Series = $Series; Episode = $Episode; EpisodeDir = $EpisodeDir\n")
	b.WriteString("    ScriptFile = $ScriptFile; BinDir = $BinDir; ScriptsDir = $ScriptsDir\n")
	b.WriteString("    ReleaseDir = $ReleaseDir; Group = " + psQuote(v.Group) + "; Tag = " + psQuote(v.Tag) + "\n")
	b.WriteString("    OutputName = $OutputName; ReleaseFolder = $ReleaseFolder\n")
	b.WriteString("    HevcFile = $HevcFile; AudioFile = $AudioFile\n")
	b.WriteString("    DiscordWebhook = " + psQuote(v.DiscordWebhook) + "\n")
	b.WriteString("    DefaultX265Args = $DefaultX265Args\n")
	b.WriteString("}\n\n")

	// Link each step's PowerShell into the final script (dedupe by key: a
	// flow may reference the same template twice with different params).
	emitted := map[string]bool{}
	b.WriteString("# --- Step implementations (linked from step templates) ---\n")
	for i, t := range templates {
		if emitted[t.Key] {
			continue
		}
		emitted[t.Key] = true
		fmt.Fprintf(&b, "\n# Step template: %s (%s)\n%s\n", psComment(t.Key), psComment(t.Label), t.PowerShell)
		_ = i
	}
	b.WriteString("\n# --- Pipeline execution ---\n")

	// Track invoked function names: two templates defining the same function
	// would silently override each other, so refuse instead.
	invoked := map[string]string{}
	for i, st := range f.Steps {
		t := templates[i]
		fn := psFuncName.FindStringSubmatch(t.PowerShell)[1]
		if owner, dup := invoked[fn]; dup && owner != t.Key {
			return "", fmt.Errorf("flow %q: templates %q and %q both define %s", f.Name, owner, t.Key, fn)
		}
		invoked[fn] = t.Key
		// st.Type and t.Key are sanitized before interpolation; step labels
		// live only in comments, newline-stripped.
		safeType := psSafeIdent(string(st.Type))
		if safeType == "" {
			return "", fmt.Errorf("flow %q step %d: unsafe step type %q", f.Name, i, st.Type)
		}
		fmt.Fprintf(&b, "\n# --- Step %d: %s ---\n", i+1, psComment(t.Key))
		fmt.Fprintf(&b, "Write-Output \"ENCODE_STEP %s start\"\n", safeType)
		b.WriteString("try {\n")
		fmt.Fprintf(&b, "    $stepParams = [pscustomobject]@{%s}\n", paramsLiteral(st, t))
		fmt.Fprintf(&b, "    %s -Job $Job -Params $stepParams\n", fn)
		b.WriteString("} catch {\n")
		fmt.Fprintf(&b, "    Write-Output \"ENCODE_STEP_FAILED %s $($_.Exception.Message)\"\n", safeType)
		b.WriteString("    exit 1\n}\n")
	}

	b.WriteString("\nWrite-Output \"ENCODE_JOB_DONE\"\n")
	return b.String(), nil
}

// paramsLiteral renders the step's parameter values as a pscustomobject
// literal, restricted to the template's declared params (unknown keys are
// dropped so stale params cannot leak into a re-imported flow).
func paramsLiteral(st model.Step, t *model.StepTemplate) string {
	var parts []string
	for _, pd := range t.Params {
		val := ""
		if v, ok := st.Params[pd.Key]; ok {
			val = v
		}
		// Param keys become bare PowerShell identifiers — sanitize to the
		// safe alphabet so no key can close the hashtable and inject code.
		key := psSafeIdent(pd.Key)
		if key == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf(" %s = %s", key, psQuote(val)))
	}
	return strings.Join(parts, "; ") + " "
}

func name(f *model.Flow) string {
	if f == nil {
		return ""
	}
	return f.Name
}

// ValidateForRender reports whether every step of f resolves against resolve
// and every referenced template defines a function — the exact checks Render
// applies, so anything that validates here also renders without panicking.
func ValidateForRender(f *model.Flow, resolve TemplateResolver) error {
	if resolve == nil {
		resolve = BuiltinResolver()
	}
	if f == nil || len(f.Steps) == 0 {
		return fmt.Errorf("flow has no steps")
	}
	for i, st := range f.Steps {
		t, err := resolve(st.TemplateKey())
		if err != nil {
			return fmt.Errorf("step %d: %w", i, err)
		}
		if psFuncName.FindStringSubmatch(t.PowerShell) == nil {
			return fmt.Errorf("step %d (%s): template defines no function", i, t.Key)
		}
	}
	return nil
}

