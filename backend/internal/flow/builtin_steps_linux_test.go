package flow

import (
	"regexp"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// linuxTestJob returns a job suitable for bash rendering on a Linux node.
func linuxTestJob() *model.Job {
	return &model.Job{
		ID:         42,
		Series:     "Test Series",
		Episode:    "01",
		EpisodeDir: "Test Series/Ep 01",
		ScriptType: "vpy",
	}
}

// linuxTestVars returns Linux-path Vars for bash rendering.
func linuxTestVars() Vars {
	return Vars{
		BinDir:     "/bin",
		ScriptsDir: "/encodes/scripts",
		ReleaseDir: "/encodes/ReleaseFolders",
		Group:      "TestGroup",
		Tag:        "1080p",
	}
}

// allBuiltinKeys returns the key of every built-in template, in declaration
// order. Used to iterate the full inventory so a new template added later
// is not silently missed by the parity tests.
func allBuiltinKeys() []string {
	var keys []string
	for _, t := range BuiltinStepTemplates() {
		keys = append(keys, t.Key)
	}
	return keys
}

// singleStepFlow builds a one-step flow referencing the given template key.
func singleStepFlow(key string) *model.Flow {
	return &model.Flow{
		Name:  "linux-" + key,
		Steps: []model.Step{{Type: model.StepType(key)}},
	}
}

// renderBuiltinBash renders a single-step flow for the given built-in key
// through the bash dialect (BuiltinResolver serves the template). Returns
// the rendered script or the render error. Fails the test on unexpected
// resolver errors (NOT on render errors — the caller distinguishes refused
// templates from ported ones).
func renderBuiltinBash(t *testing.T, key string) (string, error) {
	t.Helper()
	resolve := BuiltinResolver()
	f := singleStepFlow(key)
	return RenderDialect(f, linuxTestJob(), linuxTestVars(), resolve, BashDialect{})
}

// renderBuiltinPS renders the same single-step flow through the PowerShell
// dialect (the canonical source). Used to extract PS ENCODE_STEP marker
// sequences for parity comparison.
func renderBuiltinPS(t *testing.T, key string) string {
	t.Helper()
	resolve := BuiltinResolver()
	f := singleStepFlow(key)
	script, err := RenderDialect(f, linuxTestJob(), linuxTestVars(), resolve, PowerShellDialect{})
	if err != nil {
		t.Fatalf("PS render of %q failed: %v", key, err)
	}
	return script
}

// encodeStepMarkers extracts the ENCODE_STEP <name> <pct> progress markers
// (and ENCODE_STEP <name> start) from a rendered script, in order. These are
// the lines the agent's stdout parser consumes; parity between dialects means
// the bash render emits the SAME marker sequence the PS render does.
var encodeStepLineRe = regexp.MustCompile(`ENCODE_STEP[^\r\n]*`)

func encodeStepMarkers(script string) []string {
	var out []string
	for _, m := range encodeStepLineRe.FindAllString(script, -1) {
		out = append(out, m)
	}
	return out
}

// markersFromFunctionBody extracts ENCODE_STEP progress markers that appear
// INSIDE the step function body (the progress lines the function writes),
// excluding the StepCall wrapper's "start" and "FAILED" markers. This
// isolates the per-template progress sequence for parity comparison.
//
// It handles BOTH dialect shapes:
//   - PS:    Write-Output "ENCODE_STEP <name> <pct>"
//   - bash:  echo 'ENCODE_STEP <name> <pct>'
//
// extracting the bare marker text (ENCODE_STEP <name> <pct>) from each so
// the two dialects can be compared apples-to-apples.
func markersFromFunctionBody(script string) []string {
	var out []string
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		// Skip the wrapper's start marker and failure markers (those are
		// emitted by StepCall, not the template body).
		if strings.HasSuffix(trimmed, "start\"") || strings.HasSuffix(trimmed, "start'") {
			continue
		}
		if strings.Contains(trimmed, "ENCODE_STEP_FAILED") {
			continue
		}
		// PS form: Write-Output "ENCODE_STEP ..."
		if strings.HasPrefix(trimmed, "Write-Output ") {
			s := strings.TrimSpace(trimmed)
			s = strings.TrimPrefix(s, "Write-Output ")
			s = strings.Trim(s, "\"")
			if strings.HasPrefix(s, "ENCODE_STEP ") {
				out = append(out, s)
			}
			continue
		}
		// bash form: echo 'ENCODE_STEP ...'
		if strings.HasPrefix(trimmed, "echo ") {
			s := strings.TrimSpace(trimmed)
			s = strings.TrimPrefix(s, "echo ")
			s = strings.Trim(s, "'")
			if strings.HasPrefix(s, "ENCODE_STEP ") {
				out = append(out, s)
			}
			continue
		}
	}
	return out
}

