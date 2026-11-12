package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestDeleteSeriesAuthz pins the authorization table for DELETE
// /api/series/{id}: admin 204, viewer session 403, read-scope token 403,
// anonymous 401, unknown id 404, malformed id 400. Non-admin attempts must
// leave the registry row intact; the successful admin delete is audited as
// series.delete with the actor and series name.
func TestDeleteSeriesAuthz(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	sr, err := e.server.Store.UpsertSeriesByName(ctxBg(), "AuthzShow")
	if err != nil {
		t.Fatal(err)
	}
	viewerTok := mintViewerSession(t, e, "series-viewer", "viewer-pass-123")

	// Read-scope API token (minted through the real tokens endpoint).
	_, tokBody := doJSON(t, "POST", ts.URL+"/api/tokens", adminTok, map[string]any{"name": "series-reader", "scope": "read"})
	var created map[string]any
	if err := json.Unmarshal(tokBody, &created); err != nil {
		t.Fatal(err)
	}
	readTok := created["token"].(string)

	delURL := ts.URL + "/api/series/" + itoa(sr.ID)
	cases := []struct {
		name  string
		token string
		url   string
		want  int
	}{
		{"viewer session", viewerTok, delURL, 403},
		{"read-scope token", readTok, delURL, 403},
		{"anonymous", "", delURL, 401},
		{"admin unknown id", adminTok, ts.URL + "/api/series/999999", 404},
		{"admin malformed id", adminTok, ts.URL + "/api/series/not-a-number", 400},
	}
	for _, tc := range cases {
		resp, out := doJSON(t, "DELETE", tc.url, tc.token, nil)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: DELETE = %d, want %d (%s)", tc.name, resp.StatusCode, tc.want, out)
		}
		// Every rejected attempt must leave the row in place.
		if _, err := e.server.Store.GetSeries(ctxBg(), sr.ID); err != nil {
			t.Fatalf("%s: series row vanished after a rejected delete: %v", tc.name, err)
		}
	}

	// Admin delete succeeds with 204 and removes the registry row.
	resp, out := doJSON(t, "DELETE", delURL, adminTok, nil)
	if resp.StatusCode != 204 {
		t.Fatalf("admin DELETE = %d, want 204 (%s)", resp.StatusCode, out)
	}
	if _, err := e.server.Store.GetSeries(ctxBg(), sr.ID); err == nil {
		t.Fatal("series row still present after admin delete")
	}
	// Gone from the list endpoint too.
	_, listBody := doJSON(t, "GET", ts.URL+"/api/series", adminTok, nil)
	if strings.Contains(string(listBody), "AuthzShow") {
		t.Fatalf("deleted series still listed: %s", listBody)
	}
	// A second delete of the same id is a 404, not a silent success.
	resp, _ = doJSON(t, "DELETE", delURL, adminTok, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("repeat DELETE = %d, want 404", resp.StatusCode)
	}

	// Audit: series.delete with actor admin, object series:<id>, name detail.
	_, auditBody := doJSON(t, "GET", ts.URL+"/api/audit?limit=50", adminTok, nil)
	var events []model.AuditEvent
	if err := json.Unmarshal(auditBody, &events); err != nil {
		t.Fatal(err)
	}
	var found *model.AuditEvent
	for i := range events {
		if events[i].Action == "series.delete" {
			found = &events[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("audit missing series.delete: %s", auditBody)
	}
	if found.Actor != "admin" || found.Object != "series:"+itoa(sr.ID) {
		t.Fatalf("audit actor/object = %q/%q, want admin/series:%d", found.Actor, found.Object, sr.ID)
	}
	if !strings.Contains(found.Detail, "AuthzShow") {
		t.Fatalf("audit detail missing series name: %q", found.Detail)
	}
}

// TestDeleteSeriesPreservesHistory proves the history-preserving contract:
// after the registry row is deleted, the series' jobs stay listable with
// their series/episode data, fleet stats are unchanged, progress counts
// still derive from the jobs, and files on the scripts share are untouched.
func TestDeleteSeriesPreservesHistory(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Real files on the scripts share (source + a finished encode output).
	scriptsRoot := e.server.currentSettings(ctx).ScriptsRoot
	epDir := filepath.Join(scriptsRoot, "HistoryShow", "Ep 01")
	if err := os.MkdirAll(epDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(epDir, "src.m2ts")
	outPath := filepath.Join(epDir, "HistoryShow - 01 [1080p].mkv")
	if err := os.WriteFile(srcPath, []byte("source-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outPath, []byte("encoded-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Series + jobs via the store (one done, one failed, one pending).
	sr, err := e.server.Store.UpsertSeriesByName(ctx, "HistoryShow")
	if err != nil {
		t.Fatal(err)
	}
	flows, err := e.server.Store.ListFlows(ctx)
	if err != nil || len(flows) == 0 {
		t.Fatalf("no seeded flow: %v", err)
	}
	mkJob := func(ep, epDir string) *model.Job {
		j, err := e.server.Store.CreateJob(ctx, &model.Job{
			Series: "HistoryShow", Episode: ep, EpisodeDir: epDir,
			ScriptType: "vpy", FlowID: flows[0].ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	j1 := mkJob("01", "HistoryShow/Ep 01")
	j2 := mkJob("02", "HistoryShow/Ep 02")
	j3 := mkJob("03", "HistoryShow/Ep 03")
	if err := e.server.Store.FinishJob(ctx, j1.ID, model.JobDone, 0, "", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.FinishJob(ctx, j2.ID, model.JobFailed, 1, "boom", nil, ""); err != nil {
		t.Fatal(err)
	}
	_ = j3 // stays pending

	// Snapshots BEFORE the delete: stats, job list, progress counts.
	statsResp, statsBefore := doJSON(t, "GET", ts.URL+"/api/stats?range=all", adminTok, nil)
	if statsResp.StatusCode != 200 {
		t.Fatalf("stats before = %d", statsResp.StatusCode)
	}
	_, jobsBefore := doJSON(t, "GET", ts.URL+"/api/jobs", adminTok, nil)
	var jobsBeforeList []model.Job
	if err := json.Unmarshal(jobsBefore, &jobsBeforeList); err != nil {
		t.Fatal(err)
	}
	progressBefore, err := e.server.Store.SeriesProgressCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Delete the registry row.
	resp, out := doJSON(t, "DELETE", ts.URL+"/api/series/"+itoa(sr.ID), adminTok, nil)
	if resp.StatusCode != 204 {
		t.Fatalf("delete = %d (%s)", resp.StatusCode, out)
	}

	// Jobs survive: same count, same series/episode data, same statuses.
	_, jobsAfter := doJSON(t, "GET", ts.URL+"/api/jobs", adminTok, nil)
	var jobsAfterList []model.Job
	if err := json.Unmarshal(jobsAfter, &jobsAfterList); err != nil {
		t.Fatal(err)
	}
	if len(jobsAfterList) != len(jobsBeforeList) {
		t.Fatalf("job count changed: %d → %d", len(jobsBeforeList), len(jobsAfterList))
	}
	byID := map[int64]model.Job{}
	for _, j := range jobsAfterList {
		byID[j.ID] = j
	}
	for _, want := range jobsBeforeList {
		got, ok := byID[want.ID]
		if !ok {
			t.Fatalf("job %d lost after series delete", want.ID)
		}
		if got.Series != want.Series || got.Episode != want.Episode ||
			got.EpisodeDir != want.EpisodeDir || got.Status != want.Status {
			t.Fatalf("job %d mutated: %+v → %+v", want.ID, want, got)
		}
	}

	// Stats unchanged (byte-identical aggregate over the same job rows).
	statsResp, statsAfter := doJSON(t, "GET", ts.URL+"/api/stats?range=all", adminTok, nil)
	if statsResp.StatusCode != 200 {
		t.Fatalf("stats after = %d", statsResp.StatusCode)
	}
	var before, after model.Stats
	if err := json.Unmarshal(statsBefore, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(statsAfter, &after); err != nil {
		t.Fatal(err)
	}
	if before.Totals != after.Totals {
		t.Fatalf("stats totals changed: %+v → %+v", before.Totals, after.Totals)
	}

	// Progress counts still derive from the surviving jobs.
	progressAfter, err := e.server.Store.SeriesProgressCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if progressBefore["HistoryShow"] != progressAfter["HistoryShow"] {
		t.Fatalf("progress changed: %+v → %+v", progressBefore["HistoryShow"], progressAfter["HistoryShow"])
	}
	if progressAfter["HistoryShow"].Done != 1 || progressAfter["HistoryShow"].Failed != 1 || progressAfter["HistoryShow"].Active != 1 {
		t.Fatalf("unexpected progress counts: %+v", progressAfter["HistoryShow"])
	}

	// Files untouched: same paths, same bytes.
	for path, want := range map[string]string{srcPath: "source-bytes", outPath: "encoded-bytes"} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("file lost after series delete: %s: %v", path, err)
		}
		if string(got) != want {
			t.Fatalf("file content changed: %s = %q, want %q", path, got, want)
		}
	}
}

// TestDeleteSeriesAdminTokenAuditActor pins that a series deleted with an
// admin-scope API token is audited under "api-token:<name>" (not the admin
// session username), with the series name in the detail.
func TestDeleteSeriesAdminTokenAuditActor(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	sr, err := e.server.Store.UpsertSeriesByName(ctxBg(), "TokenShow")
	if err != nil {
		t.Fatal(err)
	}
	_, tokBody := doJSON(t, "POST", ts.URL+"/api/tokens", adminTok, map[string]any{"name": "radarr", "scope": "admin"})
	var created map[string]any
	if err := json.Unmarshal(tokBody, &created); err != nil {
		t.Fatal(err)
	}
	tok := created["token"].(string)

	resp, out := doJSON(t, "DELETE", ts.URL+"/api/series/"+itoa(sr.ID), tok, nil)
	if resp.StatusCode != 204 {
		t.Fatalf("admin-token DELETE = %d, want 204 (%s)", resp.StatusCode, out)
	}

	_, auditBody := doJSON(t, "GET", ts.URL+"/api/audit?limit=50", adminTok, nil)
	var events []model.AuditEvent
	if err := json.Unmarshal(auditBody, &events); err != nil {
		t.Fatal(err)
	}
	var found *model.AuditEvent
	for i := range events {
		if events[i].Action == "series.delete" && events[i].Object == "series:"+itoa(sr.ID) {
			found = &events[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("audit missing series.delete for series:%d: %s", sr.ID, auditBody)
	}
	if found.Actor != "api-token:radarr" {
		t.Fatalf("audit actor = %q, want api-token:radarr", found.Actor)
	}
	if !strings.Contains(found.Detail, "TokenShow") {
		t.Fatalf("audit detail missing series name: %q", found.Detail)
	}
}
