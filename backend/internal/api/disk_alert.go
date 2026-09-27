package api

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/notify"
)

// diskAlertCooldown is how long one node stays quiet after a low-disk
// alert. A node hovering just under the threshold heartbeats every ~15s;
// without a cooldown it would spam Discord ~240×/hour.
const diskAlertCooldown = time.Hour

// diskGuardState tracks the last low-disk alert per node. In-memory only:
// a controller restart resets the windows, which costs at most one extra
// alert — acceptable for observability state.
type diskGuardState struct {
	mu   sync.Mutex
	last map[int64]time.Time
}

// shouldAlert reports whether node id may fire a low-disk alert at time now
// (i.e. no alert within the last cooldown) and records the decision when it
// returns true. One call site decides, so checking and recording are atomic
// with respect to concurrent heartbeats from the same node.
func (g *diskGuardState) shouldAlert(nodeID int64, now time.Time, cooldown time.Duration) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if last, ok := g.last[nodeID]; ok && now.Sub(last) < cooldown {
		return false
	}
	g.last[nodeID] = now
	return true
}

// checkDisk implements the settings.DiskAlertGB contract on one heartbeat:
//   - threshold 0 (default): disabled — no alert, no drain, old behavior.
//   - free < threshold: fire an Alert through the configured notifier (with
//     the per-node cooldown) and report low=true so the caller soft-drains
//     the node (no new assignments until disk recovers). Running jobs are
//     NOT cancelled — they may still fit; a full disk fails the encode and
//     the retry lands on another node.
//   - free >= threshold: low=false, assignment proceeds normally.
//
// The agent's DiskFreeGB measures the drive holding its work dirs (the
// scripts/release share), which is exactly what fills up during encodes.
func (s *Server) checkDisk(ctx context.Context, node *model.Node, hb *model.Heartbeat) (low bool) {
	threshold := s.currentSettings(ctx).DiskAlertGB
	if threshold <= 0 || hb.Metrics == nil || hb.Metrics.DiskFreeGB <= 0 {
		// Disabled, or the node never reported disk data (old agent /
		// non-Windows collector miss): treat as healthy rather than
		// draining the fleet on missing telemetry.
		return false
	}
	if hb.Metrics.DiskFreeGB >= threshold {
		return false
	}
	if s.diskGuard.shouldAlert(node.ID, time.Now().UTC(), diskAlertCooldown) {
		content := fmt.Sprintf(
			"💾 **Low disk space** — node `%s`\nfree: %d GB (alert threshold %d GB)\nNew jobs are held on this node until space recovers; running jobs continue.",
			node.Name, hb.Metrics.DiskFreeGB, threshold)
		if s.Notifier != nil {
			s.Notifier.Alert(ctx, content)
		} else {
			st := s.currentSettings(ctx)
			notify.NewDiscordWithLink(st.DiscordWebhook, st.ControllerURL, s.Log).Alert(ctx, content)
		}
		s.Log.Warn("low disk space on node", "node", node.Name,
			"free_gb", hb.Metrics.DiskFreeGB, "threshold_gb", threshold)
	}
	return true
}