// refusedTemplates is the set of built-in templates that genuinely have no
// Linux equivalent (Windows/Nvidia-only tools). They are refused by the bash
// dialect with a clear error rather than ported incorrectly.
var refusedTemplates = map[string]string{
	"dgindex": "DGIndexNV requires an Nvidia GPU and is Windows-only; Linux nodes use VapourSynth source filters instead (no dgi index needed)",
}

// builtinLinuxToolMap records the Linux tool each ported template MUST
// reference in its bash body. A template missing its expected tool indicates
// a broken port (e.g. audio still calling eac3to). Verified per-template.
var builtinLinuxToolMap = map[string]string{
	"source_rename":  "mv",        // bash mv replaces Rename-Item
	"media_probe":    "mediainfo", // MediaInfo CLI (lowercase binary name on Linux)
	"hdr_probe":      "mediainfo", // MediaInfo CLI
	"audio":          "opusenc",   // opusenc is the encoder (ffmpeg decode-only)
	"audio_branch":   "opusenc",   // opusenc + mediainfo + ffmpeg decode
	"audio_lang":     "opusenc",   // opusenc + mediainfo + ffmpeg decode
	"flac_audio":     "flac",      // flac encoder (ffmpeg decode-only)
	"encode":         "x265",      // x265 encoder (vspipe pipes in)
	"encode_4k":      "x265",      // x265 encoder (vspipe pipes in)
	"mux":            "mkvmerge",  // mkvmerge (same binary on Linux)
	"verify_output":  "mkvmerge",  // mkvmerge -J (same binary)
	"crc32_rename":   "cksum",     // POSIX cksum (CRC32) or python fallback
	"release_copy":   "cp",        // bash cp replaces Copy-Item
	"keyframes":      "ffmpeg",    // ffmpeg decode -> SCXvid (python)
	"discord_notify": "curl",      // curl replaces WebClient
}

