package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/auth"
	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/store"
	"github.com/badskater/encode-system/backend/internal/update"
)

const adminTok = "admin-test-token"

// testEnv wires a Server backed by temp-dir state for httptest runs.
type testEnv struct {
	srv    *http.Server
	server *Server
	node   *model.Node
	token  string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	up, err := update.NewStore(dir + "/updates")
	if err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		AdminToken: adminTok, ScriptsRoot: dir + "/scripts", ReleaseRoot: dir + "/release",
		NodeBinDir: `C:\bin`, NodeScriptsDir: `C:\Encodes\scripts`, NodeReleaseDir: `C:\Encodes\ReleaseFolders`,
		Group: "OldFartsSubs", Tag: "1080p", TasksBeforeReboot: 10,
	}
	s, err := New(st, up, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Register a node with a known token.
	token, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.CreateNode(ctxBg(), "enc-01", auth.HashToken(token))
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)
	// Store base URL on the server for request building via a wrapper.
	return &testEnv{server: s, node: node, token: token}
}

// envRoutes serves the API through an httptest server per call set.
func (e *testEnv) serve(t *testing.T) *httptest.Server {
	ts := httptest.NewServer(e.server.Routes())
	t.Cleanup(ts.Close)
	return ts
}

func doJSON(t *testing.T, method, url, token string, body any) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func heartbeat(node string, tasks int, jobID int64) model.Heartbeat {
	return model.Heartbeat{Node: node, AgentVersion: "0.1.0", LibVersion: 1, TasksSinceBoot: tasks, JobID: jobID}
}

func TestHeartbeatIdleNodeGetsNoInstruction(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 1, 0))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var reply model.HeartbeatReply
	json.Unmarshal(body, &reply)
	if reply.Instruction != "none" {
		t.Fatalf("want none, got %s (%s)", reply.Instruction, body)
	}
}

func TestHeartbeatRejectsBadToken(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	resp, _ := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", "wrong-token", heartbeat("enc-01", 0, 0))
	if resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func TestHeartbeatAssignsPendingJob(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Create a job via the store using the seeded default flow.
	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 1, 0))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var reply model.HeartbeatReply
	json.Unmarshal(body, &reply)
	if reply.Instruction != "job" {
		t.Fatalf("want job instruction, got %q (%s)", reply.Instruction, body)
	}
	if reply.Job == nil || reply.Job.ID != job.ID {
		t.Fatalf("wrong job payload: %+v", reply.Job)
	}
	if len(reply.Job.Script) == 0 || reply.Job.Vars["series"] != "S" {
		t.Fatalf("payload incomplete: %+v", reply.Job)
	}

	// Job must now be assigned to our node.
	got, _ := e.server.Store.GetJob(ctx, job.ID)
	if got.Status != model.JobAssigned || got.NodeID != e.node.ID {
		t.Fatalf("job not assigned: %+v", got)
	}
}

func TestRebootIssuedAtTaskLimit(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 10, 0))
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	var reply model.HeartbeatReply
	json.Unmarshal(body, &reply)
	if reply.Instruction != "reboot" {
		t.Fatalf("want reboot at limit, got %q (%s)", reply.Instruction, body)
	}
	if reply.RebootDelay != 30 {
		t.Errorf("reboot delay = %d", reply.RebootDelay)
	}

	// Node must be flagged so it receives no further work.
	n, _ := e.server.Store.GetNode(ctxBg(), e.node.ID)
	if !n.RebootPending {
		t.Fatal("reboot_pending not persisted")
	}
	_, body2 := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 10, 0))
	var reply2 model.HeartbeatReply
	json.Unmarshal(body2, &reply2)
	if reply2.Instruction == "job" {
		t.Fatal("node at reboot limit must not receive jobs")
	}
}

func TestDisabledNodeGetsNothing(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	e.server.Store.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID})

	// Disable the node via the UI API.
	n, _ := e.server.Store.GetNode(ctx, e.node.ID)
	f := false
	n.Enabled = f
	e.server.Store.UpdateNode(ctx, n)

	_, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 0, 0))
	var reply model.HeartbeatReply
	json.Unmarshal(body, &reply)
	if reply.Instruction != "none" {
		t.Fatalf("disabled node must get none, got %q", reply.Instruction)
	}
}

