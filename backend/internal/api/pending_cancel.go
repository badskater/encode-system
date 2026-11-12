package api

// Pending-cancel queue (F5). The stall watchdog fails a wedged job
// server-side, but the agent on an online node keeps the process running —
// and an auto-retry re-dispatched to the SAME node would be deduped by the
// agent's job registry against the stuck run, burning retries for nothing.
// The queue carries job IDs to the node's next heartbeat reply as a
// "cancel" instruction; the agent kills the process tree, deregisters the
// job, and frees the slot without sending a completion report (the
// controller already finalized the job; late reports are status-guarded).
//
// The queue is per-node, mutex-guarded, and drained exactly once into the
// reply. Entries are also cleared when a node goes offline or is deleted
// (node-alert loop) and when a node re-registers (pairing), so unreachable
// nodes never accumulate unbounded state.

// queuePendingCancel schedules jobID for cancellation on nodeID's next
// heartbeat. Duplicate IDs are ignored (two watchdog passes over one stale
// snapshot must not double-queue).
func (s *Server) queuePendingCancel(nodeID, jobID int64) {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	for _, id := range s.pendingCancels[nodeID] {
		if id == jobID {
			return
		}
	}
	s.pendingCancels[nodeID] = append(s.pendingCancels[nodeID], jobID)
}

// drainPendingCancels takes the node's queued job IDs, clearing the queue
// (IDs are sent exactly once). Returns nil when nothing is queued.
func (s *Server) drainPendingCancels(nodeID int64) []int64 {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	ids := s.pendingCancels[nodeID]
	delete(s.pendingCancels, nodeID)
	return ids
}

// clearPendingCancels drops a node's queued IDs without sending them —
// used when the node goes offline, is deleted, or re-registers.
func (s *Server) clearPendingCancels(nodeID int64) {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	delete(s.pendingCancels, nodeID)
}
