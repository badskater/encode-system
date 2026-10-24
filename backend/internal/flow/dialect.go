// Package flow: Dialect abstraction.
//
// Render used to be PowerShell-only: psQuote/psSafeIdent/psComment were
// free functions and the renderer inlined PS preamble lines directly.
// To support Linux nodes (bash), the dialect-specific pieces moved
// behind the Dialect interface so RenderDialect can emit either
// PowerShell or bash without the caller caring which.
//
// Per-dialect step templates:
//   - The model still carries one PowerShell source per StepTemplate
//     (the canonical, editable form users see in the UI).
//   - PowerShellDialect emits that source verbatim, as before.
//   - BashDialect inspects the source: a body that is pure external
//     commands (no cmdlets) is rendered into a bash function; a body
//     using PowerShell-only cmdlets (Get-FileHash, Resolve-Tool, …)
//     returns a clear render error for now. Task 1.3 will add Linux
//     tool mappings behind the same Dialect.TemplateBody seam exposed
//     to Render via a dialect-aware lookup (the resolver itself does not
//     change shape; BashDialect validates the body it resolves).
package flow

import (
	"fmt"
	"strings"

	"github.com/badskater/encode-system/backend/internal/model"
)

// Dialect abstracts the script language Render emits. Every
// PS-specific operation (quoting, comments, marker emission, preamble)
// lives behind a Dialect so the renderer is dialect-agnostic.
//
// The interface is split so the renderer is byte-identical for PS: the PS
// implementations reproduce the exact lines the pre-refactor renderer
// emitted, while bash provides its own preamble, job-context block, and
// per-step wrapper. The step protocol markers (ENCODE_STEP /
// ENCODE_STEP_FAILED / ENCODE_JOB_DONE) stay identical across dialects.
type Dialect interface {
	// Ext is the script file extension, e.g. ".ps1" or ".sh".
	Ext() string

	// Quote renders a string as a safe literal in the dialect's
	// language: PS single-quote doubling, or POSIX single-quote
	// escaping. Every interpolated value passes through here, which
	// is the primary injection guard.
	Quote(s string) string

	// SafeIdent restricts an identifier (template key, param key,
	// step type) to a safe alphabet before it is interpolated as a
	// bare token. Defense in depth on top of API-boundary validation.
	SafeIdent(s string) string

	// Comment renders a value safe for interpolation into a comment
	// line: PS must avoid block-comment termination rules, bash must
	// prefix '#' and neutralize newlines so a smuggled line can never
	// become executable.
	Comment(s string) string

	// Preamble emits the script preamble that precedes the job context.
	// Includes the shebang/header, the generated-by comment line, the
	// param/LibPath line (PS) or nothing (bash owns its own header).
	Preamble(j *model.Job, f *model.Flow, v Vars) string

	// JobContext emits the shared job-context block (variables the
	// step functions read). PS builds a $Job pscustomobject; bash
	// exports env vars / shell variables. Episode and scriptName are
	// pre-computed by the renderer so both dialects share the logic.
	JobContext(j *model.Job, v Vars, episode, scriptName string) string

	// StepCall emits the per-step wrapper: the start marker, the
	// params literal, the function invocation, and the failure
	// handler. PS uses try/catch; bash uses a subshell/trap.
	StepCall(idx int, t *model.StepTemplate, st model.Step, safeType, fn string) (string, error)

	// FuncName extracts the function name a template's source defines.
	// PS uses the `function <Name>` regex; bash looks for its own
	// function declaration form.
	FuncName(src string) string

	// StepBody returns the executable body for a resolved template. PS
	// returns the PowerShell source verbatim. Bash validates that the
	// body is renderable as bash (pure external commands) or returns a
	// clear error; Task 1.3 will map PS tool calls to Linux tools here.
	StepBody(t *model.StepTemplate) (string, error)

	// ParamsLiteral renders the step's params as a dialect-native
	// object/literal the step body consumes (PS: pscustomobject;
	// bash: positional env vars).
	ParamsLiteral(st model.Step, t *model.StepTemplate) string

	// JobDone emits the terminal ENCODE_JOB_DONE marker.
	JobDone() string
}

// --- helpers shared by both dialects (language-agnostic) -----------

// stripNewlines flattens CR/LF to spaces. Both dialects use this for
// comment-safety: a value interpolated into a comment must never
// introduce a line break.
func stripNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// safeIdentAlpha restricts an identifier to [a-z0-9_] (lowercased).
// Both dialects accept this alphabet for bare tokens.
func safeIdentAlpha(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// DialectForPlatform selects the script dialect for a node's reported platform.
// "linux" → BashDialect; anything else (including "" for legacy rows and old
// agents that never reported a platform) → PowerShellDialect, the historical
// fleet default. The controller calls this at dispatch time so a mixed
// Windows/Linux farm renders the correct script per node.
func DialectForPlatform(platform string) Dialect {
	if platform == "linux" {
		return BashDialect{}
	}
	return PowerShellDialect{}
}

// --- PowerShellDialect (byte-identical to the pre-refactor renderer) ---

// PowerShellDialect emits the same PowerShell the renderer always did.
// psQuote/psSafeIdent/psComment are kept as their original free
// functions (tests reference them) and this dialect delegates to them,
// so PS output is byte-for-byte unchanged.
type PowerShellDialect struct{}

func (PowerShellDialect) Ext() string { return ".ps1" }

func (PowerShellDialect) Quote(s string) string     { return psQuote(s) }
func (PowerShellDialect) SafeIdent(s string) string { return psSafeIdent(s) }
func (PowerShellDialect) Comment(s string) string   { return psComment(s) }

// Preamble reproduces the exact lines the pre-refactor renderer emitted
// before the job-context block: the generated-by comment, the mandatory
// $LibPath param, strict mode, and the dot-source of the lib. PS output
// is byte-identical to the original.
func (PowerShellDialect) Preamble(j *model.Job, f *model.Flow, _ Vars) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by encode-system controller - job %d, flow %s\n", j.ID, psComment(f.Name))
	b.WriteString("param([Parameter(Mandatory=$true)][string]$LibPath)\n")
	b.WriteString("Set-StrictMode -Version Latest\n")
	b.WriteString("$ErrorActionPreference = 'Stop'\n")
	b.WriteString(". $LibPath\n\n")
	return b.String()
}

