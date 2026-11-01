package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDo records the last request and returns a canned response. It is the
// test double for doFunc — no real network is touched.
type fakeDo struct {
	method      string
	path        string
	bodyBytes   []byte
	contentType string

	status       int
	respBody     string
	respErr      error
	callCount    int
	recordBodies bool
}

func (f *fakeDo) do(method, path string, body io.Reader, contentType string) (*http.Response, error) {
	f.method = method
	f.path = path
	f.contentType = contentType
	f.callCount++
	if body != nil {
		b, err := io.ReadAll(body)
		if err != nil {
			panic(err)
		}
		f.bodyBytes = b
	} else {
		f.bodyBytes = nil
	}
	if f.respErr != nil {
		return nil, f.respErr
	}
	status := f.status
	if status == 0 {
		status = 200
	}
	resp := &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(f.respBody)),
		Header:     make(http.Header),
	}
	if f.contentType != "" {
		resp.Header.Set("Content-Type", f.contentType)
	}
	return resp, nil
}

// runCLI is a thin wrapper around run that uses a fake do and a bytes.Buffer
// for output. It resets the global connection vars first so a -server/-token
// from one test doesn't leak into the next.
func runCLI(t *testing.T, fd *fakeDo, args ...string) (string, error) {
	t.Helper()
	globalServer = ""
	globalToken = ""
	var out bytes.Buffer
	err := run(&out, args, fd.do)
	return out.String(), err
}

func TestNoCommandIsUsageError(t *testing.T) {
	fd := &fakeDo{}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token")
	if !isUsageError(err) {
		t.Fatalf("expected usage error, got %v", err)
	}
	if fd.callCount != 0 {
		t.Fatalf("expected no API call, got %d", fd.callCount)
	}
}

func TestUnknownCommandIsUsageError(t *testing.T) {
	fd := &fakeDo{}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "bogus")
	if !isUsageError(err) {
		t.Fatalf("expected usage error, got %v", err)
	}
}

func TestMissingServerIsUsageError(t *testing.T) {
	fd := &fakeDo{}
	_, err := runCLI(t, fd, "-token", "test-token", "health")
	if !isUsageError(err) {
		t.Fatalf("missing -server must be usage error, got %v", err)
	}
}

func TestMissingTokenIsUsageError(t *testing.T) {
	fd := &fakeDo{}
	_, err := runCLI(t, fd, "-server", "http://x", "health")
	if !isUsageError(err) {
		t.Fatalf("missing -token must be usage error, got %v", err)
	}
}

func TestEnvFallbackForServerAndToken(t *testing.T) {
	t.Setenv(envServer, "http://from-env")
	t.Setenv(envToken, "env-token")
	fd := &fakeDo{respBody: `{"ok":true}`}
	out, err := runCLI(t, fd, "health")
	if err != nil {
		t.Fatalf("env fallback should succeed, got %v", err)
	}
	if fd.method != "GET" || fd.path != "/api/health" {
		t.Fatalf("expected GET /api/health, got %s %s", fd.method, fd.path)
	}
	if globalServer != "http://from-env" {
		t.Fatalf("globalServer not set from env: %q", globalServer)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("expected output to include body, got %q", out)
	}
}

func TestFlagOverridesEnv(t *testing.T) {
	t.Setenv(envServer, "http://from-env")
	t.Setenv(envToken, "env-token")
	fd := &fakeDo{respBody: `{"ok":true}`}
	_, err := runCLI(t, fd, "-server", "http://flag", "-token", "flag-token", "health")
	if err != nil {
		t.Fatalf("flag override should succeed, got %v", err)
	}
	if globalServer != "http://flag" || globalToken != "flag-token" {
		t.Fatalf("flags should win over env: server=%q token=%q", globalServer, globalToken)
	}
}

func TestHealthRequestAndRender(t *testing.T) {
	fd := &fakeDo{respBody: `{"status":"ok","version":"1.2.3"}`}
	out, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "health")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if fd.method != "GET" || fd.path != "/api/health" {
		t.Fatalf("expected GET /api/health, got %s %s", fd.method, fd.path)
	}
	if !strings.Contains(out, "status") || !strings.Contains(out, "ok") {
		t.Fatalf("health output missing status: %q", out)
	}
}

