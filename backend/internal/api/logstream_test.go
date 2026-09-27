package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestLogHubSubscribeReceivesPushes verifies the hub's basic fan-out: a
// subscriber receives events published for its job and nothing for others.
func TestLogHubSubscribeReceivesPushes(t *testing.T) {
	h := newLogHub()
	ch, unsub := h.subscribe(7)
	defer unsub()

	h.publish(7, "step=encode 40")
	h.publish(8, "other job") // must not appear on ch

	select {
	case ev := <-ch:
		if ev != "step=encode 40" {
			t.Fatalf("event = %q", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event received")
	}
	select {
	case ev := <-ch:
		t.Fatalf("received foreign-job event %q", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestLogHubSlowSubscriberDropped verifies a stalled subscriber does not
// block publishers (the hub drops events for full channels instead).
func TestLogHubSlowSubscriberDropped(t *testing.T) {
	h := newLogHub()
	ch, unsub := h.subscribe(1)
	defer unsub()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < logStreamChanCap*3; i++ {
			h.publish(1, "x")
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked on slow subscriber")
	}
	_ = <-ch // drain one; rest were dropped or buffered
}

// TestJobLogStreamSSE verifies the streaming endpoint: it sends an initial
// snapshot event, live heartbeat updates flow through as they arrive, and the
// stream closes with a final event when the job reaches a terminal state.
func TestJobLogStreamSSE(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := context.Background()
	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "S", Episode: "01", EpisodeDir: "S/Ep 01",
		ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.UpdateJobStatus(ctx, job.ID, model.JobRunning, "dgindex", 10, "first tail line"); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest("GET", ts.URL+"/api/jobs/"+strconv.FormatInt(job.ID, 10)+"/log/stream", nil)
	req.Header.Set("Authorization", "Bearer "+adminTok)
	// Cancel after a bounded window so the test terminates even if the
	// handler fails to close the stream on its own.
	ctxReq, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req = req.WithContext(ctxReq)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	// Read events in the background; push a heartbeat update and a finish.
	events := make(chan string, 16)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				events <- string(buf[:n])
			}
			if err != nil {
				close(events)
				return
			}
		}
	}()

	// 1. Initial snapshot must arrive (carries the persisted log tail).
	waitForSSE(t, events, "first tail line")

	// 2. A heartbeat progress update flows through live.
	hb := heartbeat("enc-01", 1, job.ID)
	hb.JobStatus = "running"
	hb.Step = "encode"
	hb.StepProgress = 55
	hb.LogTail = "live encoder output"
	if _, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, hb); false {
		t.Fatalf("heartbeat: %s", body)
	}
	waitForSSE(t, events, "live encoder output")

	// 3. Completion closes the stream with a final event.
	rep := map[string]any{"status": "done", "exit_code": 0, "outputs": []string{}, "log_tail": "done tail", "log_full": "full done log"}
	b, _ := json.Marshal(rep)
	cReq, _ := http.NewRequest("POST", ts.URL+"/api/agent/job/"+strconv.FormatInt(job.ID, 10)+"/complete", strings.NewReader(string(b)))
	cReq.Header.Set("Authorization", "Bearer "+e.token)
	cReq.Header.Set("Content-Type", "application/json")
	cResp, err := http.DefaultClient.Do(cReq)
	if err != nil {
		t.Fatal(err)
	}
	cResp.Body.Close()

	waitForSSE(t, events, "done")
	// Stream must close on its own after the terminal event.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return // closed: success
			}
		case <-deadline:
			t.Fatal("stream did not close after job finished")
		}
	}
}

// TestJobLogStreamRequiresAdmin verifies the endpoint rejects anonymous and
// node-token callers (log content can hold paths/errors — admin only).
func TestJobLogStreamRequiresAdmin(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := context.Background()
	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "S", Episode: "01", EpisodeDir: "S/Ep 01",
		ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	url := ts.URL + "/api/jobs/" + strconv.FormatInt(job.ID, 10) + "/log/stream"

	req, _ := http.NewRequest("GET", url, nil)
	ctxReq, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctxReq))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous status = %d, want 401", resp.StatusCode)
	}

	req2, _ := http.NewRequest("GET", url, nil)
	req2.Header.Set("Authorization", "Bearer "+e.token) // node token, not admin
	ctxReq2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	resp2, err := http.DefaultClient.Do(req2.WithContext(ctxReq2))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 403 && resp2.StatusCode != 401 {
		t.Fatalf("node-token status = %d, want 401/403", resp2.StatusCode)
	}
}

// waitForSSE accumulates raw SSE chunks until want appears (or fails after a
// deadline). Raw chunk reads may split event frames arbitrarily, so matching
// is done on the accumulated buffer.
func waitForSSE(t *testing.T, events <-chan string, want string) {
	t.Helper()
	var acc strings.Builder
	deadline := time.After(8 * time.Second)
	for {
		if strings.Contains(acc.String(), want) {
			return
		}
		select {
		case chunk, ok := <-events:
			if !ok {
				t.Fatalf("stream closed before seeing %q (got: %s)", want, acc.String())
			}
			acc.WriteString(chunk)
		case <-deadline:
			t.Fatalf("timed out waiting for %q in SSE stream (got: %s)", want, acc.String())
		}
	}
}
