package api

// Final integration-review regressions (W7): audit detail summarization at
// the SOURCE so the 1024-byte writeAudit cap never drops key fields.
//
// Two worst-case shapes proved the truncation hole:
//   - backup.offsite with a large retention prune (364 names ≈ 10KB of
//     detail) — the cap cut the JSON mid-array, dropping shares_key_bundled
//     and corrupting the row into unparseable text.
//   - ingest.staged with a 1024-rune torrent name (≈1.1KB) — series and
//     staged_path were truncated away.
//
// The fix summarizes where the detail is BUILT (pruned list > 20 names →
// first 20 + "...and N more"; torrent name capped at 200 runes — the full
// name stays in the ledger and structured logs), so every audit row stays
// valid JSON with its key fields intact.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/ingest"
	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/offsite"
)

// TestOffsiteAuditPrunedListSummarized: a manual backup whose retention prune
// removes 364 old generations must audit a detail that (a) fits the 1024-byte
// cap WITHOUT truncation, (b) parses as JSON, (c) keeps every key field, and
// (d) summarizes the pruned list as first 20 + "...and N more".
func TestOffsiteAuditPrunedListSummarized(t *testing.T) {
	e, ts := syncHookEnv(t)
	seedOffsiteSettings(t, e, true, 1) // keepN=1 → prune everything older
	fake := newAPIFakeDest("s3")
	// 364 old generations, valid snapshot names, all older than the new one.
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	const oldGens = 364
	for i := 0; i < oldGens; i++ {
		name := fmt.Sprintf("encode-%s-sched.db", base.Add(time.Duration(i)*time.Second).Format("20060102-150405"))
		fake.objects[name] = []byte("old")
	}
	e.server.OffsiteDestinations = func(st *model.Settings) ([]offsite.Destination, []offsite.Result) {
		return []offsite.Destination{fake}, nil
	}

	resp, body := doJSON(t, "POST", ts+"/api/backup", adminTok, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("manual backup: %d %s", resp.StatusCode, body)
	}
	var info BackupInfo
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatal(err)
	}

	events, err := e.server.Store.ListAudit(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var detail string
	for _, ev := range events {
		if ev.Action == "backup.offsite" {
			detail = ev.Detail
		}
	}
	if detail == "" {
		t.Fatalf("missing backup.offsite audit: %v", auditActions(t, e))
	}
	if len(detail) > 1024 {
		t.Fatalf("audit detail exceeds the 1024-byte cap (%d bytes) — it will be truncated: %s", len(detail), detail)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(detail), &m); err != nil {
		t.Fatalf("audit detail is not valid JSON (truncated?): %v\n%s", err, detail)
	}
	if m["name"] != info.Name {
		t.Errorf("snapshot name dropped from audit detail: %v", m)
	}
	if m["destination"] != "s3" {
		t.Errorf("destination dropped from audit detail: %v", m)
	}
	if m["shares_key_bundled"] != true {
		t.Errorf("shares_key_bundled dropped from audit detail: %v", m)
	}
	pruned, ok := m["pruned"].([]any)
	if !ok {
		t.Fatalf("pruned list missing/not an array: %v", m)
	}
	// 20 kept names + one "...and N more" summary entry.
	if len(pruned) != 21 {
		t.Fatalf("pruned list must be summarized to 20 names + summary, got %d entries", len(pruned))
	}
	wantSummary := fmt.Sprintf("...and %d more", oldGens-20)
	if pruned[20] != wantSummary {
		t.Errorf("summary entry = %v, want %q", pruned[20], wantSummary)
	}
	for _, p := range pruned[:20] {
		s, _ := p.(string)
		if !strings.HasPrefix(s, "encode-2025") {
			t.Errorf("kept pruned entry is not a snapshot name: %v", p)
		}
	}
}

// TestIngestAuditNameCapped: a 1024-rune torrent name must be capped at 200
// runes in the ingest.staged / ingest.duplicate audit details so series and
// staged_path survive the 1024-byte cap. The full name stays in the ledger
// and structured logs (not asserted here — those paths are unchanged).
func TestIngestAuditNameCapped(t *testing.T) {
	e := newTestEnv(t)
	// Intercept alert fan-out (the env default webhook is non-empty).
	e.server.Notifier = &recordingNotifier{}
	longName := strings.Repeat("N", 1024)
	stagedPath := "/data/scripts/Show/Ep 01"

	e.server.onIngestStaged(ingest.StagedEvent{
		Provider: "qbittorrent", Hash: "cap-hash", Name: longName,
		Series: "Show", Episode: "01", StagedPath: stagedPath,
		Files: []string{"source.mkv"},
	})
	e.server.onIngestStaged(ingest.StagedEvent{
		Provider: "qbittorrent", Hash: "dup-hash", Name: longName,
		Series: "Show", Episode: "01", StagedPath: stagedPath,
		// zero files → ingest.duplicate
	})

	events, err := e.server.Store.ListAudit(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	byAction := map[string]string{}
	for _, ev := range events {
		if ev.Action == "ingest.staged" || ev.Action == "ingest.duplicate" {
			byAction[ev.Action] = ev.Detail
		}
	}
	for _, action := range []string{"ingest.staged", "ingest.duplicate"} {
		detail := byAction[action]
		if detail == "" {
			t.Fatalf("missing %s audit: %v", action, auditActions(t, e))
		}
		if len(detail) > 1024 {
			t.Fatalf("%s detail exceeds the 1024-byte cap (%d bytes): %s", action, len(detail), detail)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(detail), &m); err != nil {
			t.Fatalf("%s detail is not valid JSON (truncated?): %v\n%s", action, err, detail)
		}
		if m["series"] != "Show" {
			t.Errorf("%s: series dropped from audit detail: %v", action, m)
		}
		if m["staged_path"] != stagedPath {
			t.Errorf("%s: staged_path dropped from audit detail: %v", action, m)
		}
		wantName := strings.Repeat("N", 200) + "…"
		if m["name"] != wantName {
			t.Errorf("%s: name must be capped at 200 runes + ellipsis, got %d runes", action, len([]rune(m["name"].(string))))
		}
	}
}
