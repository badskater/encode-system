package api

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// seriesProgressRow is the JSON shape the GET /api/series handler now emits:
// the embedded Series fields plus the four progress counters.
type seriesProgressRow struct {
	model.Series
	Jobs           int `json:"jobs"`
	EpisodesDone   int `json:"episodes_done"`
	EpisodesFailed int `json:"episodes_failed"`
	EpisodesActive int `json:"episodes_active"`
	EpisodesTotal  int `json:"episodes_total"`
}

// listSeriesViaAPI calls GET /api/series and decodes the progress-enriched
// rows, keyed by series name for assertion convenience.
func listSeriesViaAPI(t *testing.T, ts *httptest.Server, tok string) map[string]seriesProgressRow {
	t.Helper()
	resp, body := doJSON(t, "GET", ts.URL+"/api/series", tok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/series: %d %s", resp.StatusCode, body)
	}
	var rows []seriesProgressRow
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode series list: %v: %s", err, body)
	}
	out := make(map[string]seriesProgressRow, len(rows))
	for _, r := range rows {
		out[r.Name] = r
	}
	return out
}

// finishJob is a test helper that flips a created job to a terminal (or
// non-terminal) status by calling the store's FinishJob path.
func finishJob(t *testing.T, e *testEnv, jobID int64, status model.JobStatus) {
	t.Helper()
	if err := e.server.Store.FinishJob(ctxBg(), jobID, status, 0, "", nil, ""); err != nil {
		t.Fatalf("finish job %d as %s: %v", jobID, status, err)
	}
}

// makeJob creates a job in the given series/episode dir and immediately
// finishes it with the requested status. Returns the created job id (rarely
// needed — the test asserts via the list endpoint).
func makeJob(t *testing.T, e *testEnv, series, epDir string, status model.JobStatus) {
	t.Helper()
	ctx := ctxBg()
	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	j, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: series, Episode: filepath.Base(epDir), EpisodeDir: epDir,
		ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatalf("create job %s/%s: %v", series, epDir, err)
	}
	if status == model.JobPending {
		return // created as pending already
	}
	// To reach assigned/running we use UpdateJobStatus (only works on
	// assigned/running jobs); to reach done/failed we use FinishJob.
	switch status {
	case model.JobAssigned, model.JobRunning:
		if err := e.server.Store.UpdateJobStatus(ctx, j.ID, status, "step", 0, ""); err != nil {
			t.Fatalf("update job %d to %s: %v", j.ID, status, err)
		}
	default:
		finishJob(t, e, j.ID, status)
	}
}

// TestSeriesProgressCounts covers the four progress counters served by
// GET /api/series across a mix of job lifecycle states:
//   - done episodes (counted in episodes_done)
//   - a failed-then-done retry (must count as done, NOT failed — the
//     "eventually done wins" semantic)
//   - active episodes (pending/assigned/running)
//   - a purely failed episode (no later success)
//   - a series with zero jobs (all zeros)
//
// The series dir is scaffolded on the scripts root with 5 episode folders,
// so episodes_total must reflect the filesystem count (5) even though only
// 4 episode dirs appear in the jobs table.
func TestSeriesProgressCounts(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Register the series and scaffold 5 episode folders on the scripts root.
	if _, err := e.server.Store.UpsertSeriesByName(ctx, "Progress Show"); err != nil {
		t.Fatal(err)
	}
	st := e.server.currentSettings(ctx)
	seriesDir := filepath.Join(st.ScriptsRoot, "Progress Show")
	for _, ep := range []string{"Ep 01", "Ep 02", "Ep 03", "Ep 04", "Ep 05"} {
		if err := os.MkdirAll(filepath.Join(seriesDir, ep), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Ep 01: done.
	makeJob(t, e, "Progress Show", "Progress Show/Ep 01", model.JobDone)

	// Ep 02: failed then done (retried successfully) → must count as done,
	// NOT failed. Two job rows on the same episode_dir: the earlier one
	// failed, the later one succeeded.
	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	failedJ, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "Progress Show", Episode: "02", EpisodeDir: "Progress Show/Ep 02",
		ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	finishJob(t, e, failedJ.ID, model.JobFailed)
	doneJ, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "Progress Show", Episode: "02", EpisodeDir: "Progress Show/Ep 02",
		ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	finishJob(t, e, doneJ.ID, model.JobDone)

	// Ep 03: purely failed.
	makeJob(t, e, "Progress Show", "Progress Show/Ep 03", model.JobFailed)

	// Ep 04: active (pending).
	makeJob(t, e, "Progress Show", "Progress Show/Ep 04", model.JobPending)

	// Ep 05: active (running).
	makeJob(t, e, "Progress Show", "Progress Show/Ep 05", model.JobRunning)

	// Also register an empty series with no jobs at all.
	if _, err := e.server.Store.UpsertSeriesByName(ctx, "Empty Show"); err != nil {
		t.Fatal(err)
	}

	rows := listSeriesViaAPI(t, ts, adminTok)

	got, ok := rows["Progress Show"]
	if !ok {
		t.Fatalf("Progress Show not in series list: %+v", rows)
	}
	if got.EpisodesDone != 2 {
		t.Errorf("episodes_done = %d, want 2 (Ep 01 done + Ep 02 retried-then-done)", got.EpisodesDone)
	}
	if got.EpisodesFailed != 1 {
		t.Errorf("episodes_failed = %d, want 1 (Ep 03 only; Ep 02's failed attempt does not count)", got.EpisodesFailed)
	}
	if got.EpisodesActive != 2 {
		t.Errorf("episodes_active = %d, want 2 (Ep 04 pending + Ep 05 running)", got.EpisodesActive)
	}
	if got.EpisodesTotal != 5 {
		t.Errorf("episodes_total = %d, want 5 (scaffolded Ep 01..Ep 05 folders)", got.EpisodesTotal)
	}

	empty, ok := rows["Empty Show"]
	if !ok {
		t.Fatalf("Empty Show not in series list: %+v", rows)
	}
	if empty.EpisodesDone != 0 || empty.EpisodesFailed != 0 || empty.EpisodesActive != 0 || empty.EpisodesTotal != 0 {
		t.Errorf("empty series counts not zero: %+v", empty)
	}
}

// TestSeriesProgressTotalFallsBackToJobs verifies that when the scripts root
// is NOT readable (no scaffolded folders), episodes_total falls back to the
// count of distinct episode dirs seen in the jobs table.
func TestSeriesProgressTotalFallsBackToJobs(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Register a series but do NOT scaffold any folders — the scripts root
	// dir for this series simply does not exist on disk.
	if _, err := e.server.Store.UpsertSeriesByName(ctx, "No Folders Show"); err != nil {
		t.Fatal(err)
	}
	// Two jobs on two distinct episode dirs, both done.
	makeJob(t, e, "No Folders Show", "No Folders Show/Ep 01", model.JobDone)
	makeJob(t, e, "No Folders Show", "No Folders Show/Ep 02", model.JobDone)

	rows := listSeriesViaAPI(t, ts, adminTok)
	got := rows["No Folders Show"]
	if got.EpisodesTotal != 2 {
		t.Errorf("episodes_total fallback = %d, want 2 (distinct dirs in jobs)", got.EpisodesTotal)
	}
	if got.EpisodesDone != 2 {
		t.Errorf("episodes_done = %d, want 2", got.EpisodesDone)
	}
}