// TestBuiltinBashAllTemplatesResolveOrRefuse: every built-in template must
// either render through the bash dialect (ported) or be explicitly refused
// (listed in refusedTemplates). A template that does neither is a bug — it
// means StepBody returned an error for a template that should port, or the
// template was missed entirely.
func TestBuiltinBashAllTemplatesResolveOrRefuse(t *testing.T) {
	for _, key := range allBuiltinKeys() {
		key := key
		t.Run(key, func(t *testing.T) {
			script, err := renderBuiltinBash(t, key)
			if _, refused := refusedTemplates[key]; refused {
				if err == nil {
					t.Fatalf("template %q must be REFUSED by bash dialect (listed as refused), but rendered:\n%s", key, script)
				}
				if !strings.Contains(err.Error(), "bash") && !strings.Contains(err.Error(), "Linux") {
					t.Errorf("refusal error for %q must mention bash/Linux: %v", key, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("template %q must PORT to bash but render failed: %v", key, err)
			}
			if strings.TrimSpace(script) == "" {
				t.Fatalf("template %q rendered an empty script", key)
			}
		})
	}
}

// TestBuiltinBashNoPowerShellCmdlets: for every PORTED template, the bash
// output must contain zero PowerShell-only cmdlet tokens. This is the hard
// guarantee that the dialect did not pass PS through verbatim. The refused
// templates are skipped (they error before producing output).
func TestBuiltinBashNoPowerShellCmdlets(t *testing.T) {
	psTokens := []string{
		"function Invoke-",       // PS function declaration
		"param($Job",             // PS param block
		"Write-Output",           // PS output cmdlet
		"pscustomobject",         // PS object literal
		"Join-Path",              // PS path cmdlet
		"Resolve-Tool",           // PS/EncodeLib helper
		"Invoke-Tool",            // PS/EncodeLib helper
		"Find-SourceFile",        // PS/EncodeLib helper
		"Assert-SafeName",        // PS/EncodeLib helper
		"Get-ChildItem",          // PS cmdlet
		"Get-Item",               // PS cmdlet
		"Test-Path",              // PS cmdlet
		"Remove-Item",            // PS cmdlet
		"Rename-Item",            // PS cmdlet
		"Copy-Item",              // PS cmdlet
		"New-Item",               // PS cmdlet
		"ConvertFrom-Json",       // PS cmdlet
		"ConvertTo-Json",         // PS cmdlet
		"ForEach-Object",         // PS pipeline cmdlet
		"Where-Object",           // PS pipeline cmdlet
		"Select-Object",          // PS pipeline cmdlet
		"Get-FileHash",           // PS cmdlet (sha256sum replaces it)
		"Set-StrictMode",         // PS strict mode
		"$ErrorActionPreference", // PS error pref
		"Invoke-RestMethod",      // PS HTTP cmdlet
		"System.Net.WebClient",   // PS .NET HTTP
		"System.IO.File",         // PS .NET file IO
		"System.Diagnostics",     // PS .NET process
		"Add-Type",               // PS type compilation
		"[regex]::",              // PS regex
		"[math]::",               // PS math
		"[int]",                  // PS cast
		"[double]",               // PS cast
		"[bool]",                 // PS cast
		"[string]",               // PS cast
		"[char]",                 // PS cast
		"[Environment]",          // PS .NET env
		"$LASTEXITCODE",          // PS exit code var
		"$_.",                    // PS pipeline var
		"$Params.",               // PS param access
		"$Job.",                  // PS job access (in templates; bash uses $Job_ env vars instead)
	}
	for _, key := range allBuiltinKeys() {
		key := key
		if _, refused := refusedTemplates[key]; refused {
			continue
		}
		t.Run(key, func(t *testing.T) {
			script, err := renderBuiltinBash(t, key)
			if err != nil {
				t.Fatalf("render %q: %v", key, err)
			}
			for _, tok := range psTokens {
				if strings.Contains(script, tok) {
					t.Errorf("bash output for %q leaked PowerShell token %q", key, tok)
				}
			}
		})
	}
}

// TestBuiltinBashExpectedLinuxTool: each ported template must reference its
// expected Linux tool (from builtinLinuxToolMap). A missing tool means the
// port is incomplete or wrong (e.g. audio calling eac3to instead of ffmpeg).
func TestBuiltinBashExpectedLinuxTool(t *testing.T) {
	for _, key := range allBuiltinKeys() {
		key := key
		if _, refused := refusedTemplates[key]; refused {
			continue
		}
		want, ok := builtinLinuxToolMap[key]
		if !ok {
			t.Errorf("template %q has no expected-tool mapping — add it to builtinLinuxToolMap", key)
			continue
		}
		t.Run(key, func(t *testing.T) {
			script, err := renderBuiltinBash(t, key)
			if err != nil {
				t.Fatalf("render %q: %v", key, err)
			}
			// Match the tool as a word boundary so "cp" doesn't match inside
			// "scp" etc. Use a simple substring check first, then a regex
			// for tools that are common substrings.
			if !toolPresent(script, want) {
				t.Errorf("bash output for %q missing expected Linux tool %q", key, want)
			}
		})
	}
}

// toolPresent checks whether the tool appears as a command in the script,
// not just as a substring of a longer word.
func toolPresent(script, tool string) bool {
	// Build a word-boundary regex for the tool.
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(tool) + `\b`)
	return re.MatchString(script)
}

// TestBuiltinBashNoEac3to: the user's HARD constraint — eac3to does not exist
// on Linux. It must never be INVOKED as a command in any bash render output
// (audio steps use ffmpeg decode-only instead). A reference in a descriptive
// comment line (e.g. "# Step template: audio (Audio (eac3to → Opus))") is
// the PS label surfacing in the comment header and is NOT a command
// invocation — the constraint is that the eac3to binary is never called.
func TestBuiltinBashNoEac3to(t *testing.T) {
	for _, key := range allBuiltinKeys() {
		key := key
		if _, refused := refusedTemplates[key]; refused {
			continue
		}
		t.Run(key, func(t *testing.T) {
			script, err := renderBuiltinBash(t, key)
			if err != nil {
				t.Fatalf("render %q: %v", key, err)
			}
			// Check each non-comment line for eac3to as a command. A line
			// is "executable" if it doesn't start with '#' (after trimming).
			for i, line := range strings.Split(script, "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "#") {
					continue // comment line — descriptive only
				}
				if strings.Contains(strings.ToLower(trimmed), "eac3to") {
					t.Errorf("bash output for %q invokes eac3to on line %d (does not exist on Linux): %q", key, i, trimmed)
				}
			}
		})
	}
}

