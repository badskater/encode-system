package api

// Regressions for the P4 quality review (settings surface):
//   - D1: SFTP host_key is REQUIRED when the destination is enabled, parsed
//     at save time (known-hosts style or bare base64), normalized, and
//     garbage is refused with an actionable ssh-keyscan hint.
//   - Q2: GET /api/settings must not expose s3.access_key to viewer sessions
//     or read-scope API tokens (admins keep the full value).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// offsiteHostKey generates a fresh ed25519 public host key in known-hosts
// form ("ssh-ed25519 AAAA…").
func offsiteHostKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(sshPub.Marshal())
}

// TestSettingsOffsiteHostKeyValidation: save-time pinning requirements.
func TestSettingsOffsiteHostKeyValidation(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	hk := offsiteHostKey(t)

	put := func(mutate func(sf map[string]any)) (int, string) {
		b := offsiteBlock()
		mutate(b["sftp"].(map[string]any))
		code, body := putOffsite(t, ts.URL, b)
		return code, string(body)
	}

	// Known-hosts style accepted and stored normalized (type + base64).
	code, body := put(func(sf map[string]any) { sf["host_key"] = hk })
	if code != 200 {
		t.Fatalf("valid host key must be accepted: %d %s", code, body)
	}
	st, err := e.server.Store.GetSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.BackupOffsite.SFTP.HostKey != hk {
		t.Fatalf("host key must store normalized, got %q want %q", st.BackupOffsite.SFTP.HostKey, hk)
	}

	// Bare base64 accepted, normalized to the known-hosts form.
	bare := strings.TrimPrefix(hk, "ssh-ed25519 ")
	code, body = put(func(sf map[string]any) { sf["host_key"] = bare })
	if code != 200 {
		t.Fatalf("bare base64 host key must be accepted: %d %s", code, body)
	}
	st, _ = e.server.Store.GetSettings(context.Background())
	if st.BackupOffsite.SFTP.HostKey != hk {
		t.Fatalf("bare key must normalize to %q, got %q", hk, st.BackupOffsite.SFTP.HostKey)
	}

	// Garbage refused with an actionable ssh-keyscan hint.
	code, body = put(func(sf map[string]any) { sf["host_key"] = "not-a-host-key" })
	if code != 400 || !strings.Contains(body, "ssh-keyscan") {
		t.Fatalf("garbage host key must 400 with ssh-keyscan hint: %d %s", code, body)
	}

	// Enabled SFTP without a host key is refused.
	code, body = put(func(sf map[string]any) { delete(sf, "host_key") })
	if code != 400 || !strings.Contains(body, "host_key") {
		t.Fatalf("enabled sftp without host_key must 400: %d %s", code, body)
	}

	// Disabled SFTP needs no host key.
	code, body = put(func(sf map[string]any) {
		sf["enabled"] = false
		delete(sf, "host_key")
	})
	if code != 200 {
		t.Fatalf("disabled sftp must not require host_key: %d %s", code, body)
	}

	// host_key is PUBLIC material: it round-trips on GET (unlike secrets).
	code, body = put(func(sf map[string]any) { sf["host_key"] = hk })
	if code != 200 {
		t.Fatalf("re-pin: %d %s", code, body)
	}
	resp, getBody := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(getBody), hk) {
		t.Fatalf("host_key must echo on GET (public material): %d %s", resp.StatusCode, getBody)
	}
}

// TestGetSettingsMasksAccessKeyForNonAdmin: viewer sessions and read-scope
// tokens must not see the S3 access_key value; admins must. The mask works
// on a copy (stored settings untouched) and reports presence via
// has_s3_access_key.
func TestGetSettingsMasksAccessKeyForNonAdmin(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	accessKey := "AKIA" + "VIEWERMASK42"
	sk, pw, key := offsiteSecrets()
	block := offsiteBlockWith(sk, pw, key)
	block["s3"].(map[string]any)["access_key"] = accessKey
	if code, body := putOffsite(t, ts.URL, block); code != 200 {
		t.Fatalf("seed: %d %s", code, body)
	}
	viewerTok := mintViewerSession(t, e, "offsite-viewer", "viewer-pass-off")
	readTok := mintAPIToken(t, e, "offsite-read", "read")
	adminScopeTok := mintAPIToken(t, e, "offsite-admin", "admin")

	for _, tc := range []struct {
		name     string
		bearer   string
		wantFull bool
	}{
		{"viewer session", viewerTok, false},
		{"read-scope api token", readTok, false},
		{"admin session", adminTok, true},
		{"admin-scope api token", adminScopeTok, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doJSON(t, "GET", ts.URL+"/api/settings", tc.bearer, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("GET: %d %s", resp.StatusCode, body)
			}
			got := string(body)
			if tc.wantFull {
				if !strings.Contains(got, accessKey) {
					t.Fatalf("admin must see the access_key: %s", got)
				}
				return
			}
			if strings.Contains(got, accessKey) {
				t.Fatalf("non-admin GET leaked s3.access_key: %s", got)
			}
			var parsed map[string]any
			if err := json.Unmarshal(body, &parsed); err != nil {
				t.Fatal(err)
			}
			off, _ := parsed["backup_offsite"].(map[string]any)
			if off == nil || off["has_s3_access_key"] != true {
				t.Fatalf("has_s3_access_key must report presence: %v", off)
			}
			// Endpoint/bucket stay visible (operational context, not secrets).
			if !strings.Contains(got, "minio.example.com") {
				t.Fatalf("endpoint must stay visible: %s", got)
			}
		})
	}

	// The mask never mutates the stored row: a fresh admin GET still sees it.
	resp, body := doJSON(t, "GET", ts.URL+"/api/settings", adminTok, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), accessKey) {
		t.Fatalf("viewer mask mutated stored settings: %d %s", resp.StatusCode, body)
	}
}
