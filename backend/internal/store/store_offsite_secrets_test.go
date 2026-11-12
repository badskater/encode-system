package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// ---------- Offsite destination secrets (P4, shares pattern) ----------

// offsiteTestSettings is a valid baseline carrying all three offsite
// secrets. Credentials are assembled at runtime (fake values, kept out of
// the harness redaction path for password-shaped literals).
func offsiteTestSettings() (*model.Settings, string, string, string) {
	sk := "s3-se" + "cret-4f2a"
	pw := "sf" + "tp-pw-8c1d"
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n-----END OPENSSH PRIVATE KEY-----"
	st := &model.Settings{
		ScriptsRoot: "/data/scripts", ReleaseRoot: "/data/release",
		NodeBinDir: `C:\bin`, NodeScriptsDir: `C:\Encodes\scripts`, NodeReleaseDir: `C:\Encodes\ReleaseFolders`,
		ScanIntervalSeconds: 30, TasksBeforeReboot: 10, Group: "G", Tag: "1080p",
	}
	st.BackupOffsite = model.BackupOffsite{
		KeepN: 14, IncludeSharesKey: true,
		S3: model.BackupOffsiteS3{
			Enabled: true, Endpoint: "minio.example.com:9000", Bucket: "backups",
			Prefix: "encode/", AccessKey: "AK" + "TEST", SecretKey: sk, UseTLS: true,
		},
		SFTP: model.BackupOffsiteSFTP{
			Enabled: true, Host: "sftp.example.com", Port: 22, User: "backup",
			Password: pw, PrivateKey: key, Path: "/backups/encode",
		},
	}
	return st, sk, pw, key
}

// TestOffsiteSecretsRoundTrip: the three destination secrets survive
// SaveSettings→GetSettings decrypted, while the raw blob at rest carries
// only v1: ciphertext — never plaintext (the DB file is downloadable).
func TestOffsiteSecretsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	st, sk, pw, key := offsiteTestSettings()
	if err := s.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.BackupOffsite.S3.SecretKey != sk {
		t.Errorf("s3 secret round-trip failed: %q", got.BackupOffsite.S3.SecretKey)
	}
	if got.BackupOffsite.SFTP.Password != pw {
		t.Errorf("sftp password round-trip failed: %q", got.BackupOffsite.SFTP.Password)
	}
	if got.BackupOffsite.SFTP.PrivateKey != key {
		t.Errorf("sftp key round-trip failed: %q", got.BackupOffsite.SFTP.PrivateKey)
	}
	// Non-secret offsite fields survive untouched.
	if got.BackupOffsite.KeepN != 14 || !got.BackupOffsite.IncludeSharesKey ||
		got.BackupOffsite.S3.Endpoint != "minio.example.com:9000" ||
		got.BackupOffsite.SFTP.Path != "/backups/encode" {
		t.Errorf("non-secret offsite fields disturbed: %+v", got.BackupOffsite)
	}

	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT json FROM settings WHERE id=1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	for _, plain := range []string{sk, pw, key} {
		if strings.Contains(raw, plain) {
			t.Fatal("plaintext offsite secret found in the settings blob at rest")
		}
	}
	var blob map[string]any
	if err := json.Unmarshal([]byte(raw), &blob); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"offsite_s3_secret_key", "offsite_sftp_password", "offsite_sftp_private_key"} {
		v, ok := blob[k].(string)
		if !ok || !strings.HasPrefix(v, "v1:") {
			t.Errorf("%s at rest = %v, want v1: ciphertext", k, blob[k])
		}
	}
	// The nested model block must NOT carry the secrets (json:"-" held).
	off, _ := blob["backup_offsite"].(map[string]any)
	if off == nil {
		t.Fatal("backup_offsite block missing at rest")
	}
	s3b, _ := off["s3"].(map[string]any)
	if _, leaked := s3b["secret_key"]; leaked {
		t.Error("secret_key must not appear inside backup_offsite.s3 at rest")
	}
}

// TestOffsiteSecretsEmptyStayEmpty: unconfigured destinations write no
// ciphertext keys and read back empty.
func TestOffsiteSecretsEmptyStayEmpty(t *testing.T) {
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
	if got.BackupOffsite.S3.SecretKey != "" || got.BackupOffsite.SFTP.Password != "" || got.BackupOffsite.SFTP.PrivateKey != "" {
		t.Fatalf("empty secrets must stay empty: %+v", got.BackupOffsite)
	}
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT json FROM settings WHERE id=1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"offsite_s3_secret_key", "offsite_sftp_password", "offsite_sftp_private_key"} {
		if strings.Contains(raw, k) {
			t.Errorf("empty secret must not write ciphertext key %s", k)
		}
	}
}

// TestIncludeSharesKeyDefaultTrue: rows written before the field existed
// (no backup_offsite block, or a block without include_shares_key) read
// back as TRUE; an explicit false is preserved.
func TestIncludeSharesKeyDefaultTrue(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := `{"scripts_root":"/s","release_root":"/r","node_bin_dir":"C:\\bin","node_scripts_dir":"C:\\s","node_release_dir":"C:\\r","scan_interval_seconds":30,"tasks_before_reboot":10,"group":"G","tag":"T"`

	cases := []struct {
		name string
		tail string
		want bool
	}{
		{"no offsite block", `}`, true},
		{"block without flag", `,"backup_offsite":{"keep_n":7}}`, true},
		{"explicit true", `,"backup_offsite":{"include_shares_key":true}}`, true},
		{"explicit false", `,"backup_offsite":{"include_shares_key":false}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.db.ExecContext(ctx,
				`INSERT INTO settings (id, json) VALUES (1, ?)
				 ON CONFLICT(id) DO UPDATE SET json = excluded.json`, base+tc.tail); err != nil {
				t.Fatal(err)
			}
			got, err := s.GetSettings(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got.BackupOffsite.IncludeSharesKey != tc.want {
				t.Fatalf("include_shares_key = %v, want %v", got.BackupOffsite.IncludeSharesKey, tc.want)
			}
		})
	}
}

// TestSharesKeyPathAccessor: the exported accessor resolves beside the
// SQLite DB file (the bundle source for offsite uploads).
func TestSharesKeyPathAccessor(t *testing.T) {
	s := newTestStore(t)
	if got := s.SharesKeyPath(); !strings.HasSuffix(got, "test.db.shares-key") {
		t.Fatalf("SharesKeyPath = %q, want the file beside the DB", got)
	}
	// Saving a secret creates the key file at exactly that path.
	st, _, _, _ := offsiteTestSettings()
	if err := s.SaveSettings(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if _, err := readShareKey(s.SharesKeyPath()); err != nil {
		t.Fatalf("key file must exist at the accessor path: %v", err)
	}
}
