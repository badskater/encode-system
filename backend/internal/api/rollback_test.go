package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// multipartAgent mirrors how the Settings page publishes an agent binary:
// multipart form-data with version + file fields.
func multipartAgent(t *testing.T, url, token, version, content string) (*http.Response, []byte) {
	t.Helper()
	body := "--bnd\r\n" +
		"Content-Disposition: form-data; name=\"version\"\r\n\r\n" + version + "\r\n" +
		"--bnd\r\n" +
		"Content-Disposition: form-data; name=\"file\"; filename=\"agent.exe\"\r\n" +
		"Content-Type: application/octet-stream\r\n\r\n" + content + "\r\n" +
		"--bnd--\r\n"
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary=bnd")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := make([]byte, 4096)
	n, _ := resp.Body.Read(out)
	return resp, out[:n]
}

// TestAgentRollbackEndpoint verifies the full rollback flow: publish v1,
// publish v2 (v1 rotates to prev), rollback -> manifest serves v1 with v2
// as prev, and a second rollback returns to v2.
func TestAgentRollbackEndpoint(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	if resp, body := multipartAgent(t, ts.URL+"/api/updates/agent", adminTok, "1.0.0", "binary-v1"); resp.StatusCode != 200 {
		t.Fatalf("publish v1: %d %s", resp.StatusCode, body)
	}
	if resp, body := multipartAgent(t, ts.URL+"/api/updates/agent", adminTok, "1.1.0", "binary-v2"); resp.StatusCode != 200 {
		t.Fatalf("publish v2: %d %s", resp.StatusCode, body)
	}

	// Manifest shows v2 current, v1 prev.
	resp, body := doJSON(t, "GET", ts.URL+"/api/updates/manifest", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("manifest: %d", resp.StatusCode)
	}
	var m model.UpdateManifest
	json.Unmarshal(body, &m)
	if m.AgentVersion != "1.1.0" || m.PrevAgentVersion != "1.0.0" {
		t.Fatalf("manifest = agent %q prev %q, want 1.1.0/1.0.0", m.AgentVersion, m.PrevAgentVersion)
	}

	// Rollback.
	resp, body = doJSON(t, "POST", ts.URL+"/api/updates/agent/rollback", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("rollback: %d %s", resp.StatusCode, body)
	}
	json.Unmarshal(body, &m)
	if m.AgentVersion != "1.0.0" || m.PrevAgentVersion != "1.1.0" {
		t.Fatalf("after rollback = agent %q prev %q, want 1.0.0/1.1.0", m.AgentVersion, m.PrevAgentVersion)
	}

	// The download endpoint must now serve the v1 bytes.
	dReq, _ := http.NewRequest("GET", ts.URL+"/api/agent/download/agent", nil)
	dReq.Header.Set("Authorization", "Bearer "+e.token)
	dResp, err := http.DefaultClient.Do(dReq)
	if err != nil {
		t.Fatal(err)
	}
	defer dResp.Body.Close()
	buf := make([]byte, 64)
	n, _ := dResp.Body.Read(buf)
	if string(buf[:n]) != "binary-v1" {
		t.Fatalf("served payload = %q, want binary-v1", buf[:n])
	}

	// Second rollback returns to v2 (symmetry).
	resp, body = doJSON(t, "POST", ts.URL+"/api/updates/agent/rollback", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("second rollback: %d %s", resp.StatusCode, body)
	}
	json.Unmarshal(body, &m)
	if m.AgentVersion != "1.1.0" {
		t.Fatalf("after second rollback agent = %q, want 1.1.0", m.AgentVersion)
	}
}

// TestAgentRollbackWithoutPrev verifies a 409 (not 500) when there is no
// rollback slot, and that anonymous callers are refused.
func TestAgentRollbackWithoutPrev(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// Single publish: nothing to roll back to.
	if resp, body := multipartAgent(t, ts.URL+"/api/updates/agent", adminTok, "1.0.0", "binary-v1"); resp.StatusCode != 200 {
		t.Fatalf("publish: %d %s", resp.StatusCode, body)
	}
	resp, body := doJSON(t, "POST", ts.URL+"/api/updates/agent/rollback", adminTok, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("rollback without prev: %d, want 409 (%s)", resp.StatusCode, body)
	}

	// No publish at all: still a clean 409.
	e2 := newTestEnv(t)
	ts2 := e2.serve(t)
	resp, _ = doJSON(t, "POST", ts2.URL+"/api/updates/agent/rollback", adminTok, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("rollback on empty store: %d, want 409", resp.StatusCode)
	}

	// Anonymous refused.
	resp, _ = doJSON(t, "POST", ts.URL+"/api/updates/agent/rollback", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous rollback: %d, want 401", resp.StatusCode)
	}
}
