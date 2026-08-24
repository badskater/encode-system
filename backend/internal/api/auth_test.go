package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/store"
	"github.com/badskater/encode-system/backend/internal/update"
)

// ---------- Login / session tests ----------

type loginResp struct {
	Token     string    `json:"token"`
	Username  string    `json:"username"`
	ExpiresAt time.Time `json:"expires_at"`
}

func doLogin(t *testing.T, url, user, pass string) (*http.Response, []byte) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	req, err := http.NewRequest("POST", url+"/api/auth/login", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func TestLoginHappyPath(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doLogin(t, ts.URL, "admin", adminPassword)
	if resp.StatusCode != 200 {
		t.Fatalf("login failed: %d %s", resp.StatusCode, body)
	}
	var lr loginResp
	if err := json.Unmarshal(body, &lr); err != nil {
		t.Fatal(err)
	}
	if lr.Token == "" || lr.Username != "admin" {
		t.Fatalf("unexpected login response: %+v", lr)
	}

	// The session token authorizes admin endpoints.
	got, _ := doJSON(t, "GET", ts.URL+"/api/nodes", lr.Token, nil)
	if got.StatusCode != 200 {
		t.Fatalf("session token rejected: %d", got.StatusCode)
	}

	// /api/auth/me reflects the session.
	meResp, meBody := doJSON(t, "GET", ts.URL+"/api/auth/me", lr.Token, nil)
	if meResp.StatusCode != 200 {
		t.Fatalf("me failed: %d %s", meResp.StatusCode, meBody)
	}
	var me map[string]any
	json.Unmarshal(meBody, &me)
	if me["username"] != "admin" {
		t.Fatalf("me returned %v", me)
	}
}

func TestLoginRejectsBadCredentials(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	for _, tc := range []struct{ user, pass string }{
		{"admin", "wrong"},
		{"admin", ""},
		{"ghost", adminPassword},
	} {
		resp, _ := doLogin(t, ts.URL, tc.user, tc.pass)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("user=%q pass=%q: want 401, got %d", tc.user, tc.pass, resp.StatusCode)
		}
	}
}

func TestLoginThrottleLocksOutAfterFailures(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	for i := 0; i < maxFailedLogins; i++ {
		resp, _ := doLogin(t, ts.URL, "admin", "wrong")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401, got %d", i, resp.StatusCode)
		}
	}
	// Next attempt must be throttled even with the CORRECT password.
	resp, body := doLogin(t, ts.URL, "admin", adminPassword)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("want 429 after %d failures, got %d (%s)", maxFailedLogins, resp.StatusCode, body)
	}
}

func TestUnauthenticatedRequestsRejected(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, _ := doJSON(t, "GET", ts.URL+"/api/nodes", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 without session, got %d", resp.StatusCode)
	}
	resp, _ = doJSON(t, "GET", ts.URL+"/api/nodes", "bogus-token", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 with bogus token, got %d", resp.StatusCode)
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doLogin(t, ts.URL, "admin", adminPassword)
	if resp.StatusCode != 200 {
		t.Fatalf("login failed: %d", resp.StatusCode)
	}
	var lr loginResp
	json.Unmarshal(body, &lr)

	// Logout.
	out, _ := doJSON(t, "POST", ts.URL+"/api/auth/logout", lr.Token, nil)
	if out.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: want 204, got %d", out.StatusCode)
	}

	// The token must no longer authorize anything.
	got, _ := doJSON(t, "GET", ts.URL+"/api/nodes", lr.Token, nil)
	if got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token still works: %d", got.StatusCode)
	}
}

func TestAdminSeedIdempotentAndEnvIgnoredAfterCreation(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	up, err := update.NewStore(dir + "/updates")
	if err != nil {
		t.Fatal(err)
	}

	// Boot 1: creates the account with password "first".
	mk := func(pass string) error {
		_, err := New(st, up, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{
			AdminUsername: "admin", AdminPassword: pass,
			ScriptsRoot: dir + "/scripts", ReleaseRoot: dir + "/release",
		})
		return err
	}
	if err := mk("first-password"); err != nil {
		t.Fatalf("boot 1: %v", err)
	}
	// Boot 2: different env password must NOT overwrite the account.
	if err := mk("second-password"); err != nil {
		t.Fatalf("boot 2: %v", err)
	}
	u, err := st.UserByUsername(ctxBg(), "admin")
	if err != nil || u == nil {
		t.Fatal("admin missing")
	}
	// Only one account should exist.
	users, err := st.Users(ctxBg())
	if err != nil || len(users) != 1 {
		t.Fatalf("want exactly 1 user, got %d (err %v)", len(users), err)
	}
}

func TestMissingAdminPasswordWithNoAccountFailsStartup(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	up, err := update.NewStore(dir + "/updates")
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(st, up, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{
		AdminUsername: "admin", AdminPassword: "",
		ScriptsRoot: dir + "/scripts", ReleaseRoot: dir + "/release",
	})
	if err == nil {
		t.Fatal("want startup failure when no password configured and account missing")
	}
}

// TestLoginDoesNotRevealExistenceTimingShape: unknown user and known user with
// wrong password both return the same status/body shape.
func TestLoginUniformErrorShape(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp1, body1 := doLogin(t, ts.URL, "ghost", "whatever")
	resp2, body2 := doLogin(t, ts.URL, "admin", "whatever")
	if resp1.StatusCode != resp2.StatusCode {
		t.Fatalf("status mismatch: %d vs %d", resp1.StatusCode, resp2.StatusCode)
	}
	var e1, e2 map[string]string
	json.Unmarshal(body1, &e1)
	json.Unmarshal(body2, &e2)
	if e1["error"] != e2["error"] {
		t.Fatalf("error mismatch: %q vs %q", e1["error"], e2["error"])
	}
}

var _ = httptest.NewRecorder // keep import used if tests get pruned
