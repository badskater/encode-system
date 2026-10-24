package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------- Publish bin package from URL ----------

// TestPublishBinFromURLHappyPath: the controller fetches a zip from a URL
// (GitHub-release style), validates it, and publishes it — manifest updates.
func TestPublishBinFromURLHappyPath(t *testing.T) {
	// httptest hosts listen on 127.0.0.1; the SSRF dialer blocks loopback
	// unless the operator opts in (homelab LAN package servers).
	t.Setenv("ENCODE_BIN_FETCH_PRIVATE", "1")
	e := newTestEnv(t)
	ts := e.serve(t)

	// Fake package host: serves a small valid zip.
	z := mkZip(t, map[string][]byte{"tool.exe": []byte("fake-binary")})
	sum := sha256.Sum256(z)
	asset := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(z)
	}))
	defer asset.Close()

	resp, body := doJSON(t, "POST", ts.URL+"/api/updates/bin/url", adminTok, map[string]any{
		"url":     asset.URL + "/bin-package.zip",
		"version": 1,
		"sha256":  hex.EncodeToString(sum[:]),
	})
	if resp.StatusCode != 200 {
		t.Fatalf("publish from url: %d %s", resp.StatusCode, body)
	}
	var m struct {
		BinVersion int64  `json:"bin_version"`
		BinSHA256  string `json:"bin_sha256"`
		BinSize    int64  `json:"bin_size"`
	}
	json.Unmarshal(body, &m)
	if m.BinVersion != 1 || m.BinSHA256 != hex.EncodeToString(sum[:]) || m.BinSize != int64(len(z)) {
		t.Fatalf("manifest wrong: %+v", m)
	}
}

// TestPublishBinFromURLValidation: scheme guard, sha mismatch, HTTP errors,
// bad zip, and the version counter all behave like the upload path.
func TestPublishBinFromURLValidation(t *testing.T) {
	t.Setenv("ENCODE_BIN_FETCH_PRIVATE", "1") // httptest hosts are loopback
	e := newTestEnv(t)
	ts := e.serve(t)

	z := mkZip(t, map[string][]byte{"tool.exe": []byte("fake")})
	goodSum := sha256.Sum256(z)
	asset := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(z)
	}))
	defer asset.Close()
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer notFound.Close()

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"file scheme refused", map[string]any{"url": "file:///etc/passwd", "version": 1}, 400},
		{"gopher scheme refused", map[string]any{"url": "gopher://x/", "version": 1}, 400},
		{"not a url", map[string]any{"url": "::bad::", "version": 1}, 400},
		{"version zero", map[string]any{"url": asset.URL, "version": 0}, 400},
		{"sha mismatch", map[string]any{"url": asset.URL, "version": 1, "sha256": "deadbeef"}, 400},
		{"upstream 404", map[string]any{"url": notFound.URL, "version": 1}, 502},
	}
	for _, c := range cases {
		resp, body := doJSON(t, "POST", ts.URL+"/api/updates/bin/url", adminTok, c.body)
		if resp.StatusCode != c.want {
			t.Errorf("%s: want %d, got %d %s", c.name, c.want, resp.StatusCode, body)
		}
	}
	// Nothing must have been published by the failed attempts.
	if m := e.server.Update.Manifest(); m.BinVersion != 0 {
		t.Fatalf("failed attempts must not publish: %+v", m)
	}

	// Valid publish, then a LOWER version is refused (409).
	resp, body := doJSON(t, "POST", ts.URL+"/api/updates/bin/url", adminTok, map[string]any{
		"url": asset.URL, "version": 5, "sha256": hex.EncodeToString(goodSum[:]),
	})
	if resp.StatusCode != 200 {
		t.Fatalf("first publish: %d %s", resp.StatusCode, body)
	}
	resp, _ = doJSON(t, "POST", ts.URL+"/api/updates/bin/url", adminTok, map[string]any{
		"url": asset.URL, "version": 4,
	})
	if resp.StatusCode != 409 {
		t.Errorf("lower version must be 409, got %d", resp.StatusCode)
	}

	// A corrupt zip from a URL is rejected with 400 (validation parity with
	// the upload path).
	corrupt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("this is not a zip"))
	}))
	defer corrupt.Close()
	resp, body = doJSON(t, "POST", ts.URL+"/api/updates/bin/url", adminTok, map[string]any{
		"url": corrupt.URL, "version": 6,
	})
	if resp.StatusCode != 400 {
		t.Errorf("corrupt zip must be 400, got %d %s", resp.StatusCode, body)
	}
}

// Regression (security audit 2026-10): the URL fetch must refuse
// loopback/private/link-local targets by DEFAULT (SSRF), and honor the
// ENCODE_BIN_FETCH_PRIVATE opt-in for LAN package hosts. The dial-time
// check also covers redirects and DNS rebinding (verified by
// checkBinFetchIP unit cases below).
func TestPublishBinFromURLSSRFBlockedByDefault(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// Loopback target (httptest host) must be refused with no env opt-in.
	asset := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("should never be reached"))
	}))
	defer asset.Close()

	resp, body := doJSON(t, "POST", ts.URL+"/api/updates/bin/url", adminTok, map[string]any{
		"url":     asset.URL + "/bin-package.zip",
		"version": 1,
	})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502 for blocked loopback fetch, got %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "fetch failed") {
		t.Fatalf("want generic fetch error, got %s", body)
	}
	// The generic message must not leak dial detail (SSRF recon channel).
	for _, leak := range []string{"connection refused", "127.0.0.1", "dial tcp", "refusing"} {
		if strings.Contains(strings.ToLower(string(body)), leak) {
			t.Fatalf("error body leaks internal detail %q: %s", leak, body)
		}
	}
}

// checkBinFetchIP classification: what is always refused, what needs the
// opt-in, and what passes.
func TestCheckBinFetchIPClassification(t *testing.T) {
	cases := []struct {
		ip       string
		allow    bool // permitted with ENCODE_BIN_FETCH_PRIVATE=1
		defaultOK bool // permitted with no env
	}{
		{"127.0.0.1", true, false},   // loopback: opt-in only
		{"::1", true, false},         // IPv6 loopback: opt-in only
		{"10.0.0.5", true, false},    // RFC1918: opt-in only
		{"192.168.1.1", true, false}, // RFC1918: opt-in only
		{"172.16.0.1", true, false},  // RFC1918: opt-in only
		{"169.254.169.254", false, false}, // cloud metadata: ALWAYS refused
		{"169.254.1.1", false, false},     // link-local: ALWAYS refused
		{"224.0.0.1", false, false},       // multicast: ALWAYS refused
		{"0.0.0.0", false, false},         // unspecified: ALWAYS refused
		{"8.8.8.8", true, true},           // public: always fine
		{"93.184.216.34", true, true},     // public: always fine
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad test ip %s", c.ip)
		}
		// Default (no env): expect refusal unless defaultOK.
		t.Setenv("ENCODE_BIN_FETCH_PRIVATE", "")
		err := checkBinFetchIP(ip)
		if (err == nil) != c.defaultOK {
			t.Errorf("%s default: err=%v wantAllowed=%v", c.ip, err, c.defaultOK)
		}
		// Opt-in: expect refusal unless allow.
		t.Setenv("ENCODE_BIN_FETCH_PRIVATE", "1")
		err = checkBinFetchIP(ip)
		if (err == nil) != c.allow {
			t.Errorf("%s opted-in: err=%v wantAllowed=%v", c.ip, err, c.allow)
		}
	}
}
