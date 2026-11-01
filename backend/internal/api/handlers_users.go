package api

import (
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/badskater/encode-system/backend/internal/model"
)

// ---------- Viewer middleware ----------

// withAnyUser allows any authenticated management account (admin OR viewer)
// on session auth, and maps API-token scope to an effective role:
//   - read-scope tokens → viewer-equivalent (GET/HEAD only, same as today)
//   - write-scope (admin) tokens → admin-equivalent
//
// Used on GET/HEAD routes that viewers should see. POST/PUT/PATCH/DELETE
// routes stay behind withAdmin. Management surfaces (/api/users, /api/tokens,
// /api/audit, /api/backup*) remain admin-only even for GET.
func (s *Server) withAnyUser(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, apiTok, ok := s.authenticateBearer(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "invalid or expired session")
			return
		}
		if apiTok != nil {
			// Read-scope tokens are GET/HEAD only (same as withAdmin today).
			if apiTok.Scope == model.TokenScopeRead && r.Method != http.MethodGet && r.Method != http.MethodHead {
				writeErr(w, http.StatusForbidden, "read-scope token cannot perform "+r.Method)
				return
			}
			if err := s.Store.TouchAPIToken(r.Context(), apiTok.ID); err != nil {
				s.Log.Warn("touch api token", "err", err)
			}
			// API tokens don't carry a session; treat admin-scope as admin
			// and read-scope as viewer for any downstream role checks. The
			// context gets an API-token marker, not a session, so handlers
			// that check sessionFromCtx for admin-only mutations (user CRUD)
			// see nil and reject — which is correct: tokens don't manage users.
			h(w, contextWithAPIToken(r, apiTok))
			return
		}
		// Session auth: both admin and viewer sessions pass through.
		h(w, contextWithSession(r, sess))
	}
}

// sessionIsAdmin reports whether the request's session belongs to an admin.
// API-token-authenticated requests return false (no session); withAdmin
// already enforces admin sessions before reaching handlers, so this is only
// used by handlers that need to distinguish session vs token auth for actor
// logging. Kept for future use.
func sessionIsAdmin(r *http.Request) bool {
	sess := sessionFromCtx(r)
	return sess != nil && sess.Role == model.RoleAdmin
}

var _ = sessionIsAdmin // retained for future actor-distinguishing handlers

// ---------- User CRUD handlers ----------

type createUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

// handleListUsers: GET /api/users — admin-only. Returns id, username, role,
// created_at for every account. PasswordHash is never included (json:"-").
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.Store.Users(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list users")
		return
	}
	writeJSON(w, http.StatusOK, users)
}

// handleCreateUser: POST /api/users — admin-only. Creates a new account
// with a bcrypt-hashed password and a validated role (admin|viewer).
// Duplicate username → 409. Min password length follows change-password
// validation (minPasswordLen).
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" {
		writeErr(w, http.StatusBadRequest, "username is required")
		return
	}
	if req.Role == "" {
		req.Role = model.RoleViewer
	}
	if req.Role != model.RoleAdmin && req.Role != model.RoleViewer {
		writeErr(w, http.StatusBadRequest, "role must be admin or viewer")
		return
	}
	if len(req.Password) < minPasswordLen {
		writeErr(w, http.StatusBadRequest, "password must be at least "+strconv.Itoa(minPasswordLen)+" characters")
		return
	}
	if len([]byte(req.Password)) > maxPasswordBytes {
		writeErr(w, http.StatusBadRequest, "password must be at most "+strconv.Itoa(maxPasswordBytes)+" bytes (bcrypt truncates beyond that)")
		return
	}
	// Uniqueness: check first so we return 409 instead of a generic error.
	existing, err := s.Store.UserByUsername(r.Context(), req.Username)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "user lookup failed")
		return
	}
	if existing != nil {
		writeErr(w, http.StatusConflict, "username already exists")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "hash password")
		return
	}
	user, err := s.Store.CreateUser(r.Context(), req.Username, string(hash), req.Role)
	if err != nil {
		// Race: another request created the username between our check and
		// insert. The UNIQUE constraint fires; treat it as 409.
		if isUniqueConstraintErr(err) {
			writeErr(w, http.StatusConflict, "username already exists")
			return
		}
		writeErr(w, http.StatusInternalServerError, "create user")
		return
	}
	s.audit(r, "user.create", auditObject("user", user.ID), map[string]string{"username": user.Username, "role": user.Role})
	actor := "api-token"
	if sess := sessionFromCtx(r); sess != nil {
		actor = sess.Username
	}
	s.Log.Info("user created", "username", user.Username, "role", user.Role, "actor", actor)
	// Don't echo the hash (json:"-"); return the public shape.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         user.ID,
		"username":   user.Username,
		"role":       user.Role,
		"created_at": user.CreatedAt,
	})
}

