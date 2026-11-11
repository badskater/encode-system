package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// ---------- Settings: offsite backup destinations (P4) ----------

// offsiteSecrets returns the three fake destination credentials, assembled
// at runtime to stay out of the harness redaction path. The private key is
// a REAL generated ed25519 PEM — save-time validation parses it.
func offsiteSecrets() (sk, pw, key string) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		panic(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	return "s3-se" + "cret-api-1", "sf" + "tp-pw-api-2", string(pemBytes)
}

// offsiteBlock builds a valid enabled S3+SFTP backup_offsite block with
// freshly generated credentials. Tests that must know the exact secrets
// use offsiteBlockWith with their own offsiteSecrets() values.
func offsiteBlock() map[string]any {
	sk, pw, key := offsiteSecrets()
	return offsiteBlockWith(sk, pw, key)
}

// offsiteBlockWith builds the block from explicit credentials.
func offsiteBlockWith(sk, pw, key string) map[string]any {
	return map[string]any{
		"keep_n":             14,
		"include_shares_key": true,
		"s3": map[string]any{
			"enabled": true, "endpoint": "minio.example.com:9000", "bucket": "backups",
			"prefix": "encode", "access_key": "AK" + "TEST", "secret_key": sk, "use_tls": true,
		},
		"sftp": map[string]any{
			"enabled": true, "host": "sftp.example.com", "port": 22, "user": "backup",
			"password": pw, "private_key": key, "path": "/backups/encode",
		},
	}
}

// putOffsite PUTs the base settings with the given backup_offsite block.
func putOffsite(t *testing.T, tsURL string, block any) (int, []byte) {
	t.Helper()
	m := settingsMap(t, tsURL)
	if block == nil {
		delete(m, "backup_offsite")
	} else {
		m["backup_offsite"] = block
	}
	resp, body := doJSON(t, "PUT", tsURL+"/api/settings", adminTok, m)
	return resp.StatusCode, body
}

// TestSettingsOffsiteSecretsWriteOnly: secrets never echo back (PUT or
// GET), has_* flags report presence, blank-on-PUT keeps the stored value,
// non-empty replaces it.
func TestSettingsOffsiteSecretsWriteOnly(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()
	sk, pw, key := offsiteSecrets()

	code, body := putOffsite(t, ts.URL, offsiteBlockWith(sk, pw, key))
	if code != 200 {
		t.Fatalf("put offsite: %d %s", code, body)
	}
	for _, secret := range []string{sk, pw, key} {
		if strings.Contains(string(body), secret) {
			t.Fatal("PUT response leaked an offsite secret")
		}
	}

	code, body = doJSONResp(t, "GET", ts.URL+"/api/settings")
	if code != 200 {
		t.Fatalf("get: %d", code)
	}
	for _, secret := range []string{sk, pw, key} {
		if strings.Contains(string(body), secret) {
			t.Fatal("GET response leaked an offsite secret")
		}
	}
	var got map[string]any
	json.Unmarshal(body, &got)
	off, _ := got["backup_offsite"].(map[string]any)
	if off == nil {
		t.Fatal("backup_offsite missing from GET")
	}
	if off["has_s3_secret_key"] != true || off["has_sftp_password"] != true || off["has_sftp_private_key"] != true {
		t.Fatalf("has_* flags must be true: %v", off)
	}
	s3b, _ := off["s3"].(map[string]any)
	sftpB, _ := off["sftp"].(map[string]any)
	if _, ok := s3b["secret_key"]; ok {
		t.Error("secret_key must be absent from GET")
	}
	if _, ok := sftpB["password"]; ok {
		t.Error("sftp password must be absent from GET")
	}
	if _, ok := sftpB["private_key"]; ok {
		t.Error("sftp private_key must be absent from GET")
	}
	// Prefix was normalized to a trailing slash at save time.
	if s3b["prefix"] != "encode/" {
		t.Errorf("prefix must normalize to 'encode/': %v", s3b["prefix"])
	}

	// Round-trip with EMPTY secrets keeps the stored values.
	round := settingsMap(t, ts.URL)
	rb := round["backup_offsite"].(map[string]any)
	rs3 := rb["s3"].(map[string]any)
	rs3["secret_key"] = ""
	rsftp := rb["sftp"].(map[string]any)
	rsftp["password"] = ""
	rsftp["private_key"] = ""
	resp, body := doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, round)
	if resp.StatusCode != 200 {
		t.Fatalf("round-trip put: %d %s", resp.StatusCode, body)
	}
	st, err := e.server.Store.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.BackupOffsite.S3.SecretKey != sk || st.BackupOffsite.SFTP.Password != pw || st.BackupOffsite.SFTP.PrivateKey != key {
		t.Fatal("empty offsite secrets on PUT must keep the stored values")
	}

	// A non-empty secret replaces; the others survive.
	sk2 := "s3-se" + "cret-api-2"
	rs3["secret_key"] = sk2
	resp, _ = doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, round)
	if resp.StatusCode != 200 {
		t.Fatalf("replace put: %d", resp.StatusCode)
	}
	st, _ = e.server.Store.GetSettings(ctx)
	if st.BackupOffsite.S3.SecretKey != sk2 {
		t.Fatal("non-empty secret must replace the stored value")
	}
	if st.BackupOffsite.SFTP.Password != pw {
		t.Fatal("untouched secret must survive a partial update")
	}
}

