package notify

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// recordingChannel is an in-memory Channel for fan-out tests. When panic is
// set it panics on every call — the dispatcher must contain it.
type recordingChannel struct {
	name   string
	panic  bool
	mu     sync.Mutex
	jobs   int
	alerts []string
}

func (c *recordingChannel) Name() string { return c.name }

func (c *recordingChannel) JobFinished(_ context.Context, _ *model.Job, _ string) {
	if c.panic {
		panic("channel " + c.name + " exploded")
	}
	c.mu.Lock()
	c.jobs++
	c.mu.Unlock()
}

func (c *recordingChannel) Alert(_ context.Context, content string) {
	if c.panic {
		panic("channel " + c.name + " exploded")
	}
	c.mu.Lock()
	c.alerts = append(c.alerts, content)
	c.mu.Unlock()
}

func (c *recordingChannel) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.jobs, len(c.alerts)
}

// TestFanoutIsolation: a panicking channel in the middle must not stop the
// channels after it, for both event kinds.
func TestFanoutIsolation(t *testing.T) {
	a := &recordingChannel{name: "a"}
	bad := &recordingChannel{name: "bad", panic: true}
	b := &recordingChannel{name: "b"}
	f := NewFanout([]Channel{a, bad, b}, testLog())

	f.JobFinished(context.Background(), &model.Job{ID: 1, Status: model.JobDone}, "n")
	f.Alert(context.Background(), "hello")

	if jobs, alerts := a.counts(); jobs != 1 || alerts != 1 {
		t.Errorf("channel a: jobs=%d alerts=%d, want 1/1", jobs, alerts)
	}
	if jobs, alerts := b.counts(); jobs != 1 || alerts != 1 {
		t.Errorf("channel b: jobs=%d alerts=%d, want 1/1", jobs, alerts)
	}
}

// TestFanoutErrorIsolation: a channel whose transport fails (HTTP 500) must
// not block the healthy channels around it.
func TestFanoutErrorIsolation(t *testing.T) {
	cap := &webhookCapture{status: 500}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	a := &recordingChannel{name: "a"}
	failing := &Webhook{URL: srv.URL, HTTP: srv.Client(), Log: testLog()}
	b := &recordingChannel{name: "b"}
	f := NewFanout([]Channel{a, failing, b}, testLog())

	f.JobFinished(context.Background(), &model.Job{ID: 1, Status: model.JobFailed}, "n")
	f.Alert(context.Background(), "hello")

	if jobs, alerts := a.counts(); jobs != 1 || alerts != 1 {
		t.Errorf("channel a: jobs=%d alerts=%d, want 1/1", jobs, alerts)
	}
	if jobs, alerts := b.counts(); jobs != 1 || alerts != 1 {
		t.Errorf("channel b: jobs=%d alerts=%d, want 1/1", jobs, alerts)
	}
}

// TestFanoutEmptyIsNop: no channels configured → silent no-op, no panic.
func TestFanoutEmptyIsNop(t *testing.T) {
	f := NewFanout(nil, testLog())
	f.JobFinished(context.Background(), &model.Job{ID: 1, Status: model.JobDone}, "n")
	f.Alert(context.Background(), "x")
}

// TestBuildChannelsOffByDefault: zero-value settings produce no channels —
// every new transport is opt-in.
func TestBuildChannelsOffByDefault(t *testing.T) {
	st := &model.Settings{}
	if chs := BuildChannels(st, "", nil, testLog()); len(chs) != 0 {
		t.Fatalf("default settings must build 0 channels, got %d", len(chs))
	}
	if AnyConfigured(st) {
		t.Fatal("AnyConfigured must be false on default settings")
	}
}

// TestBuildChannelsIndependently: each transport enables on its own fields,
// without requiring Discord.
func TestBuildChannelsIndependently(t *testing.T) {
	cases := []struct {
		name string
		st   *model.Settings
		hook string
		want []string
	}{
		{"webhook only", &model.Settings{GenericWebhookURL: "http://localhost:9/hook"}, "", []string{"webhook"}},
		{"ntfy only", &model.Settings{NtfyTopicURL: "http://localhost:9/topic"}, "", []string{"ntfy"}},
		{"email only", &model.Settings{SMTPHost: "mail.local", SMTPFrom: "a@example.com", SMTPTo: "b@example.com"}, "", []string{"email"}},
		{"discord only", &model.Settings{}, "http://localhost:9/discord", []string{"discord"}},
		{"all", &model.Settings{
			DiscordWebhook:    "http://localhost:9/discord",
			GenericWebhookURL: "http://localhost:9/hook",
			NtfyTopicURL:      "http://localhost:9/topic",
			SMTPHost:          "mail.local", SMTPFrom: "a@example.com", SMTPTo: "b@example.com",
		}, "", []string{"discord", "webhook", "ntfy", "email"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chs := BuildChannels(tc.st, tc.hook, &fakeSMTP{}, testLog())
			var names []string
			for _, ch := range chs {
				names = append(names, ch.Name())
			}
			if strings.Join(names, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("channels = %v, want %v", names, tc.want)
			}
			if !AnyConfigured(tc.st) && tc.hook == "" {
				t.Fatal("AnyConfigured must be true when a channel is configured")
			}
		})
	}
}