// TestBuiltinBashAudioNoVideoEncode: the user's HARD constraint — ffmpeg is
// for AUDIO ONLY. Audio templates must never invoke ffmpeg with video-encode
// args (-c:v with libx264/libx265/svt-av1/etc.). The audio templates (audio,
// audio_branch, audio_lang, flac_audio) are checked specifically.
func TestBuiltinBashAudioNoVideoEncode(t *testing.T) {
	audioTemplates := []string{"audio", "audio_branch", "audio_lang", "flac_audio"}
	videoCodecRe := regexp.MustCompile(`-c:v\s+(libx264|libx265|svt_av1|svtav1|h264_nvenc|hevc_nvenc|libvpx)`)
	for _, key := range audioTemplates {
		key := key
		t.Run(key, func(t *testing.T) {
			script, err := renderBuiltinBash(t, key)
			if err != nil {
				t.Fatalf("render %q: %v", key, err)
			}
			if videoCodecRe.MatchString(script) {
				t.Errorf("audio template %q invokes ffmpeg with a video encode codec (HARD constraint: ffmpeg audio only)", key)
			}
			// Also assert ffmpeg appears only in decode shape (no -c:v at all
			// in audio output, since audio decode doesn't need it).
			if strings.Contains(script, "-c:v") {
				t.Errorf("audio template %q uses ffmpeg -c:v (should be decode-only, no video codec)", key)
			}
		})
	}
}

// TestBuiltinBashMarkerParity: the ENCODE_STEP progress marker sequence
// emitted by the bash render of each ported template must EXACTLY match the
// sequence the PS render emits (same step names + percentages, in order).
// This is the agent-parser-agnostic guarantee: a node's stdout parser sees
// the same progress line sequence regardless of dialect.
func TestBuiltinBashMarkerParity(t *testing.T) {
	for _, key := range allBuiltinKeys() {
		key := key
		if _, refused := refusedTemplates[key]; refused {
			continue
		}
		t.Run(key, func(t *testing.T) {
			psScript := renderBuiltinPS(t, key)
			bashScript, err := renderBuiltinBash(t, key)
			if err != nil {
				t.Fatalf("bash render %q: %v", key, err)
			}
			psMarkers := markersFromFunctionBody(psScript)
			bashMarkers := markersFromFunctionBody(bashScript)
			if len(psMarkers) == 0 {
				t.Fatalf("template %q: PS render produced no ENCODE_STEP markers (test bug)", key)
			}
			if len(bashMarkers) != len(psMarkers) {
				t.Errorf("template %q: marker count mismatch (PS=%d, bash=%d)\nPS:   %v\nBash: %v",
					key, len(psMarkers), len(bashMarkers), psMarkers, bashMarkers)
			}
			for i := 0; i < len(psMarkers) && i < len(bashMarkers); i++ {
				if psMarkers[i] != bashMarkers[i] {
					t.Errorf("template %q: marker %d mismatch\n  PS:   %q\n  Bash: %q",
						key, i, psMarkers[i], bashMarkers[i])
				}
			}
		})
	}
}

// TestBuiltinBashStartAndFailedMarkers: every ported template's bash render
// must emit the StepCall wrapper markers (ENCODE_STEP <name> start and
// ENCODE_STEP_FAILED <name>) so the agent's parser sees step boundaries.
func TestBuiltinBashStartAndFailedMarkers(t *testing.T) {
	for _, key := range allBuiltinKeys() {
		key := key
		if _, refused := refusedTemplates[key]; refused {
			continue
		}
		t.Run(key, func(t *testing.T) {
			script, err := renderBuiltinBash(t, key)
			if err != nil {
				t.Fatalf("render %q: %v", key, err)
			}
			if !strings.Contains(script, "ENCODE_STEP "+key+" start") {
				t.Errorf("template %q: bash render missing 'ENCODE_STEP %s start' wrapper marker", key, key)
			}
			if !strings.Contains(script, "ENCODE_STEP_FAILED "+key) {
				t.Errorf("template %q: bash render missing 'ENCODE_STEP_FAILED %s' wrapper marker", key, key)
			}
		})
	}
}

