package oidc

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/badskater/encode-system/backend/internal/auth"
)

const (
	// flowTTL bounds how long a started login may take to come back through
	// the callback. Ten minutes is generous for an interactive IdP login.
	flowTTL = 10 * time.Minute
	// maxPendingFlows caps the in-memory state table so the unauthenticated
	// login endpoint cannot be used to exhaust controller memory.
	maxPendingFlows = 1024
	// providerTimeout bounds every outbound OIDC request (discovery, token
	// exchange, JWKS refresh) so a hung provider cannot hang a login.
	providerTimeout = 15 * time.Second
)

// FlowCookieName is the browser-binding cookie set at /login and verified at
// the callback. It ties the flow to the browser that started it: an attacker
// cannot initiate a flow and have the victim's browser complete it (login
// CSRF), because the victim's browser carries no matching cookie.
const FlowCookieName = "encode_oidc_flow"

// FlowCookieMaxAge is the flow cookie's lifetime in seconds (the flow TTL).
const FlowCookieMaxAge = int(flowTTL / time.Second)

// Provider drives the authorization-code flow with state, nonce, PKCE and a
// browser-binding flow cookie (login-CSRF protection). Discovery is LAZY:
// NewProvider performs no network I/O, so the controller boots even while the
// IdP is down; the first login attempt discovers the provider and caches it
// (failures are retried on the next attempt, never cached).
//
// HA note: pending flows live in memory on the replica that started them, so
// the callback must reach the SAME replica (sticky sessions or a single
// replica). This matches the existing per-replica in-memory surfaces
// (loginThrottle, SSE logHub); see Docs/md/Operations.md.
type Provider struct {
	cfg    *Config
	log    *slog.Logger
	client *http.Client

	mu         sync.Mutex
	discovered *gooidc.Provider
	verifier   *gooidc.IDTokenVerifier
	oauthCfg   oauth2.Config
	flows      map[string]flowState
}

// flowState is the server-side half of one started login. The state value
// itself is the map key (random, single-use); nonce, PKCE verifier and the
// flow-cookie hash never leave the server. cookieHash binds the flow to the
// browser that started it (only the hash is stored, like session tokens).
type flowState struct {
	nonce      string
	verifier   string
	cookieHash string
	expires    time.Time
}

// NewProvider builds a provider handle for a validated config. No network
// I/O happens here — see Provider for the lazy-discovery contract.
func NewProvider(cfg *Config, log *slog.Logger) *Provider {
	if log == nil {
		log = slog.Default()
	}
	return &Provider{
		cfg:    cfg,
		log:    log,
		client: &http.Client{Timeout: providerTimeout},
		flows:  map[string]flowState{},
	}
}

// ensureLocked runs discovery once and caches the provider, verifier and
// oauth2 config. Caller holds p.mu. Errors are returned to the caller and
// NOT cached, so a transient IdP outage heals on the next login attempt.
func (p *Provider) ensureLocked(ctx context.Context) error {
	if p.discovered != nil {
		return nil
	}
	prov, err := gooidc.NewProvider(gooidc.ClientContext(ctx, p.client), p.cfg.Issuer)
	if err != nil {
		return fmt.Errorf("OIDC discovery for %s: %w", p.cfg.Issuer, err)
	}
	// JWKS refreshes use a background context carrying our bounded client —
	// key rotation must not depend on any single request's lifetime.
	vctx := gooidc.ClientContext(context.Background(), p.client)
	p.verifier = prov.VerifierContext(vctx, &gooidc.Config{ClientID: p.cfg.ClientID})
	p.oauthCfg = oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.cfg.ClientSecret,
		Endpoint:     prov.Endpoint(),
		RedirectURL:  p.cfg.RedirectURL,
		Scopes:       []string{"openid", "profile", "email"},
	}
	p.discovered = prov
	return nil
}

