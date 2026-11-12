package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/badskater/encode-system/backend/internal/auth"
	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/oidc"
)

// OIDC login handlers. Registered ONLY when s.OIDC is non-nil (env-gated);
// with OIDC disabled these routes do not exist (404) and /api/health reports
// oidc_enabled=false. Local login (handleLogin) is untouched — both methods
// coexist. Sessions issued here are ordinary sessions: same token shape,
// same TTL/sliding expiry, same logout/revocation as local login.

// handleOIDCLogin starts the authorization-code flow and redirects the
// browser to the provider. The UI's "Sign in with SSO" button navigates here.
// A crypto-random flow cookie binds the flow to THIS browser; the callback
// verifies it (login-CSRF protection).
func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	// Same lockout mechanism as local login, with its OWN counter: every
	// start consumes flow-table capacity, so an unauthenticated burst must
	// not be able to churn the state map (state-cap DoS). OIDC lockout never
	// blocks local login, and vice versa.
	if ok, wait := s.oidcThrottle.allow(); !ok {
		writeErr(w, http.StatusTooManyRequests,
			"too many OIDC login attempts; retry in "+wait.Round(time.Second).String())
		return
	}
	s.oidcThrottle.fail()
	url, flowToken, err := s.OIDC.LoginURL(r.Context())
	if err != nil {
		// Discovery/network failure: the controller stays up, the login
		// attempt fails cleanly and the next attempt retries discovery.
		// Log a sanitized class only — provider error bodies may echo codes.
		s.Log.Warn("oidc login start failed", "err", oidc.ErrorSummary(err))
		clearOIDCFlowCookie(w, r)
		writeErr(w, http.StatusServiceUnavailable, "OIDC provider unavailable — check controller logs")
		return
	}
	setOIDCFlowCookie(w, r, flowToken, oidc.FlowCookieMaxAge)
	http.Redirect(w, r, url, http.StatusFound)
}

// setOIDCFlowCookie writes the browser-binding flow cookie. HttpOnly (no JS
// access), SameSite=Lax (sent on the top-level IdP→callback GET navigation,
// withheld from cross-site subrequests), Secure when the request is TLS,
// scoped to the OIDC routes only. maxAge<0 clears the cookie.
func setOIDCFlowCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     oidc.FlowCookieName,
		Value:    value,
		Path:     "/api/auth/oidc",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
}

// clearOIDCFlowCookie expires the flow cookie. Called on EVERY callback
// outcome and on login-start failure, before any body write.
func clearOIDCFlowCookie(w http.ResponseWriter, r *http.Request) {
	setOIDCFlowCookie(w, r, "", -1)
}

