package api

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// deleteBurst fires n concurrent DELETE requests at url with the given
// bearer token and returns the status-code histogram. Every request is
// built up front and released together through a start barrier so the
// handlers' GetSeries/GetShare → Delete race window is exercised: several
// requests pass the existence check before the winner's DELETE lands, and
// the losers' DELETE then matches zero rows (sql.ErrNoRows).
func deleteBurst(t *testing.T, url, token string, n int) map[int]int {
	t.Helper()
	start := make(chan struct{})
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodDelete, url, nil)
			if err != nil {
				t.Errorf("build request: %v", err)
				return
			}
			req.Header.Set("Authorization", "Bearer "+token)
			<-start
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("delete: %v", err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			codes[i] = resp.StatusCode
		}(i)
	}
	close(start)
	wg.Wait()
	hist := map[int]int{}
	for _, c := range codes {
		hist[c]++
	}
	return hist
}

// TestDeleteSeriesConcurrentRace pins the concurrent-delete contract for
// DELETE /api/series/{id}: when N requests race on the same id, exactly one
// wins (204), every loser gets 404 — never 500 from the store's
// sql.ErrNoRows — and exactly one series.delete audit event is written.
func TestDeleteSeriesConcurrentRace(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	sr, err := e.server.Store.UpsertSeriesByName(ctxBg(), "RaceShow")
	if err != nil {
		t.Fatal(err)
	}
	const n = 12
	hist := deleteBurst(t, ts.URL+"/api/series/"+itoa(sr.ID), adminTok, n)
	if hist[http.StatusNoContent] != 1 || hist[http.StatusNotFound] != n-1 || len(hist) != 2 {
		t.Fatalf("concurrent DELETE histogram = %v, want exactly one 204 and %d 404 (no 500)", hist, n-1)
	}
	if _, err := e.server.Store.GetSeries(ctxBg(), sr.ID); err == nil {
		t.Fatal("series row still present after the winning delete")
	}

	// Exactly one audit event for the deleted series — losers must not audit.
	_, auditBody := doJSON(t, "GET", ts.URL+"/api/audit?limit=200", adminTok, nil)
	var events []model.AuditEvent
	if err := json.Unmarshal(auditBody, &events); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, ev := range events {
		if ev.Action == "series.delete" && ev.Object == "series:"+itoa(sr.ID) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("series.delete audit count = %d, want 1:\n%s", count, auditBody)
	}
}

// TestDeleteShareConcurrentRace pins the same contract for DELETE
// /api/shares/{id}: one 204, the rest 404, zero 500, one share.delete audit.
func TestDeleteShareConcurrentRace(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, out := doJSON(t, "POST", ts.URL+"/api/shares", adminTok, map[string]any{
		"name": "race-nfs", "kind": "nfs", "role": "scripts",
		"server": "nas01", "path": "encodes", "enabled": true,
	})
	if resp.StatusCode != 201 {
		t.Fatalf("create share = %d %s", resp.StatusCode, out)
	}
	var created map[string]any
	if err := json.Unmarshal(out, &created); err != nil {
		t.Fatal(err)
	}
	id := int64(created["id"].(float64))

	const n = 12
	hist := deleteBurst(t, ts.URL+"/api/shares/"+itoa(id), adminTok, n)
	if hist[http.StatusNoContent] != 1 || hist[http.StatusNotFound] != n-1 || len(hist) != 2 {
		t.Fatalf("concurrent DELETE histogram = %v, want exactly one 204 and %d 404 (no 500)", hist, n-1)
	}
	if _, err := e.server.Store.GetShare(ctxBg(), id); err == nil {
		t.Fatal("share row still present after the winning delete")
	}

	_, auditBody := doJSON(t, "GET", ts.URL+"/api/audit?limit=200", adminTok, nil)
	var events []model.AuditEvent
	if err := json.Unmarshal(auditBody, &events); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, ev := range events {
		if ev.Action == "share.delete" && ev.Object == "share:"+itoa(id) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("share.delete audit count = %d, want 1:\n%s", count, auditBody)
	}
}
