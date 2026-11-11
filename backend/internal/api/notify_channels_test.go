package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/notify"
)

// ---------- Notification fan-out: webhook / ntfy / email integration ----------

// chanCapture records every request hitting a mock channel endpoint.
type chanCapture struct {
	mu     sync.Mutex
	bodies []string
	paths  []string
	auths  []string
}

func (c *chanCapture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, string(body))
		c.paths = append(c.paths, r.URL.Path)
		c.auths = append(c.auths, r.Header.Get("Authorization"))
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
}

func (c *chanCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func (c *chanCapture) last() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		return "", ""
	}
	return c.bodies[len(c.bodies)-1], c.paths[len(c.paths)-1]
}

// apiFakeSMTP is the in-api-package SMTP seam fake (the notify package has
// its own; this one asserts the API wiring end to end).
type apiFakeSMTP struct {
	mu    sync.Mutex
	calls []apiSMTPCall
}

type apiSMTPCall struct {
	addr string
	auth smtp.Auth
	from string
	to   []string
	msg  string
}

func (f *apiFakeSMTP) SendMail(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, apiSMTPCall{addr: addr, auth: auth, from: from, to: to, msg: string(msg)})
	return nil
}

func (f *apiFakeSMTP) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// notifyTestMocks starts mock Discord/webhook/ntfy endpoints and saves a
// settings row enabling all of them plus email through the fake seam.
func notifyTestMocks(t *testing.T, e *testEnv) (*chanCapture, *chanCapture, *chanCapture, *apiFakeSMTP) {
	t.Helper()
	discordCap, hookCap, ntfyCap := &chanCapture{}, &chanCapture{}, &chanCapture{}
	discordSrv := httptest.NewServer(discordCap.handler())
	hookSrv := httptest.NewServer(hookCap.handler())
	ntfySrv := httptest.NewServer(ntfyCap.handler())
	t.Cleanup(discordSrv.Close)
	t.Cleanup(hookSrv.Close)
	t.Cleanup(ntfySrv.Close)

	fake := &apiFakeSMTP{}
	e.server.MailSender = fake

	st := e.server.defaultsSettings()
	st.DiscordWebhook = discordSrv.URL
	st.GenericWebhookURL = hookSrv.URL
	st.NtfyTopicURL = ntfySrv.URL + "/encode"
	st.ControllerURL = "http://ctrl:8080"
	st.SMTPHost = "mail.example.com"
	st.SMTPPort = 587
	st.SMTPFrom = "encode@example.com"
	st.SMTPTo = "ops@example.com"
	if err := e.server.Store.SaveSettings(ctxBg(), st); err != nil {
		t.Fatal(err)
	}
	return discordCap, hookCap, ntfyCap, fake
}

// createAssignedJob creates a job on the default flow and assigns it to the
// test node so completeJob can report a terminal outcome.
func createAssignedJob(t *testing.T, e *testEnv, series string) *model.Job {
	t.Helper()
	ctx := ctxBg()
	fl, err := e.server.Store.FlowByName(ctx, "default-1080")
	if err != nil {
		t.Fatal(err)
	}
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: series, Episode: "01", EpisodeDir: series + "/Ep 01",
		ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}
	return job
}

// TestNotifyChannelsJobOutcome: a terminal failure reaches EVERY configured
// channel — Discord (unchanged content), generic webhook (structured JSON),
// ntfy (topic path + facts) and email (envelope + facts via the seam).
func TestNotifyChannelsJobOutcome(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	discordCap, hookCap, ntfyCap, fake := notifyTestMocks(t, e)

	job := createAssignedJob(t, e, "FanoutShow")
	completeJob(t, ts.URL, e, job.ID, "failed")

	if discordCap.count() != 1 {
		t.Errorf("discord calls = %d, want 1", discordCap.count())
	}
	if body, _ := discordCap.last(); !strings.Contains(body, "Encode failed") {
		t.Errorf("discord content unchanged expectation failed: %s", body)
	}
	if hookCap.count() != 1 {
		t.Fatalf("webhook calls = %d, want 1", hookCap.count())
	}
	body, _ := hookCap.last()
	var p map[string]any
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("webhook payload not JSON: %s", body)
	}
	if p["event"] != "job_finished" || p["series"] != "FanoutShow" || p["status"] != "failed" || p["node"] != "enc-01" {
		t.Errorf("webhook payload wrong: %s", body)
	}
	if ntfyCap.count() != 1 {
		t.Fatalf("ntfy calls = %d, want 1", ntfyCap.count())
	}
	if body, path := ntfyCap.last(); path != "/encode" || !strings.Contains(body, "FanoutShow") {
		t.Errorf("ntfy request wrong: path=%s body=%s", path, body)
	}
	if fake.count() != 1 {
		t.Fatalf("email calls = %d, want 1", fake.count())
	}
	fake.mu.Lock()
	call := fake.calls[0]
	fake.mu.Unlock()
	if call.addr != "mail.example.com:587" || call.from != "encode@example.com" || len(call.to) != 1 || call.to[0] != "ops@example.com" {
		t.Errorf("email envelope wrong: %+v", call)
	}
	if !strings.Contains(call.msg, "Subject: Encode failed") || !strings.Contains(call.msg, "FanoutShow") {
		t.Errorf("email content wrong: %s", call.msg)
	}
}