// handleOIDCCallback completes the flow: state/nonce/PKCE + ID-token
// verification happen inside oidc.Provider.Callback; on success the user is
// auto-provisioned (or role-synced) and a session is issued exactly like
// local login. The browser is redirected to the SPA with the session token
// in the URL FRAGMENT — fragments are never sent to servers or logs, and the
// SPA consumes and strips it immediately (consumeOidcFragment in client.ts).
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	// Clear the browser-binding cookie on EVERY outcome (success, denial,
	// rejection). Set before any body write so the header always lands.
	clearOIDCFlowCookie(w, r)
	q := r.URL.Query()
	// Provider-side denial (user cancelled, consent refused, ...).
	if e := q.Get("error"); e != "" {
		s.Log.Warn("oidc login denied by provider", "error", e, "description", q.Get("error_description"))
		writeErr(w, http.StatusUnauthorized, "OIDC login was denied by the provider")
		return
	}
	code, state := q.Get("code"), q.Get("state")
	if code == "" || state == "" {
		writeErr(w, http.StatusBadRequest, "code and state are required")
		return
	}
	flowToken := ""
	if c, err := r.Cookie(oidc.FlowCookieName); err == nil {
		flowToken = c.Value
	}
	ident, err := s.OIDC.Callback(r.Context(), code, state, flowToken)
	if err != nil {
		// Generic client message; the server log keeps only a sanitized
		// error class/status — raw IdP error bodies can echo codes/PII.
		s.Log.Warn("oidc callback rejected", "err", oidc.ErrorSummary(err))
		writeErr(w, http.StatusUnauthorized, "OIDC login failed")
		return
	}

	user, provisioned, err := s.oidcProvisionUser(r.Context(), ident)
	if err != nil {
		s.Log.Error("oidc user provisioning failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "OIDC user provisioning failed")
		return
	}

	// Session issuance — identical to handleLogin.
	tok, err := auth.NewToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "token generation failed")
		return
	}
	expires := time.Now().UTC().Add(sessionTTL)
	if err := s.Store.CreateSession(r.Context(), auth.HashToken(tok), user.ID, expires); err != nil {
		writeErr(w, http.StatusInternalServerError, "session creation failed")
		return
	}
	if err := s.Store.PruneSessions(r.Context()); err != nil {
		s.Log.Warn("prune sessions", "err", err)
	}
	// A completed flow resets the login-start lockout (mirrors local login).
	s.oidcThrottle.reset()
	s.Log.Info("oidc login ok", "username", user.Username, "role", user.Role, "provisioned", provisioned)
	s.writeAudit(r.Context(), user.Username, "auth.oidc_login", "user:"+user.Username,
		map[string]any{"role": user.Role, "provisioned": provisioned})
	http.Redirect(w, r, "/#oidc="+tok, http.StatusFound)
}

// oidcProvisionUser maps a verified identity to a local account:
//   - New username → create the account with the claim-mapped role and an
//     EMPTY password hash. An empty hash can never match bcrypt, so the
//     account cannot log in locally (401) until an admin explicitly sets a
//     password via user management. OIDC is the only way in by default.
//   - Existing username → role sync ONLY when ENCODE_OIDC_ADMIN_GROUP is
//     configured (admin iff the groups claim contains it, else viewer —
//     both directions). With no admin group configured the stored role is
//     kept: there is no mapping to sync from, and silently demoting an
//     existing admin would risk locking the operator out.
//
// Returns the account and whether it was just provisioned.
func (s *Server) oidcProvisionUser(ctx context.Context, ident *oidc.Identity) (*model.User, bool, error) {
	username, err := oidc.Username(ident.PreferredUsername, ident.Email, ident.Subject)
	if err != nil {
		return nil, false, err
	}
	role := s.Cfg.OIDC.RoleFor(ident.Groups)
	user, err := s.Store.UserByUsername(ctx, username)
	if err != nil {
		return nil, false, err
	}
	if user == nil {
		user, err = s.Store.CreateUser(ctx, username, "", role)
		if err != nil {
			// HA race: another replica provisioned the same username between
			// our lookup and insert. Re-read and continue as existing user.
			if !isUniqueConstraintErr(err) {
				return nil, false, err
			}
			user, err = oidcRereadUser(ctx, s.Store.UserByUsername, username)
			if err != nil {
				return nil, false, err
			}
		} else {
			return user, true, nil
		}
	}
	if s.Cfg.OIDC.AdminGroup != "" && user.Role != role {
		if err := s.Store.UpdateUserRole(ctx, user.ID, role); err != nil {
			return nil, false, err
		}
		s.Log.Info("oidc role synced", "username", user.Username, "role", role)
		user.Role = role
	}
	return user, false, nil
}

// oidcRereadUser re-reads the account after a lost provisioning race (another
// replica inserted the same username first). lookup is the store seam, kept
// as a parameter so the deleted-in-the-window case is unit-testable. The
// conflicting row may have been DELETED again in the window: (nil, nil) must
// become an explicit error, never a nil user that panics at user.ID.
func oidcRereadUser(ctx context.Context, lookup func(context.Context, string) (*model.User, error), username string) (*model.User, error) {
	user, err := lookup(ctx, username)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, fmt.Errorf("oidc account %q disappeared during the provisioning race", username)
	}
	return user, nil
}
