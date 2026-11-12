package api

// Quality-review regressions for P10 OIDC (commit 893de0e): truncation
// collisions (Q1), Unicode confusables (Q2), IdP error-body sanitization
// (Q3), flow-cookie CSRF binding (Q5) and login throttling (F2).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/oidc"
	"github.com/badskater/encode-system/backend/internal/oidc/oidctest"
	"github.com/badskater/encode-system/backend/internal/store"
	"github.com/badskater/encode-system/backend/internal/update"
)

// oidcFlowCookieName pins the wire name of the browser-binding cookie.
const oidcFlowCookieName = "encode_oidc_flow"

// newOIDCEnvWithLog is newOIDCEnv with a capturable controller log buffer
// (for asserting what does NOT get logged).
func newOIDCEnvWithLog(t *testing.T, mock *oidctest.Provider, adminGroup string, logBuf *bytes.Buffer) *testEnv {
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
	st, err := store.Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	up, err := update.NewStore(dir + "/updates")
	if err != nil {
		t.Fatal(err)
	}
	var w io.Writer = io.Discard
	if logBuf != nil {
		w = logBuf
	}
	s, err := New(st, up, slog.New(slog.NewTextHandler(w, nil)), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &testEnv{server: s, dir: dir}
}

// oidcStartCookie drives GET /login and returns the authorization query plus
// the flow cookie (nil when the server sets none).
func oidcStartCookie(t *testing.T, tsURL string) (url.Values, *http.Cookie) {
	t.Helper()
	resp, err := noRedirectClient().Get(tsURL + "/api/auth/oidc/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("oidc login status = %d, want 302", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("bad authorization redirect: %v", err)
	}
	if loc.Path != "/authorize" {
		t.Fatalf("redirect path = %q, want the provider /authorize", loc.Path)
	}
	var flow *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == oidcFlowCookieName {
			flow = c
		}
	}
	return loc.Query(), flow
}

// oidcCallbackCookie drives the callback presenting the given flow cookie
// (nil = no cookie, as in a cross-browser CSRF attempt).
func oidcCallbackCookie(t *testing.T, tsURL, code, state string, c *http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", tsURL+"/api/auth/oidc/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c != nil {
		req.AddCookie(c)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// ---------- Q1: truncation must not merge distinct IdP subjects ----------

func TestOIDCTruncationCollisionIndependentAccounts(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "encode-admins")
	ts := e.serve(t)

	prefix := strings.Repeat("a", 70)
	mock.SetIdentity("sub-victim", prefix+"-victim", "")
	mock.SetGroups([]string{"encode-admins"})
	tok1 := oidcFullLogin(t, ts.URL, mock)
	status1, me1 := meWith(t, ts.URL, tok1)

	mock.SetIdentity("sub-attacker", prefix+"-attacker", "")
	mock.SetGroups([]string{"users"})
	tok2 := oidcFullLogin(t, ts.URL, mock)
	status2, me2 := meWith(t, ts.URL, tok2)

	if status1 != 200 || status2 != 200 {
		t.Fatalf("logins failed: %d %d", status1, status2)
	}
	if me1["username"] == me2["username"] {
		t.Fatalf("distinct subjects share one account: %v", me1["username"])
	}
	if me1["role"] != model.RoleAdmin || me2["role"] != model.RoleViewer {
		t.Fatalf("roles not independent: victim=%v attacker=%v", me1["role"], me2["role"])
	}
	// The victim's ORIGINAL session keeps its role after the attacker login.
	if _, me1b := meWith(t, ts.URL, tok1); me1b["role"] != model.RoleAdmin {
		t.Fatalf("victim session role flipped by attacker login: %v", me1b["role"])
	}
	// Two separate account rows, one per identity.
	_, body := doJSON(t, "GET", ts.URL+"/api/users", adminTok, nil)
	var users []model.User
	json.Unmarshal(body, &users)
	counts := map[string]int{}
	for _, u := range users {
		counts[u.Username]++
	}
	if counts[me1["username"].(string)] != 1 || counts[me2["username"].(string)] != 1 {
		t.Fatalf("expected exactly one row per identity, got %v", counts)
	}
}

// ---------- Q2: Unicode confusables must never reach the local admin ----------

func TestOIDCConfusableNotLocalAdmin(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := e.serve(t)

	for i, name := range []string{"admİn", "adm\u212An"} {
		mock.SetIdentity("sub-conf", name, "")
		tok := oidcFullLogin(t, ts.URL, mock)
		status, me := meWith(t, ts.URL, tok)
		if status != 200 {
			t.Fatalf("confusable %d login = %d", i, status)
		}
		if me["username"] == "admin" {
			t.Fatalf("confusable %q landed on the local admin account", name)
		}
		if me["username"] != "adm-n" || me["role"] != model.RoleViewer {
			t.Fatalf("confusable %q → %v, want adm-n/viewer", name, me)
		}
	}
	// The real local admin is untouched.
	if _, me := meWith(t, ts.URL, adminTok); me["role"] != model.RoleAdmin {
		t.Fatalf("local admin session demoted: %v", me)
	}
}

// ---------- Q3: IdP error bodies never reach the client or the logs ----------

func TestOIDCTokenErrorBodySanitized(t *testing.T) {
	var buf bytes.Buffer
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnvWithLog(t, mock, "", &buf)
	ts := e.serve(t)

	// Secret-shaped marker (random hex) inside the IdP's 400 body.
	marker := oidctest.ClientSecret(t)
	mock.SetTokenError(http.StatusBadRequest,
		`{"error":"invalid_grant","error_description":"stolen code and secret: `+marker+`"}`)

	q, c := oidcStartCookie(t, ts.URL)
	mock.SetNonce(q.Get("nonce"))
	resp := oidcCallbackCookie(t, ts.URL, "auth-code", q.Get("state"), c)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token-error callback = %d, want 401", resp.StatusCode)
	}
	if strings.Contains(string(body), marker) {
		t.Fatalf("IdP error body echoed to unauthenticated caller: %s", body)
	}
	if !strings.Contains(string(body), "OIDC login failed") {
		t.Fatalf("generic client message missing: %s", body)
	}
	logs := buf.String()
	if strings.Contains(logs, marker) {
		t.Fatalf("IdP error body written to controller logs:\n%s", logs)
	}
	// Operators still get the error class/status.
	if !strings.Contains(logs, "400") {
		t.Fatalf("sanitized log lost the HTTP status:\n%s", logs)
	}
}

