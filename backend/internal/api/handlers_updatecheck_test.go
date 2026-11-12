package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/auth"
	"github.com/badskater/encode-system/backend/internal/store"
	"github.com/badskater/encode-system/backend/internal/update"
)

// newUpdateCheckEnv builds a Server with GitHub config pointed at a stub
// GitHub API server. Returns the server and the stub for customization.
func newUpdateCheckEnv(t *testing.T, stubHandler http.HandlerFunc) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	up, err := update.NewStore(dir + "/updates")
	if err != nil {
		t.Fatal(err)
	}

	// Stub GitHub API.
	stub := httptest.NewServer(stubHandler)
	t.Cleanup(stub.Close)

	cfg := Config{
		AdminUsername: "admin", AdminPassword: adminPassword,
		ScriptsRoot: dir + "/scripts", ReleaseRoot: dir + "/release",
		NodeBinDir: `C:\bin`, NodeScriptsDir: `C:\Encodes\scripts`, NodeReleaseDir: `C:\Encodes\ReleaseFolders`,
		Group: "OldFartsSubs", Tag: "1080p", TasksBeforeReboot: 10,
		GitHubAPI:  stub.URL,
		GitHubRepo: "badskater/encode-system",
		Version:    "1.0.0",
	}
	s, err := New(st, up, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Mint admin session.
	u, err := st.UserByUsername(ctxBg(), "admin")
	if err != nil || u == nil {
		t.Fatal("admin not seeded")
	}
	sessTok, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctxBg(), auth.HashToken(sessTok), u.ID, time.Now().UTC().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	adminTok = sessTok

	return s, stub
}

// githubReleaseJSON builds a GitHub releases/latest response body.
func githubReleaseJSON(tag, htmlURL string) string {
	return `{"tag_name":"` + tag + `","html_url":"` + htmlURL + `"}`
}

func TestUpdateCheckNewerVersionAvailable(t *testing.T) {
	s, _ := newUpdateCheckEnv(t, func(w http.ResponseWriter, r *http.Request) {
		// Verify the request looks right.
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("Accept header: want application/vnd.github+json, got %q", r.Header.Get("Accept"))
		}
		if r.Header.Get("User-Agent") != "encode-system" {
			t.Errorf("User-Agent: want encode-system, got %q", r.Header.Get("User-Agent"))
		}
		// No token set → no Authorization header.
		if r.Header.Get("Authorization") != "" {
			t.Errorf("Authorization should be empty, got %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(githubReleaseJSON("v1.5.0", "https://github.com/badskater/encode-system/releases/tag/v1.5.0")))
	})
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)

	r, b := doJSON(t, "GET", ts.URL+"/api/updates/check", adminTok, nil)
	if r.StatusCode != 200 {
		t.Fatalf("update check: %d %s", r.StatusCode, b)
	}
	var resp struct {
		Current         string `json:"current"`
		Latest          string `json:"latest"`
		UpdateAvailable bool   `json:"update_available"`
		ReleaseURL      string `json:"release_url"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Current != "1.0.0" {
		t.Errorf("current: want 1.0.0, got %q", resp.Current)
	}
	if resp.Latest != "v1.5.0" {
		t.Errorf("latest: want v1.5.0, got %q", resp.Latest)
	}
	if !resp.UpdateAvailable {
		t.Error("update_available: want true (1.0.0 < 1.5.0)")
	}
	if resp.ReleaseURL == "" {
		t.Error("release_url should not be empty")
	}
}

func TestUpdateCheckEqualVersion(t *testing.T) {
	s, _ := newUpdateCheckEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(githubReleaseJSON("v1.0.0", "https://example.com")))
	})
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)

	r, b := doJSON(t, "GET", ts.URL+"/api/updates/check", adminTok, nil)
	if r.StatusCode != 200 {
		t.Fatalf("update check: %d %s", r.StatusCode, b)
	}
	var resp struct {
		UpdateAvailable bool `json:"update_available"`
	}
	json.Unmarshal(b, &resp)
	if resp.UpdateAvailable {
		t.Error("update_available: want false (1.0.0 == 1.0.0)")
	}
}

func TestUpdateCheckOlderVersion(t *testing.T) {
	s, _ := newUpdateCheckEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(githubReleaseJSON("v0.9.0", "https://example.com")))
	})
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)

	r, b := doJSON(t, "GET", ts.URL+"/api/updates/check", adminTok, nil)
	if r.StatusCode != 200 {
		t.Fatalf("update check: %d %s", r.StatusCode, b)
	}
	var resp struct {
		UpdateAvailable bool `json:"update_available"`
	}
	json.Unmarshal(b, &resp)
	if resp.UpdateAvailable {
		t.Error("update_available: want false (1.0.0 > 0.9.0)")
	}
}

func TestUpdateCheckGitHub404Returns502(t *testing.T) {
	s, _ := newUpdateCheckEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	})
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)

	r, b := doJSON(t, "GET", ts.URL+"/api/updates/check", adminTok, nil)
	if r.StatusCode != 502 {
		t.Fatalf("update check 404: want 502, got %d %s", r.StatusCode, b)
	}
}

func TestUpdateCheckMalformedJSONReturns502(t *testing.T) {
	s, _ := newUpdateCheckEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{not valid json`))
	})
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)

	r, b := doJSON(t, "GET", ts.URL+"/api/updates/check", adminTok, nil)
	if r.StatusCode != 502 {
		t.Fatalf("malformed JSON: want 502, got %d %s", r.StatusCode, b)
	}
}

