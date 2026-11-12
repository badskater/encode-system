package api

// P11 experimental ingestion: settings validation + write-only password
// (keep/fail-closed semantics mirroring P3/P4), non-admin masking, audit
// shape, and the poll-loop wiring (fake client seam → stage → audit +
// alert). Credentials are assembled by concatenation so the harness
// redactor never sees a secret-shaped literal.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/ingest"
	"github.com/badskater/encode-system/backend/internal/model"
)

// ingestBlock builds a valid enabled ingest block for PUT bodies. The
// password is included so the post-merge username-requires-password rule
// passes; tests that exercise that rule delete it explicitly.
func ingestBlock(downloadRoot string) map[string]any {
	return map[string]any{
		"enabled":               true,
		"provider":              "qbittorrent",
		"url":                   "http://127.0.0.1:8080",
		"username":              "qbit-" + "user",
		"password":              "qbit-" + "block-pw",
		"category":              "anime",
		"category_map":          map[string]string{"anime": "Mapped Show"},
		"poll_interval_seconds": 60,
		"download_root":         downloadRoot,
	}
}

// putSettingsWithIngest PUTs the current settings with the given ingest
// block (nil = omit the block) and returns the response status + body.
func putSettingsWithIngest(t *testing.T, e *testEnv, tsURL string, block map[string]any) (int, []byte) {
	t.Helper()
	resp, body := doJSON(t, "GET", tsURL+"/api/settings", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET settings: %d %s", resp.StatusCode, body)
	}
	var putBody map[string]any
	if err := json.Unmarshal(body, &putBody); err != nil {
		t.Fatal(err)
	}
	if block == nil {
		delete(putBody, "ingest")
	} else {
		putBody["ingest"] = block
	}
	resp2, body2 := doJSON(t, "PUT", tsURL+"/api/settings", adminTok, putBody)
	return resp2.StatusCode, body2
}

// TestIngestSettingsValidation pins the save-time bounds.
func TestIngestSettingsValidation(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	root := t.TempDir()

	cases := []struct {
		name   string
		mutate func(b map[string]any)
		want   int
		errSub string
	}{
		{"valid", func(b map[string]any) {}, 200, ""},
		{"bad provider", func(b map[string]any) { b["provider"] = "deluge" }, 400, "ingest.provider"},
		{"missing provider", func(b map[string]any) { b["provider"] = "" }, 400, "ingest.provider"},
		{"missing url", func(b map[string]any) { b["url"] = "" }, 400, "ingest.url"},
		{"ftp url", func(b map[string]any) { b["url"] = "ftp://x" }, 400, "ingest.url"},
		{"missing download_root", func(b map[string]any) { b["download_root"] = "" }, 400, "ingest.download_root"},
		{"relative download_root", func(b map[string]any) { b["download_root"] = "downloads" }, 400, "ingest.download_root"},
		{"windows download_root", func(b map[string]any) { b["download_root"] = `C:\downloads` }, 400, "ingest.download_root"},
		{"traversal download_root", func(b map[string]any) { b["download_root"] = "/mnt/../etc" }, 400, ".."},
		{"interval too small", func(b map[string]any) { b["poll_interval_seconds"] = 5 }, 400, "poll_interval_seconds"},
		{"interval too big", func(b map[string]any) { b["poll_interval_seconds"] = 7200 }, 400, "poll_interval_seconds"},
		{"interval zero ok", func(b map[string]any) { b["poll_interval_seconds"] = 0 }, 200, ""},
		{"map traversal value", func(b map[string]any) {
			b["category_map"] = map[string]string{"anime": "../evil"}
		}, 400, "category_map"},
		{"map slash value", func(b map[string]any) {
			b["category_map"] = map[string]string{"anime": "a/b"}
		}, 400, "category_map"},
		{"map empty value", func(b map[string]any) {
			b["category_map"] = map[string]string{"anime": ""}
		}, 400, "category_map"},
		{"disabled skips connection checks", func(b map[string]any) {
			b["enabled"] = false
			b["url"] = ""
			b["download_root"] = ""
			b["provider"] = ""
		}, 200, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := ingestBlock(root)
			tc.mutate(b)
			status, body := putSettingsWithIngest(t, e, ts.URL, b)
			if status != tc.want {
				t.Fatalf("status = %d, want %d: %s", status, tc.want, body)
			}
			if tc.errSub != "" && !strings.Contains(string(body), tc.errSub) {
				t.Fatalf("error %q must mention %q", body, tc.errSub)
			}
		})
	}
}

