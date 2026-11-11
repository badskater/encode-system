package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// F5 regressions: the stall watchdog fails jobs server-side, but an online
// node keeps the wedged process — and an auto-retry re-dispatched to the
// same node is deduped by the agent registry against the stuck run. The
// watchdog must queue a cancel instruction for the node's next heartbeat.

// setNodeLastSeen backdates/refreshes a node's heartbeat stamp so the
// watchdog's online check (same StaleAfter rule as handleListNodes) sees a
// controlled state.
func setNodeLastSeen(t *testing.T, e *testEnv, at time.Time) {
	t.Helper()
	n, err := e.server.Store.GetNode(ctxBg(), e.node.ID)
	if err != nil {
		t.Fatal(err)
	}
	n.LastSeen = &at
	if err := e.server.Store.UpdateNode(ctxBg(), n); err != nil {
		t.Fatal(err)
	}
}

// heartbeatReplyFor sends one agent heartbeat and decodes the reply.
func heartbeatReplyFor(t *testing.T, e *testEnv, tsURL string) model.HeartbeatReply {
	t.Helper()
	resp, body := doJSON(t, "POST", tsURL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 1, 0))
	if resp.StatusCode != 200 {
		t.Fatalf("heartbeat: %d %s", resp.StatusCode, body)
	}
	var reply model.HeartbeatReply
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

// TestStallWatchdogQueuesCancelForOnlineNode: failing a stalled job on an
// ONLINE node queues the job ID, the next heartbeat reply carries the
// cancel instruction with that ID, and the queue is drained (second
// heartbeat is back to none).
func TestStallWatchdogQueuesCancelForOnlineNode(t *testing.T) {
	e, _, job := stallEnv(t, 0)
	ts := e.serve(t)
	enableStallWatchdog(t, e, 30)
	setNodeLastSeen(t, e, time.Now().UTC()) // node is online

	e.server.stallWatchdogTick(ctxBg())

	got, _ := e.server.Store.GetJob(ctxBg(), job.ID)
	if got.Status != model.JobFailed {
		t.Fatalf("job status = %s, want failed", got.Status)
	}
	reply := heartbeatReplyFor(t, e, ts.URL)
	if reply.Instruction != "cancel" {
		t.Fatalf("heartbeat instruction = %q, want cancel (wedged run must be stopped)", reply.Instruction)
	}
	if len(reply.CancelJobIDs) != 1 || reply.CancelJobIDs[0] != job.ID {
		t.Fatalf("cancel_job_ids = %v, want [%d]", reply.CancelJobIDs, job.ID)
	}
	// Drained once sent — the next heartbeat must not repeat the cancel.
	reply2 := heartbeatReplyFor(t, e, ts.URL)
	if reply2.Instruction == "cancel" || len(reply2.CancelJobIDs) != 0 {
		t.Fatalf("cancel re-sent after drain: %+v", reply2)
	}
}

// TestStallWatchdogNoCancelForOfflineNode: an offline node's agent is not
// heartbeating — no cancel is queued (orphan recovery owns that path).
func TestStallWatchdogNoCancelForOfflineNode(t *testing.T) {
	e, _, job := stallEnv(t, 0)
	ts := e.serve(t)
	enableStallWatchdog(t, e, 30)
	setNodeLastSeen(t, e, time.Now().UTC().Add(-2*time.Hour)) // offline

	e.server.stallWatchdogTick(ctxBg())

	got, _ := e.server.Store.GetJob(ctxBg(), job.ID)
	if got.Status != model.JobFailed {
		t.Fatalf("job status = %s, want failed", got.Status)
	}
	reply := heartbeatReplyFor(t, e, ts.URL)
	if reply.Instruction == "cancel" || len(reply.CancelJobIDs) != 0 {
		t.Fatalf("cancel queued for an offline node: %+v", reply)
	}
}

// TestPendingCancelsClearedOnOfflineTransition: a queued cancel is dropped
// when the node goes offline (detected by the node-alert loop's transition
// diff), so unreachable nodes never accumulate unbounded cancel state.
func TestPendingCancelsClearedOnOfflineTransition(t *testing.T) {
	e, _, _ := stallEnv(t, 0)
	ts := e.serve(t)
	enableStallWatchdog(t, e, 30)
	ctx := ctxBg()
	setNodeLastSeen(t, e, time.Now().UTC()) // online

	e.server.stallWatchdogTick(ctxBg()) // queues the cancel
	e.server.nodeAlertTick(ctx)         // baseline: node online

	// Node drops off the network; the alert loop observes the transition.
	setNodeLastSeen(t, e, time.Now().UTC().Add(-2*time.Hour))
	e.server.nodeAlertTick(ctx)

	reply := heartbeatReplyFor(t, e, ts.URL)
	if reply.Instruction == "cancel" || len(reply.CancelJobIDs) != 0 {
		t.Fatalf("stale cancel survived the offline transition: %+v", reply)
	}
}
