package api

import (
	"context"
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
	s.writeAudit(r.Context(), actor, action, object, detail)
}

// auditSystem records an audit event raised by a background loop (no HTTP
// request/session): the actor is "controller", matching the node name the
// controller-detected failure paths already use in notifications. Same
// fire-and-forget contract as audit.
func (s *Server) auditSystem(ctx context.Context, action, object string, detail any) {
	s.writeAudit(ctx, "controller", action, object, detail)
}

// writeAudit is the shared audit write path for request-scoped and
// system-scoped events. Detail is marshaled leniently and capped at 1024
// bytes — audit rows are forensic breadcrumbs, not data dumps. (The cap
// was 512 until P11: the settings.update detail carries ~25 secret-free
// booleans, and the ingest fields pushed the secret-change flags past
// 512 — truncation silently dropped smtp_password_changed, which the
// no-secrets-in-audit contract test pins. 1024 keeps every flag with
// headroom while staying bounded.)
func (s *Server) writeAudit(ctx context.Context, actor, action, object string, detail any) {
	detailStr := ""
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			detailStr = string(b)
			if len(detailStr) > 1024 {
				detailStr = detailStr[:1024] + "…"
			}
		}
	}
	if err := s.Store.AppendAudit(ctx, actor, action, object, detailStr); err != nil {
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