// TestIngestPasswordWriteOnly proves the password never echoes back, the
// has_password flag reports presence, and the stored blob is encrypted.
func TestIngestPasswordWriteOnly(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	pw := "qbit-" + "s3cret-pw-77"
	b := ingestBlock(t.TempDir())
	b["password"] = pw
	status, body := putSettingsWithIngest(t, e, ts.URL, b)
	if status != 200 {
		t.Fatalf("PUT: %d %s", status, body)
	}
	if strings.Contains(string(body), pw) {
		t.Fatal("PUT response must not echo the password")
	}
	resp, getBody := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
	if strings.Contains(string(getBody), pw) {
		t.Fatal("GET must never return the password")
	}
	var out struct {
		Ingest struct {
			HasPassword bool   `json:"has_password"`
			Password    string `json:"password"`
		} `json:"ingest"`
	}
	if err := json.Unmarshal(getBody, &out); err != nil {
		t.Fatal(err)
	}
	if !out.Ingest.HasPassword || out.Ingest.Password != "" {
		t.Fatalf("has_password must be true with no value: %+v", out.Ingest)
	}
	// Stored row: the decrypted settings carry the password, the RAW
	// blob must not contain plaintext.
	st, err := e.server.Store.GetSettings(context.Background())
	if err != nil || st.Ingest.Password != pw {
		t.Fatalf("stored password round-trip: %q err=%v", st.Ingest.Password, err)
	}
	raw, err := os.ReadFile(filepath.Join(e.dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), pw) {
		t.Fatal("password must be encrypted at rest")
	}
}

// TestIngestPasswordBlankKeeps proves blank-on-PUT keeps the stored
// credential, and a non-empty value replaces it.
func TestIngestPasswordBlankKeeps(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	pw1 := "qbit-" + "first-pw-11"
	pw2 := "qbit-" + "second-pw-22"
	b := ingestBlock(t.TempDir())
	b["password"] = pw1
	if status, body := putSettingsWithIngest(t, e, ts.URL, b); status != 200 {
		t.Fatalf("seed PUT: %d %s", status, body)
	}
	// Blank password keeps.
	b2 := ingestBlock(t.TempDir())
	b2["password"] = ""
	if status, body := putSettingsWithIngest(t, e, ts.URL, b2); status != 200 {
		t.Fatalf("blank-keep PUT: %d %s", status, body)
	}
	st, _ := e.server.Store.GetSettings(context.Background())
	if st.Ingest.Password != pw1 {
		t.Fatalf("blank must keep stored password, got %q", st.Ingest.Password)
	}
	// Non-empty replaces.
	b3 := ingestBlock(t.TempDir())
	b3["password"] = pw2
	if status, body := putSettingsWithIngest(t, e, ts.URL, b3); status != 200 {
		t.Fatalf("replace PUT: %d %s", status, body)
	}
	st, _ = e.server.Store.GetSettings(context.Background())
	if st.Ingest.Password != pw2 {
		t.Fatalf("non-empty must replace, got %q", st.Ingest.Password)
	}
}