func TestJobCompletionFlow(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	job, _ := e.server.Store.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID})
	e.server.Store.AssignJob(ctx, job.ID, e.node.ID)

	// Complete via agent endpoint.
	rep := map[string]any{"status": "done", "exit_code": 0, "outputs": []string{"S - 01 [1080p].mkv"}, "log_tail": "ENCODE_JOB_DONE"}
	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/job/"+itoa(job.ID)+"/complete", e.token, rep)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	got, _ := e.server.Store.GetJob(ctx, job.ID)
	if got.Status != model.JobDone || len(got.Outputs) != 1 {
		t.Fatalf("job not finished: %+v", got)
	}
	n, _ := e.server.Store.GetNode(ctx, e.node.ID)
	if n.Status != model.NodeIdle {
		t.Fatalf("node not released: %+v", n)
	}
}

func TestJobCompletionByWrongNodeForbidden(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	other, err := e.server.Store.CreateNode(ctx, "enc-02", auth.HashToken("tok2"))
	if err != nil {
		t.Fatal(err)
	}
	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	job, _ := e.server.Store.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID})
	e.server.Store.AssignJob(ctx, job.ID, other.ID)

	resp, _ := doJSON(t, "POST", ts.URL+"/api/agent/job/"+itoa(job.ID)+"/complete", e.token, map[string]any{"status": "done"})
	if resp.StatusCode != 403 {
		t.Fatalf("want 403, got %d", resp.StatusCode)
	}
}

func TestAdminAuthRequired(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	resp, _ := doJSON(t, "GET", ts.URL+"/api/jobs", "", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("want 401 without token, got %d", resp.StatusCode)
	}
	resp2, _ := doJSON(t, "GET", ts.URL+"/api/jobs", "bad", nil)
	if resp2.StatusCode != 401 {
		t.Fatalf("want 401 with bad token, got %d", resp2.StatusCode)
	}
}

func TestUIFlowCRUDViaAPI(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// Create a flow through the API (validation runs render).
	req := model.Flow{Name: "opus-fast", Steps: []model.Step{
		{Type: model.StepDGIndex},
		{Type: model.StepAudio, Params: map[string]string{"bitrate": "160"}},
	}}
	resp, body := doJSON(t, "POST", ts.URL+"/api/flows", adminTok, req)
	if resp.StatusCode != 201 {
		t.Fatalf("create flow: %d %s", resp.StatusCode, body)
	}
	var created model.Flow
	json.Unmarshal(body, &created)
	if created.ID == 0 {
		t.Fatal("flow id missing")
	}

	// Invalid flow (unknown step) must be rejected before persistence.
	bad := model.Flow{Name: "bad", Steps: []model.Step{{Type: "teleport"}}}
	resp2, _ := doJSON(t, "POST", ts.URL+"/api/flows", adminTok, bad)
	if resp2.StatusCode != 400 {
		t.Fatalf("want 400 for invalid flow, got %d", resp2.StatusCode)
	}

	// List includes seeded default + the new flow.
	resp3, body3 := doJSON(t, "GET", ts.URL+"/api/flows", adminTok, nil)
	if resp3.StatusCode != 200 {
		t.Fatal(resp3.StatusCode)
	}
	var flows []model.Flow
	json.Unmarshal(body3, &flows)
	if len(flows) != 2 {
		t.Fatalf("want 2 flows, got %d", len(flows))
	}
}

func TestHealthEndpoint(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	resp, body := doJSON(t, "GET", ts.URL+"/api/health", "", nil)
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	if !bytes.Contains(body, []byte("ok")) {
		t.Fatalf("health body: %s", body)
	}
}

