package api

import (
	"context"
	"encoding/json"
	"fmt"
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

// auditListKeep is the maximum number of list entries an audit detail carries
// before summarizing (see summarizeAuditList).
const auditListKeep = 20

// auditNameCap is the maximum rune length of a torrent name embedded in an
// ingest audit detail (see capAuditName).
const auditNameCap = 200

// summarizeAuditList caps a string list embedded in an audit detail: lists
// longer than keep become the first keep entries plus a "...and N more"
// summary entry. writeAudit's 1024-byte cap would otherwise truncate the
// marshaled JSON mid-list, dropping the key fields that follow it (a
// backup.offsite prune of 364 names marshaled to ~10KB). The full list stays
// in the structured logs; the audit row keeps a bounded, parseable summary.
func summarizeAuditList(names []string, keep int) []string {
	if len(names) <= keep {
		return names
	}
	out := make([]string, 0, keep+1)
	out = append(out, names[:keep]...)
	out = append(out, fmt.Sprintf("...and %d more", len(names)-keep))
	return out
}

// capAuditName caps a torrent name for an audit detail at max runes (plus an
// ellipsis when cut). A 1024-rune name marshaled to ~1.1KB and pushed series
// and staged_path past the 1024-byte detail cap. The FULL name stays in the
// ingest ledger and the structured log line — the audit row is a breadcrumb.
func capAuditName(name string, max int) string {
	r := []rune(name)
	if len(r) <= max {
		return name
	}
	return string(r[:max]) + "…"
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
