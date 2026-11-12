package api

import (
	"context"
	"reflect"
	"testing"

	"github.com/badskater/encode-system/backend/internal/flow"
	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/store"
)

// default-subs seeding guards (P6).
//
// The extras flow is seeded name-guarded on FRESH installs only, exactly
// like the encode defaults: an existing flow — factory or user-edited,
// even one already NAMED default-subs — is never touched, and the new
// builtins never leak into existing encode flows. The default-flow flag
// must stay on the configured encode default, never on default-subs.

// TestSeedFreshInstallsGetSubsFlow: a brand-new store boots with the
// default-subs flow (exactly the four extraction/mux steps in order), the
// four builtin templates in the catalog, and the default flag still on
// default-1080 (the extras flow is queued manually, never auto-default).
func TestSeedFreshInstallsGetSubsFlow(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	seedTestServer(t, st)

	ctx := context.Background()
	f, err := st.FlowByName(ctx, "default-subs")
	if err != nil {
		t.Fatalf("default-subs not seeded: %v", err)
	}
	want := []string{"subtitle_extract", "font_extract", "chapter_extract", "mux_extras"}
	if len(f.Steps) != len(want) {
		t.Fatalf("default-subs step count = %d, want %d (%+v)", len(f.Steps), len(want), f.Steps)
	}
	for i, key := range want {
		if f.Steps[i].TemplateKey() != key {
			t.Errorf("default-subs step %d = %q, want %q", i, f.Steps[i].TemplateKey(), key)
		}
	}
	for _, key := range want {
		tpl, err := st.StepTemplateByKey(ctx, key)
		if err != nil || tpl == nil {
			t.Fatalf("builtin template %q not seeded: %v", key, err)
		}
		if !tpl.Builtin {
			t.Errorf("template %q must be builtin", key)
		}
	}
	def, err := st.DefaultFlow(ctx)
	if err != nil {
		t.Fatalf("no default flow marked: %v", err)
	}
	if def.Name != "default-1080" {
		t.Errorf("default flag = %q, want default-1080 (default-subs must never become the default)", def.Name)
	}
}

// TestSeedNeverModifiesExistingFlowsForSubs: an existing install restarts
// on the new build. Pre-existing factory flows and user flows come back
// byte-for-byte unchanged (no extraction steps injected), and a flow the
// operator already created UNDER THE NAME default-subs survives untouched
// (the name guard protects user flows, not just factory names).
func TestSeedNeverModifiesExistingFlowsForSubs(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()

	// Legacy factory flows (the current encode defaults — they carry no
	// extraction steps) plus a user-edited custom flow.
	legacy := map[string]*model.Flow{}
	for _, factory := range []*model.Flow{flow.DefaultFlow(), flow.Default4kFlow(), flow.Default4kCPUFlow()} {
		l := &model.Flow{Name: factory.Name, Steps: append([]model.Step{}, factory.Steps...)}
		if _, err := st.CreateFlow(ctx, l); err != nil {
			t.Fatal(err)
		}
		legacy[l.Name] = l
	}
	userFlow := &model.Flow{Name: "my-custom-flow", Steps: []model.Step{
		{Type: model.StepMux},
		{Type: model.StepReleaseCopy},
	}}
	if _, err := st.CreateFlow(ctx, userFlow); err != nil {
		t.Fatal(err)
	}
	// An operator-created flow that ALREADY owns the default-subs name:
	// the seed must not replace or extend it.
	userSubs := &model.Flow{Name: "default-subs", Steps: []model.Step{
		{Type: model.StepType("subtitle_extract")},
		{Type: model.StepType("mux_extras"), Params: map[string]string{"output": "custom.mkv"}},
	}}
	if _, err := st.CreateFlow(ctx, userSubs); err != nil {
		t.Fatal(err)
	}
	legacy["default-subs"] = userSubs
	legacy["my-custom-flow"] = userFlow

	// Restart: boot the new build over the existing store.
	seedTestServer(t, st)

	// The store round-trips empty param maps as nil (JSON omitempty) —
	// normalize both sides so DeepEqual compares semantics, not that
	// pre-existing serialization detail.
	normalize := func(steps []model.Step) []model.Step {
		out := append([]model.Step{}, steps...)
		for i := range out {
			if len(out[i].Params) == 0 {
				out[i].Params = nil
			}
		}
		return out
	}
	for name, before := range legacy {
		after, err := st.FlowByName(ctx, name)
		if err != nil {
			t.Fatalf("flow %q vanished: %v", name, err)
		}
		if !reflect.DeepEqual(normalize(before.Steps), normalize(after.Steps)) {
			t.Fatalf("flow %q modified on restart:\nbefore: %+v\nafter:  %+v", name, before.Steps, after.Steps)
		}
		for _, step := range after.Steps {
			switch step.TemplateKey() {
			case "font_extract", "chapter_extract":
				t.Fatalf("flow %q: extraction step %q injected into an existing flow", name, step.TemplateKey())
			}
		}
	}
	// The user's default-subs keeps its exact two steps (not the factory
	// four) — the name guard skipped seeding entirely.
	after, err := st.FlowByName(ctx, "default-subs")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Steps) != 2 || after.Steps[1].Params["output"] != "custom.mkv" {
		t.Fatalf("user default-subs flow replaced by the factory seed: %+v", after.Steps)
	}
	// The new builtin templates ARE available on existing installs
	// (catalog seeding is insert-if-absent) for manual flow building.
	for _, key := range []string{"subtitle_extract", "font_extract", "chapter_extract", "mux_extras"} {
		if _, err := st.StepTemplateByKey(ctx, key); err != nil {
			t.Fatalf("template %q must be seeded even on existing installs: %v", key, err)
		}
	}
}

// TestSeedSubsFlowRestartIdempotent: booting twice over a fresh store does
// not duplicate or alter the seeded default-subs flow.
func TestSeedSubsFlowRestartIdempotent(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	seedTestServer(t, st)
	ctx := context.Background()
	before, err := st.FlowByName(ctx, "default-subs")
	if err != nil {
		t.Fatal(err)
	}
	seedTestServer(t, st) // second boot
	after, err := st.FlowByName(ctx, "default-subs")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Steps, after.Steps) {
		t.Fatalf("default-subs changed across restarts:\nbefore: %+v\nafter:  %+v", before.Steps, after.Steps)
	}
	flows, err := st.ListFlows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, f := range flows {
		if f.Name == "default-subs" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("default-subs seeded %d times, want exactly 1", count)
	}
}
