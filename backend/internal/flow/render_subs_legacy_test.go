package flow

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// PS 5.1 legacy argument-passing regressions (P6 review Q1a/Q13).
//
// Windows PowerShell 5.1 (the agent's fallback interpreter on Windows
// nodes) serializes native arguments with the LEGACY algorithm: wrap in
// quotes WITHOUT escaping embedded quotes/backslashes. An untrusted
// mkvmerge track_name like 'x" --attach-file C:\secret y" z' breaks out of
// its argument and injects mkvmerge flags (arbitrary file attachment =
// exfiltration into the released MKV). EncodeLib.Invoke-Tool pre-escapes
// every argument with MSVCRT rules when Test-LegacyNativeArgumentPassing
// reports the legacy serializer (PS < 7, or the test seam overridden).
//
// Two layers of proof:
//  1. Oracle: ConvertTo-LegacyNativeArgument's output, fed through the
//     validated CommandLineToArgvW simulator (testdata/winsim.py, itself
//     validated against live pwsh 7.4.6 Legacy passing), must reconstruct
//     the ORIGINAL argv exactly for 15 hostile shapes.
//  2. Live full flow: the whole default-subs flow runs under REAL pwsh with
//     $PSNativeCommandArgumentPassing='Legacy' and the seam forced true —
//     the hostile names must arrive at the (argv-logging) mkvmerge stub as
//     single intact arguments with no injected flags.

// legacyHostileShapes are the 15 argument shapes the reviewer probe
// validated against live pwsh Legacy passing: empty, plain, whitespace,
// quote-break flag injection, quote-break output redirect, backslash
// variants (single/double/trailing/quote-adjacent), lone quote, newline,
// tab and unit separator.
var legacyHostileShapes = []string{
	"",
	"plain",
	"with space",
	`x" --attach-file C:\secret y" z`,
	`x" --output "C:\pwned.mkv`,
	`a\b`,
	`a\\b`,
	`trail\`,
	`trail\\`,
	`q"\`,
	`"`,
	`\"`,
	"line1\nline2",
	"tab\there",
	"a\x1ftrue\x1fyes",
}

// TestLegacyNativeArgumentOracle checks the escaper against the winsim
// oracle: legacy serialization of the ESCAPED args, parsed by
// CommandLineToArgvW, must reproduce the ORIGINAL argv exactly.
func TestLegacyNativeArgumentOracle(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not installed; skipping legacy argument oracle test")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed; skipping winsim oracle test")
	}
	libPath, err := filepath.Abs("../../../powershell/EncodeLib.ps1")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	origPath := filepath.Join(dir, "orig.json")
	escapedPath := filepath.Join(dir, "escaped.json")
	orig, err := json.Marshal(legacyHostileShapes)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(origPath, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	psQuote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	ps := ". " + psQuote(libPath) + "\n" +
		"$shapes = Get-Content -Raw " + psQuote(origPath) + " | ConvertFrom-Json\n" +
		"$esc = @($shapes | ForEach-Object { ConvertTo-LegacyNativeArgument -Argument $_ })\n" +
		"ConvertTo-Json -InputObject @($esc) -Depth 3 -Compress\n"
	cmd := exec.Command(pwsh, "-NoProfile", "-Command", ps)
	cmd.Env = append(os.Environ(), "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pwsh escaper run failed: %v\n%s", err, out)
	}
	var escaped []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &escaped); err != nil {
		t.Fatalf("escaper output is not a JSON array: %v\n%s", err, out)
	}
	if len(escaped) != len(legacyHostileShapes) {
		t.Fatalf("escaper returned %d args, want %d", len(escaped), len(legacyHostileShapes))
	}
	escJSON, err := json.Marshal(escaped)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(escapedPath, escJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	oracle := exec.Command(python, "testdata/winsim.py", "--check", origPath, escapedPath)
	oracleOut, err := oracle.CombinedOutput()
	if err != nil {
		t.Fatalf("winsim oracle rejected the escaping: %v\n%s", err, oracleOut)
	}
	if !strings.Contains(string(oracleOut), "WINSIM-OK") {
		t.Fatalf("winsim oracle did not confirm the round-trip:\n%s", oracleOut)
	}
	t.Logf("oracle: %s", strings.TrimSpace(string(oracleOut)))
}

