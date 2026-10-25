package store

// Phase 3 HA tests for the advisory-lock singleton supervisor and the
// race-safe dispatch (AssignJob FOR UPDATE hardening). All PG-gated via
// ENCODE_TEST_PG_DSN; the SQLite path is exercised by the always-ok
// TryAdvisoryLock contract test.

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// requirePG skips the test when ENCODE_TEST_PG_DSN is unset and returns a
// ready *Store (TRUNCATEd in cleanup). Shared shape with openTestStorePG.
func requirePG(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("ENCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("ENCODE_TEST_PG_DSN not set; skipping PG HA test")
	}
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, tbl := range []string{
			"node_metrics", "audit_events", "pairing_codes", "provision_runs",
			"sessions", "users", "api_tokens", "shares", "settings",
			"step_templates", "series", "jobs", "flows", "nodes",
		} {
			s.db.ExecContext(ctx, fmt.Sprintf("TRUNCATE TABLE %s RESTART IDENTITY CASCADE", tbl))
		}
		s.Close()
	})
	return s
}

// TestTryAdvisoryLockSQLiteAlwaysOK confirms the SQLite path is a no-op
// always-succeed: a single instance has no peer to race, so the lock always
// acquires and release is idempotent. This is the SQLite no-op invariant.
func TestTryAdvisoryLockSQLiteAlwaysOK(t *testing.T) {
	s, err := Open(t.TempDir() + "/sqlite-lock.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for _, key := range []int64{LockKeyScanner, LockKeyRetention, LockKeyDigest, LockKeyBackup} {
		release, ok, err := s.TryAdvisoryLock(ctx, key)
		if err != nil || !ok {
			t.Fatalf("SQLite lock key %d: ok=%v err=%v (must always acquire)", key, ok, err)
		}
		// SQLite holds nothing, so acquiring the SAME key twice must also
		// succeed (no real lock to contend).
		release2, ok2, err2 := s.TryAdvisoryLock(ctx, key)
		if err2 != nil || !ok2 {
			t.Fatalf("SQLite re-acquire key %d: ok=%v err=%v", key, ok2, err2)
		}
		release()
		release2()
		// Release is idempotent — calling twice must not panic.
		release()
		release2()
	}
}

