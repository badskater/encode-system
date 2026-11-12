package flow

import (
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// P6 render contracts: the extraction/mux builtins must carry the exact
// mkvtoolnix syntax (verified against mkvtoolnix.download/doc) in BOTH
// dialects, the artifact names, the identity guards and the atomic
// replace. Runtime behavior is proven by render_subs_e2e_test.go.

// TestSubsBuiltinsParamDefs pins the declared params (the UI surfaces
// builtins through ParamDefs alone — the frontend fixture mirrors these).
func TestSubsBuiltinsParamDefs(t *testing.T) {
	byKey := map[string]*model.StepTemplate{}
	for _, tpl := range BuiltinStepTemplates() {
		byKey[tpl.Key] = tpl
	}
	for _, key := range []string{"subtitle_extract", "font_extract", "chapter_extract"} {
		tpl, ok := byKey[key]
		if !ok {
			t.Fatalf("builtin %q not registered", key)
		}
		if len(tpl.Params) != 0 {
			t.Errorf("%s must declare no params (got %d)", key, len(tpl.Params))
		}
		if !tpl.Builtin {
			t.Errorf("%s must be builtin", key)
		}
	}
	mux, ok := byKey["mux_extras"]
	if !ok {
		t.Fatal("builtin mux_extras not registered")
	}
	if len(mux.Params) != 1 || mux.Params[0].Key != "output" {
		t.Fatalf("mux_extras must declare exactly the 'output' param (got %+v)", mux.Params)
	}
}

// TestSubsBuiltinsRenderContracts pins the tool syntax and safety guards
// in both dialects. A drift here means the rendered job would call
// mkvtoolnix with wrong arguments or lose a safety property.
func TestSubsBuiltinsRenderContracts(t *testing.T) {
	extractKeys := []string{"subtitle_extract", "font_extract", "chapter_extract"}
	for _, key := range extractKeys {
		key := key
		t.Run(key, func(t *testing.T) {
			ps := renderBuiltinPS(t, key)
			bash, err := renderBuiltinBash(t, key)
			if err != nil {
				t.Fatalf("bash render: %v", err)
			}
			// Tool resolution + package-naming error.
			for _, want := range []string{"mkvmerge", "mkvextract", "mkvtoolnix"} {
				if !strings.Contains(ps, want) {
					t.Errorf("PS render missing %q", want)
				}
				if !strings.Contains(bash, want) {
					t.Errorf("bash render missing %q", want)
				}
			}
			// Identify via mkvmerge -J on the discovered source.
			if !strings.Contains(ps, "-J $src") {
				t.Error("PS must identify the source with mkvmerge -J")
			}
			if !strings.Contains(bash, `-J "$src"`) {
				t.Error("bash must identify the source with mkvmerge -J")
			}
			// Graceful skip path (success marker, not a failure).
			if !strings.Contains(ps, "- skipping") || !strings.Contains(bash, "- skipping") {
				t.Error("both dialects must carry the graceful-skip log line")
			}
		})
	}
	t.Run("subtitle_extract", func(t *testing.T) {
		ps := renderBuiltinPS(t, "subtitle_extract")
		bash, _ := renderBuiltinBash(t, "subtitle_extract")
		// mkvextract syntax: source FIRST, then the tracks mode.
		if !strings.Contains(ps, "@($src, 'tracks')") {
			t.Error("PS must call mkvextract <source> tracks <specs>")
		}
		if !strings.Contains(bash, `extract_args=("$src" "tracks")`) {
			t.Error("bash must call mkvextract <source> tracks <specs>")
		}
		for _, want := range []string{"subs.json", "subs_", "S_TEXT/UTF8", "S_TEXT/ASS", "S_HDMV/PGS"} {
			if !strings.Contains(ps, want) || !strings.Contains(bash, want) {
				t.Errorf("both dialects must carry %q (artifact/codec contract)", want)
			}
		}
	})
	t.Run("font_extract", func(t *testing.T) {
		ps := renderBuiltinPS(t, "font_extract")
		bash, _ := renderBuiltinBash(t, "font_extract")
		if !strings.Contains(ps, "@($src, 'attachments')") {
			t.Error("PS must call mkvextract <source> attachments <specs>")
		}
		if !strings.Contains(bash, `extract_args=("$src" "attachments")`) {
			t.Error("bash must call mkvextract <source> attachments <specs>")
		}
		for _, want := range []string{"fonts.json", "application/x-truetype-font", "application/vnd.ms-opentype", "content_type", "mime_type"} {
			if !strings.Contains(ps, want) || !strings.Contains(bash, want) {
				t.Errorf("both dialects must carry %q (font detection contract)", want)
			}
		}
	})
	t.Run("chapter_extract", func(t *testing.T) {
		ps := renderBuiltinPS(t, "chapter_extract")
		bash, _ := renderBuiltinBash(t, "chapter_extract")
		// mkvextract chapters <source> <dest> (XML is the default format).
		if !strings.Contains(ps, "@($src, 'chapters', $dest)") {
			t.Error("PS must call mkvextract <source> chapters <dest>")
		}
		if !strings.Contains(bash, `"$mkvextract" "$src" chapters "$dest"`) {
			t.Error("bash must call mkvextract <source> chapters <dest>")
		}
		if !strings.Contains(ps, "chapters.xml") || !strings.Contains(bash, "chapters.xml") {
			t.Error("both dialects must write chapters.xml")
		}
	})
	t.Run("mux_extras", func(t *testing.T) {
		ps := renderBuiltinPS(t, "mux_extras")
		bash, err := renderBuiltinBash(t, "mux_extras")
		if err != nil {
			t.Fatalf("bash render: %v", err)
		}
		// mkvmerge argument contract (documented option names).
		for _, want := range []string{
			"--no-subtitles", "--no-chapters", "--no-attachments",
			"--no-video", "--no-audio",
			"--default-track-flag", "--forced-display-flag",
			"--chapters", "--attach-file", "--language", "--track-name",
		} {
			if !strings.Contains(ps, want) {
				t.Errorf("PS render missing mkvmerge option %q", want)
			}
			if !strings.Contains(bash, want) {
				t.Errorf("bash render missing mkvmerge option %q", want)
			}
		}
		// Artifact contracts + temp name.
		for _, want := range []string{"subs.json", "fonts.json", "chapters.xml", MuxExtrasTempName} {
			if !strings.Contains(ps, want) || !strings.Contains(bash, want) {
				t.Errorf("both dialects must carry %q", want)
			}
		}
		// Identity guard (never mux into the source) + atomic replace.
		if !strings.Contains(ps, "Test-SameFile") {
			t.Error("PS must guard output discovery with Test-SameFile")
		}
		if !strings.Contains(bash, "-ef") {
			t.Error("bash must guard output discovery with -ef")
		}
		if !strings.Contains(ps, "Move-Item") {
			t.Error("PS must replace the output atomically (Move-Item)")
		}
		if !strings.Contains(bash, "mv -f") {
			t.Error("bash must replace the output atomically (mv -f)")
		}
		// No-extras success path must not invoke mkvmerge.
		if !strings.Contains(ps, "output unchanged") || !strings.Contains(bash, "output unchanged") {
			t.Error("both dialects must document the no-extras no-op path")
		}
	})
}

// TestDefaultSubsFlowShape: the seeded flow is exactly the four extraction/
// mux steps in order, and it renders in both dialects (no refused steps).
func TestDefaultSubsFlowShape(t *testing.T) {
	f := DefaultSubsFlow()
	if f.Name != "default-subs" {
		t.Fatalf("flow name = %q, want default-subs", f.Name)
	}
	want := []string{"subtitle_extract", "font_extract", "chapter_extract", "mux_extras"}
	if len(f.Steps) != len(want) {
		t.Fatalf("step count = %d, want %d", len(f.Steps), len(want))
	}
	for i, key := range want {
		if f.Steps[i].TemplateKey() != key {
			t.Errorf("step %d = %q, want %q", i, f.Steps[i].TemplateKey(), key)
		}
	}
	if err := ValidateForRender(f, BuiltinResolver()); err != nil {
		t.Fatalf("ValidateForRender: %v", err)
	}
	for _, d := range []Dialect{PowerShellDialect{}, BashDialect{}} {
		if _, err := RenderDialect(f, linuxTestJob(), linuxTestVars(), BuiltinResolver(), d); err != nil {
			t.Errorf("render %T: %v", d, err)
		}
	}
}