func TestScanRequest(t *testing.T) {
	fd := &fakeDo{respBody: `{"triggered":true}`}
	out, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "scan")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if fd.method != "POST" || fd.path != "/api/scan" {
		t.Fatalf("expected POST /api/scan, got %s %s", fd.method, fd.path)
	}
	if !strings.Contains(out, "scan triggered") {
		t.Fatalf("expected 'scan triggered', got %q", out)
	}
}

func TestNodesRequestAndTable(t *testing.T) {
	fd := &fakeDo{respBody: `[{"id":1,"name":"node1","status":"online","platform":"linux","agent_version":"1.0","enabled":true,"active_jobs":1,"max_concurrent_jobs":2,"last_seen":null}]`}
	out, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "nodes")
	if err != nil {
		t.Fatalf("nodes: %v", err)
	}
	if fd.method != "GET" || fd.path != "/api/nodes" {
		t.Fatalf("expected GET /api/nodes, got %s %s", fd.method, fd.path)
	}
	// Header + one data row, tab-separated, greppable.
	if !strings.Contains(out, "ID\tNAME") {
		t.Fatalf("missing table header: %q", out)
	}
	if !strings.Contains(out, "node1") || !strings.Contains(out, "linux") {
		t.Fatalf("missing node data: %q", out)
	}
}

func TestNodesEmptyPlatformDefaultsToWindows(t *testing.T) {
	fd := &fakeDo{respBody: `[{"id":2,"name":"winbox","status":"online","platform":"","agent_version":"1.0"}]`}
	out, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "nodes")
	if err != nil {
		t.Fatalf("nodes: %v", err)
	}
	// The model treats "" as "windows" (historical fleet default).
	if !strings.Contains(out, "winbox\t") {
		t.Fatalf("expected winbox row: %q", out)
	}
	fields := strings.Split(strings.Split(out, "\n")[1], "\t")
	if fields[3] != "windows" {
		t.Fatalf("empty platform should render as windows, got %q", fields[3])
	}
}

func TestJobsDefaultRequest(t *testing.T) {
	fd := &fakeDo{respBody: `[]`}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "jobs")
	if err != nil {
		t.Fatalf("jobs: %v", err)
	}
	if fd.path != "/api/jobs" {
		t.Fatalf("expected bare /api/jobs, got %s", fd.path)
	}
}

func TestJobsStatusAndLimitQuery(t *testing.T) {
	fd := &fakeDo{respBody: `[]`}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "jobs", "-status", "failed", "-limit", "5")
	if err != nil {
		t.Fatalf("jobs: %v", err)
	}
	// Query order is deterministic (status then limit).
	if fd.path != "/api/jobs?status=failed&limit=5" {
		t.Fatalf("expected status+limit query, got %s", fd.path)
	}
}

func TestJobsRendersTable(t *testing.T) {
	fd := &fakeDo{respBody: `[{"id":42,"series":"Show","episode":"01","status":"failed","node_id":1,"step":"encode","progress":0.5,"error":"boom","created_at":"2025-01-01T00:00:00Z"}]`}
	out, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "jobs")
	if err != nil {
		t.Fatalf("jobs: %v", err)
	}
	if !strings.Contains(out, "42\tShow") {
		t.Fatalf("expected job row with id+series: %q", out)
	}
	if !strings.Contains(out, "failed") {
		t.Fatalf("expected status in row: %q", out)
	}
}

func TestJobRetryRequest(t *testing.T) {
	fd := &fakeDo{respBody: `{"id":7,"status":"queued"}`}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "job-retry", "7")
	if err != nil {
		t.Fatalf("job-retry: %v", err)
	}
	if fd.method != "POST" || fd.path != "/api/jobs/7/retry" {
		t.Fatalf("expected POST /api/jobs/7/retry, got %s %s", fd.method, fd.path)
	}
}

func TestJobCancelRequest(t *testing.T) {
	fd := &fakeDo{respBody: `{"status":"cancelled"}`}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "job-cancel", "9")
	if err != nil {
		t.Fatalf("job-cancel: %v", err)
	}
	if fd.method != "POST" || fd.path != "/api/jobs/9/cancel" {
		t.Fatalf("expected POST /api/jobs/9/cancel, got %s %s", fd.method, fd.path)
	}
}