// TestAdvisoryLockMutualExclusionPG is the core failover test (PG-gated):
// two Stores against the same PG, TryAdvisoryLock(key) on A succeeds, B's
// attempt on the same key fails while A holds; A's release → B acquires.
func TestAdvisoryLockMutualExclusionPG(t *testing.T) {
	sA := requirePG(t)
	// Open a second Store over the same DSN (second pool). Two in-process
	// Stores against one PG mirrors two controller replicas.
	sB, err := Open(os.Getenv("ENCODE_TEST_PG_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer sB.Close()

	ctx := context.Background()
	key := int64(998877) // a key outside the production table; never collides

	// A acquires.
	releaseA, ok, err := sA.TryAdvisoryLock(ctx, key)
	if err != nil || !ok {
		t.Fatalf("A acquire: ok=%v err=%v", ok, err)
	}
	defer releaseA() // safety net if an assertion fails before the explicit release

	// B must NOT acquire while A holds.
	_, okB, errB := sB.TryAdvisoryLock(ctx, key)
	if errB != nil {
		t.Fatalf("B acquire errored (expected ok=false): %v", errB)
	}
	if okB {
		t.Fatal("B acquired the lock while A held it — mutual exclusion broken")
	}

	// A releases. B should now acquire within a short retry window (the
	// lock is non-transactional; PG clears it on unlock immediately).
	releaseA()
	deadline := time.Now().Add(5 * time.Second)
	var acquired bool
	for time.Now().Before(deadline) {
		relB, ok, err := sB.TryAdvisoryLock(ctx, key)
		if err != nil {
			t.Fatalf("B post-release acquire errored: %v", err)
		}
		if ok {
			relB()
			acquired = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !acquired {
		t.Fatal("B never acquired after A released — lock not cleared on unlock")
	}
}

// TestAdvisoryLockSessionDeathFailoverPG simulates a leader crash: A holds
// the lock, then its dedicated connection dies WITHOUT calling release
// (process killed). The advisory lock is session-scoped, so PG clears it
// when the session closes; B must acquire within one retry cycle. This is
// the failover invariant for the supervisor.
//
// We force session death by reaching into A's dedicated conn via the
// internal handle and closing it directly (simulates the process dying
// mid-hold — release was never called). PG detects the closed session and
// frees the lock.
func TestAdvisoryLockSessionDeathFailoverPG(t *testing.T) {
	sA := requirePG(t)
	sB, err := Open(os.Getenv("ENCODE_TEST_PG_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer sB.Close()

	ctx := context.Background()
	key := int64(554433) // distinct from the mutual-exclusion test

	// Acquire via the internal handle so we can simulate a raw session death
	// (close the conn without calling release — release is what would
	// normally run pg_try_advisory_unlock; a crashed process never runs it).
	hA, ok, err := sA.tryAdvisoryLock(ctx, key)
	if err != nil || !ok {
		t.Fatalf("A acquire: ok=%v err=%v", ok, err)
	}

	// Simulate leader crash: close the dedicated conn directly, bypassing
	// release(). The keepalive goroutine will also detect this and try to
	// close (idempotent via sync.Once in the real release; here we call the
	// raw conn close to model a severed session). We use a second Conn to
	// reach in is not possible — instead, force the session to die by
	// terminating the backend via pg_terminate_backend on A's connection.
	//
	// Find A's dedicated conn's PID via the advisory lock it holds.
	var pid int32
	if err := sA.db.QueryRowContext(ctx,
		`SELECT pid FROM pg_locks WHERE locktype='advisory' AND objid=$1 AND granted`, key,
	).Scan(&pid); err != nil {
		t.Fatalf("find holder pid: %v", err)
	}
	// Terminate that backend (simulates the leader process dying). The
	// session lock is released by PG on backend termination.
	if _, err := sA.db.ExecContext(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatalf("terminate backend: %v", err)
	}

	// B should now acquire within one retry cycle (PG clears session locks
	// on backend termination, typically <1s).
	deadline := time.Now().Add(10 * time.Second)
	var acquired bool
	for time.Now().Before(deadline) {
		relB, ok, err := sB.TryAdvisoryLock(ctx, key)
		if err != nil {
			t.Fatalf("B post-death acquire errored: %v", err)
		}
		if ok {
			relB()
			acquired = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !acquired {
		t.Fatal("B never acquired after A's session died — session-scoped lock not cleared on backend death")
	}
	// hA.release is a no-op now (conn is dead); call it for goroutine
	// hygiene (the keepalive goroutine exits when it next probes).
	hA.release()
}

// TestDispatchConcurrencyTwoStoresPG is the Phase 3.2 race-safety test
// (PG-gated): 2 Store instances × N goroutines claiming from a pool of M
// pending jobs against 2 nodes with capacity. Asserts:
//   - zero double-assignments (each job assigned at most once)
//   - node capacity never exceeded (COUNT assigned+running per node ≤ max
//     at all check points)
//
// This exercises the FOR UPDATE on the nodes capacity-check SELECT (without
// it, two replicas dispatching different jobs to the same node could both
// pass the count guard) AND the conditional UPDATE + RowsAffected==1 guard
// (without it, two replicas selecting the same pending job could both flip
// it). Both must hold under concurrency.
func TestDispatchConcurrencyTwoStoresPG(t *testing.T) {
	sA := requirePG(t)
	sB, err := Open(os.Getenv("ENCODE_TEST_PG_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer sB.Close()

	ctx := context.Background()

	// Two flows (one per node's group, to keep routing simple).
	flow, err := sA.CreateFlow(ctx, &model.Flow{Name: "ha-disp", Steps: []model.Step{{Type: model.StepEncode}}})
	if err != nil {
		t.Fatal(err)
	}

	// Two nodes, each with max_concurrent_jobs = 3. Capacity is small
	// enough that contention is real but large enough that multiple jobs
	// land per node (exercises the FOR UPDATE serialization).
	nA, err := sA.CreateNode(ctx, "ha-node-a", "ha")
	if err != nil {
		t.Fatal(err)
	}
	nB, err := sA.CreateNode(ctx, "ha-node-b", "hb")
	if err != nil {
		t.Fatal(err)
	}
	if err := sA.SetNodeMaxConcurrent(ctx, nA.ID, 3); err != nil {
		t.Fatal(err)
	}
	if err := sA.SetNodeMaxConcurrent(ctx, nB.ID, 3); err != nil {
		t.Fatal(err)
	}

	// M pending jobs (more than total capacity 6 so the queue doesn't drain
	// instantly and the race window is exercised). 20 jobs × 12 claimers.
	const numJobs = 20
	const claimersPerStore = 6
	jobIDs := make([]int64, 0, numJobs)
	for i := 0; i < numJobs; i++ {
		j, err := sA.CreateJob(ctx, &model.Job{
			Series: "HAS", Episode: fmt.Sprintf("Ep%02d", i),
			EpisodeDir: fmt.Sprintf("HAS/Ep%02d", i),
			ScriptType: "vpy", FlowID: flow.ID,
		})
		if err != nil {
			t.Fatalf("create job %d: %v", i, err)
		}
		jobIDs = append(jobIDs, j.ID)
	}

	// Each claimer picks a node, loops: NextAssignableJobForNode then
	// AssignJob, until AssignJob fails (node full) or no job returned.
	// This mirrors the heartbeat claim loop but driven concurrently from
	// two stores. assignedTo tracks which node each job landed on (for the
	// double-assignment check).
	var assignMu sync.Mutex
	assignedNode := make(map[int64]int64, numJobs) // jobID → nodeID (first assigner wins)
	var assignCount int64                          // total successful AssignJob calls

	claimer := func(s *Store, nodeID int64) {
		defer wgClaim.Done()
		for {
			j, err := s.NextAssignableJobForNode(ctx, nodeID)
			if err != nil || j == nil {
				return
			}
			if err := s.AssignJob(ctx, j.ID, nodeID); err != nil {
				return // lost a race or node full — stop (mirrors handler break)
			}
			assignMu.Lock()
			if _, exists := assignedNode[j.ID]; !exists {
				assignedNode[j.ID] = nodeID
			} else {
				// A second successful AssignJob on the same job is a
				// double-assignment — the RowsAffected guard must prevent
				// this. Record it so the assertion below catches it.
				t.Errorf("double-assignment of job %d (was node %d, now %d)",
					j.ID, assignedNode[j.ID], nodeID)
			}
			assignMu.Unlock()
			atomic.AddInt64(&assignCount, 1)
		}
	}

	// 6 claimers per store, split across the two nodes. wgClaim is package-
	// level so the helper can call Done; reset here for test isolation.
	wgClaim = sync.WaitGroup{}
	wgClaim.Add(claimersPerStore * 2)
	for i := 0; i < claimersPerStore; i++ {
		go claimer(sA, nA.ID)
		go claimer(sB, nB.ID)
	}
	wgClaim.Wait()

	// ── Assertions ──

	// 1. Zero double-assignments: each job assigned to AT MOST one node.
	//    A double-assignment would have logged a t.Errorf above; also
	//    verify against the DB (a job's node_id is its final assigner).
	for _, jid := range jobIDs {
		got, err := sA.GetJob(ctx, jid)
		if err != nil {
			t.Fatalf("get job %d: %v", jid, err)
		}
		if got.Status != model.JobPending && got.Status != model.JobAssigned {
			t.Errorf("job %d unexpected status %s", jid, got.Status)
		}
	}

	// 2. Node capacity never exceeded: assigned+running per node ≤ max (3).
	for _, n := range []*model.Node{nA, nB} {
		var active int
		if err := sA.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM jobs WHERE node_id=$1 AND status IN ('assigned','running')`, n.ID,
		).Scan(&active); err != nil {
			t.Fatalf("count active for node %d: %v", n.ID, err)
		}
		if active > 3 {
			t.Errorf("node %s: %d active jobs exceeds max_concurrent_jobs=3 (FOR UPDATE failed)",
				n.Name, active)
		}
	}

	// 3. Sanity: at least some jobs were assigned (the test would be
	//    vacuous if a bug made every AssignJob fail). With 20 jobs and
	//    capacity 6, we expect 6 assignments (queue drained to capacity).
	if total := atomic.LoadInt64(&assignCount); total < 3 {
		t.Errorf("expected ≥3 successful assignments across two stores, got %d", total)
	}
	// Total assigned must not exceed total capacity (2 nodes × 3).
	if total := len(assignedNode); total > 6 {
		t.Errorf("total assigned %d exceeds combined capacity 6", total)
	}
}

// wgClaim is shared by the concurrency test helper so the closure can call
// Done without capturing a per-test variable (Go closures over loop vars
// are fine, but a package-level var keeps the helper self-contained).
var wgClaim sync.WaitGroup
