// Package oidctest provides an in-process mock OIDC provider (discovery
// document + JWKS + token endpoint, RSA-signed ID tokens) for tests. This
// PACKAGE is imported only by test code, but the go-jose library itself IS
// linked into the controller binary — go-oidc uses it for JWKS and ID-token
// verification. Only this mock signer is test-only.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// Provider is a mock OIDC issuer. Tests set the knobs (Nonce, Groups,
// Expiry, ...) before driving the callback; the token endpoint signs an ID
// token with the current knob values. All knobs are guarded by mu because
// the token endpoint runs on the httptest server goroutine.
type Provider struct {
	TS     *httptest.Server
	Issuer string // equals TS.URL — use as ENCODE_OIDC_ISSUER
	Key    *rsa.PrivateKey

	mu                sync.Mutex
	nonce             string
	omitNonce         bool
	audience          string
	expiry            time.Duration
	issuerOverride    string
	subject           string
	preferredUsername string
	email             string
	groups            []string
	groupsClaim       string
	omitIDToken       bool
	tokenErrStatus    int
	tokenErrBody      string
	tokenDelay        time.Duration
	lastCode          string
	lastCodeVerifier  string
}

// New starts a mock provider for the given client ID. The RSA signing key
// is generated fresh per test; the server closes via t.Cleanup.
func New(t *testing.T, clientID string) *Provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{
		Key:               key,
		audience:          clientID,
		expiry:            5 * time.Minute,
		subject:           "mock-sub-1",
		preferredUsername: "alice",
		email:             "alice@example.org",
		groupsClaim:       "groups",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                p.Issuer,
			"authorization_endpoint":                p.TS.URL + "/authorize",
			"token_endpoint":                        p.TS.URL + "/token",
			"jwks_uri":                              p.TS.URL + "/jwks",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &p.Key.PublicKey, KeyID: "test-key-1", Algorithm: "RS256", Use: "sig"},
		}})
	})
	mux.HandleFunc("/token", p.handleToken)
	p.TS = httptest.NewServer(mux)
	p.Issuer = p.TS.URL
	t.Cleanup(p.TS.Close)
	return p
}

// SetNonce sets the nonce embedded in the next ID token (tests copy it from
// the authorization redirect URL).
func (p *Provider) SetNonce(n string) { p.mu.Lock(); defer p.mu.Unlock(); p.nonce = n }

// SetGroups sets the group list emitted under the configured claim name.
func (p *Provider) SetGroups(g []string) { p.mu.Lock(); defer p.mu.Unlock(); p.groups = g }

// SetIdentity overrides the subject / preferred_username / email claims.
func (p *Provider) SetIdentity(sub, preferred, email string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subject, p.preferredUsername, p.email = sub, preferred, email
}

// SetGroupsClaim changes the claim name the groups are emitted under.
func (p *Provider) SetGroupsClaim(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.groupsClaim = name
}

// SetAudience overrides the "aud" claim (wrong-audience tests).
func (p *Provider) SetAudience(a string) { p.mu.Lock(); defer p.mu.Unlock(); p.audience = a }

// SetExpiry offsets the ID token expiry from now (negative = expired).
func (p *Provider) SetExpiry(d time.Duration) { p.mu.Lock(); defer p.mu.Unlock(); p.expiry = d }

// SetIssuerOverride emits a different "iss" claim (wrong-issuer tests).
func (p *Provider) SetIssuerOverride(iss string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.issuerOverride = iss
}

// OmitNonce drops the nonce claim entirely.
func (p *Provider) OmitNonce() { p.mu.Lock(); defer p.mu.Unlock(); p.omitNonce = true }

// OmitIDToken makes the token response carry no id_token.
func (p *Provider) OmitIDToken() { p.mu.Lock(); defer p.mu.Unlock(); p.omitIDToken = true }

// SetTokenError makes the token endpoint reply with the given HTTP status
// and raw body instead of signing an ID token (error-path tests; the body
// may carry secret-shaped content to prove it is never echoed).
func (p *Provider) SetTokenError(status int, body string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tokenErrStatus, p.tokenErrBody = status, body
}

// SetTokenDelay stalls the token endpoint by d before responding, so tests
// can prove outbound exchanges are bounded. The stall ends early when the
// caller disconnects (request context cancelled), keeping cleanup fast.
func (p *Provider) SetTokenDelay(d time.Duration) { p.mu.Lock(); defer p.mu.Unlock(); p.tokenDelay = d }

// LastCodeVerifier returns the code_verifier received on the last token
// exchange (lets tests prove PKCE was actually sent).
func (p *Provider) LastCodeVerifier() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastCodeVerifier
}

// LastCode returns the authorization code received on the last exchange.
func (p *Provider) LastCode() string { p.mu.Lock(); defer p.mu.Unlock(); return p.lastCode }

// handleToken signs and returns an ID token per the current knobs.
func (p *Provider) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastCode = r.PostFormValue("code")
	p.lastCodeVerifier = r.PostFormValue("code_verifier")
	if p.tokenDelay > 0 {
		select {
		case <-time.After(p.tokenDelay):
		case <-r.Context().Done():
		}
	}
	if p.tokenErrStatus != 0 {
		w.WriteHeader(p.tokenErrStatus)
		io.WriteString(w, p.tokenErrBody)
		return
	}
	if p.lastCode == "" {
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	if p.omitIDToken {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "mock-access", "token_type": "Bearer"})
		return
	}
	now := time.Now()
	iss := p.Issuer
	if p.issuerOverride != "" {
		iss = p.issuerOverride
	}
	claims := map[string]any{
		"iss":                iss,
		"sub":                p.subject,
		"aud":                p.audience,
		"exp":                now.Add(p.expiry).Unix(),
		"iat":                now.Unix(),
		"preferred_username": p.preferredUsername,
		"email":              p.email,
	}
	if !p.omitNonce {
		claims["nonce"] = p.nonce
	}
	if p.groups != nil {
		claims[p.groupsClaim] = p.groups
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		http.Error(w, "marshal claims", http.StatusInternalServerError)
		return
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: p.Key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test-key-1"),
	)
	if err != nil {
		http.Error(w, "signer", http.StatusInternalServerError)
		return
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		http.Error(w, "sign", http.StatusInternalServerError)
		return
	}
	compact, err := jws.CompactSerialize()
	if err != nil {
		http.Error(w, "serialize", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"access_token": "mock-access",
		"token_type":   "Bearer",
		"expires_in":   300,
		"id_token":     compact,
	})
}

// ClientSecret generates a random client secret at runtime (never a
// hardcoded credential-shaped literal).
func ClientSecret(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", b)
}