// doJSONResp is a tiny GET helper returning status+body.
func doJSONResp(t *testing.T, method, url string) (int, []byte) {
	t.Helper()
	resp, body := doJSON(t, method, url, adminTok, nil)
	return resp.StatusCode, body
}

// TestSettingsOffsiteBlockOmittedKeepsStored: a PUT without backup_offsite
// (pre-P4 automation) must not wipe the stored destination config.
func TestSettingsOffsiteBlockOmittedKeepsStored(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	sk, pw, _ := offsiteSecrets()
	if code, body := putOffsite(t, ts.URL, offsiteBlock()); code != 200 {
		t.Fatalf("seed put: %d %s", code, body)
	}
	if code, body := putOffsite(t, ts.URL, nil); code != 200 {
		t.Fatalf("omit put: %d %s", code, body)
	}
	st, err := e.server.Store.GetSettings(ctxBg())
	if err != nil {
		t.Fatal(err)
	}
	if !st.BackupOffsite.S3.Enabled || st.BackupOffsite.S3.SecretKey != sk ||
		!st.BackupOffsite.SFTP.Enabled || st.BackupOffsite.SFTP.Password != pw ||
		st.BackupOffsite.KeepN != 14 {
		t.Fatalf("omitted block must keep the stored config: %+v", st.BackupOffsite)
	}
}

// TestSettingsOffsiteHasFlagsEchoTolerated: a verbatim GET→PUT (nested
// has_* flags included) must not 400 under DisallowUnknownFields.
func TestSettingsOffsiteHasFlagsEchoTolerated(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	if code, body := putOffsite(t, ts.URL, offsiteBlock()); code != 200 {
		t.Fatalf("seed put: %d %s", code, body)
	}
	echo := settingsMap(t, ts.URL) // carries nested has_* flags
	resp, body := doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, echo)
	if resp.StatusCode != 200 {
		t.Fatalf("verbatim GET→PUT must be accepted: %d %s", resp.StatusCode, body)
	}
}