// handleDeleteUser: DELETE /api/users/{id} — admin-only.
// Guards: self-delete → 409; last-admin delete → 409.
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid user id")
		return
	}
	target, err := s.Store.UserByID(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "user lookup failed")
		return
	}
	if target == nil {
		writeErr(w, http.StatusNotFound, "user not found")
		return
	}
	// Self-delete guard.
	sess := sessionFromCtx(r)
	if sess != nil && sess.UserID == id {
		writeErr(w, http.StatusConflict, "cannot delete your own account")
		return
	}
	// Last-admin guard: don't remove the final admin account.
	if target.Role == model.RoleAdmin {
		count, err := s.Store.CountUsersByRole(r.Context(), model.RoleAdmin)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "count admins")
			return
		}
		if count <= 1 {
			writeErr(w, http.StatusConflict, "cannot delete the last admin account")
			return
		}
	}
	if err := s.Store.DeleteUser(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "delete user")
		return
	}
	s.audit(r, "user.delete", auditObject("user", id), map[string]string{"username": target.Username, "role": target.Role})
	actor := "api-token"
	if sess := sessionFromCtx(r); sess != nil {
		actor = sess.Username
	}
	s.Log.Info("user deleted", "username", target.Username, "actor", actor)
	w.WriteHeader(http.StatusNoContent)
}

// handleResetUserPassword: PUT /api/users/{id}/password — admin resets
// another user's password. Does not require the current password (admin
// reset). Revokes all of the target's sessions so stale tokens die.
type resetPasswordRequest struct {
	Password string `json:"password"`
}

func (s *Server) handleResetUserPassword(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid user id")
		return
	}
	var req resetPasswordRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.Password) < minPasswordLen {
		writeErr(w, http.StatusBadRequest, "password must be at least "+strconv.Itoa(minPasswordLen)+" characters")
		return
	}
	if len([]byte(req.Password)) > maxPasswordBytes {
		writeErr(w, http.StatusBadRequest, "password must be at most "+strconv.Itoa(maxPasswordBytes)+" bytes (bcrypt truncates beyond that)")
		return
	}
	target, err := s.Store.UserByID(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "user lookup failed")
		return
	}
	if target == nil {
		writeErr(w, http.StatusNotFound, "user not found")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "hash password")
		return
	}
	if err := s.Store.UpdateUserPassword(r.Context(), id, string(hash)); err != nil {
		writeErr(w, http.StatusInternalServerError, "update password")
		return
	}
	// Revoke ALL of the target's sessions — a password reset invalidates
	// every prior token (no keepTokenHash).
	if err := s.Store.DeleteUserSessions(r.Context(), id, ""); err != nil {
		s.Log.Warn("revoke sessions after admin password reset", "err", err, "user", target.Username)
	}
	s.audit(r, "user.password_reset", auditObject("user", id), map[string]string{"username": target.Username})
	actor := "api-token"
	if sess := sessionFromCtx(r); sess != nil {
		actor = sess.Username
	}
	s.Log.Info("user password reset by admin", "username", target.Username, "actor", actor)
	writeJSON(w, http.StatusOK, map[string]string{"status": "password updated"})
}

// isUniqueConstraintErr reports whether an error from an INSERT is a
// UNIQUE constraint violation (SQLite or Postgres). Used to turn a race
// between check-then-insert into a clean 409.
func isUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// SQLite: "UNIQUE constraint failed: users.username"
	// Postgres: "duplicate key value violates unique constraint"
	return strings.Contains(msg, "UNIQUE constraint") ||
		strings.Contains(msg, "duplicate key value")
}
