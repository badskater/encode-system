package api

import (
	"context"
	"testing"

	"github.com/badskater/encode-system/backend/internal/flow"
	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/store"
)

// Live-farm regression (v1.25.0 → v1.25.1 upgrade): builtins seed
// insert-if-absent, so a release that FIXES a builtin template must carry a
// guarded factory upgrade chain — otherwise upgraded installs keep running
// the buggy seeded script forever (job 30/31 kept failing after the 1.25.1
// controller upgrade until a manual /reset). These tests prove the
// scene_detect chain upgrades untouched V1/V1a factory copies and NEVER
// touches user-edited scripts.
func TestSeedUpgradesSceneDetectFactoryCopies(t *testing.T) {
	for _, tc := range []struct {
		name string
		old  string
	}{
		{"v1.25.0 factory (StrictMode-unsafe)", flow.SceneDetectFactoryV1},
		{"v1.25.1 factory (unescaped filtergraph)", flow.SceneDetectFactoryV1a},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			st, err := store.Open(dir + "/test.db")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			if _, err := st.InsertStepTemplateIfAbsent(context.Background(), &model.StepTemplate{
				Key: "scene_detect", Builtin: true, PowerShell: tc.old,
			}); err != nil {
				t.Fatal(err)
			}
			seedTestServer(t, st) // New() runs seedStepTemplates
			got, err := st.StepTemplateByKey(context.Background(), "scene_detect")
			if err != nil {
				t.Fatal(err)
			}
			if got.PowerShell != flow.SceneDetectTemplate().PowerShell {
				t.Fatalf("untouched factory copy must upgrade to the current script (len %d vs %d)", len(got.PowerShell), len(flow.SceneDetectTemplate().PowerShell))
			}
		})
	}
}

func TestSeedPreservesUserEditedSceneDetect(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	edited := flow.SceneDetectFactoryV1 + "\n# my custom tweak\n"
	if _, err := st.InsertStepTemplateIfAbsent(context.Background(), &model.StepTemplate{
		Key: "scene_detect", Builtin: true, PowerShell: edited,
	}); err != nil {
		t.Fatal(err)
	}
	seedTestServer(t, st)
	got, err := st.StepTemplateByKey(context.Background(), "scene_detect")
	if err != nil {
		t.Fatal(err)
	}
	if got.PowerShell != edited {
		t.Fatal("user-edited scene_detect must survive seeding byte-for-byte")
	}
}
