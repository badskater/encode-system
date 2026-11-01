package api

import (
	"net/http"
)

// ScanTrigger returns the scan-now trigger channel for the scanner loop to
// select on. The channel is buffered-1 and built in New; main passes it to
// scanner.RunLoop so POST /api/scan can request an out-of-cycle scan.
func (s *Server) ScanTrigger() <-chan struct{} { return s.scanTrigger }

// handleScanNow triggers an immediate scan of the scripts share by sending a
// non-blocking signal to the scanner loop's trigger channel. The response is
// always 202 {"triggered":true} — the scan is asynchronous and may coalesce
// with an already-pending trigger (buffered-1 channel: a second request while
// one is queued is absorbed, and the single resulting scan covers both).
//
// On a multi-replica (HA/Postgres) deployment, only the leader replica's
// scanner holds the advisory lock and consumes the channel. A trigger sent on
// a non-leader is a no-op (the channel is never drained) — the request still
// returns 202 because the operator's intent ("scan now") is honored by the
// leader on its own schedule regardless.
func (s *Server) handleScanNow(w http.ResponseWriter, r *http.Request) {
	select {
	case s.scanTrigger <- struct{}{}:
	default:
		// Channel buffer is full (a trigger is already queued): coalesce.
		// The pending scan will cover this request too.
	}
	s.audit(r, "scan.trigger", "scanner", nil)
	writeJSON(w, http.StatusAccepted, map[string]bool{"triggered": true})
}
