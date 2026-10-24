package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// multipartAgentPlatform publishes an agent binary for a specific platform,
// mirroring how the Settings page uploads: multipart form-data with version,
// file, and platform fields.
func multipartAgentPlatform(t *testing.T, url, token, version, platform, content string) (*http.Response, []byte) {
	t.Helper()
	body := "--bnd\r\n" +
		"Content-Disposition: form-data; name=\"version\"\r\n\r\n" + version + "\r\n" +
		"--bnd\r\n" +
		"Content-Disposition: form-data; name=\"platform\"\r\n\r\n" + platform + "\r\n" +
		"--bnd\r\n" +
		"Content-Disposition: form-data; name=\"file\"; filename=\"agent.bin\"\r\n" +
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
	out := make([]byte, 8192)
	n, _ := resp.Body.Read(out)
	return resp, out[:n]
}

// publishLib uploads an EncodeLib.ps1 via multipart form-data (mirrors the
// Settings page). The version field must be a positive integer string.
func publishLib(t *testing.T, url, token, version, content string) (*http.Response, []byte) {
	t.Helper()
	body := "--bnd\r\n" +
		"Content-Disposition: form-data; name=\"version\"\r\n\r\n" + version + "\r\n" +
		"--bnd\r\n" +
		"Content-Disposition: form-data; name=\"file\"; filename=\"EncodeLib.ps1\"\r\n" +
		"Content-Type: text/plain\r\n\r\n" + content + "\r\n" +
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
	out := make([]byte, 8192)
	n, _ := resp.Body.Read(out)
	return resp, out[:n]
}

// TestHeartbeatLinuxNodeGetsScopedUpdate is the ISSUE 3 regression: a Linux
// node whose linux_agent_version differs from its node.AgentVersion must
// receive instruction=update with Update carrying the LINUX version + sha in
// AgentVersion/AgentSHA256 and Lib*/Bin* zeroed. Before the fix the heartbeat
// path read the un-scoped (Windows) manifest fields — so a Linux node was
// handed the Windows sha, checksummed its downloaded Linux ELF against it, and
// refused the update forever.
func TestHeartbeatLinuxNodeGetsScopedUpdate(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Mark the test node as Linux and running the older Linux agent version.
	n, _ := e.server.Store.GetNode(ctx, e.node.ID)
	n.Platform = model.PlatformLinux
	n.AgentVersion = "1.0.0"
	if err := e.server.Store.UpdateNode(ctx, n); err != nil {
		t.Fatal(err)
	}

	// Publish a Windows agent (v2.0.0) and a Linux agent (v3.0.0): both slots
	// populated so the manifest carries DIFFERENT version/sha per platform.
	if resp, body := multipartAgentPlatform(t, ts.URL+"/api/updates/agent", adminTok, "2.0.0", "windows", "windows-binary-v2"); resp.StatusCode != 200 {
		t.Fatalf("publish windows: %d %s", resp.StatusCode, body)
	}
	if resp, body := multipartAgentPlatform(t, ts.URL+"/api/updates/agent", adminTok, "3.0.0", "linux", "linux-binary-v3"); resp.StatusCode != 200 {
		t.Fatalf("publish linux: %d %s", resp.StatusCode, body)
	}

	// Idle Linux heartbeat at the older version — should trigger update.
	hb := heartbeat("enc-01", 0, 0)
	hb.Platform = model.PlatformLinux
	hb.AgentVersion = "1.0.0"
	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, hb)
	if resp.StatusCode != 200 {
		t.Fatalf("heartbeat: %d %s", resp.StatusCode, body)
	}
	var reply model.HeartbeatReply
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if reply.Instruction != "update" {
		t.Fatalf("want update instruction for linux node, got %q (%s)", reply.Instruction, body)
	}
	if reply.Update == nil {
		t.Fatalf("update payload missing: %s", body)
	}
	// The scoped manifest must carry the LINUX version + sha — NOT the
	// Windows ones. This is the crux of the bug: pre-fix the Windows sha
	// was served, the Linux ELF checksummed against it, and the update was
	// refused forever.
	wantManifest := e.server.Update.Manifest()
	if reply.Update.AgentVersion != wantManifest.LinuxAgentVersion {
		t.Fatalf("Update.AgentVersion = %q, want linux %q", reply.Update.AgentVersion, wantManifest.LinuxAgentVersion)
	}
	if reply.Update.AgentSHA256 != wantManifest.LinuxAgentSHA256 {
		t.Fatalf("Update.AgentSHA256 = %q, want linux %q", reply.Update.AgentSHA256, wantManifest.LinuxAgentSHA256)
	}
	// Linux nodes must never carry lib/bin versions (Windows-only artifacts).
	if reply.Update.LibVersion != 0 || reply.Update.LibSHA256 != "" {
		t.Fatalf("scoped linux update must zero lib: lib_version=%d lib_sha256=%q", reply.Update.LibVersion, reply.Update.LibSHA256)
	}
	if reply.Update.BinVersion != 0 || reply.Update.BinSHA256 != "" {
		t.Fatalf("scoped linux update must zero bin: bin_version=%d bin_sha256=%q", reply.Update.BinVersion, reply.Update.BinSHA256)
	}
	// The Windows sha must NOT appear (it would checksum-mismatch the ELF).
	if wantManifest.AgentSHA256 != "" && reply.Update.AgentSHA256 == wantManifest.AgentSHA256 {
		t.Fatal("scoped linux update served the WINDOWS sha — the bug")
	}
}

