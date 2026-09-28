package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestSharesCRUD verifies the share API: create returns masked output (no
// plaintext), list masks, update keeps the stored password when the field
// is omitted, and every mutation is audited.
func TestSharesCRUD(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// Create an SMB share.
	resp, out := doJSON(t, "POST", ts.URL+"/api/shares", adminTok, map[string]any{
		"name": "unraid-smb", "kind": "smb", "role": "scripts",
		"server": "unraid01", "path": "encodes", "username": "encode",
		"password": "s3cret", "enabled": true, "mount_path": `C:\Encodes\scripts`,
	})
	if resp.StatusCode != 201 {
		t.Fatalf("create = %d %s", resp.StatusCode, out)
	}
	// Plaintext must never appear in the response.
	if strings.Contains(string(out), "s3cret") {
		t.Fatal("create response leaked the plaintext password")
	}
	var created map[string]any
	if err := json.Unmarshal(out, &created); err != nil {
		t.Fatal(err)
	}
	if created["has_password"] != true {
		t.Fatalf("has_password = %v, want true", created["has_password"])
	}
	id := int64(created["id"].(float64))

	// List masks too.
	resp, out = doJSON(t, "GET", ts.URL+"/api/shares", adminTok, nil)
	if resp.StatusCode != 200 || strings.Contains(string(out), "s3cret") {
		t.Fatalf("list = %d leak=%v", resp.StatusCode, strings.Contains(string(out), "s3cret"))
	}

	// Get one.
	resp, out = doJSON(t, "GET", ts.URL+"/api/shares/"+itoa(id), adminTok, nil)
	if resp.StatusCode != 200 || strings.Contains(string(out), "s3cret") {
		t.Fatalf("get = %d leak=%v", resp.StatusCode, strings.Contains(string(out), "s3cret"))
	}

	// Update WITHOUT a password keeps the stored credential.
	resp, out = doJSON(t, "PUT", ts.URL+"/api/shares/"+itoa(id), adminTok, map[string]any{
		"name": "unraid-smb", "kind": "smb", "role": "scripts",
		"server": "unraid01", "path": "encodes2", "username": "encode", "enabled": true,
	})
	if resp.StatusCode != 200 {
		t.Fatalf("update = %d %s", resp.StatusCode, out)
	}
	stored, err := e.server.Store.GetShare(ctxBg(), id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Password != "s3cret" {
		t.Fatalf("password after no-cred update = %q, want kept", stored.Password)
	}
	if stored.Path != "encodes2" {
		t.Fatalf("path = %q, want encodes2", stored.Path)
	}

	// Update WITH a new password replaces it.
	resp, _ = doJSON(t, "PUT", ts.URL+"/api/shares/"+itoa(id), adminTok, map[string]any{
		"name": "unraid-smb", "kind": "smb", "role": "scripts",
		"server": "unraid01", "path": "encodes2", "username": "encode",
		"password": "n3wpass", "enabled": true,
	})
	if resp.StatusCode != 200 {
		t.Fatalf("update-pw = %d", resp.StatusCode)
	}
	stored, _ = e.server.Store.GetShare(ctxBg(), id)
	if stored.Password != "n3wpass" {
		t.Fatalf("password after update = %q, want n3wpass", stored.Password)
	}

	// Audited — and no plaintext anywhere in the audit log.
	_, audit := doJSON(t, "GET", ts.URL+"/api/audit?limit=20", adminTok, nil)
	var events []model.AuditEvent
	if err := json.Unmarshal(audit, &events); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, ev := range events {
		seen[ev.Action] = true
		if strings.Contains(ev.Detail, "s3cret") || strings.Contains(ev.Detail, "n3wpass") {
			t.Fatalf("audit leaked a password: %s", ev.Detail)
		}
	}
	for _, want := range []string{"share.create", "share.update"} {
		if !seen[want] {
			t.Fatalf("audit missing %s: %v", want, seen)
		}
	}

	// Delete.
	resp, _ = doJSON(t, "DELETE", ts.URL+"/api/shares/"+itoa(id), adminTok, nil)
	if resp.StatusCode != 204 {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	resp, _ = doJSON(t, "GET", ts.URL+"/api/shares/"+itoa(id), adminTok, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("get-after-delete = %d", resp.StatusCode)
	}
}

// TestSharesValidation covers per-kind required fields and enum rejection.
func TestSharesValidation(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"bad kind", map[string]any{"name": "x", "kind": "ftp", "role": "scripts", "server": "s", "path": "p"}},
		{"bad role", map[string]any{"name": "x", "kind": "smb", "role": "media", "server": "s", "path": "p"}},
		{"smb no server", map[string]any{"name": "x", "kind": "smb", "role": "scripts", "path": "p"}},
		{"s3 no bucket", map[string]any{"name": "x", "kind": "s3", "role": "release", "endpoint": "http://minio:9000"}},
		{"s3 no endpoint/server", map[string]any{"name": "x", "kind": "s3", "role": "release", "path": "bucket"}},
		{"no name", map[string]any{"kind": "smb", "role": "scripts", "server": "s", "path": "p"}},
	}
	for _, tc := range cases {
		resp, _ := doJSON(t, "POST", ts.URL+"/api/shares", adminTok, tc.body)
		if resp.StatusCode != 400 {
			t.Errorf("%s: create = %d, want 400", tc.name, resp.StatusCode)
		}
	}

	// Valid S3 with endpoint only.
	resp, _ := doJSON(t, "POST", ts.URL+"/api/shares", adminTok, map[string]any{
		"name": "minio", "kind": "s3", "role": "release", "path": "releases",
		"endpoint": "http://minio:9000", "username": "AKIA", "password": "secret", "enabled": true,
	})
	if resp.StatusCode != 201 {
		t.Fatalf("valid s3 = %d", resp.StatusCode)
	}
}

// TestShareAuthRejectsAnonymous verifies /api/shares requires admin (401
// without a token, 403 for a read-scoped API token on writes).
func TestShareAuthRejectsAnonymous(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, _ := doJSON(t, "GET", ts.URL+"/api/shares", "", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous = %d, want 401", resp.StatusCode)
	}
	// read-scoped token: GET allowed, POST forbidden.
	_, tok := doJSON(t, "POST", ts.URL+"/api/tokens", adminTok, map[string]any{"name": "share-reader", "scope": "read"})
	var created map[string]any
	if err := json.Unmarshal(tok, &created); err != nil {
		t.Fatal(err)
	}
	rtok := created["token"].(string)
	resp, _ = doJSON(t, "GET", ts.URL+"/api/shares", rtok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("read-token GET = %d, want 200", resp.StatusCode)
	}
	resp, _ = doJSON(t, "POST", ts.URL+"/api/shares", rtok, map[string]any{
		"name": "x", "kind": "smb", "role": "scripts", "server": "s", "path": "p",
	})
	if resp.StatusCode != 403 {
		t.Fatalf("read-token POST = %d, want 403", resp.StatusCode)
	}
}