// JobContext reproduces the exact PS $BinDir/$ScriptsDir/… assignments
// and the $Job pscustomobject the original renderer emitted.
func (d PowerShellDialect) JobContext(j *model.Job, v Vars, episode, scriptName string) string {
	var b strings.Builder
	b.WriteString("# --- Job context (shared by every step) ---\n")
	fmt.Fprintf(&b, "$BinDir     = %s\n", d.Quote(v.BinDir))
	fmt.Fprintf(&b, "$ScriptsDir = %s\n", d.Quote(v.ScriptsDir))
	fmt.Fprintf(&b, "$ReleaseDir = %s\n", d.Quote(v.ReleaseDir))
	fmt.Fprintf(&b, "$Series     = %s\n", d.Quote(j.Series))
	fmt.Fprintf(&b, "$Episode    = %s\n", d.Quote(episode))
	fmt.Fprintf(&b, "$EpisodeDir = Join-Path $ScriptsDir %s\n", d.Quote(j.EpisodeDir))
	fmt.Fprintf(&b, "$ScriptFile = Join-Path $EpisodeDir %s\n", d.Quote(scriptName))
	fmt.Fprintf(&b, "$HevcFile   = Join-Path $EpisodeDir %s\n", d.Quote(EncodeHevcName))
	fmt.Fprintf(&b, "$AudioFile  = Join-Path $EpisodeDir %s\n", d.Quote(AudioWavName))
	fmt.Fprintf(&b, "$OutputName = %s\n", d.Quote(OutputName(j.Series, episode, v.Tag)))
	fmt.Fprintf(&b, "$ReleaseFolder = %s\n", d.Quote(ReleaseFolderName(v.Group, j.Series, v.Tag)))
	fmt.Fprintf(&b, "$DefaultX265Args = %s\n\n", d.Quote(DefaultX265Args))

	b.WriteString("$Job = [pscustomobject]@{\n")
	b.WriteString("    Series = $Series; Episode = $Episode; EpisodeDir = $EpisodeDir\n")
	b.WriteString("    ScriptFile = $ScriptFile; BinDir = $BinDir; ScriptsDir = $ScriptsDir\n")
	b.WriteString("    ReleaseDir = $ReleaseDir; Group = " + d.Quote(v.Group) + "; Tag = " + d.Quote(v.Tag) + "\n")
	b.WriteString("    OutputName = $OutputName; ReleaseFolder = $ReleaseFolder\n")
	b.WriteString("    HevcFile = $HevcFile; AudioFile = $AudioFile\n")
	b.WriteString("    DiscordWebhook = " + d.Quote(v.DiscordWebhook) + "\n")
	b.WriteString("    DefaultX265Args = $DefaultX265Args\n")
	b.WriteString("}\n\n")
	return b.String()
}

// StepCall reproduces the exact PS per-step block: the step comment, the
// start marker, the try/catch wrapping the params literal + invocation,
// and the failure marker + exit 1.
func (d PowerShellDialect) StepCall(idx int, t *model.StepTemplate, st model.Step, safeType, fn string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "\n# --- Step %d: %s ---\n", idx+1, d.Comment(t.Key))
	fmt.Fprintf(&b, "Write-Output \"ENCODE_STEP %s start\"\n", safeType)
	b.WriteString("try {\n")
	fmt.Fprintf(&b, "    $stepParams = [pscustomobject]@{%s}\n", d.ParamsLiteral(st, t))
	fmt.Fprintf(&b, "    %s -Job $Job -Params $stepParams\n", fn)
	b.WriteString("} catch {\n")
	fmt.Fprintf(&b, "    Write-Output \"ENCODE_STEP_FAILED %s $($_.Exception.Message)\"\n", safeType)
	b.WriteString("    exit 1\n}\n")
	return b.String(), nil
}

func (PowerShellDialect) FuncName(src string) string {
	if m := psFuncName.FindStringSubmatch(src); m != nil {
		return m[1]
	}
	return ""
}
func (PowerShellDialect) StepBody(t *model.StepTemplate) (string, error) {
	return t.PowerShell, nil
}
func (d PowerShellDialect) ParamsLiteral(st model.Step, t *model.StepTemplate) string {
	return paramsLiteral(st, t)
}
func (PowerShellDialect) JobDone() string {
	return `Write-Output "ENCODE_JOB_DONE"`
}
