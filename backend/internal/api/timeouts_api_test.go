package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// ---------- Flow timeout validation + round-trip ----------

// TestFlowJobTimeoutValidation covers the 0-10080 bound on flow create for
// both the job timeout and per-step timeouts (400 on invalid).
func TestFlowJobTimeoutValidation(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"negative job timeout", map[string]any{
			"name": "bad-neg", "steps": []map[string]any{{"type": "encode"}},
			"job_timeout_minutes": -1,
		}, 400},
		{"job timeout above ceiling", map[string]any{
			"name": "bad-big", "steps": []map[string]any{{"type": "encode"}},
			"job_timeout_minutes": 10081,
		}, 400},
		{"negative step timeout", map[string]any{
			"name": "bad-step", "steps": []map[string]any{{"type": "encode", "timeout_minutes": -5}},
		}, 400},
		{"step timeout above ceiling", map[string]any{
			"name": "bad-step-big", "steps": []map[string]any{{"type": "encode", "timeout_minutes": 10081}},
		}, 400},
		{"ceiling is valid", map[string]any{
			"name": "ok-ceiling", "steps": []map[string]any{{"type": "encode", "timeout_minutes": 10080}},
			"job_timeout_minutes": 10080,
		}, 201},
		{"zero is off and valid", map[string]any{
			"name": "ok-zero", "steps": []map[string]any{{"type": "encode"}},
			"job_timeout_minutes": 0,
		}, 201},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, body := doJSON(t, "POST", ts.URL+"/api/flows", adminTok, c.body)
			if resp.StatusCode != c.want {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, c.want, body)
			}
		})
	}
}

// TestFlowPutTimeoutRoundTrip proves PUT replaces the timeout fields (0 =
// OFF persists) and GET returns them, including per-step timeouts.
func TestFlowPutTimeoutRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "POST", ts.URL+"/api/flows", adminTok, map[string]any{
		"name":                  "rt-flow",
		"steps":                 []map[string]any{{"type": "encode", "timeout_minutes": 30}, {"type": "mux"}},
		"max_retries":           2,
		"retry_backoff_minutes": 10,
		"job_timeout_minutes":   90,
	})
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var created model.Flow
	json.Unmarshal(body, &created)
	if created.JobTimeoutMinutes != 90 || created.Steps[0].TimeoutMinutes != 30 {
		t.Fatalf("create did not persist timeouts: %+v", created)
	}

	// GET (list) returns the fields. There is no single-flow GET route;
	// the list is the read surface the UI uses.
	getFlow := func(id int64) model.Flow {
		t.Helper()
		resp, body := doJSON(t, "GET", ts.URL+"/api/flows", adminTok, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("get flows: %d %s", resp.StatusCode, body)
		}
		var flows []model.Flow
		json.Unmarshal(body, &flows)
		for _, f := range flows {
			if f.ID == id {
				return f
			}
		}
		t.Fatalf("flow %d not in list", id)
		return model.Flow{}
	}
	got := getFlow(created.ID)
	if got.JobTimeoutMinutes != 90 || got.Steps[0].TimeoutMinutes != 30 || got.Steps[1].TimeoutMinutes != 0 {
		t.Fatalf("GET round-trip: %+v", got)
	}

	// PUT replaces: turn everything OFF (0 must persist, not keep stale).
	resp, body = doJSON(t, "PUT", ts.URL+"/api/flows/"+itoa(created.ID), adminTok, map[string]any{
		"name":                  "rt-flow",
		"steps":                 []map[string]any{{"type": "encode"}, {"type": "mux"}},
		"max_retries":           2,
		"retry_backoff_minutes": 10,
		"job_timeout_minutes":   0,
	})
	if resp.StatusCode != 200 {
		t.Fatalf("put off: %d %s", resp.StatusCode, body)
	}
	got = getFlow(created.ID)
	if got.JobTimeoutMinutes != 0 || got.Steps[0].TimeoutMinutes != 0 {
		t.Fatalf("PUT OFF did not persist: %+v", got)
	}

	// PUT validates bounds too.
	resp, body = doJSON(t, "PUT", ts.URL+"/api/flows/"+itoa(created.ID), adminTok, map[string]any{
		"name":                "rt-flow",
		"steps":               []map[string]any{{"type": "encode"}},
		"job_timeout_minutes": 10081,
	})
	if resp.StatusCode != 400 {
		t.Fatalf("PUT invalid timeout: status = %d, want 400: %s", resp.StatusCode, body)
	}

	// PUT a new value back on.
	resp, body = doJSON(t, "PUT", ts.URL+"/api/flows/"+itoa(created.ID), adminTok, map[string]any{
		"name":                "rt-flow",
		"steps":               []map[string]any{{"type": "encode", "timeout_minutes": 45}},
		"job_timeout_minutes": 120,
	})
	if resp.StatusCode != 200 {
		t.Fatalf("put on: %d %s", resp.StatusCode, body)
	}
	got = getFlow(created.ID)
	if got.JobTimeoutMinutes != 120 || got.Steps[0].TimeoutMinutes != 45 {
		t.Fatalf("PUT round-trip: %+v", got)
	}
}

