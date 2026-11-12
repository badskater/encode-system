package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/oidc"
	"github.com/badskater/encode-system/backend/internal/oidc/oidctest"
)

// ---------- OIDC integration tests (mock provider, real HTTP handlers) ----------

const testOIDCClientID = "encode-web"

// noRedirectClient stops at the first redirect so tests can inspect Location.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// newOIDCEnv builds a test env with OIDC enabled against the mock provider.
func newOIDCEnv(t *testing.T, mock *oidctest.Provider, adminGroup string) *testEnv {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{
		AdminUsername: "admin", AdminPassword: adminPassword,
		ScriptsRoot: dir + "/scripts", ReleaseRoot: dir + "/release",
		NodeBinDir: `C:\bin`, NodeScriptsDir: `C:\Encodes\scripts`, NodeReleaseDir: `C:\Encodes\ReleaseFolders`,
		Group: "OldFartsSubs", Tag: "1080p", TasksBeforeReboot: 10,
		OIDC: &oidc.Config{
			Issuer:       mock.Issuer,
			ClientID:     testOIDCClientID,
			ClientSecret: oidctest.ClientSecret(t),
			RedirectURL:  "https://ctl.example/api/auth/oidc/callback",
			AdminGroup:   adminGroup,
			GroupsClaim:  "groups",
		},
	}
	return newTestEnvWithConfig(t, dir, cfg)
}

// oidcStart drives GET /api/auth/oidc/login and returns the authorization
// redirect query (state, nonce, code_challenge, ...). The flow cookie set by
// the response is available via oidcStartCookie.
func oidcStart(t *testing.T, tsURL string) url.Values {
	t.Helper()
	q, _ := oidcStartCookie(t, tsURL)
	return q
}

// oidcCallback drives GET /api/auth/oidc/callback WITHOUT a flow cookie
// (negative tests); oidcCallbackCookie presents one.
func oidcCallback(t *testing.T, tsURL, code, state string) *http.Response {
	t.Helper()
	return oidcCallbackCookie(t, tsURL, code, state, nil)
}

// oidcFullLogin runs the whole flow (with the browser-binding flow cookie)
// and returns the issued session token (extracted from the SPA redirect
// fragment).
func oidcFullLogin(t *testing.T, tsURL string, mock *oidctest.Provider) string {
	t.Helper()
	q, c := oidcStartCookie(t, tsURL)
	mock.SetNonce(q.Get("nonce"))
	resp := oidcCallbackCookie(t, tsURL, "auth-code", q.Get("state"), c)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d, want 302", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/#oidc=") {
		t.Fatalf("callback redirect = %q, want /#oidc=<token>", loc)
	}
	tok := strings.TrimPrefix(loc, "/#oidc=")
	if len(tok) != 64 {
		t.Fatalf("session token shape wrong: %d chars", len(tok))
	}
	return tok
}

// meWith fetches /api/auth/me with the given token.
func meWith(t *testing.T, tsURL, tok string) (int, map[string]any) {
	t.Helper()
	resp, body := doJSON(t, "GET", tsURL+"/api/auth/me", tok, nil)
	var out map[string]any
	json.Unmarshal(body, &out)
	return resp.StatusCode, out
}

func TestOIDCDisabledNoRoutesAndFlagFalse(t *testing.T) {
	e := newTestEnv(t) // no OIDC config — the default install
	ts := e.serve(t)

	for _, path := range []string{"/api/auth/oidc/login", "/api/auth/oidc/callback?code=x&state=y"} {
		resp, _ := doJSON(t, "GET", ts.URL+path, "", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s with OIDC disabled = %d, want 404", path, resp.StatusCode)
		}
	}
	resp, body := doJSON(t, "GET", ts.URL+"/api/health", "", nil)
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	var health map[string]any
	json.Unmarshal(body, &health)
	if health["oidc_enabled"] != false {
		t.Fatalf("oidc_enabled = %v, want false", health["oidc_enabled"])
	}
	if health["status"] != "ok" {
		t.Fatalf("health status changed: %v", health)
	}
}