// TestSubsFlowLegacyNativePassing runs the FULL default-subs flow under
// live pwsh with Legacy argument passing forced (the 5.1 serializer) and
// hostile track_names in the identify fixture. The argv-logging mkvmerge
// stub proves the names arrive intact as single arguments and no flag
// injection happened (exactly one --attach-file: the legitimate font).
func TestSubsFlowLegacyNativePassing(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not installed; skipping live Legacy full-flow test")
	}
	// Hostile identify: quote-break flag injection + backslash/quote mix.
	// Neither name carries C0 controls, so sanitization passes them through
	// unchanged — the escaping layer is what must neutralize them.
	injName := `x" --attach-file /etc/passwd y" z`
	bsName := `back\slash "quote" trail\`
	tc := subsCase{
		step: "mux_extras",
		identify: subsIdentify(
			append(subsVideoAudio(),
				subsTrack(2, "S_TEXT/UTF8", "eng", injName, false, false),
				subsTrack(3, "S_TEXT/ASS", "jpn", bsName, true, false)),
			[]any{map[string]any{"id": 10, "file_name": "OpenSans-Regular.ttf", "content_type": "font/ttf"}},
			[]any{map[string]any{"num_entries": 4}},
		),
	}
	v, epDir, pathEnv := subsSetup(t, tc, ".exe")
	j := &model.Job{ID: 83, Series: subsSeries, Episode: "01",
		EpisodeDir: subsSeries + "/Ep 01", ScriptType: "vpy"}
	script, err := RenderDialect(DefaultSubsFlow(), j, v, BuiltinResolver(), PowerShellDialect{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	scriptPath := filepath.Join(epDir, "job.ps1")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	// Modified lib copy: force the legacy-escaping seam ON under pwsh 7
	// (production decides via PSVersion.Major < 7). Appending a redefinition
	// after the dot-sourced original overrides it — the documented test seam.
	libRaw, err := os.ReadFile("../../../powershell/EncodeLib.ps1")
	if err != nil {
		t.Fatal(err)
	}
	libCopy := filepath.Join(t.TempDir(), "EncodeLib-legacy-test.ps1")
	libForced := string(libRaw) + "\n# TEST OVERRIDE: force legacy argument escaping under pwsh 7.\nfunction Test-LegacyNativeArgumentPassing { return $true }\n"
	if err := os.WriteFile(libCopy, []byte(libForced), 0o644); err != nil {
		t.Fatal(err)
	}
	psQuote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	// -Command (not -File) so $PSNativeCommandArgumentPassing='Legacy' is
	// set in the parent scope of the script — the exact 5.1 serializer.
	psCmd := "$PSNativeCommandArgumentPassing = 'Legacy'; & " + psQuote(scriptPath) + " -LibPath " + psQuote(libCopy)
	cmd := exec.Command(pwsh, "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", psCmd)
	cmd.Env = append(os.Environ(), "PATH="+pathEnv, "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1")
	out, runErr := cmd.CombinedOutput()
	text := string(out)
	t.Logf("legacy full-flow output:\n%s", text)
	if runErr != nil {
		t.Fatalf("legacy full flow failed: %v\n%s", runErr, text)
	}
	for _, marker := range []string{
		"ENCODE_STEP subtitle_extract 100",
		"ENCODE_STEP font_extract 100",
		"ENCODE_STEP chapter_extract 100",
		"ENCODE_STEP mux_extras 100",
		"ENCODE_JOB_DONE",
	} {
		if !strings.Contains(text, marker) {
			t.Errorf("missing marker %q\n%s", marker, text)
		}
	}
	if strings.Contains(text, "command not found") {
		t.Fatalf("linkage broken:\n%s", text)
	}
	// Exact argv proof (NUL log survives any argument content).
	args := subsReadNulArgs(t, filepath.Join(v.BinDir, "mkvmerge_args.nul"))
	wantNames := []string{"0:" + injName, "0:" + bsName}
	var gotNames, gotAttach []string
	for i, a := range args {
		switch a {
		case "--track-name":
			if i+1 < len(args) {
				gotNames = append(gotNames, args[i+1])
			}
		case "--attach-file":
			if i+1 < len(args) {
				gotAttach = append(gotAttach, args[i+1])
			}
		}
		if a == "/etc/passwd" {
			t.Errorf("injected /etc/passwd argument reached mkvmerge: %q", args)
		}
	}
	if len(gotNames) != len(wantNames) {
		t.Fatalf("track-name argv count = %d, want %d\nargv: %q", len(gotNames), len(wantNames), args)
	}
	for i := range wantNames {
		if gotNames[i] != wantNames[i] {
			t.Errorf("track-name argv[%d] = %q, want %q (legacy escaping broke the argument)", i, gotNames[i], wantNames[i])
		}
	}
	if len(gotAttach) != 1 || gotAttach[0] != filepath.Join(epDir, "fonts", "10_OpenSans-Regular.ttf") {
		t.Errorf("--attach-file argv = %q, want exactly the legitimate font (flag injection!)", gotAttach)
	}
	// The muxed output must not carry exfiltrated content.
	raw, err := os.ReadFile(filepath.Join(epDir, subsOutputName()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "MUXED-EXTRAS") {
		t.Errorf("output was not muxed: %q", raw)
	}
	if strings.Contains(string(raw), "--- input: /etc/passwd") || strings.Contains(string(raw), "root:") {
		t.Errorf("exfiltrated file content found in the muxed output: %q", raw)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(epDir, ".mux_extras.*.tmp.mkv")); len(leftovers) > 0 {
		t.Errorf("temp leftovers: %v", leftovers)
	}
}