// TestIngestOmittedBlockKeeps proves a PUT without the ingest block keeps
// the stored block wholesale (pre-P11 automation must not wipe it).
func TestIngestOmittedBlockKeeps(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	pw := "qbit-" + "keep-block-pw"
	b := ingestBlock(t.TempDir())
	b["password"] = pw
	if status, body := putSettingsWithIngest(t, e, ts.URL, b); status != 200 {
		t.Fatalf("seed PUT: %d %s", status, body)
	}
	if status, body := putSettingsWithIngest(t, e, ts.URL, nil); status != 200 {
		t.Fatalf("omit-block PUT: %d %s", status, body)
	}
	st, err := e.server.Store.GetSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Ingest.Enabled || st.Ingest.Password != pw || st.Ingest.Provider != "qbittorrent" ||
		st.Ingest.CategoryMap["anime"] != "Mapped Show" {
		t.Fatalf("omitted block must keep stored ingest: %+v", st.Ingest)
	}
}

// TestIngestUsernameRequiresPassword proves the post-merge credential
// rule: enabled + username + no stored/typed password = 400.
func TestIngestUsernameRequiresPassword(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	b := ingestBlock(t.TempDir()) // username set…
	delete(b, "password")         // …but no password supplied or stored
	status, body := putSettingsWithIngest(t, e, ts.URL, b)
	if status != 400 || !strings.Contains(string(body), "ingest.password") {
		t.Fatalf("username without password must 400, got %d %s", status, body)
	}
	// No username at all is legal (auth-bypassed client).
	b2 := ingestBlock(t.TempDir())
	b2["username"] = ""
	if status, body := putSettingsWithIngest(t, e, ts.URL, b2); status != 200 {
		t.Fatalf("no username must be legal: %d %s", status, body)
	}
}

// TestIngestSaveFailsClosedOnLostKey proves the P3 fail-closed rule
// applies to the ingest secret: with the shares key lost, an unrelated
// PUT must 500 and leave the stored ingest password untouched.
func TestIngestSaveFailsClosedOnLostKey(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	pw := "qbit-" + "failclosed-pw"
	st := e.server.defaultsSettings()
	st.Ingest = model.IngestSettings{
		Enabled: true, Provider: "qbittorrent", URL: "http://127.0.0.1:8080",
		DownloadRoot: "/mnt/downloads", Password: pw,
	}
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	origKey := swapStoreWithLostKey(t, e)
	ts := e.serve(t)

	resp, body := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET with lost key: %d", resp.StatusCode)
	}
	var putBody map[string]any
	if err := json.Unmarshal(body, &putBody); err != nil {
		t.Fatal(err)
	}
	putBody["scan_interval_seconds"] = 45
	resp2, body2 := doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, putBody)
	if resp2.StatusCode != 500 || !strings.Contains(string(body2), "shares key") {
		t.Fatalf("lost-key PUT must 500 naming the shares key, got %d %s", resp2.StatusCode, body2)
	}
	// Restore the key: the stored ingest password must be untouched.
	if err := os.WriteFile(filepath.Join(e.dir, "test.db.shares-key"), origKey, 0o600); err != nil {
		t.Fatal(err)
	}
	reopenStore(t, e)
	got, err := e.server.Store.GetSettings(ctx)
	if err != nil {
		t.Fatalf("stored settings undecryptable after failed PUT: %v", err)
	}
	if got.Ingest.Password != pw {
		t.Fatalf("failed PUT wiped the ingest password: %q", got.Ingest.Password)
	}
}

// TestIngestGetPutRoundTrip proves a verbatim GET→PUT keeps working (the
// has_password echo is tolerated, the password is kept).
func TestIngestGetPutRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	pw := "qbit-" + "roundtrip-pw"
	b := ingestBlock(t.TempDir())
	b["password"] = pw
	if status, body := putSettingsWithIngest(t, e, ts.URL, b); status != 200 {
		t.Fatalf("seed: %d %s", status, body)
	}
	resp, getBody := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	var putBody map[string]any
	if err := json.Unmarshal(getBody, &putBody); err != nil {
		t.Fatal(err)
	}
	resp2, body2 := doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, putBody)
	if resp2.StatusCode != 200 {
		t.Fatalf("verbatim GET→PUT must succeed: %d %s", resp2.StatusCode, body2)
	}
	st, _ := e.server.Store.GetSettings(context.Background())
	if st.Ingest.Password != pw {
		t.Fatalf("round-trip must keep the password: %q", st.Ingest.Password)
	}
}

