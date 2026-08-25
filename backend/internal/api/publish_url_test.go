package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ---------- Publish bin package from URL ----------

// TestPublishBinFromURLHappyPath: the controller fetches a zip from a URL
// (GitHub-release style), validates it, and publishes it — manifest updates.
func TestPublishBinFromURLHappyPath(t *testing.T) {
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
