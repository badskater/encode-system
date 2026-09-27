package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/badskater/encode-system/backend/internal/auth"
	"github.com/badskater/encode-system/backend/internal/model"
)

// handleCreateAPIToken issues a scoped token for external automation.
// The plaintext token is returned exactly once — only its hash is stored.
// Admin sessions only (the route is wrapped in withAdmin, and token
// authentication is explicitly rejected below so a leaked read/admin token
// cannot mint more tokens).
func (s *Server) handleCreateAPIToken(w http.ResponseWriter, r *http.Request) {
	if isAPITokenAuth(r) {
		writeErr(w, http.StatusForbidden, "api tokens cannot create api tokens")
		return
	}
	var req struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name required")
		return
	}
	if req.Scope == "" {
		req.Scope = model.TokenScopeRead
	}
	if req.Scope != model.TokenScopeAdmin && req.Scope != model.TokenScopeRead {
		writeErr(w, http.StatusBadRequest, "scope must be admin or read")
		return
	}
	if len(req.Name) > 64 {
		writeErr(w, http.StatusBadRequest, "name too long (max 64)")
		return
	}
	plaintext, err := auth.NewToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "generate token")
		return
	}
	tok := &model.APIToken{Name: req.Name, Scope: req.Scope, TokenHash: auth.HashToken(plaintext)}
	if err := s.Store.CreateAPIToken(r.Context(), tok); err != nil {
		writeErr(w, http.StatusConflict, "token name taken")
		return
	}
	s.audit(r, "token.create", auditObject("token", tok.ID), map[string]any{"name": tok.Name, "scope": tok.Scope})
	s.Log.Info("api token created", "name", tok.Name, "scope", tok.Scope)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": tok.ID, "name": tok.Name, "scope": tok.Scope, "token": plaintext,
	})
}

// handleListAPITokens returns all tokens (hashes are json:"-"-stripped).
func (s *Server) handleListAPITokens(w http.ResponseWriter, r *http.Request) {
	if isAPITokenAuth(r) {
		writeErr(w, http.StatusForbidden, "api tokens cannot list api tokens")
		return
	}
	tokens, err := s.Store.ListAPITokens(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list tokens")
		return
	}
	writeJSON(w, http.StatusOK, tokens)
}

// handleDeleteAPIToken revokes a token. Idempotent: unknown ids 404 so the
// UI can refresh honestly, but a double-delete of a known-stale id is fine.
func (s *Server) handleDeleteAPIToken(w http.ResponseWriter, r *http.Request) {
	if isAPITokenAuth(r) {
		writeErr(w, http.StatusForbidden, "api tokens cannot delete api tokens")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad token id")
		return
	}
	tokens, err := s.Store.ListAPITokens(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list tokens")
		return
	}
	found := false
	name := ""
	for _, t := range tokens {
		if t.ID == id {
			found = true
			name = t.Name
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "token not found")
		return
	}
	if err := s.Store.DeleteAPIToken(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "delete token")
		return
	}
	s.audit(r, "token.delete", auditObject("token", id), map[string]any{"name": name})
	s.Log.Info("api token deleted", "name", name, "id", id)
	w.WriteHeader(http.StatusNoContent)
}

// apiTokenCtxKey marks a request as authenticated by API token (vs an
// interactive session) and carries the token for scope checks.
type apiTokenCtxKey struct{}

// isAPITokenAuth reports whether the request authenticated via API token.
func isAPITokenAuth(r *http.Request) bool {
	return r.Context().Value(apiTokenCtxKey{}) != nil
}

// apiTokenFromCtx returns the authenticated token, or nil for session auth.
func apiTokenFromCtx(r *http.Request) *model.APIToken {
	if v := r.Context().Value(apiTokenCtxKey{}); v != nil {
		return v.(*model.APIToken)
	}
	return nil
}

// authenticateBearer resolves a Bearer credential to either a session or an
// API token. Sessions win (checked first) and slide; tokens are scope-
// checked by the caller. Returns (session-or-nil, token-or-nil, ok).
func (s *Server) authenticateBearer(r *http.Request) (*model.Session, *model.APIToken, bool) {
	tok := bearer(r)
	if tok == "" {
		return nil, nil, false
	}
	hash := auth.HashToken(tok)
	if sess, err := s.Store.SessionByTokenHash(r.Context(), hash); err == nil && sess != nil {
		if err := s.Store.SlideSession(r.Context(), hash, time.Now().UTC().Add(sessionTTL)); err != nil {
			s.Log.Warn("slide session", "err", err)
		}
		return sess, nil, true
	}
	if apiTok, err := s.Store.APITokenByHash(r.Context(), hash); err == nil && apiTok != nil {
		return nil, apiTok, true
	}
	return nil, nil, false
}
