package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// multipartAgentFields builds a multipart body from string fields + one file
// part. Returns the body reader and content type (with boundary).
func multipartAgentFields(fields map[string]string, fileField, fileName string, content []byte) (*bytes.Reader, string) {
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile(fileField, fileName)
	fw.Write(content)
	mw.Close()
	return bytes.NewReader(body.Bytes()), mw.FormDataContentType()
}

// platformHTTP flips enc-01's platform via a heartbeat against the live
// httptest server, returning nothing. Used by the per-platform download /
// manifest tests.
func platformHTTP(t *testing.T, e *testEnv, url string, platform string) {
	t.Helper()
	hb := platformHeartbeat("enc-01", 0, platform)
	resp, body := doJSON(t, "POST", url+"/api/agent/heartbeat", e.token, hb)
	if resp.StatusCode != 200 {
		t.Fatalf("set platform heartbeat: %d %s", resp.StatusCode, body)
	}
}

// publishAgentPlatform posts a platform-scoped agent publish (version + file
// + platform) using a multipart body. Mirrors publishMultipart but adds the
// platform field.
func publishAgentPlatform(t *testing.T, url, version, platform string, content []byte) (*http.Response, []byte) {
	t.Helper()
	body, ct := multipartAgentFields(map[string]string{
		"version":  version,
		"platform": platform,
	}, "file", "agent", content)
	req, _ := http.NewRequest("POST", url, body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer "+adminTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 8192)
	n, _ := resp.Body.Read(buf)
	return resp, buf[:n]
}

// TestManifestLinuxNodeScopedToLinuxFields verifies handleManifest returns a
// manifest whose agent_version/agent_sha256 come from the LINUX fields for a
// Linux node, and whose lib/bin fields are ZEROED (linux nodes must not sync
// windows-only lib/bin).
func TestManifestLinuxNodeScopedToLinuxFields(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// Publish windows + linux + lib + bin so we can distinguish scoping.
	publishMultipart(t, ts.URL, "/api/updates/agent", "1.0.0", "agent.exe", []byte("MZ-windows"))
	publishAgentPlatform(t, ts.URL+"/api/updates/agent", "1.0.0", "linux", []byte("\x7fELF-linux"))
	publishMultipart(t, ts.URL, "/api/updates/lib", "1", "EncodeLib.ps1", []byte("Write-Output hi"))
	z := mkZip(t, map[string][]byte{"tool.exe": []byte("fake")})
	publishMultipart(t, ts.URL, "/api/updates/bin", "1", "bin.zip", z)

	// Flip the node to linux.
	platformHTTP(t, e, ts.URL, "linux")

	// Linux manifest: agent_version = linux version, lib/bin zeroed.
	resp, body := doJSON(t, "GET", ts.URL+"/api/agent/manifest", e.token, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("manifest: %d %s", resp.StatusCode, body)
	}
	var m model.UpdateManifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.AgentVersion != "1.0.0" {
		t.Fatalf("linux node agent_version = %q, want 1.0.0", m.AgentVersion)
	}
	if m.AgentSHA256 == "" {
		t.Fatal("linux node agent_sha256 empty")
	}
	if m.LibVersion != 0 || m.LibSHA256 != "" {
		t.Fatalf("linux node must not see lib: %+v", m)
	}
	if m.BinVersion != 0 || m.BinSHA256 != "" {
		t.Fatalf("linux node must not see bin: %+v", m)
	}
}

// TestManifestWindowsNodeUnscoped verifies a windows (legacy empty) node gets
// the windows agent fields + real lib/bin (the historical, unscoped view).
func TestManifestWindowsNodeUnscoped(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	publishMultipart(t, ts.URL, "/api/updates/agent", "2.0.0", "agent.exe", []byte("MZ-windows"))
	publishAgentPlatform(t, ts.URL+"/api/updates/agent", "2.0.0", "linux", []byte("\x7fELF-linux"))
	publishMultipart(t, ts.URL, "/api/updates/lib", "1", "EncodeLib.ps1", []byte("lib"))
	z := mkZip(t, map[string][]byte{"tool.exe": []byte("fake")})
	publishMultipart(t, ts.URL, "/api/updates/bin", "1", "bin.zip", z)

	// Node platform stays empty (legacy windows default) — no heartbeat flip.
	resp, body := doJSON(t, "GET", ts.URL+"/api/agent/manifest", e.token, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("manifest: %d", resp.StatusCode)
	}
	var m model.UpdateManifest
	json.Unmarshal(body, &m)
	if m.AgentVersion != "2.0.0" {
		t.Fatalf("windows node agent_version = %q, want 2.0.0", m.AgentVersion)
	}
	if m.LibVersion != 1 || m.BinVersion != 1 {
		t.Fatalf("windows node must see real lib/bin: %+v", m)
	}
}

// TestDownloadAgentLinuxServesELF verifies a linux node downloading the
// agent gets the linux (ELF-ish) bytes, not the windows PE.
func TestDownloadAgentLinuxServesELF(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	publishMultipart(t, ts.URL, "/api/updates/agent", "1.0.0", "agent.exe", []byte("MZ-windows-bytes"))
	publishAgentPlatform(t, ts.URL+"/api/updates/agent", "1.0.0", "linux", []byte("\x7fELF-linux-bytes"))
	platformHTTP(t, e, ts.URL, "linux")

	req, _ := http.NewRequest("GET", ts.URL+"/api/agent/download/agent", nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("download: %d", resp.StatusCode)
	}
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	if string(buf[:n]) != "\x7fELF-linux-bytes" {
		t.Fatalf("linux node got %q, want ELF-linux-bytes", buf[:n])
	}
}

// TestDownloadAgentWindowsServesPE verifies a windows node downloading the
// agent gets the windows (PE-ish) bytes.
func TestDownloadAgentWindowsServesPE(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	publishMultipart(t, ts.URL, "/api/updates/agent", "1.0.0", "agent.exe", []byte("MZ-windows-bytes"))
	publishAgentPlatform(t, ts.URL+"/api/updates/agent", "1.0.0", "linux", []byte("\x7fELF-linux-bytes"))
	// Node stays windows (legacy default, no heartbeat flip).

	req, _ := http.NewRequest("GET", ts.URL+"/api/agent/download/agent", nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("download: %d", resp.StatusCode)
	}
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	if string(buf[:n]) != "MZ-windows-bytes" {
		t.Fatalf("windows node got %q, want MZ-windows-bytes", buf[:n])
	}
}

// TestDownloadAgentLinux404WhenOnlyWindowsPublished verifies a linux node
// hitting the download endpoint when only windows is published gets a clean
// 404 — NOT the windows PE binary (the merge-blocker this whole change
// fixes).
func TestDownloadAgentLinux404WhenOnlyWindowsPublished(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	publishMultipart(t, ts.URL, "/api/updates/agent", "1.0.0", "agent.exe", []byte("MZ-windows-only"))
	platformHTTP(t, e, ts.URL, "linux")

	req, _ := http.NewRequest("GET", ts.URL+"/api/agent/download/agent", nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for linux download with only windows published, got %d", resp.StatusCode)
	}
	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	if string(buf[:n]) == "MZ-windows-only" {
		t.Fatal("linux node was served the windows PE — merge-blocker regressed")
	}
}

// TestPublishAgentLinuxStoresToLinuxSlot verifies publishing with
// platform=linux stores to the linux slot (manifest linux fields populated,
// windows untouched).
func TestPublishAgentLinuxStoresToLinuxSlot(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := publishAgentPlatform(t, ts.URL+"/api/updates/agent", "1.0.0", "linux", []byte("\x7fELF-linux"))
	if resp.StatusCode != 200 {
		t.Fatalf("publish linux: %d %s", resp.StatusCode, body)
	}
	var m model.UpdateManifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.LinuxAgentVersion != "1.0.0" || m.LinuxAgentSHA256 == "" {
		t.Fatalf("linux slot not populated: %+v", m)
	}
	if m.AgentVersion != "" {
		t.Fatalf("windows slot disturbed by linux publish: %+v", m)
	}
}

// TestPublishAgentDefaultPlatformWindows verifies the default platform is
// windows when the field is absent — the existing UI/curl publish path must
// keep working unchanged. This is the back-compat test for the publish form.
func TestPublishAgentDefaultPlatformWindows(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// publishMultipart sends NO platform field — must default to windows.
	resp, body := publishMultipart(t, ts.URL, "/api/updates/agent", "1.0.0", "agent.exe", []byte("MZ-bytes"))
	if resp.StatusCode != 200 {
		t.Fatalf("default-platform publish: %d %s", resp.StatusCode, body)
	}
	var m model.UpdateManifest
	json.Unmarshal(body, &m)
	if m.AgentVersion != "1.0.0" {
		t.Fatalf("default publish did not hit windows slot: %+v", m)
	}
}

// TestPublishAgentInvalidPlatform400 verifies an unknown platform value is
// rejected with 400.
func TestPublishAgentInvalidPlatform400(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, _ := publishAgentPlatform(t, ts.URL+"/api/updates/agent", "1.0.0", "darwin", []byte("bytes"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid platform: want 400, got %d", resp.StatusCode)
	}
}
