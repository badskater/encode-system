package api

import (
	"encoding/json"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestConcurrentDispatch verifies a node with max_concurrent_jobs=2 gets
// BOTH slots filled in one heartbeat reply ("jobs" instruction), and the
// next heartbeat assigns nothing while both run. Default max=1 nodes keep
// the legacy single "job" instruction.
func TestConcurrentDispatch(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	if err := e.server.Store.SetNodeMaxConcurrent(ctx, e.node.ID, 2); err != nil {
		t.Fatal(err)
	}
	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range []string{"01", "02", "03"} {
		if _, err := e.server.Store.CreateJob(ctx, &model.Job{
			Series: "Conc Show", Episode: ep, EpisodeDir: "Conc Show/Ep " + ep,
			ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending,
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 1, 0))
	var reply model.HeartbeatReply
	json.Unmarshal(body, &reply)
	if reply.Instruction != "jobs" || len(reply.Jobs) != 2 {
		t.Fatalf("reply = %+v, want jobs x2", reply)
	}
	if reply.Job == nil || reply.Job.ID != reply.Jobs[0].ID {
		t.Fatal("legacy Job field must mirror Jobs[0]")
	}

	// Both slots occupied: an idle heartbeat with both reported assigns nothing.
	hb := heartbeat("enc-01", 2, 0)
	hb.Jobs = []model.HeartbeatJobReport{
		{JobID: reply.Jobs[0].ID, JobStatus: "running", Step: "encode"},
		{JobID: reply.Jobs[1].ID, JobStatus: "running", Step: "encode"},
	}
	_, body = doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, hb)
	reply = model.HeartbeatReply{}
	json.Unmarshal(body, &reply)
	if reply.Instruction != "none" {
		t.Fatalf("at capacity got %q, want none", reply.Instruction)
	}

	// One job finishes: exactly one new assignment (single "job" reply).
	if err := e.server.Store.FinishJob(ctx, hb.Jobs[0].JobID, model.JobDone, 0, "", nil, ""); err != nil {
		t.Fatal(err)
	}
	hb2 := heartbeat("enc-01", 3, 0)
	hb2.Jobs = []model.HeartbeatJobReport{
		{JobID: hb.Jobs[1].JobID, JobStatus: "running", Step: "mux"},
	}
	_, body = doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, hb2)
	reply = model.HeartbeatReply{}
	json.Unmarshal(body, &reply)
	if reply.Instruction != "job" || reply.Job == nil {
		t.Fatalf("after finish got %q, want single job", reply.Instruction)
	}
}

// TestSetBasedOrphanRecovery verifies that with two running jobs, dropping
// ONE from the heartbeat fails only that job; the still-reported survivor
// keeps running.
func TestSetBasedOrphanRecovery(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	if err := e.server.Store.SetNodeMaxConcurrent(ctx, e.node.ID, 2); err != nil {
		t.Fatal(err)
	}
	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	j1, _ := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "Orph Show", Episode: "01", EpisodeDir: "Orph Show/Ep 01",
		ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending})
	j2, _ := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "Orph Show", Episode: "02", EpisodeDir: "Orph Show/Ep 02",
		ScriptType: "avs", FlowID: fl.ID, Status: model.JobPending})
	for _, j := range []*model.Job{j1, j2} {
		if err := e.server.Store.AssignJob(ctx, j.ID, e.node.ID); err != nil {
			t.Fatal(err)
		}
		// Acknowledge both so they reach 'running' (orphan rule only fires
		// on acknowledged jobs).
		if err := e.server.Store.UpdateJobStatus(ctx, j.ID, model.JobRunning, "encode", 10, ""); err != nil {
			t.Fatal(err)
		}
	}

	// Heartbeat reports ONLY j2 — j1 vanished (its process died).
	hb := heartbeat("enc-01", 2, 0)
	hb.Jobs = []model.HeartbeatJobReport{{JobID: j2.ID, JobStatus: "running", Step: "encode", StepProgress: 50}}
	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, hb)
	if resp.StatusCode != 200 {
		t.Fatalf("heartbeat: %d %s", resp.StatusCode, body)
	}

	got1, _ := e.server.Store.GetJob(ctx, j1.ID)
	if got1.Status != model.JobFailed && got1.Status != model.JobPending {
		// Flows in the test env have no retry policy → failed; with one it
		// would requeue to pending. Either way it must NOT stay running.
		t.Fatalf("orphan j1 status = %s, want failed/pending", got1.Status)
	}
	got2, _ := e.server.Store.GetJob(ctx, j2.ID)
	if got2.Status != model.JobRunning {
		t.Fatalf("survivor j2 status = %s, want running", got2.Status)
	}
}

// TestPatchNodeMaxConcurrent verifies the PATCH field round-trips and
// rejects out-of-range values with 400.
func TestPatchNodeMaxConcurrent(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "PATCH", ts.URL+"/api/nodes/"+i64s(e.node.ID), adminTok, map[string]any{"max_concurrent_jobs": 3})
	if resp.StatusCode != 200 {
		t.Fatalf("patch: %d %s", resp.StatusCode, body)
	}
	var got model.Node
	json.Unmarshal(body, &got)
	if got.MaxConcurrentJobs != 3 {
		t.Fatalf("max = %d, want 3", got.MaxConcurrentJobs)
	}
	for _, bad := range []int{0, -1, 9} {
		resp, _ = doJSON(t, "PATCH", ts.URL+"/api/nodes/"+i64s(e.node.ID), adminTok, map[string]any{"max_concurrent_jobs": bad})
		if resp.StatusCode != 400 {
			t.Fatalf("max=%d accepted (%d), want 400", bad, resp.StatusCode)
		}
	}
}