func TestJobDeleteRequest(t *testing.T) {
	fd := &fakeDo{respBody: `{"status":"deleted"}`}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "job-delete", "10")
	if err != nil {
		t.Fatalf("job-delete: %v", err)
	}
	// DELETE against the job's own resource (new endpoint).
	if fd.method != "DELETE" || fd.path != "/api/jobs/10" {
		t.Fatalf("expected DELETE /api/jobs/10, got %s %s", fd.method, fd.path)
	}
}

func TestJobRetryMissingIdIsUsageError(t *testing.T) {
	fd := &fakeDo{}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "job-retry")
	if !isUsageError(err) {
		t.Fatalf("missing id must be usage error, got %v", err)
	}
}

func TestJobRetryNonNumericIdIsUsageError(t *testing.T) {
	fd := &fakeDo{}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "job-retry", "abc")
	if !isUsageError(err) {
		t.Fatalf("non-numeric id must be usage error, got %v", err)
	}
}

func TestPruneRequest(t *testing.T) {
	fd := &fakeDo{respBody: `{"deleted":3,"days":7}`}
	out, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "prune", "-days", "7")
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if fd.method != "POST" || fd.path != "/api/jobs/prune" {
		t.Fatalf("expected POST /api/jobs/prune, got %s %s", fd.method, fd.path)
	}
	if fd.contentType != "application/json" {
		t.Fatalf("expected json content-type, got %q", fd.contentType)
	}
	var req map[string]int
	if err := json.Unmarshal(fd.bodyBytes, &req); err != nil {
		t.Fatalf("prune body not json: %v", err)
	}
	if req["days"] != 7 {
		t.Fatalf("expected days=7 in body, got %v", req)
	}
	if !strings.Contains(out, "pruned 3 jobs older than 7 days") {
		t.Fatalf("unexpected prune output: %q", out)
	}
}

func TestPruneMissingDaysIsUsageError(t *testing.T) {
	fd := &fakeDo{}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "prune")
	if !isUsageError(err) {
		t.Fatalf("prune without -days must be usage error, got %v", err)
	}
}

func TestPruneZeroDaysIsUsageError(t *testing.T) {
	fd := &fakeDo{}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "prune", "-days", "0")
	if !isUsageError(err) {
		t.Fatalf("prune -days 0 must be usage error, got %v", err)
	}
}

func TestSeriesRequestAndRender(t *testing.T) {
	fd := &fakeDo{respBody: `[{"id":1,"name":"Show A","flow_id":3,"enabled":true,"paused":false,"tag":"x265"}]`}
	out, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "series")
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if fd.method != "GET" || fd.path != "/api/series" {
		t.Fatalf("expected GET /api/series, got %s %s", fd.method, fd.path)
	}
	if !strings.Contains(out, "Show A") || !strings.Contains(out, "x265") {
		t.Fatalf("series output missing data: %q", out)
	}
}

func TestStatsRequest(t *testing.T) {
	fd := &fakeDo{respBody: `{"per_node":[],"per_flow":[]}`}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "stats")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if fd.method != "GET" || fd.path != "/api/stats" {
		t.Fatalf("expected GET /api/stats, got %s %s", fd.method, fd.path)
	}
}

func TestManifestRequest(t *testing.T) {
	fd := &fakeDo{respBody: `{"agent_version":"1.0","linux_agent_version":"1.1"}`}
	out, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "manifest")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if fd.method != "GET" || fd.path != "/api/updates/manifest" {
		t.Fatalf("expected GET /api/updates/manifest, got %s %s", fd.method, fd.path)
	}
	if !strings.Contains(out, "agent_version") {
		t.Fatalf("manifest output missing version: %q", out)
	}
}

func TestRollbackAgentDefaultNoPlatform(t *testing.T) {
	fd := &fakeDo{respBody: `{}`}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "rollback-agent")
	if err != nil {
		t.Fatalf("rollback-agent: %v", err)
	}
	if fd.method != "POST" || fd.path != "/api/updates/agent/rollback" {
		t.Fatalf("expected POST /api/updates/agent/rollback, got %s %s", fd.method, fd.path)
	}
}