// ---------- Q5: flow cookie binds the login to the browser ----------

func TestOIDCFlowCookieSetAtLogin(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := e.serve(t)

	_, c := oidcStartCookie(t, ts.URL)
	if c == nil {
		t.Fatal("login did not set a flow cookie — state has no browser binding")
	}
	if !c.HttpOnly {
		t.Fatal("flow cookie not HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("flow cookie SameSite = %v, want Lax", c.SameSite)
	}
	if c.Path != "/api/auth/oidc" {
		t.Fatalf("flow cookie Path = %q, want /api/auth/oidc", c.Path)
	}
	if c.MaxAge != 600 {
		t.Fatalf("flow cookie MaxAge = %d, want 600 (flow TTL)", c.MaxAge)
	}
	if c.Secure {
		t.Fatal("flow cookie marked Secure over plain HTTP")
	}
	if len(c.Value) != 64 {
		t.Fatalf("flow cookie value shape wrong: %d chars", len(c.Value))
	}
}

func TestOIDCFlowCookieSecureOnTLS(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := httptest.NewTLSServer(e.server.Routes())
	t.Cleanup(ts.Close)
	client := ts.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	resp, err := client.Get(ts.URL + "/api/auth/oidc/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("TLS login = %d, want 302", resp.StatusCode)
	}
	var c *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == oidcFlowCookieName {
			c = ck
		}
	}
	if c == nil || !c.Secure {
		t.Fatalf("flow cookie over TLS must be Secure, got %+v", c)
	}
}

func TestOIDCFlowCookieBinding(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := e.serve(t)

	// 1. Callback WITHOUT the cookie → rejected (state alone is not enough).
	q1, _ := oidcStartCookie(t, ts.URL)
	mock.SetNonce(q1.Get("nonce"))
	resp := oidcCallback(t, ts.URL, "auth-code", q1.Get("state"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cookie-less callback = %d, want 401", resp.StatusCode)
	}

	// 2. Callback with a WRONG cookie → rejected.
	q2, _ := oidcStartCookie(t, ts.URL)
	mock.SetNonce(q2.Get("nonce"))
	wrong := &http.Cookie{Name: oidcFlowCookieName, Value: strings.Repeat("f", 64)}
	resp = oidcCallbackCookie(t, ts.URL, "auth-code", q2.Get("state"), wrong)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-cookie callback = %d, want 401", resp.StatusCode)
	}

	// 3. Callback with the CORRECT cookie → success.
	q3, c3 := oidcStartCookie(t, ts.URL)
	mock.SetNonce(q3.Get("nonce"))
	resp = oidcCallbackCookie(t, ts.URL, "auth-code", q3.Get("state"), c3)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("correct-cookie callback = %d, want 302", resp.StatusCode)
	}
	tok := strings.TrimPrefix(resp.Header.Get("Location"), "/#oidc=")
	if status, me := meWith(t, ts.URL, tok); status != 200 || me["username"] != "alice" {
		t.Fatalf("session from cookie-bound flow invalid: %d %v", status, me)
	}
}

// The login-CSRF attack: an attacker starts a flow and tricks the victim's
// browser into completing it. The victim has no (or a different) flow cookie,
// so the callback must be rejected — no session is issued in the victim's
// browser under the attacker's identity.
func TestOIDCLoginCSRFCrossClientRejected(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := e.serve(t)

	// Attacker-initiated flow, completed by a cookie-less victim browser.
	qA, _ := oidcStartCookie(t, ts.URL)
	mock.SetNonce(qA.Get("nonce"))
	resp := oidcCallback(t, ts.URL, "auth-code", qA.Get("state"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cross-client (no cookie) callback = %d, want 401", resp.StatusCode)
	}

	// Victim with their OWN flow cookie completing the attacker's state.
	qA2, _ := oidcStartCookie(t, ts.URL)
	mock.SetNonce(qA2.Get("nonce"))
	_, cV := oidcStartCookie(t, ts.URL) // the victim's own browser flow
	resp = oidcCallbackCookie(t, ts.URL, "auth-code", qA2.Get("state"), cV)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cross-client (foreign cookie) callback = %d, want 401", resp.StatusCode)
	}
}