// TestNotifyChannelsAutoRetrySilent: the silent-auto-retry suppression applies
// to ALL channels — a re-queued failure notifies nobody.
func TestNotifyChannelsAutoRetrySilent(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	discordCap, hookCap, ntfyCap, fake := notifyTestMocks(t, e)
	ctx := ctxBg()

	fl, err := e.server.Store.CreateFlow(ctx, &model.Flow{
		Name:                "retry-fanout",
		Steps:               []model.Step{{Type: model.StepDGIndex}},
		MaxRetries:          2,
		RetryBackoffMinutes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "RetryShow", Episode: "01", EpisodeDir: "RetryShow/Ep 01",
		ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.AssignJob(ctx, job.ID, e.node.ID); err != nil {
		t.Fatal(err)
	}
	completeJob(t, ts.URL, e, job.ID, "failed")

	if got, _ := e.server.Store.GetJob(ctx, job.ID); got.Status != model.JobPending {
		t.Fatalf("job must be re-queued, got %q", got.Status)
	}
	if discordCap.count()+hookCap.count()+ntfyCap.count()+fake.count() != 0 {
		t.Fatalf("auto-retry must be silent on every channel: discord=%d webhook=%d ntfy=%d email=%d",
			discordCap.count(), hookCap.count(), ntfyCap.count(), fake.count())
	}
}

// TestNotifyChannelsMutedSeries: a muted series skips every channel.
func TestNotifyChannelsMutedSeries(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	discordCap, hookCap, ntfyCap, fake := notifyTestMocks(t, e)
	ctx := ctxBg()

	sr, err := e.server.Store.UpsertSeriesByName(ctx, "MutedFanout")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.SetSeriesNotify(ctx, sr.ID, false); err != nil {
		t.Fatal(err)
	}
	job := createAssignedJob(t, e, "MutedFanout")
	completeJob(t, ts.URL, e, job.ID, "failed")

	if discordCap.count()+hookCap.count()+ntfyCap.count()+fake.count() != 0 {
		t.Fatal("muted series must notify no channel")
	}
}

// TestNotifyChannelsDigest: digest mode buffers instead of alerting per job;
// the flush delivers ONE summary to every channel.
func TestNotifyChannelsDigest(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	discordCap, hookCap, ntfyCap, fake := notifyTestMocks(t, e)
	ctx := ctxBg()

	st, err := e.server.Store.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st.NotifyDigest = true
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}

	job := createAssignedJob(t, e, "DigestShow")
	completeJob(t, ts.URL, e, job.ID, "done")
	if discordCap.count()+hookCap.count()+ntfyCap.count()+fake.count() != 0 {
		t.Fatal("digest mode must not alert per job")
	}

	e.server.flushDigestTick(ctx)
	if hookCap.count() != 1 || ntfyCap.count() != 1 || discordCap.count() != 1 || fake.count() != 1 {
		t.Fatalf("digest flush must reach every channel once: discord=%d webhook=%d ntfy=%d email=%d",
			discordCap.count(), hookCap.count(), ntfyCap.count(), fake.count())
	}
	if body, _ := hookCap.last(); !strings.Contains(body, "Encode digest") {
		t.Errorf("webhook digest content wrong: %s", body)
	}
}

// TestNodeStatusAlertChannelsWithoutDiscord: node-status alerts fire on the
// new channels even when no Discord webhook is configured (the old gate
// required Discord; the new gate requires ANY channel).
func TestNodeStatusAlertChannelsWithoutDiscord(t *testing.T) {
	e := newTestEnv(t)
	_, hookCap, ntfyCap, fake := notifyTestMocks(t, e)
	ctx := ctxBg()

	st, err := e.server.Store.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st.DiscordWebhook = "" // Discord OFF — new channels must carry the alert
	st.NotifyNodeStatus = true
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	e.node.LastSeen = &now
	if err := e.server.Store.UpdateNode(ctx, e.node); err != nil {
		t.Fatal(err)
	}
	e.server.nodeAlertTick(ctx) // seed — silent
	if hookCap.count()+ntfyCap.count()+fake.count() != 0 {
		t.Fatal("seed tick must be silent")
	}

	stale := time.Now().UTC().Add(-10 * time.Minute)
	e.node.LastSeen = &stale
	if err := e.server.Store.UpdateNode(ctx, e.node); err != nil {
		t.Fatal(err)
	}
	e.server.nodeAlertTick(ctx) // online→offline

	if hookCap.count() != 1 || ntfyCap.count() != 1 || fake.count() != 1 {
		t.Fatalf("offline transition must reach every channel: webhook=%d ntfy=%d email=%d",
			hookCap.count(), ntfyCap.count(), fake.count())
	}
	if body, _ := hookCap.last(); !strings.Contains(body, "offline") {
		t.Errorf("webhook alert content wrong: %s", body)
	}
}

// TestNodeStatusAlertNoChannels: NotifyNodeStatus on but nothing configured
// → ticks stay silent (and keep updating the baseline).
func TestNodeStatusAlertNoChannels(t *testing.T) {
	e := newTestEnv(t)
	ctx := ctxBg()
	st := e.server.defaultsSettings()
	st.DiscordWebhook = ""
	st.NotifyNodeStatus = true
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	e.node.LastSeen = &now
	e.server.Store.UpdateNode(ctx, e.node)
	e.server.nodeAlertTick(ctx)
	stale := time.Now().UTC().Add(-10 * time.Minute)
	e.node.LastSeen = &stale
	e.server.Store.UpdateNode(ctx, e.node)
	e.server.nodeAlertTick(ctx) // must not panic with zero channels
}

// TestDiskAlertChannels: a low-disk heartbeat alerts the new channels (the
// Discord-only fallback is replaced by the fan-out).
func TestDiskAlertChannels(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	_, hookCap, ntfyCap, fake := notifyTestMocks(t, e)
	ctx := ctxBg()

	st, err := e.server.Store.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st.DiskAlertGB = 50
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}

	hb := heartbeat("enc-01", 1, 0)
	hb.Metrics = &model.NodeMetrics{DiskFreeGB: 20}
	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, hb)
	if resp.StatusCode != 200 {
		t.Fatalf("heartbeat: %d %s", resp.StatusCode, body)
	}
	if hookCap.count() != 1 || ntfyCap.count() != 1 || fake.count() != 1 {
		t.Fatalf("disk alert must reach every channel: webhook=%d ntfy=%d email=%d",
			hookCap.count(), ntfyCap.count(), fake.count())
	}
	if body, _ := hookCap.last(); !strings.Contains(body, "Low disk space") {
		t.Errorf("webhook disk alert content wrong: %s", body)
	}
}

