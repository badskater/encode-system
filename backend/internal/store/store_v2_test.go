package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestMigrateV2Idempotent asserts the v2 migration is safe to run twice:
// opening the same DB file a second time must succeed and all v2 columns
// must be present and queryable. This pins the tolerant-ALTER pattern
// (isDuplicateColumnErr) for every new column the v2 set adds.
func TestMigrateV2Idempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "v2.db")

	st1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	st1.Close()

	// Second open against the same file re-runs migrate(); must not error
	// on already-added columns and must leave them intact.
	st2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("second open (idempotency): %v", err)
	}
	t.Cleanup(func() { st2.Close() })
	ctx := context.Background()

	// node_metrics table must exist after migration.
	if _, err := st2.db.ExecContext(ctx, `SELECT cpu_pct, mem_used_mb, mem_total_mb, disk_free_gb, gpu_util, gpu_temp, gpu_mem_used_mb, encode_fps FROM node_metrics WHERE id = 0`); err != nil {
		t.Fatalf("node_metrics columns missing after re-open: %v", err)
	}

	// Spot-check the new job columns are present by inserting a row and
	// selecting the v2 fields back.
	flow := seedFlow(t, st2)
	j, err := st2.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: flow.ID})
	if err != nil {
		t.Fatal(err)
	}
	var fullLog, timingsJSON string
	var priority, retryCount int
	var nextRetry sql.NullString
	if err := st2.db.QueryRowContext(ctx,
		`SELECT full_log, step_timings_json, priority, retry_count, next_retry_at FROM jobs WHERE id = ?`, j.ID).
		Scan(&fullLog, &timingsJSON, &priority, &retryCount, &nextRetry); err != nil {
		t.Fatalf("v2 job columns not selectable after re-open: %v", err)
	}
	if fullLog != "" || timingsJSON != "[]" || priority != 0 || retryCount != 0 {
		t.Fatalf("v2 job defaults wrong: full_log=%q timings=%q priority=%d retry=%d", fullLog, timingsJSON, priority, retryCount)
	}

	// series.notify must exist and default to 1.
	sr, err := st2.UpsertSeriesByName(ctx, "NotifyDefault")
	if err != nil {
		t.Fatal(err)
	}
	if !sr.Notify {
		t.Fatalf("new series notify must default true, got %+v", sr)
	}
}

// TestJobV2FieldsRoundTrip pins every job SELECT scanner (GetJob via
// scanJob, ListJobs via scanJobRow, ActiveJobForNode via scanJobRow) by
// setting all five v2 fields, then reading them back through each path and
// asserting exact equality. This is the regression guard for the bug class
// where a column is added but a SELECT misses it (cf. script_file, 4f98255).
func TestJobV2FieldsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)
	node, err := s.CreateNode(ctx, "enc-rt", "h")
	if err != nil {
		t.Fatal(err)
	}

	want := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	wantTimings := []model.StepTiming{
		{Step: "encode", StartedAt: time.Date(2026, 8, 30, 11, 0, 0, 0, time.UTC), DurationSec: 12.5},
		{Step: "mux", StartedAt: time.Date(2026, 8, 30, 11, 0, 12, 500000000, time.UTC), DurationSec: 3.25},
	}

	// CreateJob only persists the base columns; set the v2 fields via a
	// direct UPDATE (the way later phases' job-completion paths will).
	j, err := s.CreateJob(ctx, &model.Job{
		Series:     "RT",
		Episode:    "01",
		EpisodeDir: "RT/Ep 01",
		ScriptType: "vpy",
		FlowID:     flow.ID,
		Priority:   7,
	})
	if err != nil {
		t.Fatal(err)
	}
	timingsJSON := `[{"step":"encode","started_at":"2026-08-30T11:00:00Z","duration_sec":12.5},{"step":"mux","started_at":"2026-08-30T11:00:12.5Z","duration_sec":3.25}]`
	if _, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET full_log=?, step_timings_json=?, priority=?, retry_count=?, next_retry_at=? WHERE id=?`,
		"line1\nline2", timingsJSON, 7, 2, fmtTime(want), j.ID); err != nil {
		t.Fatal(err)
	}
	// Assign so ActiveJobForNode (the third SELECT path) returns it.
	if err := s.AssignJob(ctx, j.ID, node.ID); err != nil {
		t.Fatal(err)
	}

	check := func(label string, got *model.Job) {
		t.Helper()
		if got.FullLog != "line1\nline2" {
			t.Fatalf("%s: full_log=%q want %q", label, got.FullLog, "line1\nline2")
		}
		if got.Priority != 7 {
			t.Fatalf("%s: priority=%d want 7", label, got.Priority)
		}
		if got.RetryCount != 2 {
			t.Fatalf("%s: retry_count=%d want 2", label, got.RetryCount)
		}
		if got.NextRetryAt == nil || !got.NextRetryAt.Equal(want) {
			t.Fatalf("%s: next_retry_at=%v want %v", label, got.NextRetryAt, want)
		}
		if len(got.StepTimings) != len(wantTimings) {
			t.Fatalf("%s: step_timings len=%d want %d (%+v)", label, len(got.StepTimings), len(wantTimings), got.StepTimings)
		}
		for i, st := range got.StepTimings {
			w := wantTimings[i]
			if st.Step != w.Step {
				t.Fatalf("%s: step[%d].step=%q want %q", label, i, st.Step, w.Step)
			}
			if !st.StartedAt.Equal(w.StartedAt) {
				t.Fatalf("%s: step[%d].started_at=%v want %v", label, i, st.StartedAt, w.StartedAt)
			}
			if st.DurationSec != w.DurationSec {
				t.Fatalf("%s: step[%d].duration_sec=%v want %v", label, i, st.DurationSec, w.DurationSec)
			}
		}
	}

	// Path 1: GetJob -> scanJob (*sql.Row).
	gj, err := s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	check("GetJob", gj)

	// Path 2: ListJobs -> scanJobRow (*sql.Rows).
	list, err := s.ListJobs(ctx, model.JobAssigned, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("ListJobs returned %d, want 1", len(list))
	}
	check("ListJobs", list[0])

	// Path 3: ActiveJobForNode -> scanJobRow (*sql.Rows).
	active, err := s.ActiveJobForNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if active == nil {
		t.Fatal("ActiveJobForNode returned nil")
	}
	check("ActiveJobForNode", active)
}

// TestRetryJobClearsNextRetryAt asserts that RetryJob — the clean-slate
// primitive for re-running a job — clears next_retry_at along with the other
// v2 capture fields. A manual retry must never remain blocked by a stale
// backoff gate once the queue later gates dispatch on next_retry_at.
func TestRetryJobClearsNextRetryAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)

	j, err := s.CreateJob(ctx, &model.Job{
		Series:     "RJ",
		Episode:    "01",
		EpisodeDir: "RJ/Ep 01",
		ScriptType: "vpy",
		FlowID:     flow.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Mark the job failed and stamp a future next_retry_at (the stale
	// backoff gate a manual retry must clear), mirroring the direct-UPDATE
	// pattern used elsewhere for v2 fields.
	stamp := time.Date(2026, 8, 30, 13, 0, 0, 0, time.UTC)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status='failed', next_retry_at=? WHERE id=?`,
		fmtTime(stamp), j.ID); err != nil {
		t.Fatal(err)
	}

	if n, err := s.RetryJob(ctx, j.ID); err != nil || n != 1 {
		t.Fatalf("RetryJob: rows=%d err=%v (want 1, nil)", n, err)
	}

	got, err := s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.NextRetryAt != nil {
		t.Fatalf("RetryJob did not clear next_retry_at: got %v want nil", got.NextRetryAt)
	}
}

