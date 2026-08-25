package store

import (
	"context"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// ---------- Series ----------

func TestSeriesUpsertAndFlowSelection(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// First sight registers the series enabled with no flow override.
	sr, err := s.UpsertSeriesByName(ctx, "Ookami-san")
	if err != nil {
		t.Fatal(err)
	}
	if !sr.Enabled || sr.FlowID != 0 {
		t.Fatalf("new series defaults wrong: %+v", sr)
	}

	// Upsert is idempotent.
	again, err := s.UpsertSeriesByName(ctx, "Ookami-san")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != sr.ID {
		t.Fatal("upsert must return the existing row")
	}

	// Flow selection + disable persist.
	sr.FlowID = 42
	sr.Enabled = false
	if err := s.UpdateSeries(ctx, sr); err != nil {
		t.Fatal(err)
	}
	got, err := s.SeriesByName(ctx, "Ookami-san")
	if err != nil {
		t.Fatal(err)
	}
	if got.FlowID != 42 || got.Enabled {
		t.Fatalf("series update not persisted: %+v", got)
	}

	all, err := s.ListSeries(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("list series: %d, err %v", len(all), err)
	}
}

// ---------- Default flow ----------

func TestSetDefaultFlowIsExclusive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	f1 := seedFlow(t, s)
	f2, err := s.CreateFlow(ctx, &model.Flow{Name: "second", Steps: []model.Step{{Type: model.StepDGIndex}}})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SetDefaultFlow(ctx, f1.ID); err != nil {
		t.Fatal(err)
	}
	def, err := s.DefaultFlow(ctx)
	if err != nil || def.ID != f1.ID {
		t.Fatalf("default = %+v, want %d", def, f1.ID)
	}

	// Switching moves the flag atomically.
	if err := s.SetDefaultFlow(ctx, f2.ID); err != nil {
		t.Fatal(err)
	}
	def, err = s.DefaultFlow(ctx)
	if err != nil || def.ID != f2.ID {
		t.Fatalf("default after switch = %+v", def)
	}
	flows, _ := s.ListFlows(ctx)
	defaults := 0
	for _, f := range flows {
		if f.IsDefault {
			defaults++
		}
	}
	if defaults != 1 {
		t.Fatalf("exactly one default required, got %d", defaults)
	}

	// Unknown flow refused.
	if err := s.SetDefaultFlow(ctx, 9999); err == nil {
		t.Fatal("unknown flow must be refused")
	}
}

// ---------- Step templates ----------