func TestOIDCEnabledHealthFlagTrue(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := e.serve(t)

	_, body := doJSON(t, "GET", ts.URL+"/api/health", "", nil)
	var health map[string]any
	json.Unmarshal(body, &health)
	if health["oidc_enabled"] != true {
		t.Fatalf("oidc_enabled = %v, want true", health["oidc_enabled"])
	}
}

func TestOIDCHappyPathProvisionsViewer(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := e.serve(t)

	tok := oidcFullLogin(t, ts.URL, mock)
	status, me := meWith(t, ts.URL, tok)
	if status != 200 {
		t.Fatalf("me status = %d", status)
	}
	if me["username"] != "alice" || me["role"] != model.RoleViewer {
		t.Fatalf("me = %v, want alice/viewer", me)
	}
	// Viewer session: reads pass, mutations are forbidden.
	if resp, _ := doJSON(t, "GET", ts.URL+"/api/nodes", tok, nil); resp.StatusCode != 200 {
		t.Fatalf("viewer read = %d, want 200", resp.StatusCode)
	}
	if resp, _ := doJSON(t, "POST", ts.URL+"/api/nodes", tok, map[string]string{"name": "nope"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer mutation = %d, want 403", resp.StatusCode)
	}
}

func TestOIDCAdminGroupMapping(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "encode-admins")
	ts := e.serve(t)

	mock.SetGroups([]string{"users", "encode-admins"})
	tok := oidcFullLogin(t, ts.URL, mock)
	status, me := meWith(t, ts.URL, tok)
	if status != 200 || me["role"] != model.RoleAdmin {
		t.Fatalf("admin-group login: status=%d me=%v", status, me)
	}
	// Admin session can mutate.
	if resp, _ := doJSON(t, "POST", ts.URL+"/api/nodes", tok, map[string]string{"name": "oidc-node"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("admin mutation = %d, want 201", resp.StatusCode)
	}
}

func TestOIDCProvisionIdempotent(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := e.serve(t)

	tok1 := oidcFullLogin(t, ts.URL, mock)
	tok2 := oidcFullLogin(t, ts.URL, mock)
	if tok1 == tok2 {
		t.Fatal("each login must issue a fresh session token")
	}
	// Both sessions work and belong to the SAME single account.
	_, me1 := meWith(t, ts.URL, tok1)
	_, me2 := meWith(t, ts.URL, tok2)
	if me1["username"] != "alice" || me2["username"] != "alice" {
		t.Fatalf("usernames diverged: %v vs %v", me1, me2)
	}
	resp, body := doJSON(t, "GET", ts.URL+"/api/users", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("list users = %d", resp.StatusCode)
	}
	var users []model.User
	json.Unmarshal(body, &users)
	count, id := 0, int64(0)
	for _, u := range users {
		if u.Username == "alice" {
			count++
			id = u.ID
		}
	}
	if count != 1 {
		t.Fatalf("second login duplicated the account: %d alice rows", count)
	}
	if id == 0 {
		t.Fatal("alice not found")
	}
}

func TestOIDCRoleSyncBothDirections(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "encode-admins")
	ts := e.serve(t)

	// First login: in the admin group → admin.
	mock.SetGroups([]string{"encode-admins"})
	tok := oidcFullLogin(t, ts.URL, mock)
	if _, me := meWith(t, ts.URL, tok); me["role"] != model.RoleAdmin {
		t.Fatalf("first login role = %v, want admin", me["role"])
	}
	// Second login: group removed → demoted to viewer on the SAME account.
	mock.SetGroups([]string{"users"})
	tok = oidcFullLogin(t, ts.URL, mock)
	if _, me := meWith(t, ts.URL, tok); me["role"] != model.RoleViewer {
		t.Fatalf("demotion sync failed: %v", me["role"])
	}
	// Third login: group restored → promoted again.
	mock.SetGroups([]string{"encode-admins"})
	tok = oidcFullLogin(t, ts.URL, mock)
	if _, me := meWith(t, ts.URL, tok); me["role"] != model.RoleAdmin {
		t.Fatalf("promotion sync failed: %v", me["role"])
	}
}