// TestHeartbeatLinuxMatchWindowsDiffersNoUpdate is the false-trigger case: a
// Linux node whose linux_agent_version MATCHES the node's version but whose
// WINDOWS version differs must NOT be told to update. Pre-fix the heartbeat
// compared the Windows version, so this node was falsely triggered.
func TestHeartbeatLinuxMatchWindowsDiffersNoUpdate(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Publish Windows v2.0.0 and Linux v3.0.0.
	if resp, body := multipartAgentPlatform(t, ts.URL+"/api/updates/agent", adminTok, "2.0.0", "windows", "windows-binary-v2"); resp.StatusCode != 200 {
		t.Fatalf("publish windows: %d %s", resp.StatusCode, body)
	}
	if resp, body := multipartAgentPlatform(t, ts.URL+"/api/updates/agent", adminTok, "3.0.0", "linux", "linux-binary-v3"); resp.StatusCode != 200 {
		t.Fatalf("publish linux: %d %s", resp.StatusCode, body)
	}

	// Linux node already running the current Linux version (3.0.0). The
	// Windows version (2.0.0) differs — pre-fix this node was falsely told
	// to update.
	n, _ := e.server.Store.GetNode(ctx, e.node.ID)
	n.Platform = model.PlatformLinux
	n.AgentVersion = "3.0.0"
	if err := e.server.Store.UpdateNode(ctx, n); err != nil {
		t.Fatal(err)
	}

	hb := heartbeat("enc-01", 0, 0)
	hb.Platform = model.PlatformLinux
	hb.AgentVersion = "3.0.0"
	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, hb)
	if resp.StatusCode != 200 {
		t.Fatalf("heartbeat: %d %s", resp.StatusCode, body)
	}
	var reply model.HeartbeatReply
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if reply.Instruction == "update" {
		t.Fatalf("linux node at current linux version must NOT be told to update (false trigger): %s", body)
	}
}

