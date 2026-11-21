package store

import (
	"context"
	"testing"
)

// Series script assignment (phase 1 of series-scripts-scene-zones): a series
// carries script_type + script_template so the scanner can materialize the
// template into detected episodes. Fields must round-trip on every read path
// and survive the field-scoped setter without clobbering other columns.

func TestSeriesScriptDefaultsEmpty(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sr, err := s.UpsertSeriesByName(ctx, "Script Show")
	if err != nil {
		t.Fatal(err)
	}
	if sr.ScriptType != "" || sr.ScriptTemplate != "" {
		t.Fatalf("new series must default to no script assignment, got type=%q template=%q", sr.ScriptType, sr.ScriptTemplate)
	}
}

func TestSeriesScriptSetterRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sr, err := s.UpsertSeriesByName(ctx, "Script Show")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSeriesScript(ctx, sr.ID, "vpy", "1080.vpy"); err != nil {
		t.Fatal(err)
	}
	// Every read path must carry the fields.
	byID, err := s.GetSeries(ctx, sr.ID)
	if err != nil {
		t.Fatal(err)
	}
	byName, err := s.SeriesByName(ctx, "Script Show")
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSeries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var listed *struct{ typ, tmpl string }
	for _, x := range list {
		if x.ID == sr.ID {
			listed = &struct{ typ, tmpl string }{x.ScriptType, x.ScriptTemplate}
		}
	}
	if listed == nil {
		t.Fatal("series missing from ListSeries")
	}
	for label, got := range map[string][2]string{
		"GetSeries":    {byID.ScriptType, byID.ScriptTemplate},
		"SeriesByName": {byName.ScriptType, byName.ScriptTemplate},
		"ListSeries":   {listed.typ, listed.tmpl},
	} {
		if got[0] != "vpy" || got[1] != "1080.vpy" {
			t.Errorf("%s round-trip wrong: type=%q template=%q", label, got[0], got[1])
		}
	}
	// Clearing restores the unassigned state.
	if err := s.SetSeriesScript(ctx, sr.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	cleared, err := s.GetSeries(ctx, sr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.ScriptType != "" || cleared.ScriptTemplate != "" {
		t.Fatalf("clear must reset both fields, got %+v", cleared)
	}
}

func TestSeriesScriptSetterIsFieldScoped(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sr, err := s.UpsertSeriesByName(ctx, "Script Show")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSeriesFlow(ctx, sr.ID, 7); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSeriesTag(ctx, sr.ID, "1080"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSeriesScript(ctx, sr.ID, "avs", "2160.avs"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSeries(ctx, sr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FlowID != 7 || got.Tag != "1080" {
		t.Fatalf("script setter clobbered other fields: %+v", got)
	}
	if got.ScriptType != "avs" || got.ScriptTemplate != "2160.avs" {
		t.Fatalf("script fields not persisted: %+v", got)
	}
}

func TestSeriesUpdatePersistsScriptFields(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sr, err := s.UpsertSeriesByName(ctx, "Script Show")
	if err != nil {
		t.Fatal(err)
	}
	sr.ScriptType = "vpy"
	sr.ScriptTemplate = "1080.vpy"
	if err := s.UpdateSeries(ctx, sr); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSeries(ctx, sr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ScriptType != "vpy" || got.ScriptTemplate != "1080.vpy" {
		t.Fatalf("UpdateSeries must persist script fields, got %+v", got)
	}
}
