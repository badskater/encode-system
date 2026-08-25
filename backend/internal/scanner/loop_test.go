package scanner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// fakeStore captures CreateJob calls for loop tests.
type fakeStore struct {
	existing map[string]bool
	flow     *model.Flow
	created  []*model.Job
	series   map[string]*model.Series
	disabled map[string]bool // series name -> disabled
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

func (f *fakeStore) UpsertSeriesByName(_ context.Context, name string) (*model.Series, error) {
	if f.series == nil {
		f.series = map[string]*model.Series{}
	}
	if _, ok := f.series[name]; !ok {
		f.series[name] = &model.Series{Name: name, Enabled: !f.disabled[name]}
	}
	return f.series[name], nil
}

func (f *fakeStore) GetFlow(_ context.Context, id int64) (*model.Flow, error) {
	if f.flow != nil && f.flow.ID == id {
		return f.flow, nil
	}
	return nil, fmt.Errorf("flow %d not found", id)
}

func (f *fakeStore) DefaultFlow(_ context.Context) (*model.Flow, error) {
	if f.flow == nil {
		return nil, fmt.Errorf("no default flow")
	}
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

// TestRunLoopLiveConfigAndCancellation verifies the RunLoop contract: the
// live config is re-read every cycle (Settings-page cadence edits apply
// without a restart), an interval change does not stall the loop, and ctx
// cancellation ends it promptly.
func TestRunLoopLiveConfigAndCancellation(t *testing.T) {
	root := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := &fakeStore{existing: map[string]bool{}, flow: &model.Flow{ID: 9, Name: "default-1080"}}

	var mu sync.Mutex
	calls := 0
	interval := 30 * time.Millisecond
	cfg := func(ctx context.Context) (string, time.Duration, string) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return root, interval, "default-1080"
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunLoop(ctx, log, st, cfg)
		close(done)
	}()

	// Several cycles with the initial interval.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	first := calls
	mu.Unlock()
	if first < 3 {
		t.Fatalf("loop did not cycle: cfg called %d times", first)
	}

	// Change the cadence live: the loop must keep cycling on the new ticker.
	mu.Lock()
	interval = 20 * time.Millisecond
	mu.Unlock()
	deadline = time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= first+3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	second := calls
	mu.Unlock()
	if second < first+3 {
		t.Fatalf("loop stalled after interval change: %d -> %d", first, second)
	}

	// Cancellation ends the loop promptly.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop did not return after ctx cancel")
	}
}