func TestNodeRegistrationViaAPI(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	resp, body := doJSON(t, "POST", ts.URL+"/api/nodes", adminTok, map[string]string{"name": "enc-09"})
	if resp.StatusCode != 201 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Node  model.Node `json:"node"`
		Token string     `json:"token"`
	}
	json.Unmarshal(body, &out)
	if out.Token == "" || out.Node.Name != "enc-09" {
		t.Fatalf("registration payload wrong: %+v", out)
	}
	// Duplicate name must conflict.
	resp2, _ := doJSON(t, "POST", ts.URL+"/api/nodes", adminTok, map[string]string{"name": "enc-09"})
	if resp2.StatusCode != 409 {
		t.Fatalf("want 409 on duplicate, got %d", resp2.StatusCode)
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

// Regression (adversarial review): the reboot instruction must be RE-ISSUED on
// every idle heartbeat while the flag is set — a missed packet must not brick
// the node forever.
func TestRebootReissuedWhilePending(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	n, _ := e.server.Store.GetNode(ctx, e.node.ID)
	n.RebootPending = true
	n.Status = model.NodeReboot
	issued := time.Now().UTC()
	n.RebootIssuedAt = &issued // fresh attempt: must keep re-issuing
	e.server.Store.UpdateNode(ctx, n)

	for i := 0; i < 3; i++ {
		_, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 0, 0))
		var reply model.HeartbeatReply
		json.Unmarshal(body, &reply)
		if reply.Instruction != "reboot" {
			t.Fatalf("heartbeat %d: want reboot re-issue, got %q (%s)", i, reply.Instruction, body)
		}
	}
}

// Regression: manual reboot via UI reaches the agent on next heartbeat.
func TestManualRebootReachesAgent(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, _ := doJSON(t, "POST", ts.URL+"/api/nodes/"+itoa(e.node.ID)+"/reboot", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("manual reboot api: %d", resp.StatusCode)
	}
	_, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 0, 0))
	var reply model.HeartbeatReply
	json.Unmarshal(body, &reply)
	if reply.Instruction != "reboot" {
		t.Fatalf("want reboot after manual request, got %q", reply.Instruction)
	}
}

// Regression: after a real reboot the agent's counter resets to 0 and the
// controller must clear the flag so the node rejoins the pool.
func TestPostRebootRecoveryClearsFlag(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	n, _ := e.server.Store.GetNode(ctx, e.node.ID)
	n.RebootPending = true
	n.Status = model.NodeReboot
	n.TasksSinceBoot = 10 // counter at the moment the reboot was issued
	e.server.Store.UpdateNode(ctx, n)

	// Agent comes back with a fresh counter after the reboot.
	_, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 0, 0))
	var reply model.HeartbeatReply
	json.Unmarshal(body, &reply)
	if reply.Instruction == "reboot" {
		t.Fatal("node with reset counter must not keep receiving reboot")
	}
	n2, _ := e.server.Store.GetNode(ctx, e.node.ID)
	if n2.RebootPending {
		t.Fatal("RebootPending must be cleared after reboot recovery")
	}
}

// Regression: a node cannot update another node's job via heartbeat.
func TestHeartbeatRejectsForeignJobStatus(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	other, _ := e.server.Store.CreateNode(ctx, "enc-02", auth.HashToken("otherval"))
	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	job, _ := e.server.Store.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID})
	e.server.Store.AssignJob(ctx, job.ID, other.ID)

	hb := heartbeat("enc-01", 0, job.ID)
	hb.JobStatus = "failed"
	doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, hb)

	got, _ := e.server.Store.GetJob(ctx, job.ID)
	if got.Status != model.JobAssigned {
		t.Fatalf("foreign node overwrote job status: %+v", got)
	}
}

// Regression: duplicate completion reports are absorbed, not re-applied.
func TestJobCompleteIdempotent(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	job, _ := e.server.Store.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID})
	e.server.Store.AssignJob(ctx, job.ID, e.node.ID)

	rep := map[string]any{"status": "done", "exit_code": 0}
	resp1, _ := doJSON(t, "POST", ts.URL+"/api/agent/job/"+itoa(job.ID)+"/complete", e.token, rep)
	if resp1.StatusCode != 200 {
		t.Fatal(resp1.StatusCode)
	}
	// Simulate an agent retry after a timeout — now the job is terminal.
	rep2 := map[string]any{"status": "failed", "exit_code": 9, "error": "retry"}
	resp2, body2 := doJSON(t, "POST", ts.URL+"/api/agent/job/"+itoa(job.ID)+"/complete", e.token, rep2)
	if resp2.StatusCode != 200 {
		t.Fatalf("idempotent retry should 200, got %d", resp2.StatusCode)
	}
	if !bytes.Contains(body2, []byte("already_recorded")) {
		t.Fatalf("expected already_recorded marker: %s", body2)
	}
	got, _ := e.server.Store.GetJob(ctx, job.ID)
	if got.Status != model.JobDone {
		t.Fatalf("retry must not flip done -> failed: %+v", got)
	}
}