func TestOIDCFlowCookieClearedOnEveryOutcome(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := e.serve(t)

	cleared := func(resp *http.Response) bool {
		for _, c := range resp.Cookies() {
			if c.Name == oidcFlowCookieName && c.MaxAge < 0 {
				return true
			}
		}
		return false
	}

	// Success clears the cookie.
	q, c := oidcStartCookie(t, ts.URL)
	mock.SetNonce(q.Get("nonce"))
	resp := oidcCallbackCookie(t, ts.URL, "auth-code", q.Get("state"), c)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || !cleared(resp) {
		t.Fatalf("success outcome did not clear the flow cookie: %d", resp.StatusCode)
	}

	// Failure (unknown state) clears the cookie.
	resp = oidcCallbackCookie(t, ts.URL, "auth-code", "bogus-state", c)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !cleared(resp) {
		t.Fatalf("failure outcome did not clear the flow cookie: %d", resp.StatusCode)
	}

	// Provider denial clears the cookie.
	resp, _ = doJSON(t, "GET", ts.URL+"/api/auth/oidc/callback?error=access_denied", "", nil)
	if resp.StatusCode != http.StatusUnauthorized || !cleared(resp) {
		t.Fatalf("denial outcome did not clear the flow cookie: %d", resp.StatusCode)
	}
}

func TestOIDCDisabledModeNoFlowCookie(t *testing.T) {
	e := newTestEnv(t) // OIDC disabled — the default install
	ts := e.serve(t)
	resp, _ := doLogin(t, ts.URL, "admin", adminPassword)
	if resp.StatusCode != 200 {
		t.Fatalf("local login = %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == oidcFlowCookieName {
			t.Fatal("disabled install set an OIDC flow cookie")
		}
	}
}

// ---------- F2: /login is throttled like local login ----------

func TestOIDCLoginThrottled(t *testing.T) {
	mock := oidctest.New(t, testOIDCClientID)
	e := newOIDCEnv(t, mock, "")
	ts := e.serve(t)

	var lastQ url.Values
	var lastC *http.Cookie
	for i := 0; i < 5; i++ {
		q, c := oidcStartCookie(t, ts.URL) // 302 each (fatals otherwise)
		lastQ, lastC = q, c
	}
	// Sixth start within the window → 429.
	resp, err := noRedirectClient().Get(ts.URL + "/api/auth/oidc/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("burst of login starts = %d on 6th, want 429", resp.StatusCode)
	}
	// The local login keeps its OWN counter — OIDC lockout must not lock
	// the local admin out.
	if r, _ := doLogin(t, ts.URL, "admin", adminPassword); r.StatusCode != 200 {
		t.Fatalf("local login blocked by OIDC throttle: %d", r.StatusCode)
	}
	// Completing a pending flow successfully resets the throttle.
	mock.SetNonce(lastQ.Get("nonce"))
	r := oidcCallbackCookie(t, ts.URL, "auth-code", lastQ.Get("state"), lastC)
	r.Body.Close()
	if r.StatusCode != http.StatusFound {
		t.Fatalf("pending flow completion = %d, want 302", r.StatusCode)
	}
	resp, err = noRedirectClient().Get(ts.URL + "/api/auth/oidc/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login after successful callback = %d, want 302 (throttle reset)", resp.StatusCode)
	}
}

// ---------- Q4: HA re-read nil-race guard ----------

func TestOIDCRereadUserNilRaceReturnsError(t *testing.T) {
	// The conflicting row was deleted again in the race window: the lookup
	// returns (nil, nil). That must become an explicit error — never a nil
	// user that panics at user.ID downstream.
	lookup := func(context.Context, string) (*model.User, error) { return nil, nil }
	u, err := oidcRereadUser(context.Background(), lookup, "ghost")
	if err == nil {
		t.Fatal("nil user after re-read returned nil error — nil-deref panic downstream")
	}
	if u != nil {
		t.Fatalf("expected nil user, got %+v", u)
	}
	// Store errors propagate unchanged.
	sentinel := errors.New("db down")
	lookup = func(context.Context, string) (*model.User, error) { return nil, sentinel }
	if _, err := oidcRereadUser(context.Background(), lookup, "ghost"); !errors.Is(err, sentinel) {
		t.Fatalf("store error not propagated: %v", err)
	}
	// Happy path: the row exists.
	want := &model.User{ID: 7, Username: "ghost"}
	lookup = func(context.Context, string) (*model.User, error) { return want, nil }
	u, err = oidcRereadUser(context.Background(), lookup, "ghost")
	if err != nil || u != want {
		t.Fatalf("existing user lost: %+v err=%v", u, err)
	}
}
