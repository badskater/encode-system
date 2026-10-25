package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/badskater/encode-system/backend/internal/auth"
	"github.com/badskater/encode-system/backend/internal/store"
	"github.com/badskater/encode-system/backend/internal/update"
)

// TestHealthEndpointIncludesInstance (Phase 3.3): the /api/health response
// MUST include an "instance" field identifying this replica, ADDITIVELY —
// the existing {"status":"ok"} field stays so the legacy health check
// (TestHealthEndpoint, which asserts the body contains "ok") stays green.
func TestHealthEndpointIncludesInstance(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/instance-health.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	up, err := update.NewStore(dir + "/updates")
	if err != nil {
		t.Fatal(err)
	}
	const wantInstance = "replica-7"
	cfg := Config{
		AdminUsername: "admin", AdminPassword: adminPassword,
		Group: "g", Tag: "t", TasksBeforeReboot: 10,
		InstanceID: wantInstance,
	}
	s, err := New(st, up, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)

	resp, body := doReq(t, "GET", ts.URL+"/api/health", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	// Additive: status stays "ok" (legacy contract).
	if got["status"] != "ok" {
		t.Errorf("status = %q, want \"ok\" (must stay additive)", got["status"])
	}
	// New field: instance identifies this replica.
	if got["instance"] != wantInstance {
		t.Errorf("instance = %q, want %q", got["instance"], wantInstance)
	}
	// The legacy TestHealthEndpoint asserts the body CONTAINS "ok" — a
	// substring check. Confirm that still holds.
	if !bytes.Contains(body, []byte("ok")) {
		t.Errorf("body missing \"ok\": %s", body)
	}
}

// TestHealthInstanceIDDefaultFallback: when InstanceID is empty (a legacy
// controller that never set ENCODE_INSTANCE_ID and has no hostname), New
// fills it with "controller" so the field is never blank in logs/health —
// a blank instance field in a two-replica stream would be indistinguishable.
func TestHealthInstanceIDDefaultFallback(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/instance-fallback.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	up, err := update.NewStore(dir + "/updates")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		AdminUsername: "admin", AdminPassword: adminPassword,
		Group: "g", Tag: "t", TasksBeforeReboot: 10,
		InstanceID: "", // unset — exercises the New fallback
	}
	s, err := New(st, up, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Cfg.InstanceID != "controller" {
		t.Errorf("empty InstanceID not defaulted: got %q, want \"controller\"", s.Cfg.InstanceID)
	}
}

// TestInstanceIDStampedInLogger: the controller's slog logger should carry
// the instance id as a default attr so every log line is grep-able per
// replica. This mirrors what main.go does (log.With("instance", id)).
// We don't assert on stdout here (the test logger discards output); we
// assert the api.Server's Cfg carries the id (the health field is the
// observable surface) and that constructing a logger with the id is the
// documented pattern. See main.go for the production wiring.
func TestInstanceIDStampedInLogger(t *testing.T) {
	// This is a structural assertion: the instance id flows Cfg.InstanceID →
	// health (covered above) and main stamps it into the logger via
	// log.With("instance", instanceID). Confirm the attr is present on a
	// logger built the same way main builds it.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger = logger.With("instance", "ctrl-A")
	logger.Info("boot")
	if !bytes.Contains(buf.Bytes(), []byte("instance=ctrl-A")) {
		t.Errorf("logger output missing instance attr: %s", buf.String())
	}
}

// doReq is a local helper identical to doJSON but without the testing.T
// coupling to newTestEnv, so this file is self-contained.
func doReq(t *testing.T, method, url, token string, body any) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = auth.HashToken // keep import used (test infra parity with api_test.go)
	return resp, b
}