func TestRollbackAgentWithPlatform(t *testing.T) {
	fd := &fakeDo{respBody: `{}`}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "rollback-agent", "-platform", "linux")
	if err != nil {
		t.Fatalf("rollback-agent: %v", err)
	}
	if fd.path != "/api/updates/agent/rollback?platform=linux" {
		t.Fatalf("expected platform query, got %s", fd.path)
	}
}

func TestRollbackAgentBadPlatform(t *testing.T) {
	fd := &fakeDo{}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "rollback-agent", "-platform", "mac")
	if !isUsageError(err) {
		t.Fatalf("bad platform must be usage error, got %v", err)
	}
}

func TestPublishAgentMultipartEncoding(t *testing.T) {
	// Build a temp file to upload — its bytes are asserted below.
	dir := t.TempDir()
	payload := []byte("FAKE-AGENT-BINARY-CONTENT")
	binPath := filepath.Join(dir, "encode-agent-linux-amd64")
	if err := os.WriteFile(binPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	fd := &fakeDo{respBody: `{"agent_version":"2.0"}`}
	out, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token",
		"publish-agent", "-platform", "linux", "-version", "2.0", "-file", binPath)
	if err != nil {
		t.Fatalf("publish-agent: %v", err)
	}

	// Method/path.
	if fd.method != "POST" || fd.path != "/api/updates/agent" {
		t.Fatalf("expected POST /api/updates/agent, got %s %s", fd.method, fd.path)
	}
	// Content-Type must be multipart/form-data with a boundary.
	if !strings.HasPrefix(fd.contentType, "multipart/form-data; boundary=") {
		t.Fatalf("expected multipart content-type, got %q", fd.contentType)
	}

	// Parse the multipart body and assert fields + file bytes.
	mr := multipart.NewReader(bytes.NewReader(fd.bodyBytes), boundaryFromCT(fd.contentType))
	form, err := mr.ReadForm(1 << 20)
	if err != nil {
		t.Fatalf("parse multipart: %v", err)
	}
	defer form.RemoveAll()

	if form.Value["version"][0] != "2.0" {
		t.Fatalf("version field = %q", form.Value["version"])
	}
	if form.Value["platform"][0] != "linux" {
		t.Fatalf("platform field = %q", form.Value["platform"])
	}
	fh := form.File["file"]
	if len(fh) != 1 {
		t.Fatalf("expected 1 file field, got %d", len(fh))
	}
	if fh[0].Filename != "encode-agent-linux-amd64" {
		t.Fatalf("filename = %q (want base name)", fh[0].Filename)
	}
	rc, err := fh[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("file bytes mismatch: got %q want %q", got, payload)
	}
	if !strings.Contains(out, "agent_version") {
		t.Fatalf("expected manifest render, got %q", out)
	}
}

func TestPublishAgentMissingPlatform(t *testing.T) {
	fd := &fakeDo{}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token",
		"publish-agent", "-version", "2.0", "-file", "/tmp/x")
	if !isUsageError(err) {
		t.Fatalf("missing platform must be usage error, got %v", err)
	}
}

func TestPublishAgentBadPlatform(t *testing.T) {
	fd := &fakeDo{}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token",
		"publish-agent", "-platform", "mac", "-version", "2.0", "-file", "/tmp/x")
	if !isUsageError(err) {
		t.Fatalf("bad platform must be usage error, got %v", err)
	}
}

func TestPublishAgentMissingVersion(t *testing.T) {
	fd := &fakeDo{}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token",
		"publish-agent", "-platform", "linux", "-file", "/tmp/x")
	if !isUsageError(err) {
		t.Fatalf("missing version must be usage error, got %v", err)
	}
}

func TestPublishAgentMissingFile(t *testing.T) {
	fd := &fakeDo{}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token",
		"publish-agent", "-platform", "linux", "-version", "2.0")
	if !isUsageError(err) {
		t.Fatalf("missing file must be usage error, got %v", err)
	}
}

func TestPublishAgentUnreadableFile(t *testing.T) {
	fd := &fakeDo{}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token",
		"publish-agent", "-platform", "linux", "-version", "2.0", "-file", "/no/such/path/here")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if isUsageError(err) {
		// A missing file is a runtime error (exit 1), not a usage error.
		t.Fatalf("missing file should be runtime error, got usage error: %v", err)
	}
	if fd.callCount != 0 {
		t.Fatalf("should not have hit API for unreadable file, got %d calls", fd.callCount)
	}
}

