package scanner

import (
	"context"
	"io"
	"log/slog"
	"testing"

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

func TestEpTokenStripsPrefix(t *testing.T) {
	cases := map[string]string{
		"Ep 01": "01", "Ep 12": "12", "Ep01": "Ep01", "Finale": "Finale",
	}
	for in, want := range cases {
		if got := epToken(in); got != want {
			t.Errorf("epToken(%q) = %q, want %q", in, got, want)
		}
	}
	if got := episodeFromDir("Series/Ep 03"); got != "03" {
		t.Errorf("episodeFromDir = %q", got)
	}
}