// ---------- Settings stall timeout validation ----------

// TestSettingsStallTimeoutValidation covers the 0-10080 bound on the
// settings PUT and the GET round-trip.
func TestSettingsStallTimeoutValidation(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// Start from the live settings object (PUT uses DisallowUnknownFields
	// and full validation — mutate only the new key on the real shape).
	resp, body := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get settings: %d %s", resp.StatusCode, body)
	}
	var st model.Settings
	json.Unmarshal(body, &st)

	for _, bad := range []int{-1, 10081} {
		st.StallTimeoutMinutes = bad
		resp, body := doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, st)
		if resp.StatusCode != 400 {
			t.Fatalf("stall_timeout_minutes=%d: status = %d, want 400: %s", bad, resp.StatusCode, body)
		}
	}

	st.StallTimeoutMinutes = 60
	resp, body = doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, st)
	if resp.StatusCode != 200 {
		t.Fatalf("valid stall timeout: %d %s", resp.StatusCode, body)
	}
	resp, body = doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	json.Unmarshal(body, &st)
	if st.StallTimeoutMinutes != 60 {
		t.Fatalf("GET stall_timeout_minutes = %d, want 60", st.StallTimeoutMinutes)
	}
}

// ---------- Heartbeat progress stamp ----------

// TestHeartbeatProgressStamp proves last_progress_at refreshes ONLY when
// the reported step or progress changes — an unchanged heartbeat must not
// mask a wedged encode from the stall watchdog.
func TestHeartbeatProgressStamp(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()
	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "HBShow", Episode: "01", EpisodeDir: "HBShow/Ep 01",
		ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}

	hb := func(step string, pct float64) model.Heartbeat {
		return model.Heartbeat{
			Node: "enc-01", AgentVersion: "0.1.0",
			Jobs: []model.HeartbeatJobReport{{
				JobID: job.ID, JobStatus: "running", Step: step, StepProgress: pct,
			}},
		}
	}
	post := func(h model.Heartbeat) {
		t.Helper()
		resp, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, h)
		if resp.StatusCode != 200 {
			t.Fatalf("heartbeat: %d %s", resp.StatusCode, body)
		}
	}
	// Timestamps store at second granularity — truncate so equality
	// comparisons against the stored value are exact.
	old := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)

	// Changed step/progress (stored row is ""/0) → stamp refreshes.
	if err := e.server.Store.SeedJobLastProgress(ctx, job.ID, old); err != nil {
		t.Fatal(err)
	}
	post(hb("encode", 10))
	ts1, err := e.server.Store.JobLastProgressAt(ctx, job.ID)
	if err != nil || ts1 == nil || !ts1.After(old) {
		t.Fatalf("changed progress must refresh stamp: %v err=%v", ts1, err)
	}

	// Identical report → stamp must NOT move.
	if err := e.server.Store.SeedJobLastProgress(ctx, job.ID, old); err != nil {
		t.Fatal(err)
	}
	post(hb("encode", 10))
	ts2, err := e.server.Store.JobLastProgressAt(ctx, job.ID)
	if err != nil || ts2 == nil || !ts2.Equal(old) {
		t.Fatalf("unchanged heartbeat must not refresh stamp: %v (want %v) err=%v", ts2, old, err)
	}

	// Progress-only change → stamp refreshes.
	post(hb("encode", 11))
	ts3, err := e.server.Store.JobLastProgressAt(ctx, job.ID)
	if err != nil || ts3 == nil || !ts3.After(old) {
		t.Fatalf("progress change must refresh stamp: %v err=%v", ts3, err)
	}

	// Step-only change → stamp refreshes.
	if err := e.server.Store.SeedJobLastProgress(ctx, job.ID, old); err != nil {
		t.Fatal(err)
	}
	post(hb("mux", 11))
	ts4, err := e.server.Store.JobLastProgressAt(ctx, job.ID)
	if err != nil || ts4 == nil || !ts4.After(old) {
		t.Fatalf("step change must refresh stamp: %v err=%v", ts4, err)
	}
}

