package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// handlePrometheus serves GET /metrics in the Prometheus text exposition
// format (v0.0.4). Hand-rolled rather than pulling prometheus/client_golang:
// the series set is small and static, and the dependency footprint of the
// client library (protobuf, expfmt) outweighs a ~100-line writer.
//
// No auth: scrapers live on the management network and the endpoint exposes
// only aggregate operational numbers (queue depth, node liveness, terminal
// counters) — the same data the unauthenticated health endpoint plus the
// Stats page already surface to any admin. Series names use the encode_*
// prefix; labels are minimal (node name) to keep cardinality flat.
func (s *Server) handlePrometheus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var b strings.Builder

	// ---- queue depth (gauges) ----
	counts, err := s.Store.CountJobsByStatus(ctx)
	if err != nil {
		s.Log.Error("prometheus: job counts", "err", err)
		http.Error(w, "metrics unavailable", http.StatusInternalServerError)
		return
	}
	writeGauge(&b, "encode_jobs_pending", "Jobs waiting in the queue.", float64(counts[string(model.JobPending)]))
	writeGauge(&b, "encode_jobs_assigned", "Jobs assigned to a node but not yet running.", float64(counts[string(model.JobAssigned)]))
	writeGauge(&b, "encode_jobs_running", "Jobs currently encoding.", float64(counts[string(model.JobRunning)]))
	writeCounter(&b, "encode_jobs_done_total", "Jobs finished successfully since DB start.", float64(counts[string(model.JobDone)]))
	writeCounter(&b, "encode_jobs_failed_total", "Jobs failed since DB start.", float64(counts[string(model.JobFailed)]))
	writeCounter(&b, "encode_jobs_cancelled_total", "Jobs cancelled since DB start.", float64(counts[string(model.JobCancelled)]))

	// ---- node liveness (gauge with node label) ----
	nodes, err := s.Store.ListNodes(ctx)
	if err != nil {
		s.Log.Error("prometheus: nodes", "err", err)
		http.Error(w, "metrics unavailable", http.StatusInternalServerError)
		return
	}
	// Sort for stable exposition ordering (Prometheus doesn't require it,
	// but diffs between scrapes stay readable).
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	stale := s.Cfg.StaleAfter
	if stale <= 0 {
		stale = 2 * time.Minute
	}
	now := time.Now().UTC()
	b.WriteString("# HELP encode_node_online Whether the node heartbeated within the stale window (1=online, 0=stale/offline).\n")
	b.WriteString("# TYPE encode_node_online gauge\n")
	b.WriteString("# HELP encode_node_enabled Whether the node is enabled for dispatch (1/0).\n")
	b.WriteString("# TYPE encode_node_enabled gauge\n")
	b.WriteString("# HELP encode_node_active_jobs Jobs currently occupying slots on the node.\n")
	b.WriteString("# TYPE encode_node_active_jobs gauge\n")
	b.WriteString("# HELP encode_node_max_concurrent_jobs Configured job slots on the node.\n")
	b.WriteString("# TYPE encode_node_max_concurrent_jobs gauge\n")
	for _, n := range nodes {
		online := 0.0
		if n.LastSeen != nil && now.Sub(*n.LastSeen) <= stale {
			online = 1
		}
		label := fmt.Sprintf(`node=%q`, n.Name)
		fmt.Fprintf(&b, "encode_node_online{%s} %s\n", label, fmtFloat(online))
		en := 0.0
		if n.Enabled {
			en = 1
		}
		fmt.Fprintf(&b, "encode_node_enabled{%s} %s\n", label, fmtFloat(en))
		fmt.Fprintf(&b, "encode_node_active_jobs{%s} %s\n", label, fmtFloat(float64(n.ActiveJobs)))
		fmt.Fprintf(&b, "encode_node_max_concurrent_jobs{%s} %s\n", label, fmtFloat(float64(n.MaxConcurrentJobs)))
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(b.String()))
}

// writeGauge emits one gauge family (HELP, TYPE, sample).
func writeGauge(b *strings.Builder, name, help string, v float64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n%s %s\n", name, help, name, name, fmtFloat(v))
}

// writeCounter emits one counter family.
func writeCounter(b *strings.Builder, name, help string, v float64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n%s %s\n", name, help, name, name, fmtFloat(v))
}

// fmtFloat renders a float the way Prometheus expects: integers without a
// decimal point, no exponent notation for the magnitudes we produce.
func fmtFloat(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}