// TestSeriesNotifyRoundTrip asserts the series.notify column defaults true
// for a freshly seeded series and survives a round-trip to false.
func TestSeriesNotifyRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Fresh series defaults to notify=true.
	sr, err := s.UpsertSeriesByName(ctx, "NotifyShow")
	if err != nil {
		t.Fatal(err)
	}
	if !sr.Notify {
		t.Fatalf("new series notify must default true, got %+v", sr)
	}

	// Round-trip false via UpdateSeries (full-row save).
	sr.Notify = false
	if err := s.UpdateSeries(ctx, sr); err != nil {
		t.Fatal(err)
	}
	got, err := s.SeriesByName(ctx, "NotifyShow")
	if err != nil {
		t.Fatal(err)
	}
	if got.Notify {
		t.Fatalf("series notify=false not persisted: %+v", got)
	}

	// ListSeries path must also reflect the false.
	list, err := s.ListSeries(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list series: %d err %v", len(list), err)
	}
	if list[0].Notify {
		t.Fatalf("ListSeries notify not false: %+v", list[0])
	}

	// Round-trip back to true.
	sr.Notify = true
	if err := s.UpdateSeries(ctx, sr); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.SeriesByName(ctx, "NotifyShow")
	if !got2.Notify {
		t.Fatalf("series notify=true not persisted: %+v", got2)
	}
}

// TestSettingsV2FlagsRoundTrip asserts DrainMode and NotifyDigest persist
// through SaveSettings/GetSettings (the JSON-blob single-row table). Since
// settings is a JSON column (not individual SQL columns), these fields live
// in the marshaled blob and round-trip through the existing save/load path.
func TestSettingsV2FlagsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	st := &model.Settings{
		ControllerURL:  "http://ctrl",
		ScriptsRoot:    "/data/scripts",
		ReleaseRoot:    "/data/release",
		NodeBinDir:     "C:\\bin",
		NodeScriptsDir: "C:\\scripts",
		NodeReleaseDir: "C:\\release",
		DrainMode:      true,
		NotifyDigest:   true,
	}
	if err := s.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("settings row not found after save")
	}
	if !got.DrainMode {
		t.Fatalf("drain_mode not persisted: %+v", got)
	}
	if !got.NotifyDigest {
		t.Fatalf("notify_digest not persisted: %+v", got)
	}

	// Round-trip false.
	st.DrainMode = false
	st.NotifyDigest = false
	if err := s.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSettings(ctx)
	if got.DrainMode || got.NotifyDigest {
		t.Fatalf("flags not cleared: %+v", got)
	}
}