func TestOIDCNoAdminGroupKeepsExistingRole(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "") // no group mapping configured
	ts := e.serve(t)

	// Pre-existing LOCAL admin account whose username matches the IdP's
	// preferred_username. Without a configured admin group its role must
	// survive the OIDC login untouched (no silent demotion / lockout).
	hash, err := bcrypt.GenerateFromPassword([]byte("local-"+adminPassword), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.server.Store.CreateUser(ctxBg(), "alice", string(hash), model.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	tok := oidcFullLogin(t, ts.URL, mock)
	if _, me := meWith(t, ts.URL, tok); me["role"] != model.RoleAdmin {
		t.Fatalf("existing role clobbered without group mapping: %v", me["role"])
	}
	// The local password still works alongside OIDC (coexistence).
	if resp, _ := doLogin(t, ts.URL, "alice", "local-"+adminPassword); resp.StatusCode != 200 {
		t.Fatalf("local login for hybrid account = %d, want 200", resp.StatusCode)
	}
}

func TestOIDCLocalLoginUnaffected(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "encode-admins")
	ts := e.serve(t)

	// Local admin login works exactly as before with OIDC enabled.
	resp, body := doLogin(t, ts.URL, "admin", adminPassword)
	if resp.StatusCode != 200 {
		t.Fatalf("local login with OIDC enabled = %d: %s", resp.StatusCode, body)
	}
	var lr loginResp
	json.Unmarshal(body, &lr)
	if lr.Username != "admin" || lr.Token == "" {
		t.Fatalf("local login response changed: %+v", lr)
	}

	// An OIDC-provisioned account has NO local password: any local login
	// attempt is a plain 401 (empty hash can never match bcrypt).
	oidcFullLogin(t, ts.URL, mock)
	if resp, _ := doLogin(t, ts.URL, "alice", "anything-at-all"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("local login for OIDC-only account = %d, want 401", resp.StatusCode)
	}
}

func TestOIDCLogoutRevokesSession(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := e.serve(t)

	tok := oidcFullLogin(t, ts.URL, mock)
	resp, _ := doJSON(t, "POST", ts.URL+"/api/auth/logout", tok, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout = %d, want 204", resp.StatusCode)
	}
	if status, _ := meWith(t, ts.URL, tok); status != http.StatusUnauthorized {
		t.Fatalf("revoked OIDC session still valid: %d", status)
	}
}

func TestOIDCBadStateRejected(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := e.serve(t)
	_, c := oidcStartCookie(t, ts.URL) // discovery cached, a real flow exists

	resp := oidcCallbackCookie(t, ts.URL, "auth-code", "not-a-real-state", c)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad state = %d, want 401", resp.StatusCode)
	}
}

func TestOIDCReplayedCallbackRejected(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := e.serve(t)

	q, c := oidcStartCookie(t, ts.URL)
	mock.SetNonce(q.Get("nonce"))
	state := q.Get("state")
	resp := oidcCallbackCookie(t, ts.URL, "auth-code", state, c)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("first callback = %d", resp.StatusCode)
	}
	// Replay the exact same callback URL — the state is single-use.
	resp = oidcCallbackCookie(t, ts.URL, "auth-code", state, c)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replayed callback = %d, want 401", resp.StatusCode)
	}
}

func TestOIDCBadNonceRejected(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := e.serve(t)
	q, c := oidcStartCookie(t, ts.URL)
	mock.SetNonce("a-different-nonce")
	resp := oidcCallbackCookie(t, ts.URL, "auth-code", q.Get("state"), c)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad nonce = %d, want 401", resp.StatusCode)
	}
}

func TestOIDCMissingNonceRejected(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := e.serve(t)
	q, c := oidcStartCookie(t, ts.URL)
	mock.SetNonce(q.Get("nonce"))
	mock.OmitNonce()
	resp := oidcCallbackCookie(t, ts.URL, "auth-code", q.Get("state"), c)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing nonce = %d, want 401", resp.StatusCode)
	}
}

