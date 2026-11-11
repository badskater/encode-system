//go:build linux

package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestHeartbeatCancelKillsAndDeregisters is the F5 agent regression: a
// "cancel" heartbeat reply must kill the job's process tree, deregister it
// (freeing the slot so a re-dispatch is no longer deduped against the stuck
// run), and send NO completion report — the controller already finalized
// the job server-side.
func TestHeartbeatCancelKillsAndDeregisters(t *testing.T) {
	dir := t.TempDir()
	var completes atomic.Int32
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/complete") {
			completes.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			w.WriteHeader(200)
			w.Write([]byte(`{"instruction":"cancel","cancel_job_ids":[42]}`))
			return
		}
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer controller.Close()

	a, err := New(Config{ControllerURL: controller.URL, NodeName: "n", Token: nodeTok(), DataDir: dir}, "v", testLog())
	if err != nil {
		t.Fatal(err)
	}
	job := &model.JobPayload{
		ID:     42,
		Script: "#!/usr/bin/env bash\nsleep 30\n",
		Vars:   map[string]string{},
		Flow:   "cancel-test",
	}
	done := make(chan struct{})
	go func() { defer close(done); a.executeJobWithLimits(job, jobLimits{}) }()

	// Wait for the job to register and the script to actually start.
	deadline := time.Now().Add(5 * time.Second)
	for a.activeCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if a.activeCount() != 1 {
		t.Fatal("job never registered")
	}
	time.Sleep(300 * time.Millisecond) // let bash start so the kill hits a live tree

	if err := a.heartbeat(context.Background()); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	if got := a.activeCount(); got != 0 {
		t.Fatalf("registry count = %d after cancel, want 0 (slot must be freed)", got)
	}
	// executeJob returns only once cmd.Wait reaps the killed process —
	// proof the process tree actually died, not just the bookkeeping.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("executeJob did not return after cancel — process was not killed")
	}
	if got := completes.Load(); got != 0 {
		t.Fatalf("cancelled job sent %d completion reports, want 0 (controller already finalized)", got)
	}
	// The registry dedup must no longer block a re-dispatch of the same id.
	if !a.acceptJobs([]*model.JobPayload{{ID: 42}}) {
		t.Fatal("re-dispatch after cancel was deduped against the stuck run")
	}
	a.finishJob(42) // drop the re-dispatch stub
}

// TestHeartbeatUnknownInstructionIsGraceful guards the rolling-upgrade
// contract: an instruction this agent does not know (a NEWER controller's
// future instruction) must be ignored without error, exactly as old agents
// ignore "cancel".
func TestHeartbeatUnknownInstructionIsGraceful(t *testing.T) {
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"instruction":"frobnicate","cancel_job_ids":[7]}`))
	}))
	defer controller.Close()
	a, err := New(Config{ControllerURL: controller.URL, NodeName: "n", Token: nodeTok(), DataDir: t.TempDir()}, "v", testLog())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.heartbeat(context.Background()); err != nil {
		t.Fatalf("unknown instruction must be graceful, got error: %v", err)
	}
	if got := a.activeCount(); got != 0 {
		t.Fatalf("unknown instruction disturbed the registry: %d jobs", got)
	}
}
