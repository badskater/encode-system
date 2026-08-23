package scanner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// fakeStore captures CreateJob calls for loop tests.
type fakeStore struct {
	existing map[string]bool
	flow     *model.Flow
	created  []*model.Job
}

func (f *fakeStore) JobExistsForEpisode(_ context.Context, dir string) (bool, error) {
	return f.existing[dir], nil
}

func (f *fakeStore) CreateJob(_ context.Context, j *model.Job) (*model.Job, error) {
	j.ID = int64(len(f.created) + 1)
	f.created = append(f.created, j)
	return j, nil
}

func (f *fakeStore) FlowByName(_ context.Context, _ string) (*model.Flow, error) {
	return f.flow, nil
}

func TestScanOnceCreatesJobsForNewEpisodesOnly(t *testing.T) {
	root := t.TempDir()
	mkEp(t, root, "S", "Ep 01", "src.m2ts", "1080.vpy")
	mkEp(t, root, "S", "Ep 02", "src.m2ts", "1080.avs")
	// Age sources past the stability gate so they count as ready.
	past := time.Now().Add(-10 * time.Minute)
	for _, ep := range []string{"Ep 01", "Ep 02"} {
		os.Chtimes(filepath.Join(root, "S", ep, "src.m2ts"), past, past)
	}

	st := &fakeStore{
		existing: map[string]bool{"S/Ep 01": true}, // Ep 01 already has a job
		flow:     &model.Flow{ID: 9, Name: "default-1080"},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	scanOnce(context.Background(), log, st, root, "default-1080")

	if len(st.created) != 1 {
		t.Fatalf("want 1 new job (Ep 02), got %d: %+v", len(st.created), st.created)
	}
	j := st.created[0]
	if j.EpisodeDir != "S/Ep 02" || j.Episode != "02" || j.ScriptType != "avs" {
		t.Fatalf("wrong job fields: %+v", j)
	}
	if j.FlowID != 9 {
		t.Fatalf("flow id not applied: %+v", j)
	}
}

// Episode extraction is unified on flow.EpisodeNumber (no second
// implementation in the loop to drift out of sync).
func TestLoopUsesFlowEpisodeNumber(t *testing.T) {
	root := t.TempDir()
	mkEp(t, root, "S", "Ep 03", "src.m2ts", "1080.vpy")
	// Age the source past the stability gate.
	past := time.Now().Add(-10 * time.Minute)
	os.Chtimes(filepath.Join(root, "S", "Ep 03", "src.m2ts"), past, past)

	st := &fakeStore{existing: map[string]bool{}, flow: &model.Flow{ID: 1, Name: "default-1080"}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	scanOnce(context.Background(), log, st, root, "default-1080")

	if len(st.created) != 1 {
		t.Fatalf("want 1 job, got %d", len(st.created))
	}
	if st.created[0].Episode != "03" {
		t.Errorf("episode = %q, want 03 via flow.EpisodeNumber", st.created[0].Episode)
	}
	if st.created[0].ScriptFile != "1080.vpy" {
		t.Errorf("script file not persisted on job: %+v", st.created[0])
	}
}
