package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/badskater/encode-system/backend/internal/model"
)

// shareOut is the API-facing share: identical to model.Share but with the
// password replaced by a has_password flag (plaintext never leaves the
// controller).
type shareOut struct {
	*model.Share
	HasPassword bool `json:"has_password"`
}

// MarshalJSON implements the mask: embed Share's fields, drop Password
// (already `json:"-"` on the model) and add has_password.
func (o shareOut) MarshalJSON() ([]byte, error) {
	type alias model.Share
	return json.Marshal(struct {
		alias
		HasPassword bool `json:"has_password"`
	}{alias(*o.Share), o.HasPassword})
}

func maskShare(sh *model.Share) shareOut {
	return shareOut{Share: sh, HasPassword: sh.Password != ""}
}

// shareReq is the create/update request body. model.Share's Password is
// json:"-" (write-only, never marshaled OUT), so the handler decodes into
// this shadow struct to read it from the request IN.
type shareReq struct {
	model.Share
	Password string `json:"password,omitempty"`
}

// handleListShares serves GET /api/shares (admin): all shares, passwords
// masked.
func (s *Server) handleListShares(w http.ResponseWriter, r *http.Request) {
	shares, err := s.Store.ListShares(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list shares")
		return
	}
	out := make([]shareOut, 0, len(shares))
	for _, sh := range shares {
		out = append(out, maskShare(sh))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCreateShare serves POST /api/shares (admin). Body = model.Share
// with a plaintext password; stored encrypted.
func (s *Server) handleCreateShare(w http.ResponseWriter, r *http.Request) {
	var req shareReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	sh := req.Share
	sh.Password = req.Password
	if err := validateShare(&sh); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := s.Store.CreateShare(r.Context(), &sh)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "create share: "+err.Error())
		return
	}
	s.audit(r, "share.create", auditObject("share", created.ID),
		map[string]any{"name": created.Name, "kind": created.Kind, "role": created.Role})
	writeJSON(w, http.StatusCreated, maskShare(created))
}

// handleGetShare serves GET /api/shares/{id}.
func (s *Server) handleGetShare(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid share id")
		return
	}
	sh, err := s.Store.GetShare(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "share not found")
		return
	}
	writeJSON(w, http.StatusOK, maskShare(sh))
}

// handleUpdateShare serves PUT /api/shares/{id}. Password empty = keep the
// stored credential; non-empty = replace. Plaintext is never echoed back.
func (s *Server) handleUpdateShare(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid share id")
		return
	}
	var req shareReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	sh := req.Share
	sh.Password = req.Password
	sh.ID = id
	if err := validateShare(&sh); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.UpdateShare(r.Context(), &sh); err != nil {
		writeErr(w, http.StatusNotFound, "update share")
		return
	}
	updated, err := s.Store.GetShare(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "reload share")
		return
	}
	s.audit(r, "share.update", auditObject("share", id),
		map[string]any{"name": updated.Name, "kind": updated.Kind, "role": updated.Role,
			"password_changed": sh.Password != ""})
	writeJSON(w, http.StatusOK, maskShare(updated))
}

// handleDeleteShare serves DELETE /api/shares/{id}.
func (s *Server) handleDeleteShare(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid share id")
		return
	}
	sh, err := s.Store.GetShare(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "share not found")
		return
	}
	if err := s.Store.DeleteShare(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "delete share")
		return
	}
	s.audit(r, "share.delete", auditObject("share", id), map[string]any{"name": sh.Name})
	w.WriteHeader(http.StatusNoContent)
}

// validateShare checks the kind/role enums and the per-kind required
// fields, returning a user-facing message.
func validateShare(sh *model.Share) error {
	switch sh.Kind {
	case model.ShareNFS:
		if sh.Server == "" || sh.Path == "" {
			return errSettings("nfs share needs server and path")
		}
	case model.ShareSMB:
		if sh.Server == "" || sh.Path == "" {
			return errSettings("smb share needs server and path (share name)")
		}
	case model.ShareS3:
		if sh.Path == "" {
			return errSettings("s3 share needs path (bucket)")
		}
		if sh.Endpoint == "" && sh.Server == "" {
			return errSettings("s3 share needs endpoint or server")
		}
	default:
		return errSettings("kind must be nfs, smb or s3")
	}
	switch sh.Role {
	case model.ShareRoleScripts, model.ShareRoleRelease:
	default:
		return errSettings("role must be scripts or release")
	}
	if sh.Name == "" {
		return errSettings("name required")
	}
	return nil
}