// TestBuiltinBashRefusedTemplatesErrorClearly: each refused template must
// produce a render error that names the template and explains WHY it is
// refused (so an operator reading the job log understands the platform
// mismatch, not a generic "cannot render").
func TestBuiltinBashRefusedTemplatesErrorClearly(t *testing.T) {
	for _, key := range allBuiltinKeys() {
		key := key
		reason, refused := refusedTemplates[key]
		if !refused {
			continue
		}
		t.Run(key, func(t *testing.T) {
			_, err := renderBuiltinBash(t, key)
			if err == nil {
				t.Fatalf("refused template %q must error", key)
			}
			msg := err.Error()
			if !strings.Contains(msg, key) {
				t.Errorf("refusal error must name the template %q: %v", key, err)
			}
			// Must mention a platform/tool reason (not just "cannot render").
			if !strings.Contains(msg, "Linux") && !strings.Contains(msg, "bash") && !strings.Contains(msg, "Windows") {
				t.Errorf("refusal error for %q must explain the platform reason: %v", key, err)
			}
			// The reason must be surfaceable somewhere — the test just
			// ensures the explanation is substantive. Use the reason
			// string as a documentation anchor.
			if reason == "" {
				t.Errorf("refused template %q has no documented reason in refusedTemplates", key)
			}
		})
	}
}

// TestBuiltinBashEncodeUsesVapourSynthPipe: the encode/encode_4k templates
// must pipe the filter script through vspipe into x265 (the Linux standard),
// NOT pass the .vpy directly to x265 as --input. The PS template uses
// --input $Job.ScriptFile; the bash port must use `vspipe -c y4m ... | x265
// --y4m ...` so x265 reads the y4m stream on stdin.
func TestBuiltinBashEncodeUsesVapourSynthPipe(t *testing.T) {
	for _, key := range []string{"encode", "encode_4k"} {
		key := key
		t.Run(key, func(t *testing.T) {
			script, err := renderBuiltinBash(t, key)
			if err != nil {
				t.Fatalf("render %q: %v", key, err)
			}
			if !strings.Contains(script, "vspipe") {
				t.Errorf("encode template %q must use vspipe to pipe the filter script into x265", key)
			}
			if !strings.Contains(script, "x265") {
				t.Errorf("encode template %q must invoke x265", key)
			}
			// x265 must read the y4m stream (--y4m flag) from stdin (-),
			// not --input <file>.
			if !strings.Contains(script, "--y4m") {
				t.Errorf("encode template %q: x265 must use --y4m to read the vspipe y4m stream", key)
			}
		})
	}
}

// TestBuiltinBashEncodeMirrorsX265Args: the x265 argument SET in the bash
// encode templates must mirror the PS template's argument set verbatim (same
// flags, same defaults). We assert the key flags from DefaultX265Args appear
// in the bash output so a regression that invents new defaults is caught.
func TestBuiltinBashEncodeMirrorsX265Args(t *testing.T) {
	keyFlags := []string{
		"--preset",
		"--crf",
		"--deblock=-2:-2",
		"--min-keyint",
		"23",
		"--keyint",
		"240",
		"--ref",
		"6",
		"--bframes",
		"12",
		"--aq-mode",
		"--aq-strength",
		"--psy-rd",
		"--psy-rdoq",
		"--no-sao",
	}
	for _, key := range []string{"encode", "encode_4k"} {
		key := key
		t.Run(key, func(t *testing.T) {
			script, err := renderBuiltinBash(t, key)
			if err != nil {
				t.Fatalf("render %q: %v", key, err)
			}
			for _, flag := range keyFlags {
				if !strings.Contains(script, flag) {
					t.Errorf("encode template %q: x265 arg %q missing from bash port (must mirror PS verbatim)", key, flag)
				}
			}
		})
	}
}