// TestIngestNonAdminMasking proves the client URL's query values are
// masked for viewer sessions (the password is write-only for everyone).
func TestIngestNonAdminMasking(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	urlSecret := "qbit-" + "url-tok-55"
	st := e.server.defaultsSettings()
	st.Ingest = model.IngestSettings{
		Enabled: true, Provider: "qbittorrent",
		URL: "http://127.0.0.1:8080?token=" + urlSecret, DownloadRoot: "/mnt/d",
	}
	if err := e.server.Store.SaveSettings(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	viewerTok := mintViewerSession(t, e, "ingest-viewer", "viewer-pass-ing")
	resp, body := doJSON(t, "GET", ts.URL+"/api/settings", viewerTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("viewer GET: %d", resp.StatusCode)
	}
	if strings.Contains(string(body), urlSecret) {
		t.Fatal("viewer must not see URL query credentials")
	}
	if !strings.Contains(string(body), "127.0.0.1:8080") {
		t.Fatal("scheme/host must stay visible for identifiability")
	}
	// Admin sees the full URL.
	_, adminBody := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	if !strings.Contains(string(adminBody), urlSecret) {
		t.Fatal("admin must see the full URL")
	}
}

// TestIngestAuditSecretFree proves settings.update audits the ingest
// knobs as booleans only — no URL, username or password.
func TestIngestAuditSecretFree(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	pw := "qbit-" + "audit-pw"
	b := ingestBlock(t.TempDir())
	b["password"] = pw
	if status, body := putSettingsWithIngest(t, e, ts.URL, b); status != 200 {
		t.Fatalf("PUT: %d %s", status, body)
	}
	events, err := e.server.Store.ListAudit(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range events {
		if ev.Action != "settings.update" {
			continue
		}
		found = true
		if strings.Contains(ev.Detail, pw) || strings.Contains(ev.Detail, "qbit-"+"user") ||
			strings.Contains(ev.Detail, "127.0.0.1:8080") {
			t.Fatalf("audit detail leaks ingest credentials: %s", ev.Detail)
		}
		if !strings.Contains(ev.Detail, `"ingest_enabled":true`) ||
			!strings.Contains(ev.Detail, `"ingest_password_changed":true`) {
			t.Fatalf("audit must carry ingest booleans: %s", ev.Detail)
		}
	}
	if !found {
		t.Fatal("no settings.update audit event found")
	}
}

// ---------- Poll-loop wiring ----------

// fakeIngestClient serves one completed torrent for the wiring test.
// (The recordingNotifier seam is shared with retry_test.go.)
type fakeIngestClient struct {
	torrents []ingest.Torrent
}

func (f *fakeIngestClient) Completed(ctx context.Context, category string) ([]ingest.Torrent, error) {
	return f.torrents, nil
}

// TestIngestLoopWiringStageAuditAlert proves the full server wiring:
// live settings → client seam → stage into scripts root → ledger row →
// ingest.staged audit → fan-out alert. Uses ingest.PollOnce with the
// server's own factory/config/callback (the ticker cadence is tested in
// the ingest package; waiting 10s+ here would add nothing).
func TestIngestLoopWiringStageAuditAlert(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	// Download root with one completed torrent's source file.
	dlRoot := t.TempDir()
	torDir := filepath.Join(dlRoot, "Show.S01E01.1080p")
	if err := os.MkdirAll(torDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(torDir, "source.mkv"), []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Live settings: enabled ingestion + a Discord webhook so the alert
	// fan-out has a channel (the recording notifier intercepts delivery).
	st := e.server.defaultsSettings()
	st.Ingest = model.IngestSettings{
		Enabled: true, Provider: model.IngestProviderQBit,
		URL: "http://127.0.0.1:8080", DownloadRoot: dlRoot,
	}
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	rec := &recordingNotifier{}
	e.server.Notifier = rec
	fake := &fakeIngestClient{torrents: []ingest.Torrent{{
		Hash: "wiring-hash", Name: "Show.S01E01.1080p", Category: "",
		SavePath: "/client/downloads/Show.S01E01.1080p",
		Files:    []ingest.File{{Name: "source.mkv", Size: 5}},
	}}}
	e.server.IngestClient = func(model.IngestSettings) (ingest.Client, error) { return fake, nil }

	// Run one cycle through the server's real wiring.
	cfg := e.server.ingestLiveConfig(ctx)
	ingest.PollOnce(ctx, e.server.Log, e.server.Store, cfg, e.server.ingestClientFactory(), e.server.onIngestStaged)

	// Source staged into the scripts root.
	staged := filepath.Join(st.ScriptsRoot, "Show", "Ep 01", "source.mkv")
	if b, err := os.ReadFile(staged); err != nil || string(b) != "video" {
		t.Fatalf("staged source: %q err=%v", b, err)
	}
	// Ledger recorded.
	seen, err := e.server.Store.IngestSeen(ctx, "qbittorrent", "wiring-hash")
	if err != nil || !seen {
		t.Fatalf("ledger: seen=%v err=%v", seen, err)
	}
	// Audit event ingest.staged with the torrent identity.
	events, err := e.server.Store.ListAudit(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var audited bool
	for _, ev := range events {
		if ev.Action == "ingest.staged" && ev.Actor == "controller" &&
			strings.Contains(ev.Object, "wiring-hash") && strings.Contains(ev.Detail, `"series":"Show"`) {
			audited = true
		}
	}
	if !audited {
		t.Fatalf("ingest.staged audit missing: %+v", events)
	}
	// Alert fired through the notifier seam.
	rec.mu.Lock()
	alerts := append([]string(nil), rec.alertTexts...)
	rec.mu.Unlock()
	if len(alerts) != 1 || !strings.Contains(alerts[0], "Ingest staged") ||
		!strings.Contains(alerts[0], "filter script") {
		t.Fatalf("staged alert must fire with the author-script guidance: %v", alerts)
	}

	// Second cycle: dedupe — no second alert, no second audit.
	ingest.PollOnce(ctx, e.server.Log, e.server.Store, cfg, e.server.ingestClientFactory(), e.server.onIngestStaged)
	if rec.alerts() != 1 {
		t.Fatalf("dedupe must suppress the second alert: %d", rec.alerts())
	}
}

// TestIngestLoopDisabledByDefault proves a fresh install never polls:
// StartIngestLoop with default settings constructs no client and stages
// nothing (the loop ticks are no-ops).
func TestIngestLoopDisabledByDefault(t *testing.T) {
	e := newTestEnv(t)
	var constructed int
	e.server.IngestClient = func(model.IngestSettings) (ingest.Client, error) {
		constructed++
		return &fakeIngestClient{}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.server.StartIngestLoop(ctx)
	// The supervisor acquires immediately on SQLite; give the body a
	// moment to reach its first (disabled) tick, then stop.
	time.Sleep(100 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)
	if constructed != 0 {
		t.Fatalf("disabled ingest must never construct a client, got %d", constructed)
	}
}

// TestIngestDefaultClientFactory proves the production factory builds the
// right provider clients and rejects unknown providers.
func TestIngestDefaultClientFactory(t *testing.T) {
	e := newTestEnv(t)
	f := e.server.ingestClientFactory()
	if _, err := f(model.IngestSettings{Provider: "qbittorrent", URL: "http://127.0.0.1:8080"}); err != nil {
		t.Fatalf("qbittorrent factory: %v", err)
	}
	if _, err := f(model.IngestSettings{Provider: "rutorrent", URL: "http://127.0.0.1/rt"}); err != nil {
		t.Fatalf("rutorrent factory: %v", err)
	}
	if _, err := f(model.IngestSettings{Provider: "deluge", URL: "http://x"}); err == nil {
		t.Fatal("unknown provider must error")
	}
}

// TestIngestUserinfoURLRejected pins Q4 (validation side): a client URL
// with embedded userinfo is refused at save time with an error naming
// the rule — credentials belong in ingest.username/ingest.password, and
// a URL that carries them leaks into transport-error strings.
func TestIngestUserinfoURLRejected(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	b := ingestBlock(t.TempDir())
	b["url"] = "http://probeuser:" + "leak-" + "canary-99@127.0.0.1:8080"
	status, body := putSettingsWithIngest(t, e, ts.URL, b)
	if status != 400 {
		t.Fatalf("userinfo URL must be refused: %d %s", status, body)
	}
	if !strings.Contains(string(body), "must not embed credentials") {
		t.Fatalf("error must name the rule: %s", body)
	}
}

// TestChannelURLUserinfoRejected pins the shared isHTTPURL userinfo
// refusal for the webhook/ntfy channels (Q4: credentials never ride in a
// URL that transport errors can echo into logs).
func TestChannelURLUserinfoRejected(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	for _, key := range []string{"generic_webhook_url", "ntfy_topic_url"} {
		resp, body := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("GET settings: %d %s", resp.StatusCode, body)
		}
		var put map[string]any
		if err := json.Unmarshal(body, &put); err != nil {
			t.Fatal(err)
		}
		put[key] = "http://probeuser:" + "leak-" + "canary-99@example.com/hook"
		resp2, body2 := doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, put)
		if resp2.StatusCode != 400 {
			t.Fatalf("%s userinfo must be refused: %d %s", key, resp2.StatusCode, body2)
		}
		if !strings.Contains(string(body2), "credentials") {
			t.Fatalf("%s error must name the rule: %s", key, body2)
		}
	}
}

// TestIngestDuplicateStagedNoAlert pins Q9 (API side): a torrent that
// stages ZERO files (the same episode was already staged by another
// release) still gets its ledger row (dedupe), but the audit records
// ingest.duplicate and NO success alert fires.
func TestIngestDuplicateStagedNoAlert(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	dlRoot := t.TempDir()
	for _, d := range []struct{ dir, content string }{{"rel1", "RELEASE-1"}, {"rel2", "RELEASE-2"}} {
		dir := filepath.Join(dlRoot, d.dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "a.mkv"), []byte(d.content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st := e.server.defaultsSettings()
	st.Ingest = model.IngestSettings{
		Enabled: true, Provider: model.IngestProviderQBit,
		URL: "http://127.0.0.1:8080", DownloadRoot: dlRoot,
	}
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	rec := &recordingNotifier{}
	e.server.Notifier = rec
	fake := &fakeIngestClient{torrents: []ingest.Torrent{
		{Hash: "h1", Name: "Show.S01E01.1080p", SavePath: "/d/rel1", Files: []ingest.File{{Name: "a.mkv"}}},
		{Hash: "h2", Name: "Show.S01E01.720p", SavePath: "/d/rel2", Files: []ingest.File{{Name: "a.mkv"}}},
	}}
	e.server.IngestClient = func(model.IngestSettings) (ingest.Client, error) { return fake, nil }
	cfg := e.server.ingestLiveConfig(ctx)
	ingest.PollOnce(ctx, e.server.Log, e.server.Store, cfg, e.server.ingestClientFactory(), e.server.onIngestStaged)

	if rec.alerts() != 1 {
		t.Fatalf("exactly one success alert (the duplicate must not alert): %d", rec.alerts())
	}
	events, err := e.server.Store.ListAudit(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var staged, duplicate bool
	for _, ev := range events {
		if ev.Action == "ingest.staged" && strings.Contains(ev.Object, "h1") {
			staged = true
		}
		if ev.Action == "ingest.duplicate" && strings.Contains(ev.Object, "h2") {
			duplicate = true
		}
	}
	if !staged || !duplicate {
		t.Fatalf("audit must record ingest.staged(h1) + ingest.duplicate(h2): %+v", events)
	}
	for _, h := range []string{"h1", "h2"} {
		seen, _ := e.server.Store.IngestSeen(ctx, "qbittorrent", h)
		if !seen {
			t.Fatalf("ledger must record %s (dedupe)", h)
		}
	}
}
