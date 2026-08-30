package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestFinishJobWithReportPersistsLogAndTimings is the v2 completion path:
// FinishJobWithReport must persist full_log and step_timings_json alongside
// the existing finish columns (status/exit_code/error/outputs/log_tail) and
// GetJob must round-trip them exactly — including a 2-entry timings slice
// with exact StartedAt and DurationSec, and the progress=100 stamp for done
// jobs. This pins the controller-side persistence contract the agent's Phase
// A report relies on.
func TestFinishJobWithReportPersistsLogAndTimings(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)

	j, err := s.CreateJob(ctx, &model.Job{
		Series:     "FL",
		Episode:    "01",
		EpisodeDir: "FL/Ep 01",
		ScriptType: "vpy",
		FlowID:     flow.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	wantTimings := []model.StepTiming{
		{Step: "encode", StartedAt: time.Date(2026, 8, 30, 11, 0, 0, 0, time.UTC), DurationSec: 12.5},
		{Step: "mux", StartedAt: time.Date(2026, 8, 30, 11, 0, 12, 500000000, time.UTC), DurationSec: 3.25},
	}
	fullLog := "line one\nline two\nENCODE_JOB_DONE\n"
	outputs := []string{"FL - 01 [1080p].mkv"}

	if err := s.FinishJobWithReport(ctx, j.ID, model.JobDone, 0, "", outputs, "ENCODE_JOB_DONE", fullLog, wantTimings); err != nil {
		t.Fatalf("FinishJobWithReport: %v", err)
	}

	got, err := s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobDone {
		t.Fatalf("status=%q want done", got.Status)
	}
	if got.ExitCode != 0 {
		t.Fatalf("exit_code=%d want 0", got.ExitCode)
	}
	if got.Progress != 100 {
		t.Fatalf("progress=%v want 100 for done job", got.Progress)
	}
	if len(got.Outputs) != 1 || got.Outputs[0] != outputs[0] {
		t.Fatalf("outputs=%+v want %+v", got.Outputs, outputs)
	}
	if got.LogTail != "ENCODE_JOB_DONE" {
		t.Fatalf("log_tail=%q want %q", got.LogTail, "ENCODE_JOB_DONE")
	}
	if got.FullLog != fullLog {
		t.Fatalf("full_log=%q want %q", got.FullLog, fullLog)
	}
	if len(got.StepTimings) != len(wantTimings) {
		t.Fatalf("step_timings len=%d want %d (%+v)", len(got.StepTimings), len(wantTimings), got.StepTimings)
	}
	for i, st := range got.StepTimings {
		w := wantTimings[i]
		if st.Step != w.Step {
			t.Fatalf("step[%d].step=%q want %q", i, st.Step, w.Step)
		}
		if !st.StartedAt.Equal(w.StartedAt) {
			t.Fatalf("step[%d].started_at=%v want %v", i, st.StartedAt, w.StartedAt)
		}
		if st.DurationSec != w.DurationSec {
			t.Fatalf("step[%d].duration_sec=%v want %v", i, st.DurationSec, w.DurationSec)
		}
	}
}

// TestFinishJobWithReportNilTimingsStoresEmpty asserts that a nil timings
// slice (an agent that ran zero timed steps, or a degraded report) is stored
// as the literal JSON '[]' so the column never holds NULL or a bare empty
// string — every read path unmarshals to a len-0 slice, not nil.
func TestFinishJobWithReportNilTimingsStoresEmpty(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)

	j, err := s.CreateJob(ctx, &model.Job{
		Series: "NL", Episode: "01", EpisodeDir: "NL/Ep 01", ScriptType: "vpy", FlowID: flow.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.FinishJobWithReport(ctx, j.ID, model.JobDone, 0, "", nil, "tail", "log", nil); err != nil {
		t.Fatalf("FinishJobWithReport: %v", err)
	}

	// The column must hold the literal '[]' (not NULL, not '').
	var timingsJSON string
	if err := s.db.QueryRowContext(ctx, `SELECT step_timings_json FROM jobs WHERE id=?`, j.ID).Scan(&timingsJSON); err != nil {
		t.Fatalf("select step_timings_json: %v", err)
	}
	if timingsJSON != "[]" {
		t.Fatalf("step_timings_json=%q want %q", timingsJSON, "[]")
	}

	// And GetJob unmarshals it to a len-0 (non-problematic) slice.
	got, err := s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.StepTimings) != 0 {
		t.Fatalf("step_timings len=%d want 0", len(got.StepTimings))
	}
}

// TestFinishJobWithReportCapsFullLogOverOneMiB asserts the controller-side
// guard caps full_log at 1 MiB even if an agent (or a future bug) POSTs a
// larger body. The cap cuts at the first newline at/after the len-1MiB
// boundary and prepends the truncation marker — the same rule as the agent's
// captureRunLog, so both sides agree on shape.
func TestFinishJobWithReportCapsFullLogOverOneMiB(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)

	j, err := s.CreateJob(ctx, &model.Job{
		Series: "BIG", Episode: "01", EpisodeDir: "BIG/Ep 01", ScriptType: "vpy", FlowID: flow.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Build a full log well over 1 MiB: 12-byte lines ("Lxxxxxxxxxx\n").
	const lineLen = 12
	over := (maxFullLogBytes + 64*1024) / lineLen
	var sb strings.Builder
	for i := 0; i < over; i++ {
		sb.WriteString("Lxxxxxxxxxx\n")
	}
	if err := s.FinishJobWithReport(ctx, j.ID, model.JobDone, 0, "", nil, "tail", sb.String(), nil); err != nil {
		t.Fatalf("FinishJobWithReport: %v", err)
	}

	got, err := s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The stored body must be at most 1 MiB + the marker prefix.
	if len(got.FullLog) > maxFullLogBytes+len(truncationMarker) {
		t.Fatalf("full_log not capped: len=%d (cap+marker=%d)", len(got.FullLog), maxFullLogBytes+len(truncationMarker))
	}
	if !strings.HasPrefix(got.FullLog, truncationMarker) {
		t.Fatalf("over-cap full_log must start with marker %q, got prefix %q", truncationMarker, got.FullLog[:min(len(got.FullLog), len(truncationMarker)+20)])
	}
	// The content after the marker must end on a newline boundary.
	body := strings.TrimPrefix(got.FullLog, truncationMarker)
	if len(body) > maxFullLogBytes {
		t.Fatalf("content body over cap: %d > %d", len(body), maxFullLogBytes)
	}
	if len(body) > 0 && body[len(body)-1] != '\n' {
		t.Fatalf("capped body must end on newline; last bytes %q", body[len(body)-min(len(body), 40):])
	}
}

// TestFinishJobWithReportFailedJobKeepsProgress pins the done-only progress=100
// rule from FinishJob: a failed job keeps its last progress (not stamped 100)
// so dashboards do not render failure as completion.
func TestFinishJobWithReportFailedJobKeepsProgress(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	flow := seedFlow(t, s)

	j, err := s.CreateJob(ctx, &model.Job{
		Series: "FJ", Episode: "01", EpisodeDir: "FJ/Ep 01", ScriptType: "vpy", FlowID: flow.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Stamp a partial progress so we can prove it is not overwritten to 100.
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET progress=42 WHERE id=?`, j.ID); err != nil {
		t.Fatal(err)
	}

	if err := s.FinishJobWithReport(ctx, j.ID, model.JobFailed, 9, "encode failed", nil, "tail", "log", nil); err != nil {
		t.Fatalf("FinishJobWithReport: %v", err)
	}
	got, err := s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobFailed {
		t.Fatalf("status=%q want failed", got.Status)
	}
	if got.Progress == 100 {
		t.Fatalf("failed job must not be stamped progress=100, got %v", got.Progress)
	}
}
