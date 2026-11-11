package api

// Regression for quality review Q3: key-loss → silent secret wipe.
// handleUpdateSettings merged keep-semantics secrets from currentSettings(),
// which falls back to env defaults on ANY GetSettings error. When the stored
// ciphertext is undecryptable (shares key lost), the next unrelated PUT
// overwrote the stored secrets with '' silently. The save path must read the
// stored row DIRECTLY and fail closed (500) on a decrypt/read error; only a
// genuinely absent row (nil, nil) may merge from defaults. The read path may
// still serve fallback, but must log it at Error with an actionable message.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/store"
)

// swapStoreWithLostKey simulates shares-key loss: it replaces the key file
// with a different key and reopens the store (the cipher is cached per
// handle, so a restart is when key loss actually surfaces). Returns the
// original key bytes so the test can restore them afterwards.
func swapStoreWithLostKey(t *testing.T, e *testEnv) []byte {
	t.Helper()
	keyPath := filepath.Join(e.dir, "test.db.shares-key")
	orig, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read shares key: %v", err)
	}
	// A different 32-byte key: deterministic, never the original.
	lost := bytes.Repeat([]byte{0xAB}, 32)
	if err := os.WriteFile(keyPath, lost, 0o600); err != nil {
		t.Fatalf("write lost key: %v", err)
	}
	reopenStore(t, e)
	return orig
}

// reopenStore closes the server's store and opens a fresh handle on the same
// DB (fresh cipher cache), registering cleanup.
func reopenStore(t *testing.T, e *testEnv) {
	t.Helper()
	e.server.Store.Close()
	s2, err := store.Open(filepath.Join(e.dir, "test.db"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { s2.Close() })
	e.server.Store = s2
}

// TestSaveSettingsFailsClosedOnUnreadableStored: with the shares key lost,
// an unrelated PUT must return 500 (not silently wipe the stored secrets),
// and the stored secrets must survive untouched. GET still serves the env
// fallback but logs it at Error with an actionable message.
func TestSaveSettingsFailsClosedOnUnreadableStored(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	// Seed a settings row WITH secrets (creates the shares key).
	tok := "ntfy-" + "keep-tok-55"
	pw := "smtp-" + "keep-pw-66"
	st := e.server.defaultsSettings()
	st.NtfyTopicURL = "https://ntfy.sh/encode"
	st.NtfyToken = tok
	st.SMTPHost = "mail.example.com"
	st.SMTPFrom = "a@example.com"
	st.SMTPTo = "b@example.com"
	st.SMTPPassword = pw
	if err := e.server.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}

	// Lose the key and restart the store handle.
	origKey := swapStoreWithLostKey(t, e)

	// Capture log bytes from here on.
	var logBuf bytes.Buffer
	e.server.Log = slog.New(slog.NewTextHandler(&logBuf, nil))
	ts := e.serve(t)

	// GET still serves the env fallback (read path stays available)…
	resp, body := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET settings with lost key: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), tok) || strings.Contains(string(body), pw) {
		t.Fatal("fallback GET must not expose secrets")
	}
	// …and the fallback is logged at ERROR with an actionable message.
	logs := logBuf.String()
	if !strings.Contains(logs, "level=ERROR") {
		t.Errorf("settings fallback must log at Error, got: %s", logs)
	}
	if !strings.Contains(logs, "shares key") {
		t.Errorf("fallback log must name the shares key as the likely cause, got: %s", logs)
	}

	// PUT an UNRELATED field (scan interval), round-tripping the GET body.
	var putBody map[string]any
	if err := json.Unmarshal(body, &putBody); err != nil {
		t.Fatal(err)
	}
	putBody["scan_interval_seconds"] = 45
	resp2, body2 := doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, putBody)
	if resp2.StatusCode != 500 {
		t.Fatalf("PUT with unreadable stored settings: want 500, got %d %s", resp2.StatusCode, body2)
	}
	if !strings.Contains(string(body2), "shares key") {
		t.Errorf("500 message must point at the shares key, got: %s", body2)
	}

	// The stored secrets must be UNCHANGED: restore the original key and
	// prove the row still decrypts to the original values (the failed PUT
	// must not have overwritten anything).
	if err := os.WriteFile(filepath.Join(e.dir, "test.db.shares-key"), origKey, 0o600); err != nil {
		t.Fatal(err)
	}
	reopenStore(t, e)
	got, err := e.server.Store.GetSettings(ctx)
	if err != nil {
		t.Fatalf("stored settings undecryptable after failed PUT (secrets wiped?): %v", err)
	}
	if got.NtfyToken != tok || got.SMTPPassword != pw {
		t.Fatalf("stored secrets changed by the failed PUT: token=%q pw=%q", got.NtfyToken, got.SMTPPassword)
	}
	if got.ScanIntervalSeconds == 45 {
		t.Fatal("failed PUT must not persist the unrelated field either")
	}
}

// TestSaveSettingsFreshStoreStillMergesDefaults: the fail-closed guard must
// NOT break the legitimate "no row yet" case — a first PUT on a fresh store
// merges env defaults and succeeds.
func TestSaveSettingsFreshStoreStillMergesDefaults(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET: %d %s", resp.StatusCode, body)
	}
	var putBody map[string]any
	if err := json.Unmarshal(body, &putBody); err != nil {
		t.Fatal(err)
	}
	putBody["scan_interval_seconds"] = 45
	resp2, body2 := doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, putBody)
	if resp2.StatusCode != 200 {
		t.Fatalf("first PUT on fresh store: want 200, got %d %s", resp2.StatusCode, body2)
	}
	got, err := e.server.Store.GetSettings(context.Background())
	if err != nil || got == nil || got.ScanIntervalSeconds != 45 {
		t.Fatalf("first PUT not persisted: %+v err=%v", got, err)
	}
}
