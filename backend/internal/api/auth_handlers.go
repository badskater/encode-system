package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/badskater/encode-system/backend/internal/auth"
	"github.com/badskater/encode-system/backend/internal/model"
)

// sessionTTL is the sliding expiry for management sessions. Each successful
// authenticated request renews it; an idle session dies after this window.
const sessionTTL = 24 * time.Hour

// loginThrottle is a deliberately simple in-memory lockout: after
// maxFailedLogins consecutive bad logins the endpoint sleeps for lockoutDur.
// The plane has few users; persistence of the counter would be over-engineering.
type loginThrottle struct {
	mu          sync.Mutex
	failures    int
	lockedUntil time.Time
}

const maxFailedLogins = 5
const lockoutDur = 30 * time.Second

func (t *loginThrottle) allow() (bool, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if time.Now().Before(t.lockedUntil) {
		return false, time.Until(t.lockedUntil)
	}
	return true, 0
}

func (t *loginThrottle) fail() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failures++
	if t.failures >= maxFailedLogins {
		t.lockedUntil = time.Now().Add(lockoutDur)
		t.failures = 0
	}
}

func (t *loginThrottle) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failures = 0
}

// ---------- Login / session handlers ----------

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLogin verifies credentials against the stored bcrypt hash and issues
// a session token. The response token is the Bearer credential for /api/*.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Username == "" {
		writeErr(w, http.StatusBadRequest, "username is required")
		return
	}
	// No empty-password fast path: an empty password is simply wrong and
	// takes the same bcrypt-comparison path, keeping every bad-credential
	// response shape identical (401).
	if ok, wait := s.throttle.allow(); !ok {
		writeErr(w, http.StatusTooManyRequests,
			"too many failed logins; retry in "+wait.Round(time.Second).String())
		return
	}

	user, err := s.Store.UserByUsername(r.Context(), req.Username)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "user lookup failed")
		return
	}
	// Constant work per attempt whether or not the user exists, so account
	// enumeration does not change response timing in an obvious way.
	hash := []byte("")
	if user != nil {
		hash = []byte(user.PasswordHash)
	}
	if user == nil || user.Role != "admin" || bcrypt.CompareHashAndPassword(hash, []byte(req.Password)) != nil {
		s.throttle.fail()
		s.Log.Warn("failed login", "username", req.Username)
		writeErr(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	s.throttle.reset()

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
	s.Log.Info("login ok", "username", user.Username)
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      tok,
		"username":   user.Username,
		"expires_at": expires,
	})
}

// handleLogout revokes the presented session. Idempotent by design.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if tok := bearer(r); tok != "" {
		_ = s.Store.DeleteSession(r.Context(), auth.HashToken(tok))
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMe returns the current session (used by the UI to validate a stored
// token and show who is logged in). Requires a valid session.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromCtx(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"username":   sess.Username,
		"expires_at": sess.ExpiresAt,
	})
}

// ---------- Password management ----------

// minPasswordLen is the floor for user-chosen passwords. Deliberately modest:
// this is a single-admin management plane on a trusted LAN, and the login
// endpoint already throttles brute force.
const minPasswordLen = 10

type changePasswordRequest struct {
	Current string `json:"current_password"`
	New     string `json:"new_password"`
}

// handleChangePassword lets the logged-in admin rotate their password without
// touching the controller's environment. Requires the CURRENT password,
// enforces a minimum length, and revokes every other session so stale or
// stolen tokens die immediately. The env-supplied password (if any) becomes
// irrelevant once the account exists — the hash in the database is the only
// source of truth.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromCtx(r)
	var req changePasswordRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Current == "" || req.New == "" {
		writeErr(w, http.StatusBadRequest, "current_password and new_password are required")
		return
	}
	if len(req.New) < minPasswordLen {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("new password must be at least %d characters", minPasswordLen))
		return
	}
	if req.New == req.Current {
		writeErr(w, http.StatusBadRequest, "new password must differ from the current one")
		return
	}

	user, err := s.Store.UserByUsername(r.Context(), sess.Username)
	if err != nil || user == nil {
		writeErr(w, http.StatusInternalServerError, "user lookup failed")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Current)) != nil {
		s.throttle.fail() // wrong current password participates in the lockout
		s.Log.Warn("password change rejected: wrong current password", "username", user.Username)
		writeErr(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.New), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "hash new password")
		return
	}
	if err := s.Store.UpdateUserPassword(r.Context(), user.ID, string(hash)); err != nil {
		writeErr(w, http.StatusInternalServerError, "update password")
		return
	}
	// Revoke all OTHER sessions (the caller keeps theirs).
	present := bearer(r)
	if present != "" {
		if err := s.Store.DeleteUserSessions(r.Context(), user.ID, auth.HashToken(present)); err != nil {
			s.Log.Warn("revoke sessions after password change", "err", err)
		}
	}
	s.Log.Info("admin password changed", "username", user.Username)
	writeJSON(w, http.StatusOK, map[string]string{"status": "password updated"})
}

// ---------- Middleware ----------

type sessionCtxKey struct{}

func sessionFromCtx(r *http.Request) *model.Session {
	if v := r.Context().Value(sessionCtxKey{}); v != nil {
		return v.(*model.Session)
	}
	return nil
}

// withAdmin requires a valid management session (Bearer token). Sessions are
// looked up by hash and renewed on each successful request (sliding expiry).
func (s *Server) withAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			writeErr(w, http.StatusUnauthorized, "not authenticated — log in first")
			return
		}
		hash := auth.HashToken(tok)
		sess, err := s.Store.SessionByTokenHash(r.Context(), hash)
		if err != nil || sess == nil {
			writeErr(w, http.StatusUnauthorized, "invalid or expired session")
			return
		}
		// Sliding expiry: keep active sessions alive.
		if err := s.Store.SlideSession(r.Context(), hash, time.Now().UTC().Add(sessionTTL)); err != nil {
			s.Log.Warn("slide session", "err", err)
		}
		h(w, contextWithSession(r, sess))
	}
}

func contextWithSession(r *http.Request, sess *model.Session) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), sessionCtxKey{}, sess))
}