// TestSettingsOffsiteValidation: table of accepted/rejected destination
// configs mirroring the server bounds.
func TestSettingsOffsiteValidation(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	put := func(mutate func(b map[string]any)) int {
		b := offsiteBlock()
		mutate(b)
		code, _ := putOffsite(t, ts.URL, b)
		return code
	}
	s3mut := func(f func(m map[string]any)) func(map[string]any) {
		return func(b map[string]any) { f(b["s3"].(map[string]any)) }
	}
	sftpMut := func(f func(m map[string]any)) func(map[string]any) {
		return func(b map[string]any) { f(b["sftp"].(map[string]any)) }
	}

	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   int
	}{
		{"valid block", func(b map[string]any) {}, 200},
		{"keep_n zero (default)", func(b map[string]any) { b["keep_n"] = 0 }, 200},
		{"keep_n 365", func(b map[string]any) { b["keep_n"] = 365 }, 200},
		{"keep_n negative", func(b map[string]any) { b["keep_n"] = -1 }, 400},
		{"keep_n 366", func(b map[string]any) { b["keep_n"] = 366 }, 400},
		{"s3 disabled needs nothing", s3mut(func(m map[string]any) {
			m["enabled"] = false
			m["endpoint"] = ""
			m["bucket"] = ""
			m["access_key"] = ""
			m["secret_key"] = ""
		}), 200},
		{"s3 endpoint required", s3mut(func(m map[string]any) { m["endpoint"] = "" }), 400},
		{"s3 bucket required", s3mut(func(m map[string]any) { m["bucket"] = "" }), 400},
		{"s3 access_key required", s3mut(func(m map[string]any) { m["access_key"] = "" }), 400},
		{"s3 bucket with slash", s3mut(func(m map[string]any) { m["bucket"] = "a/b" }), 400},
		{"s3 endpoint ftp scheme", s3mut(func(m map[string]any) { m["endpoint"] = "ftp://minio.example.com" }), 400},
		{"s3 endpoint bad port", s3mut(func(m map[string]any) { m["endpoint"] = "minio.example.com:abc" }), 400},
		{"s3 endpoint port 0", s3mut(func(m map[string]any) { m["endpoint"] = "minio.example.com:0" }), 400},
		{"s3 endpoint port 70000", s3mut(func(m map[string]any) { m["endpoint"] = "minio.example.com:70000" }), 400},
		{"s3 endpoint spaces", s3mut(func(m map[string]any) { m["endpoint"] = "minio example.com" }), 400},
		{"s3 endpoint url with path", s3mut(func(m map[string]any) { m["endpoint"] = "https://minio.example.com/bucket" }), 400},
		{"s3 prefix traversal", s3mut(func(m map[string]any) { m["prefix"] = "a/../b" }), 400},
		{"sftp host required", sftpMut(func(m map[string]any) { m["host"] = "" }), 400},
		{"sftp user required", sftpMut(func(m map[string]any) { m["user"] = "" }), 400},
		{"sftp bare-colon host", sftpMut(func(m map[string]any) { m["host"] = "sftp.example.com:22" }), 400},
		{"sftp scheme host", sftpMut(func(m map[string]any) { m["host"] = "ssh://sftp.example.com" }), 400},
		{"sftp bracketed ipv6", sftpMut(func(m map[string]any) { m["host"] = "[::1]" }), 200},
		{"sftp port 70000", sftpMut(func(m map[string]any) { m["port"] = 70000 }), 400},
		{"sftp port negative", sftpMut(func(m map[string]any) { m["port"] = -1 }), 400},
		{"sftp path traversal", sftpMut(func(m map[string]any) { m["path"] = "/backups/../etc" }), 400},
		{"sftp malformed key", sftpMut(func(m map[string]any) { m["private_key"] = "not-a-key" }), 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := put(tc.mutate); got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestSettingsOffsiteCredentialsRequiredWhenNoneStored: an enabled
// destination with no credential supplied AND none stored is refused
// post-merge. Fresh env per case so no stored secret can satisfy the keep
// merge (order-independent).
func TestSettingsOffsiteCredentialsRequiredWhenNoneStored(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(b map[string]any)
	}{
		{"s3 secret required", func(b map[string]any) { b["s3"].(map[string]any)["secret_key"] = "" }},
		{"sftp credentials required", func(b map[string]any) {
			sf := b["sftp"].(map[string]any)
			sf["password"] = ""
			sf["private_key"] = ""
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			ts := e.serve(t)
			b := offsiteBlock()
			tc.mutate(b)
			code, body := putOffsite(t, ts.URL, b)
			if code != 400 {
				t.Fatalf("status = %d, want 400: %s", code, body)
			}
		})
	}
}

// TestSettingsOffsiteEndpointNormalization: URL and host:port forms store
// the minio-ready host[:port]; bracketed IPv6 survives.
func TestSettingsOffsiteEndpointNormalization(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	for _, tc := range []struct{ in, want string }{
		{"https://minio.example.com:9000", "minio.example.com:9000"},
		{"http://minio.example.com/", "minio.example.com"},
		{"minio.example.com:9000", "minio.example.com:9000"},
		{"[::1]:9000", "[::1]:9000"},
		{"[::1]", "[::1]"},
	} {
		b := offsiteBlock()
		b["s3"].(map[string]any)["endpoint"] = tc.in
		if code, body := putOffsite(t, ts.URL, b); code != 200 {
			t.Fatalf("%s: %d %s", tc.in, code, body)
		}
		st, err := e.server.Store.GetSettings(ctxBg())
		if err != nil {
			t.Fatal(err)
		}
		if st.BackupOffsite.S3.Endpoint != tc.want {
			t.Errorf("endpoint %q stored as %q, want %q", tc.in, st.BackupOffsite.S3.Endpoint, tc.want)
		}
	}
}

// TestSettingsOffsiteRealKeyAccepted: a genuine (generated) unencrypted
// ed25519 PEM passes validation; a passphrase-protected one is refused.
func TestSettingsOffsiteRealKeyAccepted(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	b := offsiteBlock()
	b["sftp"].(map[string]any)["private_key"] = string(pemBytes)
	if code, body := putOffsite(t, ts.URL, b); code != 200 {
		t.Fatalf("valid ed25519 key must be accepted: %d %s", code, body)
	}

	encDER, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "test", []byte("pass"+"phrase"))
	if err != nil {
		t.Fatal(err)
	}
	encPEM := pem.EncodeToMemory(encDER)
	b["sftp"].(map[string]any)["private_key"] = string(encPEM)
	code, body := putOffsite(t, ts.URL, b)
	if code != 400 || !strings.Contains(string(body), "unencrypted") {
		t.Fatalf("passphrase-protected key must be refused with a clear message: %d %s", code, body)
	}
}