// TestNotifyChannelsDiscordOnlyUnchanged: with only Discord configured the
// new machinery must behave exactly like before (one Discord post, nothing
// else attempted).
func TestNotifyChannelsDiscordOnlyUnchanged(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	discordCap := &chanCapture{}
	discordSrv := httptest.NewServer(discordCap.handler())
	t.Cleanup(discordSrv.Close)

	st := e.server.defaultsSettings()
	st.DiscordWebhook = discordSrv.URL
	if err := e.server.Store.SaveSettings(ctxBg(), st); err != nil {
		t.Fatal(err)
	}
	job := createAssignedJob(t, e, "SoloShow")
	completeJob(t, ts.URL, e, job.ID, "done")
	if discordCap.count() != 1 {
		t.Fatalf("discord calls = %d, want 1", discordCap.count())
	}
	if body, _ := discordCap.last(); !strings.Contains(body, "Encode done") {
		t.Errorf("discord content wrong: %s", body)
	}
}

// TestNotifyChannelsNtfyTokenFromSettings: the stored (encrypted) ntfy token
// is decrypted and sent as the Bearer header on live alerts.
func TestNotifyChannelsNtfyTokenFromSettings(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	_, _, ntfyCap, _ := notifyTestMocks(t, e)
	ctx := ctxBg()

	tok := "nt" + "fy-live-tok"
	st, err := e.server.Store.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st.NtfyToken = tok
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}

	job := createAssignedJob(t, e, "TokenShow")
	completeJob(t, ts.URL, e, job.ID, "failed")

	ntfyCap.mu.Lock()
	defer ntfyCap.mu.Unlock()
	if len(ntfyCap.auths) != 1 || ntfyCap.auths[0] != "Bearer "+tok {
		t.Fatalf("ntfy auth = %v, want Bearer <stored token>", ntfyCap.auths)
	}
}

// compile-time guard: the fan-out satisfies the existing Notifier seam.
var _ notify.Notifier = (*notify.Fanout)(nil)
