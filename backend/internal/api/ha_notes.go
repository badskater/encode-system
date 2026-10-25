package api

// Phase 3 HA audit notes (comment-only file; no runtime code).
//
// This file documents the idempotency audit of every agent-facing write path
// under two controller replicas sharing one Postgres. The invariant: any
// agent request can land on either replica and produce the same durable
// state — no replica relies on in-memory state that the other replica
// wouldn't have.
//
// ════════════════════════════════════════════════════════════════════
// 3.2 dispatch race-safety — decision (a) [SELECT unlocked, rely on
// conditional UPDATE + handler break]
// ════════════════════════════════════════════════════════════════════
//
// The heartbeat claim path is select-then-AssignJob:
//
//	handlers.go ~line 345-365:
//	  for i := 0; i < freeSlots; i++ {
//	    job := s.Store.NextAssignableJobForNode(ctx, node.ID)  // unlocked read
//	    payload := s.renderJob(...)
//	    if err := s.Store.AssignJob(ctx, job.ID, node.ID); err != nil {
//	        // Lost a race (another heartbeat took the slot or the node
//	        // filled up) — stop assigning; the next heartbeat re-evaluates.
//	        break
//	    }
//	  }
//
// Decision: do NOT add FOR UPDATE SKIP LOCKED to NextAssignableJobForNode.
// Rationale (grounded in the code above):
//
//  1. The job flip is the conditional UPDATE in AssignJob:
//       UPDATE jobs SET status='assigned', node_id=?, ...
//       WHERE id=? AND status='pending'
//     with a RowsAffected==1 guard. This is atomic on BOTH backends. Even
//     if two replicas select the SAME pending job (NextAssignableJobForNode
//     is an unlocked read), exactly one UPDATE matches (RowsAffected==1) and
//     the other gets 0 rows → AssignJob returns an error.
//  2. On AssignJob error, the handler BREAKS (does not retry with the next
//     job). The comment at handlers.go:361 says: "Lost a race ... — stop
//     assigning; the next heartbeat re-evaluates." So a lost race is a clean
//     early-exit; the next idle heartbeat re-runs NextAssignableJobForNode
//     (which returns the NEXT pending job, since the winner just flipped
//     the contested one to 'assigned'). No double-assignment is possible.
//  3. The actual race that matters under active/active is TWO replicas
//     dispatching DIFFERENT jobs to the SAME node and both passing the
//     per-node capacity count. That is closed by the FOR UPDATE on the
//     nodes capacity-check SELECT inside AssignJob's tx (store.go) — the
//     second tx blocks until the first commits, then re-reads the now-
//     higher active count and rejects. SKIP LOCKED on NextAssignableJob*
//     would NOT help with this: it only prevents two tx from selecting the
//     same JOB, not the same NODE.
//
// Conclusion (a): the conditional UPDATE + RowsAffected guard + handler
// break already prevents double-assignment; FOR UPDATE on the node row
// prevents capacity overshoot. SKIP LOCKED on NextAssignableJob* is not
// needed for correctness. Simpler is better (SQLite path stays identical).
// The concurrency test (store_ha_test.go, PG-gated) covers both: zero
// double-assignments AND node capacity never exceeded under 2 stores × N
// goroutines.
//
// If a future change makes the handler RETRY with the next job on AssignJob
// failure (instead of break), re-evaluate: a retry loop could re-select a
// job that another replica is mid-flipping, and SKIP LOCKED would then
// reduce wasted render attempts. Today it does not.
//
// ════════════════════════════════════════════════════════════════════
// 3.3 idempotency audit of agent-facing write paths
// ════════════════════════════════════════════════════════════════════
//
// Every path below is a DB-only upsert / conditional UPDATE with NO reliance
// on in-memory controller state, so a request hitting replica A and a
// retry hitting replica B produce the same durable state.
//
//  1. Heartbeat (handleHeartbeat, handlers.go): upserts the node row
//     (UpdateNode) and conditionally assigns pending jobs (AssignJob, see
//     3.2 above). Both are DB statements; no in-memory node state is
//     mutated outside the row. The in-memory diskGuard/digest are
//     observability-only (see SSE note below) — a missed alert on one
//     replica costs at most one extra alert; never affects assignment.
//
//  2. UpdateJobStatus / progress (handleJobProgress, handlers.go): a plain
//     UPDATE jobs SET status=?, step=?, progress=?, log_tail=? WHERE id=?
//     AND status IN ('assigned','running'). Idempotent: re-sending the same
//     progress sample is a no-op (same values); a stale sample from a
//     network-replayed request matches the WHERE clause only while the job
//     is live and overwrites with the same/older data — safe because the
//     agent sends monotonic progress. No in-memory state.
//
//  3. Log shipping (handleJobLog, agent_log): an INSERT into agent_log
//     (or the full_log UPDATE on the job row). Pure DB writes; idempotent
//     in shape (the agent appends, never assumes a prior append landed — a
//     retry re-sends the same line; agent_log is an append-only event log
//     so a duplicate line is a cosmetic dup, not a state corruption).
//
//  4. Job report / FinishJobWithReport (handleJobReport, handlers.go):
//     UPDATE jobs SET status=?, exit_code=?, ... WHERE id=? (guarded by
//     status IN ('assigned','running') in FinishJob). A replayed report on
//     a terminal job matches zero rows (the job is no longer live) and is
//     a no-op. No in-memory state. The auto-retry scheduler
//     (ScheduleJobRetry) is itself guarded by status='failed' so a replay
//     can't double-retry.
//
//  5. Pairing (handlePair): CREATE pairing code row (insertID) — not
//     idempotent by design (a code is single-use; the redeem path deletes
//     it). A replay of the pairing ISSUE just creates a second code (the
//     first remains valid until expiry). No in-memory state.
//
// ════════════════════════════════════════════════════════════════════
// SSE log stream (logstream.go) — per-replica limitation
// ════════════════════════════════════════════════════════════════════
//
// The live progress SSE stream is purely event-driven: logHub is an
// in-memory fan-out (map[jobID]→subscribers on THIS process). A browser
// attached to replica A gets progress events ONLY for progress delivered
// to replica A (the heartbeat/report handler on A calls publishProgress →
// logHub.publish → A's subscribers). Progress delivered to replica B
// (agent heartbeats hitting B) is persisted to the job row on B but does
// NOT fan out to A's SSE subscribers.
//
// Graceful degradation already present (logstream.go:192-203): the keepalive
// tick re-reads the job row from the DB every 15s and, on detecting a
// terminal status, sends a final event and closes. So a stream on A for a
// job finishing on B WILL close correctly within 15s — it just won't show
// the intermediate progress ticks from B. A client that needs live
// intermediate ticks should reconnect to the LB (round-robin may land it on
// the replica the agent is hitting). The persisted job row remains the
// source of truth; a /api/jobs/{id} snapshot resyncs at any time.
//
// LISTEN/NOTIFY fanout (PG-native cross-replica pub/sub) is the future
// fix if this UX regresses — NOT implemented here per the plan. This note
// documents the limitation so it can go into Operations.md later.