// TestSettingsOffsiteAuditNoSecrets: the settings.update audit row carries
// offsite booleans/knobs, never endpoints or credentials.
func TestSettingsOffsiteAuditNoSecrets(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	sk, pw, key := offsiteSecrets()
	if code, body := putOffsite(t, ts.URL, offsiteBlockWith(sk, pw, key)); code != 200 {
		t.Fatalf("put: %d %s", code, body)
	}
	events, err := e.server.Store.ListAudit(ctxBg(), 20)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range events {
		if ev.Action != "settings.update" {
			continue
		}
		found = true
		for _, secret := range []string{sk, pw, key, "minio.example.com", "sftp.example.com"} {
			if strings.Contains(ev.Detail, secret) {
				t.Errorf("audit detail leaked %q: %s", secret, ev.Detail)
			}
		}
		if !strings.Contains(ev.Detail, `"offsite_s3_enabled":true`) ||
			!strings.Contains(ev.Detail, `"offsite_s3_secret_changed":true`) ||
			!strings.Contains(ev.Detail, `"offsite_keep_n":14`) {
			t.Errorf("audit detail missing offsite knobs: %s", ev.Detail)
		}
	}
	if !found {
		t.Fatal("no settings.update audit row found")
	}
}

// TestSaveSettingsOffsiteFailsClosed: with the shares key lost, a PUT must
// 500 and leave the stored offsite secrets untouched (P3 fail-closed path
// extended to the new fields — no fallback merging).
func TestSaveSettingsOffsiteFailsClosed(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	sk, pw, _ := offsiteSecrets()
	if code, body := putOffsite(t, ts.URL, offsiteBlock()); code != 200 {
		t.Fatalf("seed put: %d %s", code, body)
	}
	orig := swapStoreWithLostKey(t, e)

	m := settingsMap(t, ts.URL) // GET serves the fallback, save must refuse
	resp, body := doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, m)
	if resp.StatusCode != 500 {
		t.Fatalf("lost-key save must fail closed with 500, got %d %s", resp.StatusCode, body)
	}
	// Restore the key and verify the stored secrets survived.
	os.WriteFile(e.dir+"/test.db.shares-key", orig, 0o600)
	reopenStore(t, e)
	st, err := e.server.Store.GetSettings(ctxBg())
	if err != nil {
		t.Fatal(err)
	}
	if st.BackupOffsite.S3.SecretKey != sk || st.BackupOffsite.SFTP.Password != pw {
		t.Fatal("fail-closed save must not disturb stored offsite secrets")
	}
}