// Regression: cancelling an assigned job frees its node.
func TestCancelAssignedJobReleasesNode(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	job, _ := e.server.Store.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID})
	e.server.Store.AssignJob(ctx, job.ID, e.node.ID)

	resp, _ := doJSON(t, "POST", ts.URL+"/api/jobs/"+itoa(job.ID)+"/cancel", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("cancel assigned: %d", resp.StatusCode)
	}
	got, _ := e.server.Store.GetJob(ctx, job.ID)
	if got.Status != model.JobCancelled {
		t.Fatalf("job not cancelled: %+v", got)
	}
	n, _ := e.server.Store.GetNode(ctx, e.node.ID)
	if n.Status != model.NodeIdle {
		t.Fatalf("node not released after cancel: %+v", n)
	}
}

// Regression: the default flow cannot be deleted through the API.
func TestDeleteDefaultFlowRefused(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	def, _ := e.server.Store.FlowByName(ctx, "default-1080")
	resp, _ := doJSON(t, "DELETE", ts.URL+"/api/flows/"+itoa(def.ID), adminTok, nil)
	if resp.StatusCode != 409 {
		t.Fatalf("deleting default flow must be refused, got %d", resp.StatusCode)
	}
}

// Regression (live smoke): a node with a reboot flag but NO issue timestamp
// (e.g. flag set by an older controller version) must not be locked out
// forever — the grace-period expiry resets the attempt and, with a reset
// counter, the node rejoins the pool and receives pending jobs.
func TestStaleRebootFlagExpiresAndNodeRejoins(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	n, _ := e.server.Store.GetNode(ctx, e.node.ID)
	n.RebootPending = true
	n.Status = model.NodeReboot
	n.TasksSinceBoot = 0 // agent already reset its counter
	n.RebootIssuedAt = nil
	e.server.Store.UpdateNode(ctx, n)

	// A pending job must be pickable once the flag expires.
	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	job, _ := e.server.Store.CreateJob(ctx, &model.Job{Series: "S", Episode: "03", EpisodeDir: "S/Ep 03", ScriptType: "vpy", FlowID: fl.ID})

	_, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 0, 0))
	var reply model.HeartbeatReply
	json.Unmarshal(body, &reply)
	if reply.Instruction != "job" {
		t.Fatalf("want job after stale reboot flag expires, got %q (%s)", reply.Instruction, body)
	}
	if reply.Job.ID != job.ID {
		t.Fatalf("wrong job: %+v", reply.Job)
	}
	n2, _ := e.server.Store.GetNode(ctx, e.node.ID)
	if n2.RebootPending {
		t.Fatal("stale reboot flag must be cleared")
	}
}

// Regression: reboot attempt timestamp persists and blocks expiry while fresh.
func TestRebootFlagHeldWithinGracePeriod(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	n, _ := e.server.Store.GetNode(ctx, e.node.ID)
	n.RebootPending = true
	n.Status = model.NodeReboot
	n.TasksSinceBoot = 10
	issued := time.Now().UTC()
	n.RebootIssuedAt = &issued // fresh attempt — must NOT expire
	e.server.Store.UpdateNode(ctx, n)

	_, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 10, 0))
	var reply model.HeartbeatReply
	json.Unmarshal(body, &reply)
	if reply.Instruction != "reboot" {
		t.Fatalf("fresh reboot attempt must keep issuing reboot, got %q", reply.Instruction)
	}
	n2, _ := e.server.Store.GetNode(ctx, e.node.ID)
	if !n2.RebootPending {
		t.Fatal("fresh reboot flag must persist")
	}
}