// LoginURL starts a flow: it generates a random single-use state, a nonce,
// a PKCE verifier and a browser-binding flow token, stores them server-side
// (the flow token only as a hash), and returns the provider's authorization
// URL plus the raw flow token. The caller sets the token as the
// FlowCookieName cookie on the browser; Callback verifies it.
func (p *Provider) LoginURL(ctx context.Context) (authURL, flowToken string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureLocked(ctx); err != nil {
		return "", "", err
	}
	now := time.Now()
	p.pruneLocked(now)
	if len(p.flows) >= maxPendingFlows {
		return "", "", errors.New("too many pending OIDC logins; retry shortly")
	}
	state, err := auth.NewToken()
	if err != nil {
		return "", "", err
	}
	nonce, err := auth.NewToken()
	if err != nil {
		return "", "", err
	}
	flowToken, err = auth.NewToken()
	if err != nil {
		return "", "", err
	}
	verifier := oauth2.GenerateVerifier()
	p.flows[state] = flowState{
		nonce:      nonce,
		verifier:   verifier,
		cookieHash: auth.HashToken(flowToken),
		expires:    now.Add(flowTTL),
	}
	authURL = p.oauthCfg.AuthCodeURL(state,
		gooidc.Nonce(nonce),
		oauth2.SetAuthURLParam("code_challenge", oauth2.S256ChallengeFromVerifier(verifier)),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
	return authURL, flowToken, nil
}

// Callback completes a flow: it consumes the single-use state, verifies the
// browser-binding flow cookie (constant-time hash compare), exchanges the
// authorization code (with the PKCE verifier) over the bounded HTTP client,
// verifies the ID token (signature via JWKS, issuer, audience, expiry) and
// checks the nonce. Any failure rejects the login; the state is consumed
// even on failure so a captured callback URL cannot be replayed.
func (p *Provider) Callback(ctx context.Context, code, state, flowToken string) (*Identity, error) {
	if code == "" || state == "" {
		return nil, errors.New("missing code or state")
	}
	p.mu.Lock()
	if err := p.ensureLocked(ctx); err != nil {
		p.mu.Unlock()
		return nil, err
	}
	flow, ok := p.flows[state]
	delete(p.flows, state) // single-use: consume on ANY outcome
	p.mu.Unlock()
	if !ok || time.Now().After(flow.expires) {
		return nil, errors.New("unknown or expired login state (start the login again)")
	}
	// Browser binding: the callback must present the flow cookie set at
	// /login. Missing or foreign cookies fail here — an attacker-initiated
	// flow cannot be completed by the victim's browser (login CSRF).
	if !auth.VerifyToken(flowToken, flow.cookieHash) {
		return nil, errors.New("login flow cookie missing or mismatched (start the login again)")
	}

	// The bounded client must travel with the exchange context — x/oauth2
	// otherwise uses http.DefaultClient (no timeout) and a blackholed token
	// endpoint would hang the login forever.
	tok, err := p.oauthCfg.Exchange(gooidc.ClientContext(ctx, p.client), code, oauth2.VerifierOption(flow.verifier))
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return nil, errors.New("token response carried no id_token")
	}
	idTok, err := p.verifier.Verify(ctx, rawID)
	if err != nil {
		return nil, fmt.Errorf("id token verification: %w", err)
	}
	// go-oidc verifies signature/issuer/audience/expiry but leaves the nonce
	// to the caller. A missing nonce claim fails here too (empty != flow's).
	if subtle.ConstantTimeCompare([]byte(idTok.Nonce), []byte(flow.nonce)) != 1 {
		return nil, errors.New("id token nonce mismatch")
	}

	var claims map[string]any
	if err := idTok.Claims(&claims); err != nil {
		return nil, fmt.Errorf("id token claims: %w", err)
	}
	ident := &Identity{
		Subject:           stringClaim(claims, "sub"),
		PreferredUsername: stringClaim(claims, "preferred_username"),
		Email:             stringClaim(claims, "email"),
		Groups:            groupsFromClaim(claims[p.cfg.GroupsClaim]),
	}
	if ident.Subject == "" {
		return nil, errors.New("id token has no sub claim")
	}
	return ident, nil
}

// stringClaim reads a string claim, tolerating absent/null/non-string values.
func stringClaim(claims map[string]any, key string) string {
	s, _ := claims[key].(string)
	return s
}

// pruneLocked drops expired flows. Caller holds p.mu.
func (p *Provider) pruneLocked(now time.Time) {
	for k, f := range p.flows {
		if now.After(f.expires) {
			delete(p.flows, k)
		}
	}
}
