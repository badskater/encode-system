package api

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// auditObject builds the standard "kind:id" object label for audit rows.
func auditObject(kind string, id int64) string {
	return kind + ":" + strconv.FormatInt(id, 10)
}

// audit records one audit event for an admin request. Fire-and-forget:
// audit write failures are logged but never fail the audited operation —
// losing an audit row is bad, bricking the management API over it is worse.
// The actor is the session username; detail is marshaled leniently (a
// non-marshalable value degrades to "", not an error).
func (s *Server) audit(r *http.Request, action, object string, detail any) {
	actor := "unknown"
	if sess := sessionFromCtx(r); sess != nil && sess.Username != "" {
		actor = sess.Username
	} else if tok := apiTokenFromCtx(r); tok != nil {
		actor = "api-token:" + tok.Name
	}
	detailStr := ""
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			// Cap the detail snippet: audit rows are forensic breadcrumbs,
			// not data dumps — 512 bytes keeps a table scan cheap.
			detailStr = string(b)
			if len(detailStr) > 512 {
				detailStr = detailStr[:512] + "…"
			}
		}
	}
	if err := s.Store.AppendAudit(r.Context(), actor, action, object, detailStr); err != nil {
		s.Log.Warn("audit write failed", "err", err, "action", action)
	}
}

// handleListAudit serves GET /api/audit?limit=N (default 200, max 1000).
func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			writeErr(w, http.StatusBadRequest, "limit must be 1-1000")
			return
		}
		limit = n
	}
	events, err := s.Store.ListAudit(r.Context(), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list audit")
		return
	}
	writeJSON(w, http.StatusOK, events)
}