// TestHeartbeatWindowsNodeUnchangedBehavior verifies the scoping is a no-op
// for Windows nodes: a Windows node whose Windows version differs gets
// instruction=update with the Windows version/sha and the real lib/bin values.
func TestHeartbeatWindowsNodeUnchangedBehavior(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Publish Windows v2.0.0, Linux v3.0.0, and a lib at v1.
	if resp, body := multipartAgentPlatform(t, ts.URL+"/api/updates/agent", adminTok, "2.0.0", "windows", "windows-binary-v2"); resp.StatusCode != 200 {
		t.Fatalf("publish windows: %d %s", resp.StatusCode, body)
	}
	if resp, body := multipartAgentPlatform(t, ts.URL+"/api/updates/agent", adminTok, "3.0.0", "linux", "linux-binary-v3"); resp.StatusCode != 200 {
		t.Fatalf("publish linux: %d %s", resp.StatusCode, body)
	}
	if resp, body := publishLib(t, ts.URL+"/api/updates/lib", adminTok, "1", "lib-content-v1"); resp.StatusCode != 200 {
		t.Fatalf("publish lib: %d %s", resp.StatusCode, body)
	}

	// Windows node running the older Windows version + no lib.
	n, _ := e.server.Store.GetNode(ctx, e.node.ID)
	n.Platform = model.PlatformWindows
	n.AgentVersion = "1.0.0"
	n.LibVersion = 0
	if err := e.server.Store.UpdateNode(ctx, n); err != nil {
		t.Fatal(err)
	}

	hb := heartbeat("enc-01", 0, 0)
	hb.Platform = model.PlatformWindows
	hb.AgentVersion = "1.0.0"
	resp, body := doJSON(t, "POST", ts.URL+"/api/agent/heartbeat", e.token, hb)
	if resp.StatusCode != 200 {
		t.Fatalf("heartbeat: %d %s", resp.StatusCode, body)
	}
	var reply model.HeartbeatReply
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if reply.Instruction != "update" {
		t.Fatalf("windows node must get update, got %q (%s)", reply.Instruction, body)
	}
	if reply.Update == nil {
		t.Fatalf("update payload missing: %s", body)
	}
	wantManifest := e.server.Update.Manifest()
	if reply.Update.AgentVersion != wantManifest.AgentVersion {
		t.Fatalf("Update.AgentVersion = %q, want windows %q", reply.Update.AgentVersion, wantManifest.AgentVersion)
	}
	if reply.Update.AgentSHA256 != wantManifest.AgentSHA256 {
		t.Fatalf("Update.AgentSHA256 = %q, want windows %q", reply.Update.AgentSHA256, wantManifest.AgentSHA256)
	}
	// Windows nodes keep the real lib version (not zeroed).
	if reply.Update.LibVersion != wantManifest.LibVersion {
		t.Fatalf("Update.LibVersion = %d, want %d (windows must keep lib)", reply.Update.LibVersion, wantManifest.LibVersion)
	}
}

// TestManifestEndpointLinuxScoped is a companion check on handleManifest: a
// Linux node GET /api/agent/manifest receives the linux fields projected into
// agent_version/agent_sha256 with lib/bin zeroed; a Windows node gets the
// unchanged manifest. This guards the shared helper's manifest-path caller.
func TestManifestEndpointLinuxScoped(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	if resp, body := multipartAgentPlatform(t, ts.URL+"/api/updates/agent", adminTok, "2.0.0", "windows", "windows-binary-v2"); resp.StatusCode != 200 {
		t.Fatalf("publish windows: %d %s", resp.StatusCode, body)
	}
	if resp, body := multipartAgentPlatform(t, ts.URL+"/api/updates/agent", adminTok, "3.0.0", "linux", "linux-binary-v3"); resp.StatusCode != 200 {
		t.Fatalf("publish linux: %d %s", resp.StatusCode, body)
	}

	wantManifest := e.server.Update.Manifest()

	// Linux node: scoped.
	n, _ := e.server.Store.GetNode(ctx, e.node.ID)
	n.Platform = model.PlatformLinux
	if err := e.server.Store.UpdateNode(ctx, n); err != nil {
		t.Fatal(err)
	}
	resp, body := doJSON(t, "GET", ts.URL+"/api/agent/manifest", e.token, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("manifest: %d %s", resp.StatusCode, body)
	}
	var m model.UpdateManifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if m.AgentVersion != wantManifest.LinuxAgentVersion {
		t.Fatalf("linux manifest AgentVersion = %q, want %q", m.AgentVersion, wantManifest.LinuxAgentVersion)
	}
	if m.AgentSHA256 != wantManifest.LinuxAgentSHA256 {
		t.Fatalf("linux manifest AgentSHA256 = %q, want %q", m.AgentSHA256, wantManifest.LinuxAgentSHA256)
	}
	if m.LibVersion != 0 || m.BinVersion != 0 {
		t.Fatalf("linux manifest must zero lib/bin: lib=%d bin=%d", m.LibVersion, m.BinVersion)
	}

	// Windows node: unchanged.
	n.Platform = model.PlatformWindows
	if err := e.server.Store.UpdateNode(ctx, n); err != nil {
		t.Fatal(err)
	}
	resp, body = doJSON(t, "GET", ts.URL+"/api/agent/manifest", e.token, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("manifest: %d %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if m.AgentVersion != wantManifest.AgentVersion {
		t.Fatalf("windows manifest AgentVersion = %q, want %q", m.AgentVersion, wantManifest.AgentVersion)
	}
}