func TestOIDCWrongAudienceRejected(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := e.serve(t)
	q, c := oidcStartCookie(t, ts.URL)
	mock.SetNonce(q.Get("nonce"))
	mock.SetAudience("another-application")
	resp := oidcCallbackCookie(t, ts.URL, "auth-code", q.Get("state"), c)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong audience = %d, want 401", resp.StatusCode)
	}
}

func TestOIDCExpiredTokenRejected(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := e.serve(t)
	q, c := oidcStartCookie(t, ts.URL)
	mock.SetNonce(q.Get("nonce"))
	mock.SetExpiry(-time.Minute)
	resp := oidcCallbackCookie(t, ts.URL, "auth-code", q.Get("state"), c)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired token = %d, want 401", resp.StatusCode)
	}
}

func TestOIDCMissingCallbackParams(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := e.serve(t)
	q := oidcStart(t, ts.URL)

	resp, _ := doJSON(t, "GET", ts.URL+"/api/auth/oidc/callback?state="+q.Get("state"), "", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing code = %d, want 400", resp.StatusCode)
	}
	resp, _ = doJSON(t, "GET", ts.URL+"/api/auth/oidc/callback?code=abc", "", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing state = %d, want 400", resp.StatusCode)
	}
}

func TestOIDCProviderDenialRejected(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := e.serve(t)
	resp, _ := doJSON(t, "GET", ts.URL+"/api/auth/oidc/callback?error=access_denied&error_description=user+cancelled", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("provider denial = %d, want 401", resp.StatusCode)
	}
}

func TestOIDCDiscoveryFailure503(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	// Point the enabled config at an unreachable issuer: the controller is
	// up (health ok) but starting a login fails cleanly with 503.
	e.server.Cfg.OIDC.Issuer = "http://127.0.0.1:1"
	e.server.OIDC = oidc.NewProvider(e.server.Cfg.OIDC, e.server.Log)
	ts := e.serve(t)

	resp, _ := doJSON(t, "GET", ts.URL+"/api/auth/oidc/login", "", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unreachable issuer login = %d, want 503", resp.StatusCode)
	}
	if h, _ := doJSON(t, "GET", ts.URL+"/api/health", "", nil); h.StatusCode != 200 {
		t.Fatalf("controller health degraded by IdP outage: %d", h.StatusCode)
	}
}

func TestOIDCAuditEvent(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := e.serve(t)

	oidcFullLogin(t, ts.URL, mock)
	resp, body := doJSON(t, "GET", ts.URL+"/api/audit", adminTok, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("audit list = %d", resp.StatusCode)
	}
	var events []model.AuditEvent
	json.Unmarshal(body, &events)
	found := false
	for _, ev := range events {
		if ev.Action == "auth.oidc_login" && ev.Actor == "alice" && ev.Object == "user:alice" {
			found = true
			if !strings.Contains(ev.Detail, `"provisioned":true`) {
				t.Fatalf("audit detail missing provisioned flag: %s", ev.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("no auth.oidc_login audit event in %d events", len(events))
	}
}

func TestOIDCHostileUsernameSanitized(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := e.serve(t)

	mock.SetIdentity("sub-hostile", "Evil User/../!", "")
	tok := oidcFullLogin(t, ts.URL, mock)
	status, me := meWith(t, ts.URL, tok)
	if status != 200 || me["username"] != "evil-user" {
		t.Fatalf("hostile username not sanitized: status=%d me=%v", status, me)
	}
}

func TestOIDCSecondLoginAuditNotProvisioned(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := e.serve(t)

	oidcFullLogin(t, ts.URL, mock)
	oidcFullLogin(t, ts.URL, mock)
	_, body := doJSON(t, "GET", ts.URL+"/api/audit", adminTok, nil)
	var events []model.AuditEvent
	json.Unmarshal(body, &events)
	provisionedCount := 0
	for _, ev := range events {
		if ev.Action == "auth.oidc_login" && strings.Contains(ev.Detail, `"provisioned":true`) {
			provisionedCount++
		}
	}
	if provisionedCount != 1 {
		t.Fatalf("provisioned=true audited %d times, want exactly 1", provisionedCount)
	}
}
