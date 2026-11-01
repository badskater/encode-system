package api

import (
	"context"
	"fmt"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/notify"
	"github.com/badskater/encode-system/backend/internal/store"
)

// nodeAlertInterval is the tick cadence for the node-status alert loop. 30s
// matches the scanner cadence and is well within the StaleAfter window (45s
// default) so a transition is detected within one tick of it happening.
const nodeAlertInterval = 30 * time.Second

// nodeState is one node's liveness snapshot as computed by the alert loop.
type nodeState struct {
	online   bool
	name     string
	lastSeen *time.Time
}

// transition is a detected online↔offline state change for one node.
type transition struct {
	nodeID   int64
	name     string
	to       string // "online" | "offline"
	lastSeen *time.Time
}

// nodeAlertState holds the in-memory baseline of node online/offline states
// between ticks. A controller restart resets it — the first tick after a
// restart seeds silently (no notification storm) by calling diffNodeStates
// with an empty prev map (new nodes never produce transitions).
type nodeAlertState struct {
	prev map[int64]bool // nodeID → online (true) / offline (false)
}

// StartNodeAlertLoop launches the node online/offline alert loop as a
// background goroutine bound to ctx. Each tick (30s) lists all nodes, computes
// online with the same StaleAfter rule as handleListNodes, and diffs against
// an in-memory baseline. On the first tick (empty baseline) the state is
// seeded SILENTLY — no notifications fire, so a controller restart does not
// produce a storm of "node went offline" alerts for nodes that were already
// stale before the restart. On later ticks, real transitions send Discord
// alerts: "🔴 Node <name> went offline (last seen <RFC3339>)" and
// "🟢 Node <name> is back online".
//
// Alerts fire ONLY when settings.NotifyNodeStatus is true AND
// settings.DiscordWebhook is non-empty — both are read live each tick so a
// Settings-page toggle applies without a restart. When either is off the tick
// still updates the baseline (so transitions are not missed while disabled and
// then re-enabled mid-transition), but no alert is sent.
//
// HA: on Postgres the loop body runs under store.RunLoopSupervisor with
// LockKeyNodeAlert so exactly one replica alerts per transition (two replicas
// would double-post). On SQLite the supervisor is a transparent always-locked
// wrapper. ctx-cancel/SIGTERM stop is preserved.
func (s *Server) StartNodeAlertLoop(ctx context.Context) {
	go store.RunLoopSupervisor(ctx, s.Store, s.Log, store.LockKeyNodeAlert, "node-alert", func(ctx context.Context) {
		s.runNodeAlertLoop(ctx)
	})
}

func (s *Server) runNodeAlertLoop(ctx context.Context) {
	state := &nodeAlertState{prev: map[int64]bool{}}
	tick := time.NewTicker(nodeAlertInterval)
	defer tick.Stop()
	// Run one tick immediately on startup so the baseline is seeded without
	// waiting for the first 30s interval.
	s.nodeAlertTickWithState(ctx, state)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.nodeAlertTickWithState(ctx, state)
		}
	}
}

// nodeAlertTick runs one alert-loop pass using a fresh internal state. It is
// the test entry point (the loop's state is internal, but tests can exercise
// a single tick's detection + notification logic by calling this repeatedly
// — the state is preserved on the server's nodeAlertState).
func (s *Server) nodeAlertTick(ctx context.Context) {
	if s.nodeAlert == nil {
		s.nodeAlert = &nodeAlertState{prev: map[int64]bool{}}
	}
	s.nodeAlertTickWithState(ctx, s.nodeAlert)
}

// nodeAlertTickWithState performs one node-status diff + alert pass. It reads
// the live settings each tick, lists all nodes, computes online with the same
// StaleAfter rule as handleListNodes, diffs against prev, and sends Discord
// alerts on transitions. The first tick (empty prev) seeds silently.
func (s *Server) nodeAlertTickWithState(ctx context.Context, state *nodeAlertState) {
	st := s.currentSettings(ctx)
	alertsEnabled := st.NotifyNodeStatus && st.DiscordWebhook != ""

	nodes, err := s.Store.ListNodes(ctx)
	if err != nil {
		s.Log.Warn("node-alert: list nodes failed", "err", err)
		return
	}
	now := time.Now().UTC()
	cur := make(map[int64]nodeState, len(nodes))
	for _, n := range nodes {
		online := n.LastSeen != nil && now.Sub(*n.LastSeen) < s.Cfg.StaleAfter
		cur[n.ID] = nodeState{online: online, name: n.Name, lastSeen: n.LastSeen}
	}

	transitions := diffNodeStates(state.prev, cur)

	// Update the baseline for the next tick regardless of whether alerts
	// are enabled — this prevents a missed transition when alerts are off
	// and then turned on mid-transition (the baseline would otherwise show
	// the pre-disable state and fire a spurious transition on re-enable).
	newPrev := make(map[int64]bool, len(cur))
	for id, ns := range cur {
		newPrev[id] = ns.online
	}
	state.prev = newPrev

	if !alertsEnabled {
		return
	}
	for _, tr := range transitions {
		s.sendNodeStatusAlert(ctx, st, tr)
	}
}

// sendNodeStatusAlert formats and sends one transition alert via the
// configured notifier (injected for tests, live Discord for production).
func (s *Server) sendNodeStatusAlert(ctx context.Context, st *model.Settings, tr transition) {
	var content string
	switch tr.to {
	case "offline":
		lastSeenStr := "never"
		if tr.lastSeen != nil {
			lastSeenStr = tr.lastSeen.Format(time.RFC3339)
		}
		content = fmt.Sprintf("🔴 Node `%s` went offline (last seen %s)", tr.name, lastSeenStr)
	case "online":
		content = fmt.Sprintf("🟢 Node `%s` is back online", tr.name)
	default:
		return
	}
	if s.Notifier != nil {
		s.Notifier.Alert(ctx, content)
		return
	}
	notify.NewDiscordWithLink(st.DiscordWebhook, st.ControllerURL, s.Log).Alert(ctx, content)
	s.Log.Info("node status alert", "node", tr.name, "transition", tr.to)
}

// diffNodeStates computes the set of online↔offline transitions between a
// previous baseline (prev: nodeID → online bool) and the current snapshot
// (cur: nodeID → nodeState). A transition is emitted ONLY when a node existed
// in prev AND its online state changed in cur — a node new to cur (not in
// prev) produces no transition (the seed tick sets the baseline silently).
// A node that disappeared from cur (deleted) also produces no transition (it
// is simply dropped from the baseline).
//
// This is a pure function for unit testing.
func diffNodeStates(prev map[int64]bool, cur map[int64]nodeState) []transition {
	var out []transition
	for id, wasOnline := range prev {
		ns, ok := cur[id]
		if !ok {
			continue // node deleted — drop from baseline, no transition
		}
		if ns.online != wasOnline {
			to := "offline"
			if ns.online {
				to = "online"
			}
			out = append(out, transition{
				nodeID:   id,
				name:     ns.name,
				to:       to,
				lastSeen: ns.lastSeen,
			})
		}
	}
	return out
}
