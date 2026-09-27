package store

import (
	"context"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestNextAssignableJobSkipsPausedSeries verifies the dispatch guard: a
// pending job whose series row is paused is invisible to assignment, while
// jobs of unpaused series (and jobs whose series row is missing entirely —
// manual jobs created before registration) dispatch normally.
func TestNextAssignableJobSkipsPausedSeries(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	fl, err := s.CreateFlow(ctx, &model.Flow{Name: "f1", Steps: []model.Step{{Type: model.StepEncode}}})
	if err != nil {
		t.Fatal(err)
	}
	// Real node rows: AssignJob validates node existence/enabled.
	for _, n := range []string{"n1", "n2", "n3"} {
		if _, err := s.CreateNode(ctx, n, "hash-"+n); err != nil {
			t.Fatal(err)
		}
	}

	// Paused series with one pending job.
	paused, err := s.UpsertSeriesByName(ctx, "Paused Show")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSeriesPaused(ctx, paused.ID, true); err != nil {
		t.Fatal(err)
	}
	jPaused, err := s.CreateJob(ctx, &model.Job{
		Series: "Paused Show", Episode: "01", EpisodeDir: "Paused Show/Ep 01",
		ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Nothing assignable while the only job belongs to a paused series.
	got, err := s.NextAssignableJobForNode(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("paused-series job was dispatched (id %d)", got.ID)
	}

	// An unpaused series' job dispatches fine.
	if _, err := s.UpsertSeriesByName(ctx, "Active Show"); err != nil {
		t.Fatal(err)
	}
	jActive, err := s.CreateJob(ctx, &model.Job{
		Series: "Active Show", Episode: "01", EpisodeDir: "Active Show/Ep 01",
		ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.NextAssignableJobForNode(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != jActive.ID {
		t.Fatalf("got %v, want active job %d", got, jActive.ID)
	}

	// A job with NO series row (manual creation pre-registration) still
	// dispatches — pausing only applies to registered series.
	jOrphan, err := s.CreateJob(ctx, &model.Job{
		Series: "Ghost Show", Episode: "01", EpisodeDir: "Ghost Show/Ep 01",
		ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Assign the active one away so the ghost is the best candidate left.
	if err := s.AssignJob(ctx, jActive.ID, 2); err != nil {
		t.Fatal(err)
	}
	got, err = s.NextAssignableJobForNode(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != jOrphan.ID {
		t.Fatalf("got %v, want ghost job %d", got, jOrphan.ID)
	}

	// Unpausing makes the paused job visible again.
	if err := s.SetSeriesPaused(ctx, paused.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignJob(ctx, jOrphan.ID, 3); err != nil {
		t.Fatal(err)
	}
	got, err = s.NextAssignableJobForNode(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != jPaused.ID {
		t.Fatalf("after unpause: got %v, want %d", got, jPaused.ID)
	}
}

// TestSetSeriesPausedFieldScoped verifies pausing does not disturb the
// other series fields (flow, tag, enabled, notify).
func TestSetSeriesPausedFieldScoped(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sr, err := s.UpsertSeriesByName(ctx, "Show")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSeriesTag(ctx, sr.ID, "2160p"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSeriesPaused(ctx, sr.ID, true); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSeries(ctx, sr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Paused {
		t.Error("paused not persisted")
	}
	if got.Tag != "2160p" || !got.Enabled || !got.Notify {
		t.Errorf("other fields disturbed: tag=%q enabled=%v notify=%v", got.Tag, got.Enabled, got.Notify)
	}
}
