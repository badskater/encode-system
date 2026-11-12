package api

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/badskater/encode-system/backend/internal/flow"
	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/store"
	"github.com/badskater/encode-system/backend/internal/update"
)

// quality_gate factory-guard tests (P5).
//
// Seeding is name-guarded (seedDefaultFlow): a flow that already exists —
// factory or user-edited — is NEVER touched. Adding quality_gate to the
// factory flows therefore only affects NEW installs. These tests prove both
// halves: fresh stores get the gate immediately after verify_output, and
// pre-existing flows survive a restart byte-for-byte without it.

// seedTestServer boots a Server (which runs seedStepTemplates +
// seedDefaultFlow) over the given store with a minimal config.
func seedTestServer(t *testing.T, st *store.Store) *Server {
	t.Helper()
	dir := t.TempDir()
	up, err := update.NewStore(dir + "/updates")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		AdminUsername: "admin", AdminPassword: "test-admin-password",
		ScriptsRoot: dir + "/scripts", ReleaseRoot: dir + "/release",
		NodeBinDir: `C:\bin`, NodeScriptsDir: `C:\Encodes\scripts`, NodeReleaseDir: `C:\Encodes\ReleaseFolders`,
		Group: "TestGroup", Tag: "1080p",
	}
	s, err := New(st, up, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// assertGateAfterVerify checks the quality_gate position contract on a flow.
func assertGateAfterVerify(t *testing.T, f *model.Flow) {
	t.Helper()
	idx := -1
	for i, st := range f.Steps {
		if st.TemplateKey() == "quality_gate" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("flow %q: quality_gate missing (steps: %v)", f.Name, f.Steps)
	}
	if idx == 0 || f.Steps[idx-1].TemplateKey() != "verify_output" {
		t.Fatalf("flow %q: quality_gate must immediately follow verify_output (steps: %v)", f.Name, f.Steps)
	}
}

// TestSeedFreshInstallsGetQualityGate: a brand-new store boots with the
// quality_gate step seeded after verify_output in ALL three factory flows,
// and the quality_gate step template present in the catalog.
func TestSeedFreshInstallsGetQualityGate(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	seedTestServer(t, st)

	ctx := context.Background()
	for _, name := range []string{"default-1080", "default-4k", "default-4k-cpu"} {
		f, err := st.FlowByName(ctx, name)
		if err != nil {
			t.Fatalf("flow %q not seeded: %v", name, err)
		}
		assertGateAfterVerify(t, f)
	}
	tpl, err := st.StepTemplateByKey(ctx, "quality_gate")
	if err != nil || tpl == nil {
		t.Fatalf("quality_gate step template not seeded: %v", err)
	}
	if !tpl.Builtin {
		t.Error("quality_gate template must be builtin")
	}
}

// TestSeedNeverModifiesExistingFlows: an existing install (flows seeded by
// an OLDER factory without quality_gate, plus a user-edited flow) restarts
// on the new build. The stored flows must come back byte-for-byte unchanged
// — no quality_gate injected — while the new builtin template still appears
// in the catalog for manual use.
func TestSeedNeverModifiesExistingFlows(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()

	// Pre-seed LEGACY versions of the factory flows: the current factory
	// step lists minus quality_gate (exactly what an older build stored).
	legacy := map[string]*model.Flow{}
	for _, factory := range []*model.Flow{flow.DefaultFlow(), flow.Default4kFlow(), flow.Default4kCPUFlow()} {
		l := &model.Flow{Name: factory.Name}
		for _, step := range factory.Steps {
			if step.TemplateKey() == "quality_gate" {
				continue
			}
			l.Steps = append(l.Steps, step)
		}
		if _, err := st.CreateFlow(ctx, l); err != nil {
			t.Fatal(err)
		}
		legacy[l.Name] = l
	}
	// A user-edited flow with a custom step order must also survive.
	userFlow := &model.Flow{Name: "my-custom-flow", Steps: []model.Step{
		{Type: model.StepSourceRename, Params: map[string]string{"source_name": "raw"}},
		{Type: model.StepMux},
		{Type: model.StepReleaseCopy},
	}}
	if _, err := st.CreateFlow(ctx, userFlow); err != nil {
		t.Fatal(err)
	}

	// Restart: boot the new build over the existing store.
	seedTestServer(t, st)

	for name, before := range legacy {
		after, err := st.FlowByName(ctx, name)
		if err != nil {
			t.Fatalf("flow %q vanished: %v", name, err)
		}
		if len(after.Steps) != len(before.Steps) {
			t.Fatalf("flow %q: step count changed on restart (%d -> %d) — existing flows must NOT be modified",
				name, len(before.Steps), len(after.Steps))
		}
		for i := range before.Steps {
			if after.Steps[i].TemplateKey() != before.Steps[i].TemplateKey() {
				t.Fatalf("flow %q step %d changed: %q -> %q", name, i,
					before.Steps[i].TemplateKey(), after.Steps[i].TemplateKey())
			}
			if after.Steps[i].TemplateKey() == "quality_gate" {
				t.Fatalf("flow %q: quality_gate was injected into an existing flow", name)
			}
		}
	}
	after, err := st.FlowByName(ctx, "my-custom-flow")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Steps) != 3 || after.Steps[0].Params["source_name"] != "raw" {
		t.Fatalf("user flow modified on restart: %+v", after.Steps)
	}

	// The new builtin template IS available on existing installs (catalog
	// seeding is insert-if-absent) — operators can add it manually.
	if _, err := st.StepTemplateByKey(ctx, "quality_gate"); err != nil {
		t.Fatalf("quality_gate template must be seeded even on existing installs: %v", err)
	}
}