func TestJSONPassthrough(t *testing.T) {
	body := `{"status":"ok","version":"1.2.3","nested":{"a":1}}`
	fd := &fakeDo{respBody: body}
	out, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "-json", "health")
	if err != nil {
		t.Fatalf("json passthrough: %v", err)
	}
	// -json must emit the raw body verbatim, no table rendering.
	if strings.TrimSpace(out) != strings.TrimSpace(body) {
		t.Fatalf("json passthrough mismatch:\nwant %q\ngot  %q", body, out)
	}
}

func TestJSONPassthroughPreservesNonJSON(t *testing.T) {
	// A non-JSON 2xx body (rare but possible) should pass through raw.
	fd := &fakeDo{respBody: "plain text response", contentType: "text/plain"}
	out, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "-json", "stats")
	if err != nil {
		t.Fatalf("json passthrough: %v", err)
	}
	if !strings.Contains(out, "plain text response") {
		t.Fatalf("expected raw text passthrough, got %q", out)
	}
}

func TestErrorSurfacingFrom400(t *testing.T) {
	// The spec: fake do returns 400 {"error":"bad"} → stderr message + exit-code-bearing error.
	fd := &fakeDo{status: 400, respBody: `{"error":"bad request body"}`}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "nodes")
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	if isUsageError(err) {
		t.Fatalf("API error must not be usage error, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "400") {
		t.Fatalf("error should include status 400: %q", msg)
	}
	if !strings.Contains(msg, "bad request body") {
		t.Fatalf("error should surface server's error message: %q", msg)
	}
}

func TestErrorSurfacingFrom500(t *testing.T) {
	fd := &fakeDo{status: 500, respBody: `{"error":"internal boom"}`}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "health")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "internal boom") {
		t.Fatalf("error should surface server message: %q", err)
	}
}

func TestErrorSurfacingFromNonJSONBody(t *testing.T) {
	// A non-2xx response with no {"error":...} field should still surface the status.
	fd := &fakeDo{status: 404, respBody: "Not Found"}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "health")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error should include status: %q", err)
	}
}

func TestTransportError(t *testing.T) {
	// A transport-level failure (DNS, refused connection) surfaces as a runtime error.
	fd := &fakeDo{respErr: fmt.Errorf("connection refused")}
	_, err := runCLI(t, fd, "-server", "http://x", "-token", "test-token", "health")
	if err == nil {
		t.Fatal("expected error for transport failure")
	}
	if isUsageError(err) {
		t.Fatalf("transport error must not be usage error: %v", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error should wrap transport message: %q", err)
	}
}

func TestVersionCommand(t *testing.T) {
	old := Version
	Version = "9.9.9"
	defer func() { Version = old }()
	fd := &fakeDo{}
	out, err := runCLI(t, fd, "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if strings.TrimSpace(out) != "encodectl 9.9.9" {
		t.Fatalf("version output: %q", out)
	}
	if fd.callCount != 0 {
		t.Fatalf("version must not hit API")
	}
}

func TestVersionCommandDoesNotRequireServer(t *testing.T) {
	// `version` is a local-only command: it must work without -server/-token.
	fd := &fakeDo{}
	out, err := runCLI(t, fd, "version")
	if err != nil {
		t.Fatalf("version without server/token: %v", err)
	}
	if !strings.Contains(out, "encodectl") {
		t.Fatalf("version output: %q", out)
	}
}

func TestHelpCommand(t *testing.T) {
	fd := &fakeDo{}
	out, err := runCLI(t, fd, "help")
	if err != nil {
		t.Fatalf("help: %v", err)
	}
	for _, want := range []string{"health", "jobs", "publish-agent", "prune", "Commands:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help missing %q: %q", want, out)
		}
	}
	if fd.callCount != 0 {
		t.Fatalf("help must not hit API")
	}
}

// boundaryFromCT extracts the multipart boundary from a Content-Type header
// value like "multipart/form-data; boundary=abc123".
func boundaryFromCT(ct string) string {
	parts := strings.Split(ct, "boundary=")
	if len(parts) != 2 {
		panic("no boundary in content-type: " + ct)
	}
	return parts[1]
}
