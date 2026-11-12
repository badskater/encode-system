package scanner

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/store"
)

// These tests pin the interaction between DELETE /api/series/{id} (which
// removes ONLY the registry row) and the scanner, using the REAL store so
// the upsert/delete SQL semantics are exercised end to end:
//
//   - folder still yields ready episodes → the next scan re-registers the
//     series (fresh row, enabled) and existing jobs still dedupe;
//   - folder exists but has no ready episodes (scaffold-only) → no
//     candidates, so the deleted series stays deleted.
//
// Operators must remove/rename the folder on the share to make a series
// deletion stick while episodes are still ready.

func openReregisterStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "reregister.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.CreateFlow(context.Background(), &model.Flow{
		Name:  "default-1080",
		Steps: []model.Step{{Type: model.StepEncode}},
	}); err != nil {
		t.Fatalf("create flow: %v", err)
	}
	return st
}

func TestScanOnceReregistersDeletedSeriesWithReadyEpisodes(t *testing.T) {
	root := t.TempDir()
	st := openReregisterStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	mkEp(t, root, "DeletedShow", "Ep 01", "src.m2ts", "1080.vpy")
	past := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(filepath.Join(root, "DeletedShow", "Ep 01", "src.m2ts"), past, past); err != nil {
		t.Fatal(err)
	}

	sr, err := st.UpsertSeriesByName(ctx, "DeletedShow")
	if err != nil {
		t.Fatal(err)
	}
	// First scan queues the episode.
	scanOnce(ctx, log, st, Target{Root: root}, "default-1080")
	exists, err := st.JobExistsForEpisode(ctx, "DeletedShow/Ep 01")
	if err != nil || !exists {
		t.Fatalf("first scan did not create the job: exists=%v err=%v", exists, err)
	}

	// The API delete removes the registry row only.
	if err := st.DeleteSeries(ctx, sr.ID); err != nil {
		t.Fatalf("delete series: %v", err)
	}
	if _, err := st.SeriesByName(ctx, "DeletedShow"); err == nil {
		t.Fatal("series row still present after delete")
	}

	// Next scan: the folder still has a ready episode → re-registered as a
	// FRESH row (new id, enabled), while the existing job dedupes.
	scanOnce(ctx, log, st, Target{Root: root}, "default-1080")
	sr2, err := st.SeriesByName(ctx, "DeletedShow")
	if err != nil {
		t.Fatalf("scanner did not re-register the deleted series: %v", err)
	}
	if sr2.ID == sr.ID {
		t.Fatalf("re-registration reused the old id %d", sr.ID)
	}
	if !sr2.Enabled || sr2.Paused {
		t.Fatalf("re-registered series must be enabled/unpaused: %+v", sr2)
	}
	jobs, err := st.ListJobs(ctx, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, j := range jobs {
		if j.Series == "DeletedShow" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("re-scan duplicated the episode job: %d jobs for DeletedShow, want 1", n)
	}
}

func TestScanOnceDoesNotReregisterDeletedSeriesWithoutReadyEpisodes(t *testing.T) {
	root := t.TempDir()
	st := openReregisterStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Scaffold-only folder: the episode dir exists but has no source/script,
	// so Scan yields no candidates for it.
	mkEp(t, root, "EmptyShow", "Ep 01")

	sr, err := st.UpsertSeriesByName(ctx, "EmptyShow")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSeries(ctx, sr.ID); err != nil {
		t.Fatalf("delete series: %v", err)
	}

	scanOnce(ctx, log, st, Target{Root: root}, "default-1080")
	if _, err := st.SeriesByName(ctx, "EmptyShow"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("scaffold-only folder must not re-register the deleted series: err=%v, want sql.ErrNoRows", err)
	}
}