func TestUpdateCheckNoTokenStillWorks(t *testing.T) {
	// GitHubToken is not set in newUpdateCheckEnv; verify the request
	// succeeds without it (the stub doesn't check auth).
	s, _ := newUpdateCheckEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected Authorization header: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(githubReleaseJSON("v2.0.0", "https://example.com")))
	})
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)

	r, b := doJSON(t, "GET", ts.URL+"/api/updates/check", adminTok, nil)
	if r.StatusCode != 200 {
		t.Fatalf("no-token update check: %d %s", r.StatusCode, b)
	}
}

func TestUpdateCheckWithTokenSendsBearer(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	up, err := update.NewStore(dir + "/updates")
	if err != nil {
		t.Fatal(err)
	}

	var seenAuth string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(githubReleaseJSON("v2.0.0", "https://example.com")))
	}))
	t.Cleanup(stub.Close)

	cfg := Config{
		AdminUsername: "admin", AdminPassword: adminPassword,
		ScriptsRoot: dir + "/scripts", ReleaseRoot: dir + "/release",
		NodeBinDir: `C:\bin`, NodeScriptsDir: `C:\Encodes\scripts`, NodeReleaseDir: `C:\Encodes\ReleaseFolders`,
		Group: "OldFartsSubs", Tag: "1080p", TasksBeforeReboot: 10,
		GitHubAPI: stub.URL, GitHubRepo: "badskater/encode-system",
		GitHubToken: "ghp_testtoken123", Version: "1.0.0",
	}
	s, err := New(st, up, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := st.UserByUsername(ctxBg(), "admin")
	sessTok, _ := auth.NewToken()
	st.CreateSession(ctxBg(), auth.HashToken(sessTok), u.ID, time.Now().UTC().Add(24*time.Hour))
	adminTok = sessTok

	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)

	r, _ := doJSON(t, "GET", ts.URL+"/api/updates/check", adminTok, nil)
	if r.StatusCode != 200 {
		t.Fatalf("update check with token: %d", r.StatusCode)
	}
	if seenAuth != "Bearer ghp_testtoken123" {
		t.Errorf("Authorization: want 'Bearer ghp_testtoken123', got %q", seenAuth)
	}
}

// ---------- Version comparison unit tests ----------

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.0.1", "1.0.0", 1},
		{"1.9.0", "1.10.0", -1}, // numeric, not string
		{"2.0.0", "1.99.99", 1},
		{"v1.0.0", "v1.0.0", 0}, // v prefix stripped
		{"v1.2.3", "1.2.3", 0},
		{"1.0.0", "2.0.0", -1},
		{"", "", 0},
		{"", "1.0.0", -1},
		{"1.0.0", "", 1},
		{"1.0", "1.0.0", -1}, // shorter = less when prefix matches
		{"1.0.0", "1.0", 1},
	}
	for _, c := range cases {
		got := compareVersions(c.a, c.b)
		if got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// ---------- Health carries version ----------

func TestHealthCarriesVersion(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/test.db")
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
		ScriptsRoot: dir + "/scripts", ReleaseRoot: dir + "/release",
		NodeBinDir: `C:\bin`, NodeScriptsDir: `C:\Encodes\scripts`, NodeReleaseDir: `C:\Encodes\ReleaseFolders`,
		Group: "OldFartsSubs", Tag: "1080p", TasksBeforeReboot: 10,
		Version: "3.7.2",
	}
	s, err := New(st, up, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)

	r, b := doJSON(t, "GET", ts.URL+"/api/health", "", nil)
	if r.StatusCode != 200 {
		t.Fatalf("health: %d %s", r.StatusCode, b)
	}
	var h map[string]any
	json.Unmarshal(b, &h)
	if h["status"] != "ok" {
		t.Errorf("status: want ok, got %v", h["status"])
	}
	if h["version"] != "3.7.2" {
		t.Errorf("version: want 3.7.2, got %v", h["version"])
	}
}
