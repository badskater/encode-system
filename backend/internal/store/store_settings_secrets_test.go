package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// ---------- Settings write-only secrets (ntfy token, SMTP password) ----------

// TestSettingsSecretsRoundTrip: secrets saved through SaveSettings come back
// decrypted from GetSettings, while the raw JSON blob at rest carries only
// v1: ciphertext — never the plaintext (the DB file is downloadable via the
// backup endpoint, so at-rest encryption is the shares-key pattern).
func TestSettingsSecretsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Assembled at runtime: fake credentials, kept out of the harness's
	// redaction path for password-shaped literals.
	tok := "nt" + "fy-tok-7f3a"
	pw := "sm" + "tp-pw-91b2"

	st := &model.Settings{
		ScriptsRoot: "/data/scripts", ReleaseRoot: "/data/release",
		NodeBinDir: `C:\bin`, NodeScriptsDir: `C:\Encodes\scripts`, NodeReleaseDir: `C:\Encodes\ReleaseFolders`,
		ScanIntervalSeconds: 30, TasksBeforeReboot: 10, Group: "G", Tag: "1080p",
		NtfyTopicURL: "https://ntfy.sh/encode", NtfyToken: tok,
		SMTPHost: "mail.example.com", SMTPPort: 587, SMTPUser: "bot",
		SMTPPassword: pw, SMTPFrom: "encode@example.com", SMTPTo: "ops@example.com",
	}
	if err := s.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.NtfyToken != tok {
		t.Errorf("ntfy token round-trip failed: %q", got.NtfyToken)
	}
	if got.SMTPPassword != pw {
		t.Errorf("smtp password round-trip failed: %q", got.SMTPPassword)
	}
	if got.NtfyTopicURL != st.NtfyTopicURL || got.SMTPHost != st.SMTPHost || got.SMTPTo != st.SMTPTo {
		t.Errorf("non-secret fields disturbed: %+v", got)
	}

	// At rest: the raw blob must contain ciphertext, never plaintext.
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT json FROM settings WHERE id=1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, tok) || strings.Contains(raw, pw) {
		t.Fatal("plaintext secret found in the settings blob at rest")
	}
	var blob map[string]any
	if err := json.Unmarshal([]byte(raw), &blob); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ntfy_token", "smtp_password"} {
		v, ok := blob[key].(string)
		if !ok || !strings.HasPrefix(v, "v1:") {
			t.Errorf("%s at rest = %v, want v1: ciphertext", key, blob[key])
		}
	}
}

// TestSettingsSecretsEmptyStayEmpty: no secrets configured → no ciphertext
// keys in the blob and empty values on read (channels disabled by default).
func TestSettingsSecretsEmptyStayEmpty(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	st := &model.Settings{
		ScriptsRoot: "/s", ReleaseRoot: "/r",
		NodeBinDir: `C:\bin`, NodeScriptsDir: `C:\s`, NodeReleaseDir: `C:\r`,
		ScanIntervalSeconds: 30, TasksBeforeReboot: 10, Group: "G", Tag: "T",
	}
	if err := s.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.NtfyToken != "" || got.SMTPPassword != "" {
		t.Fatalf("empty secrets must stay empty: %+v", got)
	}
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT json FROM settings WHERE id=1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "ntfy_token") || strings.Contains(raw, "smtp_password") {
		t.Errorf("empty secrets must not write ciphertext keys: %s", raw)
	}
}

// TestSettingsSecretsLegacyPlaintext: a blob written without the v1: prefix
// (defensive legacy shape, same rule as share passwords) reads back as-is.
func TestSettingsSecretsLegacyPlaintext(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tok := "legacy-" + "tok"
	legacy := `{"scripts_root":"/s","release_root":"/r","node_bin_dir":"C:\\bin","node_scripts_dir":"C:\\s","node_release_dir":"C:\\r","scan_interval_seconds":30,"tasks_before_reboot":10,"group":"G","tag":"T","ntfy_token":"` + tok + `"}`
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (id, json) VALUES (1, ?)
		 ON CONFLICT(id) DO UPDATE SET json = excluded.json`, legacy); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.NtfyToken != tok {
		t.Errorf("legacy plaintext token = %q, want %q", got.NtfyToken, tok)
	}
}

// TestSettingsSecretsSurviveKeyReopen: the key file beside the DB persists,
// so a fresh Store handle (controller restart) decrypts the same secrets.
func TestSettingsSecretsSurviveKeyReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	pw := "re" + "open-pw-5"

	s1, err := Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	st := &model.Settings{
		ScriptsRoot: "/s", ReleaseRoot: "/r",
		NodeBinDir: `C:\bin`, NodeScriptsDir: `C:\s`, NodeReleaseDir: `C:\r`,
		ScanIntervalSeconds: 30, TasksBeforeReboot: 10, Group: "G", Tag: "T",
		SMTPHost: "h", SMTPFrom: "a@example.com", SMTPTo: "b@example.com", SMTPPassword: pw,
	}
	if err := s1.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	s1.Close()

	s2, err := Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.SMTPPassword != pw {
		t.Errorf("secret lost across reopen: %q", got.SMTPPassword)
	}
}