// TestBuildChannelsDiscordHookOverride: the effective hook passed in (the
// per-series override) wins over the global settings webhook for Discord.
func TestBuildChannelsDiscordHookOverride(t *testing.T) {
	st := &model.Settings{DiscordWebhook: "http://localhost:9/global"}
	chs := BuildChannels(st, "http://localhost:9/series", nil, testLog())
	if len(chs) != 1 {
		t.Fatalf("want 1 channel, got %d", len(chs))
	}
	d, ok := chs[0].(*Discord)
	if !ok {
		t.Fatalf("want *Discord, got %T", chs[0])
	}
	if d.WebhookURL != "http://localhost:9/series" {
		t.Errorf("discord hook = %q, want the series override", d.WebhookURL)
	}
}

// TestSMTPConfigured: email needs host+from+to together; partial config is
// not a channel (validation rejects it at save time).
func TestSMTPConfigured(t *testing.T) {
	full := &model.Settings{SMTPHost: "h", SMTPFrom: "a@example.com", SMTPTo: "b@example.com"}
	if !SMTPConfigured(full) {
		t.Fatal("full smtp config must count as configured")
	}
	for _, partial := range []*model.Settings{
		{SMTPHost: "h"},
		{SMTPFrom: "a@example.com"},
		{SMTPTo: "b@example.com"},
		{SMTPHost: "h", SMTPFrom: "a@example.com"},
	} {
		if SMTPConfigured(partial) {
			t.Errorf("partial config must not count: %+v", partial)
		}
	}
}

// TestFlushDigestToFanout: the hourly digest reaches every configured
// channel (Discord included) as one summary; an empty buffer stays silent.
func TestFlushDigestToFanout(t *testing.T) {
	discordCap := &webhookCapture{}
	discordSrv := httptest.NewServer(discordCap.handler())
	defer discordSrv.Close()
	hookCap := &webhookCapture{}
	hookSrv := httptest.NewServer(hookCap.handler())
	defer hookSrv.Close()
	ntfyCap := &ntfyCapture{}
	ntfySrv := httptest.NewServer(ntfyCap.handler())
	defer ntfySrv.Close()
	fake := &fakeSMTP{}

	st := &model.Settings{
		DiscordWebhook:    discordSrv.URL,
		GenericWebhookURL: hookSrv.URL,
		NtfyTopicURL:      ntfySrv.URL + "/digest",
		SMTPHost:          "mail.local", SMTPFrom: "a@example.com", SMTPTo: "b@example.com",
		ControllerURL: "http://ctrl:8080",
	}
	buf := NewDigestBuffer()
	buf.Push(DigestEntry{JobID: 1, Series: "S", Episode: "01", Status: model.JobDone, NodeName: "n1", At: time.Now()})
	buf.Push(DigestEntry{JobID: 2, Series: "S", Episode: "02", Status: model.JobFailed, NodeName: "n1", At: time.Now()})

	FlushDigestTo(testLog(), st, fake, buf, time.Now())

	if _, _, _, p, calls := hookCap.snapshot(); calls != 1 || p.Event != "alert" || !strings.Contains(p.Message, "Encode digest") {
		t.Errorf("webhook digest wrong: calls=%d payload=%+v", calls, p)
	}
	if got := ntfyCap.snapshot(); got.calls != 1 || !strings.Contains(got.body, "Encode digest") {
		t.Errorf("ntfy digest wrong: %+v", got)
	}
	if len(fake.calls) != 1 || !strings.Contains(fake.calls[0].msg, "Encode digest") {
		t.Errorf("email digest wrong: %+v", fake.calls)
	}
	// Discord receives {"content": ...} (its own wire shape, unchanged);
	// the fan-out assertion here is that it was called exactly once.
	if _, _, _, _, calls := discordCap.snapshot(); calls != 1 {
		t.Errorf("discord digest calls = %d, want 1", calls)
	}
	if buf.Len() != 0 {
		t.Error("flush must drain the buffer")
	}

	// Empty buffer → no posts anywhere.
	FlushDigestTo(testLog(), st, fake, buf, time.Now())
	if _, _, _, _, calls := hookCap.snapshot(); calls != 1 {
		t.Errorf("empty flush must stay silent, webhook calls=%d", calls)
	}
}

// TestFlushDigestToNoChannelsDrains: with nothing configured the buffer is
// still drained (events are not retained forever).
func TestFlushDigestToNoChannelsDrains(t *testing.T) {
	buf := NewDigestBuffer()
	buf.Push(DigestEntry{JobID: 1, Series: "S", Episode: "01", Status: model.JobDone})
	FlushDigestTo(testLog(), &model.Settings{}, nil, buf, time.Now())
	if buf.Len() != 0 {
		t.Fatal("buffer must drain even with no channels")
	}
}