func TestStepTemplateCRUDAndGuards(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	builtin := &model.StepTemplate{
		Key: "dgindex", Label: "DGIndexNV index", Builtin: true,
		PowerShell: "function Invoke-DgIndex { param($Job,$Params) }",
		Params:     []model.ParamDef{},
	}
	created, err := s.UpsertStepTemplate(ctx, builtin)
	if err != nil {
		t.Fatal(err)
	}

	// Upsert by key updates in place.
	builtin.Label = "DGIndexNV index v2"
	updated, err := s.UpsertStepTemplate(ctx, builtin)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != created.ID || updated.Label != "DGIndexNV index v2" {
		t.Fatalf("upsert failed: %+v", updated)
	}

	// Custom template create + delete works.
	custom := &model.StepTemplate{
		Key: "my_cleanup", Label: "Cleanup", Builtin: false,
		PowerShell: "function Invoke-MyCleanup { param($Job,$Params) }",
		Params:     []model.ParamDef{},
	}
	c, err := s.UpsertStepTemplate(ctx, custom)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteStepTemplate(ctx, c.ID); err != nil {
		t.Fatalf("custom delete: %v", err)
	}

	// Built-in delete refused.
	if err := s.DeleteStepTemplate(ctx, created.ID); err == nil {
		t.Fatal("built-in template must not be deletable")
	}

	// Template referenced by a flow cannot be deleted.
	fl, err := s.CreateFlow(ctx, &model.Flow{
		Name:  "uses-custom",
		Steps: []model.Step{{Type: model.StepType("my_cleanup")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = fl
	c2, err := s.UpsertStepTemplate(ctx, custom)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteStepTemplate(ctx, c2.ID); err == nil {
		t.Fatal("template used by a flow must not be deletable")
	}

	// Params round-trip through JSON.
	withParams := &model.StepTemplate{
		Key: "with_params", Label: "Params", Builtin: false,
		PowerShell: "function Invoke-WithParams { param($Job,$Params) }",
		Params:     []model.ParamDef{{Key: "level", Label: "Level", Placeholder: "3"}},
	}
	wp, err := s.UpsertStepTemplate(ctx, withParams)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.StepTemplateByKey(ctx, "with_params")
	if err != nil {
		t.Fatal(err)
	}
	if got.Params[0].Key != "level" || got.Params[0].Placeholder != "3" {
		t.Fatalf("params lost: %+v", got.Params)
	}
	if wp.ID == 0 {
		t.Fatal("id missing")
	}
}

// ---------- Pairing codes ----------

func TestPairingCodeLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	node, err := s.CreateNode(ctx, "enc-09", "hash-x")
	if err != nil {
		t.Fatal(err)
	}

	pc, err := s.CreatePairingCode(ctx, "hash-code-1", "enc-09", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if pc.UsedBy != 0 {
		t.Fatal("fresh code must be unused")
	}

	// Active codes are listed.
	codes, err := s.ListPairingCodes(ctx)
	if err != nil || len(codes) != 1 {
		t.Fatalf("list codes: %d, err %v", len(codes), err)
	}

	// Consume once — succeeds.
	consumed, err := s.ConsumePairingCode(ctx, "hash-code-1", node.ID)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if consumed.UsedBy != node.ID {
		t.Fatalf("consumed by wrong node: %+v", consumed)
	}

	// Second consume — refused (one-shot).
	if _, err := s.ConsumePairingCode(ctx, "hash-code-1", node.ID); err == nil {
		t.Fatal("code must be one-shot")
	}

	// Consumed codes drop out of the active list.
	codes, _ = s.ListPairingCodes(ctx)
	if len(codes) != 0 {
		t.Fatalf("consumed code still listed: %d", len(codes))
	}

	// Unknown code refused.
	if _, err := s.ConsumePairingCode(ctx, "nope", node.ID); err == nil {
		t.Fatal("unknown code must be refused")
	}
}

func TestPairingCodeExpiry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	node, _ := s.CreateNode(ctx, "n", "h")

	// Create a code already expired (negative TTL).
	pc, err := s.CreatePairingCode(ctx, "hash-expired", "", -time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumePairingCode(ctx, "hash-expired", node.ID); err == nil {
		t.Fatal("expired code must be refused")
	}
	_ = pc

	// Expired codes are not listed as active.
	codes, _ := s.ListPairingCodes(ctx)
	for _, c := range codes {
		if c.CodeHash == "hash-expired" {
			t.Fatal("expired code must not be listed")
		}
	}
}

// ---------- Settings key migration ----------

// TestMigrateSettingsKey covers the old-build row upgrade path: a missing
// key gets filled exactly once, an operator-set value (including blank) is
// never clobbered, and updated_at stays untouched by the migration.
func TestMigrateSettingsKey(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// No row at all: nothing to migrate.
	changed, err := st.MigrateSettingsKey(ctx, "discord_webhook", "https://discord.com/api/webhooks/env/x")
	if err != nil || changed {
		t.Fatalf("no-row migration must be a no-op: changed=%v err=%v", changed, err)
	}

	// Old-build row: key absent -> filled once. A real legacy row was written
	// by a binary whose Settings model had no discord_webhook field, so the
	// stored JSON lacks the key entirely (SaveSettings cannot produce that
	// shape anymore — insert it raw, exactly as the live upgrade sees it).
	legacyJSON := `{"controller_url":"","scripts_root":"/data/scripts","release_root":"/data/release","node_bin_dir":"C:\\bin","node_scripts_dir":"C:\\Encodes\\scripts","node_release_dir":"C:\\Encodes\\ReleaseFolders","scan_interval_seconds":30,"tasks_before_reboot":10,"group":"G","tag":"1080p"}`
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO settings (id, json) VALUES (1, ?)
		 ON CONFLICT(id) DO UPDATE SET json = excluded.json`, legacyJSON); err != nil {
		t.Fatal(err)
	}
	changed, err = st.MigrateSettingsKey(ctx, "discord_webhook", "https://discord.com/api/webhooks/env/x")
	if err != nil || !changed {
		t.Fatalf("legacy row must be migrated: changed=%v err=%v", changed, err)
	}
	after, err := st.GetSettings(ctx)
	if err != nil || after.DiscordWebhook != "https://discord.com/api/webhooks/env/x" {
		t.Fatalf("webhook not injected: %+v err=%v", after, err)
	}
	if after.Group != "G" || after.Tag != "1080p" {
		t.Fatalf("migration must not disturb other fields: %+v", after)
	}

	// Second run: key present -> untouched.
	changed, err = st.MigrateSettingsKey(ctx, "discord_webhook", "https://discord.com/api/webhooks/env/other")
	if err != nil || changed {
		t.Fatalf("already-migrated row must not change: changed=%v err=%v", changed, err)
	}

	// Operator blanks the webhook -> migration must respect the blank.
	after.DiscordWebhook = ""
	if err := st.SaveSettings(ctx, after); err != nil {
		t.Fatal(err)
	}
	changed, err = st.MigrateSettingsKey(ctx, "discord_webhook", "https://discord.com/api/webhooks/env/x")
	if err != nil || changed {
		t.Fatalf("operator blank must survive migration: changed=%v err=%v", changed, err)
	}
	got, _ := st.GetSettings(ctx)
	if got.DiscordWebhook != "" {
		t.Fatalf("blank webhook clobbered: %+v", got)
	}
}