// TestBuiltinBashCurlNotifyUsesWebhookFromEnv: the discord_notify bash port
// must read the webhook from the DiscordWebhook job-context env var (never
// inline a literal URL) and POST the same JSON payload shape via curl.
func TestBuiltinBashCurlNotifyUsesWebhookFromEnv(t *testing.T) {
	script, err := renderBuiltinBash(t, "discord_notify")
	if err != nil {
		t.Fatalf("render discord_notify: %v", err)
	}
	// curl must appear (replaces WebClient).
	if !strings.Contains(script, "curl") {
		t.Error("discord_notify bash port must use curl")
	}
	// The webhook must come from the $DiscordWebhook env var, not a literal.
	if !strings.Contains(script, "DiscordWebhook") {
		t.Error("discord_notify bash port must reference the DiscordWebhook env var")
	}
	// Must POST (not GET) with a JSON content-type header.
	if !strings.Contains(script, "-X POST") && !strings.Contains(script, "--request POST") {
		// curl -d implies POST; accept either form.
		if !strings.Contains(script, "-d ") && !strings.Contains(script, "--data") {
			t.Error("discord_notify bash port must POST with a data payload")
		}
	}
	if !strings.Contains(script, "application/json") {
		t.Error("discord_notify bash port must set Content-Type: application/json")
	}
}

// TestBuiltinBashAudioUsesFfmpegDecode: the audio templates (audio,
// audio_branch, audio_lang) must use ffmpeg in DECODE-ONLY shape
// (-c:a pcm_s24le to WAV), never for encoding. This is the user's hard
// constraint restated as a positive assertion on the decode shape.
func TestBuiltinBashAudioUsesFfmpegDecode(t *testing.T) {
	for _, key := range []string{"audio", "audio_branch", "audio_lang"} {
		key := key
		t.Run(key, func(t *testing.T) {
			script, err := renderBuiltinBash(t, key)
			if err != nil {
				t.Fatalf("render %q: %v", key, err)
			}
			// ffmpeg must appear (replaces eac3to decode).
			if !strings.Contains(script, "ffmpeg") {
				t.Errorf("audio template %q must use ffmpeg for decode (replaces eac3to)", key)
			}
			// The decode must produce a WAV (pcm_s24le) — decode-only use.
			if !strings.Contains(script, "pcm_s24le") && !strings.Contains(script, "pcm_s16le") {
				t.Errorf("audio template %q: ffmpeg must decode to WAV (pcm_s24le/pcm_s16le), not encode", key)
			}
		})
	}
}

// TestBuiltinBashFlacUsesFfmpegDecode: the flac_audio template must use
// ffmpeg to decode the source to WAV, then the `flac` encoder (which exists
// on Linux) to produce the FLAC — mirroring the PS template's eac3to FLAC
// path. opusenc is NOT involved here (the PS template doesn't use it).
func TestBuiltinBashFlacUsesFfmpegDecode(t *testing.T) {
	script, err := renderBuiltinBash(t, "flac_audio")
	if err != nil {
		t.Fatalf("render flac_audio: %v", err)
	}
	if !strings.Contains(script, "ffmpeg") {
		t.Error("flac_audio must use ffmpeg to decode the source to WAV")
	}
	if !strings.Contains(script, "flac") {
		t.Error("flac_audio must use the flac encoder to produce the FLAC")
	}
}

// TestBuiltinBashVerifyUsesSha256sum: if the verify_output template does any
// file hashing (it doesn't today, but crc32_rename and any future verify
// hash must), the bash port must use sha256sum, not Get-FileHash. This is a
// guard against a future verify hash addition using the PS cmdlet.
func TestBuiltinBashVerifyUsesSha256sum(t *testing.T) {
	// verify_output doesn't hash today, but assert no Get-FileHash leaks.
	script, err := renderBuiltinBash(t, "verify_output")
	if err != nil {
		t.Fatalf("render verify_output: %v", err)
	}
	if strings.Contains(script, "Get-FileHash") {
		t.Error("verify_output bash port must use sha256sum, not Get-FileHash")
	}
}

// TestBuiltinBashCrc32UsesCksumOrPython: the crc32_rename template must use
// a Linux CRC32 tool (POSIX cksum, which computes CRC32 with the standard
// reflected polynomial, or python as a fallback). The PS template uses
// Add-Type C# — that must NOT leak into the bash output.
func TestBuiltinBashCrc32UsesCksumOrPython(t *testing.T) {
	script, err := renderBuiltinBash(t, "crc32_rename")
	if err != nil {
		t.Fatalf("render crc32_rename: %v", err)
	}
	if !strings.Contains(script, "cksum") && !strings.Contains(script, "python") {
		t.Error("crc32_rename bash port must use cksum or python for CRC32")
	}
	if strings.Contains(script, "Add-Type") || strings.Contains(script, "EncodeCrc32") {
		t.Error("crc32_rename bash port must not use the PS Add-Type C# CRC32 path")
	}
}
