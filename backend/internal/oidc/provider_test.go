package oidc

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/badskater/encode-system/backend/internal/auth"
	"github.com/badskater/encode-system/backend/internal/oidc/oidctest"
)

const testClientID = "encode-test-client"

// newMockProvider wires a Provider against a fresh in-process mock IdP.
func newMockProvider(t *testing.T) (*Provider, *oidctest.Provider) {
	t.Helper()
	mock := oidctest.New(t, testClientID)
	cfg := &Config{
		Issuer:       mock.Issuer,
		ClientID:     testClientID,
		ClientSecret: oidctest.ClientSecret(t),
		RedirectURL:  "https://ctl.example/api/auth/oidc/callback",
		GroupsClaim:  "groups",
	}
	return NewProvider(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))), mock
}

// startFlow runs LoginURL and returns the parsed authorization query plus
// the raw browser-binding flow token (the cookie value).
func startFlow(t *testing.T, p *Provider) (url.Values, string) {
	t.Helper()
	raw, flowToken, err := p.LoginURL(context.Background())
	if err != nil {
		t.Fatalf("LoginURL: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	return u.Query(), flowToken
}

func TestLoginURLCarriesStateNoncePKCE(t *testing.T) {
	p, _ := newMockProvider(t)
	q, flowToken := startFlow(t, p)

	if q.Get("response_type") != "code" {
		t.Fatalf("response_type = %q", q.Get("response_type"))
	}
	if q.Get("client_id") != testClientID {
		t.Fatalf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("redirect_uri") != p.cfg.RedirectURL {
		t.Fatalf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if !strings.Contains(q.Get("scope"), "openid") {
		t.Fatalf("scope missing openid: %q", q.Get("scope"))
	}
	state, nonce, challenge := q.Get("state"), q.Get("nonce"), q.Get("code_challenge")
	if state == "" || nonce == "" || challenge == "" {
		t.Fatalf("state/nonce/challenge must all be present: %v", q)
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %q", q.Get("code_challenge_method"))
	}
	// The stored verifier must hash to the advertised challenge (S256).
	p.mu.Lock()
	flow, ok := p.flows[state]
	p.mu.Unlock()
	if !ok {
		t.Fatal("state not stored server-side")
	}
	if oauth2.S256ChallengeFromVerifier(flow.verifier) != challenge {
		t.Fatal("code_challenge does not match the stored PKCE verifier")
	}
	if flow.nonce != nonce {
		t.Fatal("stored nonce differs from the one in the URL")
	}
	if time.Until(flow.expires) <= 0 || time.Until(flow.expires) > flowTTL {
		t.Fatalf("flow expiry out of bounds: %v", flow.expires)
	}
	// The flow token is stored ONLY as a hash (session-token pattern).
	if len(flowToken) != 64 {
		t.Fatalf("flow token shape wrong: %d chars", len(flowToken))
	}
	if flow.cookieHash != auth.HashToken(flowToken) {
		t.Fatal("stored cookie hash does not match the returned flow token")
	}
	if flow.cookieHash == flowToken {
		t.Fatal("raw flow token stored server-side — only the hash may be stored")
	}
}

func TestCallbackHappyPath(t *testing.T) {
	p, mock := newMockProvider(t)
	q, flowToken := startFlow(t, p)
	state, nonce := q.Get("state"), q.Get("nonce")
	mock.SetNonce(nonce)
	mock.SetGroups([]string{"users", "encode-admins"})

	ident, err := p.Callback(context.Background(), "auth-code-1", state, flowToken)
	if err != nil {
		t.Fatalf("Callback: %v", err)
	}
	if ident.Subject != "mock-sub-1" || ident.PreferredUsername != "alice" || ident.Email != "alice@example.org" {
		t.Fatalf("identity claims lost: %+v", ident)
	}
	if len(ident.Groups) != 2 || ident.Groups[1] != "encode-admins" {
		t.Fatalf("groups lost: %+v", ident.Groups)
	}
	// PKCE verifier must have travelled with the exchange.
	if mock.LastCode() != "auth-code-1" {
		t.Fatalf("code not exchanged: %q", mock.LastCode())
	}
	// State consumed (single-use).
	p.mu.Lock()
	n := len(p.flows)
	p.mu.Unlock()
	if n != 0 {
		t.Fatalf("flow not consumed, %d left", n)
	}
}

func TestCallbackSendsPKCEVerifier(t *testing.T) {
	p, mock := newMockProvider(t)
	q, flowToken := startFlow(t, p)
	state := q.Get("state")
	p.mu.Lock()
	verifier := p.flows[state].verifier
	p.mu.Unlock()
	mock.SetNonce(q.Get("nonce"))
	if _, err := p.Callback(context.Background(), "code", state, flowToken); err != nil {
		t.Fatal(err)
	}
	if mock.LastCodeVerifier() != verifier {
		t.Fatalf("token exchange verifier %q != stored %q", mock.LastCodeVerifier(), verifier)
	}
}

func TestCallbackUnknownStateRejected(t *testing.T) {
	p, mock := newMockProvider(t)
	_, flowToken := startFlow(t, p) // discovery cached
	mock.SetNonce("whatever")
	if _, err := p.Callback(context.Background(), "code", "bogus-state", flowToken); err == nil {
		t.Fatal("unknown state must be rejected")
	}
}

func TestCallbackReplayRejected(t *testing.T) {
	p, mock := newMockProvider(t)
	q, flowToken := startFlow(t, p)
	state := q.Get("state")
	mock.SetNonce(q.Get("nonce"))
	if _, err := p.Callback(context.Background(), "code", state, flowToken); err != nil {
		t.Fatal(err)
	}
	// Same state again — even with a fresh valid code and the right cookie —
	// must fail.
	if _, err := p.Callback(context.Background(), "code-2", state, flowToken); err == nil {
		t.Fatal("replayed state must be rejected")
	}
}

func TestCallbackExpiredFlowRejected(t *testing.T) {
	p, mock := newMockProvider(t)
	q, flowToken := startFlow(t, p)
	state := q.Get("state")
	mock.SetNonce(q.Get("nonce"))
	// Age the flow past its TTL without waiting.
	p.mu.Lock()
	f := p.flows[state]
	f.expires = time.Now().Add(-time.Second)
	p.flows[state] = f
	p.mu.Unlock()
	if _, err := p.Callback(context.Background(), "code", state, flowToken); err == nil {
		t.Fatal("expired flow must be rejected")
	}
}

func TestCallbackMissingCodeOrState(t *testing.T) {
	p, _ := newMockProvider(t)
	if _, err := p.Callback(context.Background(), "", "state", "token"); err == nil {
		t.Fatal("missing code must be rejected")
	}
	if _, err := p.Callback(context.Background(), "code", "", "token"); err == nil {
		t.Fatal("missing state must be rejected")
	}
}

func TestCallbackBadNonceRejected(t *testing.T) {
	p, mock := newMockProvider(t)
	q, flowToken := startFlow(t, p)
	mock.SetNonce("not-the-flow-nonce")
	if _, err := p.Callback(context.Background(), "code", q.Get("state"), flowToken); err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Fatalf("wrong nonce must be rejected with a nonce error, got %v", err)
	}
}

func TestCallbackMissingNonceRejected(t *testing.T) {
	p, mock := newMockProvider(t)
	q, flowToken := startFlow(t, p)
	mock.SetNonce(q.Get("nonce"))
	mock.OmitNonce()
	if _, err := p.Callback(context.Background(), "code", q.Get("state"), flowToken); err == nil {
		t.Fatal("missing nonce claim must be rejected")
	}
}

func TestCallbackWrongAudienceRejected(t *testing.T) {
	p, mock := newMockProvider(t)
	q, flowToken := startFlow(t, p)
	mock.SetNonce(q.Get("nonce"))
	mock.SetAudience("some-other-client")
	if _, err := p.Callback(context.Background(), "code", q.Get("state"), flowToken); err == nil {
		t.Fatal("wrong audience must be rejected")
	}
}

func TestCallbackExpiredTokenRejected(t *testing.T) {
	p, mock := newMockProvider(t)
	q, flowToken := startFlow(t, p)
	mock.SetNonce(q.Get("nonce"))
	mock.SetExpiry(-time.Minute)
	if _, err := p.Callback(context.Background(), "code", q.Get("state"), flowToken); err == nil {
		t.Fatal("expired id token must be rejected")
	}
}

func TestCallbackWrongIssuerRejected(t *testing.T) {
	p, mock := newMockProvider(t)
	q, flowToken := startFlow(t, p)
	mock.SetNonce(q.Get("nonce"))
	mock.SetIssuerOverride("https://evil.example")
	if _, err := p.Callback(context.Background(), "code", q.Get("state"), flowToken); err == nil {
		t.Fatal("wrong issuer must be rejected")
	}
}

func TestCallbackNoIDTokenRejected(t *testing.T) {
	p, mock := newMockProvider(t)
	q, flowToken := startFlow(t, p)
	mock.SetNonce(q.Get("nonce"))
	mock.OmitIDToken()
	if _, err := p.Callback(context.Background(), "code", q.Get("state"), flowToken); err == nil {
		t.Fatal("token response without id_token must be rejected")
	}
}

func TestCallbackCustomGroupsClaim(t *testing.T) {
	p, mock := newMockProvider(t)
	p.cfg.GroupsClaim = "roles"
	mock.SetGroupsClaim("roles")
	mock.SetGroups([]string{"ops"})
	q, flowToken := startFlow(t, p)
	mock.SetNonce(q.Get("nonce"))
	ident, err := p.Callback(context.Background(), "code", q.Get("state"), flowToken)
	if err != nil {
		t.Fatal(err)
	}
	if len(ident.Groups) != 1 || ident.Groups[0] != "ops" {
		t.Fatalf("custom claim groups lost: %+v", ident.Groups)
	}
}

func TestDiscoveryFailureIsNotCached(t *testing.T) {
	p, mock := newMockProvider(t)
	// Point at an unreachable issuer: login must fail cleanly...
	p.cfg.Issuer = "http://127.0.0.1:1"
	if _, _, err := p.LoginURL(context.Background()); err == nil {
		t.Fatal("unreachable issuer must fail")
	}
	// ...and the SAME provider must recover once the issuer is reachable
	// (no negative caching — a transient IdP outage heals on next attempt).
	p.cfg.Issuer = mock.Issuer
	if _, _, err := p.LoginURL(context.Background()); err != nil {
		t.Fatalf("discovery failure was cached: %v", err)
	}
}

// ---------- Browser-binding flow cookie (quality review Q5) ----------

// A callback without the flow cookie is rejected even with a valid state:
// the state alone no longer authorizes completing the flow.
func TestCallbackMissingFlowCookieRejected(t *testing.T) {
	p, mock := newMockProvider(t)
	q, _ := startFlow(t, p)
	mock.SetNonce(q.Get("nonce"))
	_, err := p.Callback(context.Background(), "code", q.Get("state"), "")
	if err == nil || !strings.Contains(err.Error(), "cookie") {
		t.Fatalf("missing flow cookie must be rejected with a cookie error, got %v", err)
	}
}

// A foreign cookie (another browser's flow token) is rejected too.
func TestCallbackWrongFlowCookieRejected(t *testing.T) {
	p, mock := newMockProvider(t)
	q, _ := startFlow(t, p)
	mock.SetNonce(q.Get("nonce"))
	other, _ := auth.NewToken()
	_, err := p.Callback(context.Background(), "code", q.Get("state"), other)
	if err == nil || !strings.Contains(err.Error(), "cookie") {
		t.Fatalf("foreign flow cookie must be rejected with a cookie error, got %v", err)
	}
	// The rejection consumed the state — the rightful browser must restart.
	if _, err := p.Callback(context.Background(), "code", q.Get("state"), other); err == nil {
		t.Fatal("state must be single-use even on cookie rejection")
	}
}

// ---------- Bounded token exchange (quality review F1) ----------

// The token exchange must use the provider's bounded HTTP client, not
// x/oauth2's default (unbounded) client: a blackholed token endpoint has to
// abort within the client timeout instead of hanging the login forever.
func TestTokenExchangeBoundedByClientTimeout(t *testing.T) {
	p, mock := newMockProvider(t)
	mock.SetTokenDelay(2 * time.Second) // blackhole-ish token endpoint
	p.client.Timeout = 250 * time.Millisecond
	q, flowToken := startFlow(t, p)
	mock.SetNonce(q.Get("nonce"))

	start := time.Now()
	_, err := p.Callback(context.Background(), "code", q.Get("state"), flowToken)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("stalled token endpoint must fail the exchange")
	}
	if elapsed >= 1500*time.Millisecond {
		t.Fatalf("exchange not bounded by client timeout: took %v", elapsed)
	}
}

func TestPendingFlowCap(t *testing.T) {
	p, _ := newMockProvider(t)
	// Fill the table with fresh flows (whitebox — no network per entry).
	p.mu.Lock()
	for i := 0; i < maxPendingFlows; i++ {
		p.flows[fmt.Sprintf("state-%d", i)] = flowState{expires: time.Now().Add(flowTTL)}
	}
	p.mu.Unlock()
	if _, _, err := p.LoginURL(context.Background()); err == nil || !strings.Contains(err.Error(), "too many pending") {
		t.Fatalf("cap not enforced: %v", err)
	}
	// Expired entries are pruned, making room again.
	p.mu.Lock()
	for k := range p.flows {
		p.flows[k] = flowState{expires: time.Now().Add(-time.Second)}
	}
	p.mu.Unlock()
	if _, _, err := p.LoginURL(context.Background()); err != nil {
		t.Fatalf("expired flows not pruned: %v", err)
	}
}
