package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// ---------- Settings: live Discord webhook ----------

// TestSettingsDiscordWebhookValidation: the webhook is optional, but a set
// value must look like a Discord webhook (loopback allowed for mock tests).
func TestSettingsDiscordWebhookValidation(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Grab the current settings so we can round-trip the required fields.
	resp, body := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get settings: %d", resp.StatusCode)
	}
	var st model.Settings
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}

	setWebhook := func(url string) int {
		st.DiscordWebhook = url
		resp, _ := doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, st)
		return resp.StatusCode
	}

	if code := setWebhook(""); code != 200 {
		t.Fatalf("blank webhook must be accepted (notifications off), got %d", code)
	}
	if got, _ := e.server.Store.GetSettings(ctx); got == nil || got.DiscordWebhook != "" {
		t.Fatalf("blank webhook not persisted: %+v", got)
	}
	if code := setWebhook("https://discord.com/api/webhooks/123/abc"); code != 200 {
		t.Fatalf("valid discord.com webhook rejected: %d", code)
	}
	if code := setWebhook("https://discordapp.com/api/webhooks/123/abc"); code != 200 {
		t.Fatalf("valid discordapp.com webhook rejected: %d", code)
	}
	if code := setWebhook("http://127.0.0.1:9999/hook"); code != 200 {
		t.Fatalf("loopback webhook must be accepted (mock testing), got %d", code)
	}
	for _, bad := range []string{
		"http://evil.example.com/collect",
		"https://discord.com/api/notawebhook",
		"notaurl",
	} {
		if code := setWebhook(bad); code != 400 {
			t.Errorf("webhook %q must be rejected, got %d", bad, code)
		}
	}
}

// TestDiscordWebhookLiveResolution: discordWebhook honors the persisted
// settings row (including blank = OFF), and falls back to the env default
// only when nothing was ever saved.
func TestDiscordWebhookLiveResolution(t *testing.T) {
	e := newTestEnv(t)
	ctx := ctxBg()

	// No settings row yet -> env default from the test config.
	if got := e.server.discordWebhook(ctx); got != "https://discord.com/api/webhooks/env/default" {
		t.Fatalf("want env default, got %q", got)
	}

	// Save a settings row WITH a webhook -> saved value wins.
	st := e.server.defaultsSettings()
	st.DiscordWebhook = "https://discord.com/api/webhooks/live/one"
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	if got := e.server.discordWebhook(ctx); got != "https://discord.com/api/webhooks/live/one" {
		t.Fatalf("saved webhook must win, got %q", got)
	}

	// Save it back BLANK -> notifications OFF (no silent env fallback).
	st.DiscordWebhook = ""
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	if got := e.server.discordWebhook(ctx); got != "" {
		t.Fatalf("blank saved webhook must disable notifications, got %q", got)
	}
}

// TestDiscordWebhookReachesRenderedJobs: the live webhook is injected into
// the $Job context of every rendered job so the discord_notify step's blank
// param falls back to it — and edits take effect without a restart.
func TestDiscordWebhookReachesRenderedJobs(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Flow with the notify step and a BLANK webhook param (fallback path).
	fl, err := e.server.Store.CreateFlow(ctx, &model.Flow{
		Name: "notify-flow",
		Steps: []model.Step{
			{Type: model.StepType("discord_notify"), Params: map[string]string{"webhook": ""}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := e.server.Store.CreateJob(ctx, &model.Job{
		Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: fl.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	rendered := func() string {
		payload, err := e.server.renderJob(ctx, job)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		return payload.Script
	}

	// Env default applies before any settings row exists.
	if !strings.Contains(rendered(), "DiscordWebhook = 'https://discord.com/api/webhooks/env/default'") {
		t.Errorf("env webhook missing from rendered job:\n%s", rendered())
	}

	// Save a different webhook via the API -> the next render picks it up
	// live (no restart).
	resp, body := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get settings: %d", resp.StatusCode)
	}
	var st model.Settings
	json.Unmarshal(body, &st)
	st.DiscordWebhook = "https://discord.com/api/webhooks/live/two"
	resp, body = doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, st)
	if resp.StatusCode != 200 {
		t.Fatalf("put settings: %d %s", resp.StatusCode, body)
	}
	script := rendered()
	if !strings.Contains(script, "DiscordWebhook = 'https://discord.com/api/webhooks/live/two'") {
		t.Errorf("live webhook not picked up by the renderer:\n%s", script)
	}

	// Blank it out -> the step context carries an empty webhook (the step
	// then no-ops politely instead of posting anywhere).
	st.DiscordWebhook = ""
	if resp, body = doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, st); resp.StatusCode != 200 {
		t.Fatalf("blank webhook save: %d %s", resp.StatusCode, body)
	}
	script = rendered()
	if !strings.Contains(script, "DiscordWebhook = ''") {
		t.Errorf("blank webhook must render as empty:\n%s", script)
	}
}