// ---------- Dispatch payload wire fields ----------

// TestDispatchPayloadCarriesTimeouts proves the heartbeat claim reply
// carries the flow's job timeout and per-step timeouts (additive fields;
// the rest of the payload shape is unchanged).
func TestDispatchPayloadCarriesTimeouts(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()
	fl, err := e.server.Store.CreateFlow(ctx, &model.Flow{
		Name: "timeout-dispatch-flow",
		Steps: []model.Step{
			{Type: model.StepEncode, TimeoutMinutes: 45},
			{Type: model.StepMux}, // no timeout → omitted from the payload
		},
		JobTimeoutMinutes: 120,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "TShow", Episode: "01", EpisodeDir: "TShow/Ep 01",
		ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, heartbeat("enc-01", 1, 0))
	if resp.StatusCode != 200 {
		t.Fatalf("heartbeat: %d %s", resp.StatusCode, body)
	}
	var reply model.HeartbeatReply
	json.Unmarshal(body, &reply)
	if reply.Instruction != "job" || reply.Job == nil {
		t.Fatalf("expected a job assignment, got %s (%s)", reply.Instruction, body)
	}
	if reply.Job.ID != job.ID {
		t.Fatalf("assigned job = %d, want %d", reply.Job.ID, job.ID)
	}
	if reply.Job.JobTimeoutMinutes != 120 {
		t.Fatalf("payload job_timeout_minutes = %d, want 120", reply.Job.JobTimeoutMinutes)
	}
	if len(reply.Job.StepTimeouts) != 1 {
		t.Fatalf("payload step_timeouts = %+v, want exactly one entry", reply.Job.StepTimeouts)
	}
	st := reply.Job.StepTimeouts[0]
	if st.Index != 0 || st.Step != "encode" || st.TimeoutMinutes != 45 {
		t.Fatalf("step timeout entry = %+v, want {0 encode 45}", st)
	}
	// The claim must also stamp last_progress_at (watchdog baseline).
	stamp, err := e.server.Store.JobLastProgressAt(ctx, job.ID)
	if err != nil || stamp == nil {
		t.Fatalf("claim did not stamp last_progress_at: %v err=%v", stamp, err)
	}
}

// ---------- Prometheus stall counter ----------

// TestPrometheusStalledCounter proves encode_jobs_stalled_total counts
// failed jobs carrying the watchdog's stall error prefix.
func TestPrometheusStalledCounter(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()
	fl, _ := e.server.Store.FlowByName(ctx, "default-1080")
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "PromStall", Episode: "01", EpisodeDir: "PromStall/Ep 01",
		ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.FinishJob(ctx, job.ID, model.JobFailed, -1,
		"stalled: no progress for 30 minutes", nil, ""); err != nil {
		t.Fatal(err)
	}
	// A non-stall failure must not count.
	job2, _ := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "PromStall", Episode: "02", EpisodeDir: "PromStall/Ep 02",
		ScriptType: "vpy", FlowID: fl.ID,
	})
	e.server.Store.AssignJob(ctx, job2.ID, e.node.ID)
	e.server.Store.FinishJob(ctx, job2.ID, model.JobFailed, 1, "encode crashed", nil, "")

	resp, body := doJSON(t, "GET", ts.URL+"/metrics", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("metrics: %d", resp.StatusCode)
	}
	text := string(body)
	for _, want := range []string{
		"# TYPE encode_jobs_stalled_total counter",
		"encode_jobs_stalled_total 1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q:\n%s", want, text)
		}
	}
}
