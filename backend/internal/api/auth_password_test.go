package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/auth"
)

// doChangePassword posts a password-change request with a session token.
func doChangePassword(t *testing.T, url, token, current, new string) (*http.Response, []byte) {
	t.Helper()
	return doJSON(t, "POST", url+"/api/auth/password", token,
		map[string]string{"current_password": current, "new_password": new})
}

func TestChangePasswordHappyPath(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doLogin(t, ts.URL, "admin", adminPassword)
	if resp.StatusCode != 200 {
		t.Fatalf("login failed: %d %s", resp.StatusCode, body)
	}
	var lr loginResp
	mustUnmarshal(t, body, &lr)

	newPass := "rotated-secret-123"
	resp, body = doChangePassword(t, ts.URL, lr.Token, adminPassword, newPass)
	if resp.StatusCode != 200 {
		t.Fatalf("change failed: %d %s", resp.StatusCode, body)
	}

	// Old password no longer works.
	resp, _ = doLogin(t, ts.URL, "admin", adminPassword)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password still accepted: %d", resp.StatusCode)
	}

	// New password works.
	resp, body = doLogin(t, ts.URL, "admin", newPass)
	if resp.StatusCode != 200 {
		t.Fatalf("new password rejected: %d %s", resp.StatusCode, body)
	}
}

func TestChangePasswordRequiresCorrectCurrent(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doLogin(t, ts.URL, "admin", adminPassword)
	if resp.StatusCode != 200 {
		t.Fatalf("login failed: %d", resp.StatusCode)
	}
	var lr loginResp
	mustUnmarshal(t, body, &lr)

	resp, _ = doChangePassword(t, ts.URL, lr.Token, "wrong-current", "new-secret-12345")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 for wrong current password, got %d", resp.StatusCode)
	}

	// Original password must still work (change was rejected).
	resp, _ = doLogin(t, ts.URL, "admin", adminPassword)
	if resp.StatusCode != 200 {
		t.Fatalf("original password broken by rejected change: %d", resp.StatusCode)
	}
}

func TestChangePasswordPolicy(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doLogin(t, ts.URL, "admin", adminPassword)
	var lr loginResp
	mustUnmarshal(t, body, &lr)

	cases := []struct {
		name string
		new  string
	}{
		{"too short", "short"},
		{"same as current", adminPassword},
		{"empty", ""},
	}
	for _, tc := range cases {
		resp, _ = doChangePassword(t, ts.URL, lr.Token, adminPassword, tc.new)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d", tc.name, resp.StatusCode)
		}
	}
}

func TestChangePasswordRequiresSession(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, _ := doJSON(t, "POST", ts.URL+"/api/auth/password", "",
		map[string]string{"current_password": adminPassword, "new_password": "new-secret-12345"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 without session, got %d", resp.StatusCode)
	}
}

func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// Two concurrent sessions for the same user.
	resp, body := doLogin(t, ts.URL, "admin", adminPassword)
	if resp.StatusCode != 200 {
		t.Fatalf("login 1 failed: %d", resp.StatusCode)
	}
	var s1 loginResp
	mustUnmarshal(t, body, &s1)

	resp, body = doLogin(t, ts.URL, "admin", adminPassword)
	if resp.StatusCode != 200 {
		t.Fatalf("login 2 failed: %d", resp.StatusCode)
	}
	var s2 loginResp
	mustUnmarshal(t, body, &s2)

	// Change password via session 1.
	resp, body = doChangePassword(t, ts.URL, s1.Token, adminPassword, "rotated-secret-456")
	if resp.StatusCode != 200 {
		t.Fatalf("change failed: %d %s", resp.StatusCode, body)
	}

	// Session 1 (the one that changed) still works.
	got, _ := doJSON(t, "GET", ts.URL+"/api/nodes", s1.Token, nil)
	if got.StatusCode != 200 {
		t.Fatalf("performing session revoked: %d", got.StatusCode)
	}

	// Session 2 must be dead.
	got, _ = doJSON(t, "GET", ts.URL+"/api/nodes", s2.Token, nil)
	if got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stale session survived password change: %d", got.StatusCode)
	}
}

func TestChangePasswordHonorsThrottleLockout(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doLogin(t, ts.URL, "admin", adminPassword)
	var lr loginResp
	mustUnmarshal(t, body, &lr)

	// Drive the endpoint itself into lockout with wrong-current attempts.
	for i := 0; i < maxFailedLogins; i++ {
		resp, _ = doChangePassword(t, ts.URL, lr.Token, "wrong", "new-secret-12345")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401, got %d", i, resp.StatusCode)
		}
	}
	// Next attempt must be 429 even with the CORRECT current password.
	resp, _ = doChangePassword(t, ts.URL, lr.Token, adminPassword, "new-secret-12345")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("want 429 during lockout, got %d", resp.StatusCode)
	}
}

func TestChangePasswordRejectsOverlong(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doLogin(t, ts.URL, "admin", adminPassword)
	var lr loginResp
	mustUnmarshal(t, body, &lr)

	long := strings.Repeat("a", 73) // bcrypt truncates at 72 bytes
	resp, _ = doChangePassword(t, ts.URL, lr.Token, adminPassword, long)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for >72-byte password, got %d", resp.StatusCode)
	}
}

func TestChangePasswordWrongCurrentCountsAgainstThrottle(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doLogin(t, ts.URL, "admin", adminPassword)
	var lr loginResp
	mustUnmarshal(t, body, &lr)

	// maxFailedLogins wrong-current attempts...
	for i := 0; i < maxFailedLogins; i++ {
		resp, _ = doChangePassword(t, ts.URL, lr.Token, "wrong", "new-secret-12345")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401, got %d", i, resp.StatusCode)
		}
	}
	// ...lock out even a correct login afterwards.
	resp, _ = doLogin(t, ts.URL, "admin", adminPassword)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("want 429 after failed change attempts, got %d", resp.StatusCode)
	}
}

// mintSecondSession creates another valid session token directly in the store
// (independent of the login throttle) for revocation tests.
func mintSecondSession(t *testing.T, e *testEnv) string {
	t.Helper()
	u, err := e.server.Store.UserByUsername(ctxBg(), "admin")
	if err != nil || u == nil {
		t.Fatal("admin not found")
	}
	tok, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.CreateSession(ctxBg(), auth.HashToken(tok), u.ID, time.Now().UTC().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestChangePasswordRevokesStoreMintedSessions(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doLogin(t, ts.URL, "admin", adminPassword)
	var lr loginResp
	mustUnmarshal(t, body, &lr)

	stale := mintSecondSession(t, e)

	resp, body = doChangePassword(t, ts.URL, lr.Token, adminPassword, "rotated-secret-789")
	if resp.StatusCode != 200 {
		t.Fatalf("change failed: %d %s", resp.StatusCode, body)
	}

	got, _ := doJSON(t, "GET", ts.URL+"/api/nodes", stale, nil)
	if got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("store-minted session survived: %d", got.StatusCode)
	}
}

func mustUnmarshal(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
}
