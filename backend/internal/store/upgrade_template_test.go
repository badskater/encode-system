package store

import (
	"context"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestUpgradeStepTemplateIfFactory proves the guarded factory upgrade:
// untouched factory copies get replaced, user-edited copies never do.
func TestUpgradeStepTemplateIfFactory(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir + "/test.db")
	if err != nil {
		t.Fatal("open store:", err)
	}
	defer st.Close()
	ctx := context.Background()

	factoryV1 := "function Invoke-Mux { # v1 }"
	factoryV2 := "function Invoke-Mux { # v2 }"

	// Seed the v1 factory template.
	if _, err := st.InsertStepTemplateIfAbsent(ctx, &model.StepTemplate{
		Key: "mux", Label: "MKV mux v1", Description: "d1", PowerShell: factoryV1, Builtin: true,
	}); err != nil {
		t.Fatal("seed:", err)
	}

	// Upgrade applies: stored script equals the old factory version.
	upgraded, err := st.UpgradeStepTemplateIfFactory(ctx, "mux", factoryV1, &model.StepTemplate{
		Key: "mux", Label: "MKV mux v2", Description: "d2", PowerShell: factoryV2, Builtin: true,
	})
	if err != nil {
		t.Fatal("upgrade:", err)
	}
	if !upgraded {
		t.Fatal("upgrade should have applied to the untouched factory template")
	}
	got, err := st.StepTemplateByKey(ctx, "mux")
	if err != nil {
		t.Fatal(err)
	}
	if got.PowerShell != factoryV2 || got.Label != "MKV mux v2" {
		t.Fatalf("template not upgraded: label=%q ps=%q", got.Label, got.PowerShell)
	}

	// Simulate a user edit on the v2 template.
	got.PowerShell = factoryV2 + "\n# user customization"
	if _, err := st.UpsertStepTemplate(ctx, got); err != nil {
		t.Fatal("user edit:", err)
	}

	// Second upgrade attempt must NOT overwrite the user's customization.
	upgraded, err = st.UpgradeStepTemplateIfFactory(ctx, "mux", factoryV2, &model.StepTemplate{
		Key: "mux", Label: "MKV mux v3", Description: "d3", PowerShell: "function Invoke-Mux { # v3 }", Builtin: true,
	})
	if err != nil {
		t.Fatal("upgrade 2:", err)
	}
	if upgraded {
		t.Fatal("upgrade must not apply to a user-edited template")
	}
	got, err = st.StepTemplateByKey(ctx, "mux")
	if err != nil {
		t.Fatal(err)
	}
	if got.PowerShell != factoryV2+"\n# user customization" || got.Label != "MKV mux v2" {
		t.Fatalf("user customization was clobbered: label=%q ps=%q", got.Label, got.PowerShell)
	}
}
