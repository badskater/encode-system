package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// LockKey constants are distinct int64 keys for each background loop that
// must run as a singleton across controller replicas. They are passed to
// TryAdvisoryLock so two replicas never run the same loop concurrently.
//
// The values are arbitrary but permanent — a running cluster may have rows
// in pg_locks with these obj/subID pairs, so changing a key mid-life would
// orphan an existing lock (the old holder keeps it under the old key; the
// new holder can acquire a different key and both run). Never renumber.
const (
	// LockKeyScanner guards scanner.RunLoop (scripts-share discovery).
	LockKeyScanner int64 = 1001
	// LockKeyRetention guards the hourly job-history prune loop.
	LockKeyRetention int64 = 1002
	// LockKeyDigest guards the hourly Discord digest flush loop.
	LockKeyDigest int64 = 1003
	// LockKeyDiskAlert guards the disk-alert cooldown sweep. (The per-node
	// alert itself fires inline on the heartbeat path; this key guards any
	// future periodic sweep. Kept reserved so the key table is complete.)
	LockKeyDiskAlert int64 = 1004
	// LockKeyBackup guards the scheduled DB-snapshot loop.
	LockKeyBackup int64 = 1005
)

// advisoryLockKeepalive is how often the held-lock keepalive probes the
// dedicated connection. A SELECT 1 every 10s is cheap and detects a
// severed session (network split, DB restart) within one window; if the
// probe fails the lock is considered lost and the supervisor re-acquires.
const advisoryLockKeepalive = 10 * time.Second

// lockHandle is the internal handle returned by a successful acquire. The
// supervisor uses it to observe loss (lostCh closes when the keepalive
// detects the session is dead) and to release (release closes the conn).
type lockHandle struct {
	release func()          // idempotent: unlock + close conn
	lostCh  <-chan struct{} // closes when the keepalive detects session loss
}

// TryAdvisoryLock attempts to acquire a non-transactional Postgres advisory
// lock for key. On Postgres it dedicates a *sql.Conn from the pool for the
// lock's entire lifetime (pgx advisory locks are session-scoped, and
// database/sql pools connections, so the lock's owning session must be held
// by ONE stable Conn — acquiring the lock through the pool would release it
// when the conn returns to the pool). It runs pg_try_advisory_lock($1); on
// success it returns (release, true, nil) where release is idempotent and
// releases the lock (pg_try_advisory_unlock on the same conn) then closes
// the conn. A background keepalive goroutine runs SELECT 1 on the same conn
// every advisoryLockKeepalive; if the keepalive fails the lock is considered
// lost — release() is called and the returned release func is a no-op (the
// conn is already gone). Callers should re-acquire on loss.
//
// On SQLite this returns (noop-release, true, nil): a single instance has no
// peer to race, so the lock always succeeds and the release does nothing.
// This keeps the supervisor path identical across backends — the SQLite
// controller wraps every loop in the supervisor and always "holds" the lock,
// so its background loops run exactly as before.
//
// ctx is used ONLY for the initial acquire attempt; the keepalive runs
// under a detached context so cancelling the caller's ctx does not kill the
// keepalive mid-hold (release() owns the conn's lifetime).
func (s *Store) TryAdvisoryLock(ctx context.Context, key int64) (release func(), ok bool, err error) {
	h, ok, err := s.tryAdvisoryLock(ctx, key)
	if !ok || err != nil {
		return nil, ok, err
	}
	return h.release, true, nil
}

func (s *Store) tryAdvisoryLock(ctx context.Context, key int64) (*lockHandle, bool, error) {
	if s.dialect != dialectPostgres {
		// SQLite: single instance — no peer can hold the lock. Return an
		// always-ok handle with an idempotent no-op release and a never-
		// closing lostCh (SQLite never "loses" the lock). The supervisor
		// path is identical on both backends; SQLite always "holds" the
		// lock, so its background loops run exactly as before.
		releaseOnce := sync.Once{}
		return &lockHandle{
			release: func() { releaseOnce.Do(func() {}) },
			lostCh:  nil, // nil channel blocks forever — never signals loss
		}, true, nil
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("advisory lock: acquire conn: %w", err)
	}

	// pg_try_advisory_lock returns a single bool: true if the lock was
	// acquired, false if it is already held by another session. The $1::bigint
	// cast is required: pg_try_advisory_lock(bigint) is unambiguous, but a
	// $? placeholder arrives as type "unknown" and PG can't resolve the
	// bigint/int4 overloads — the cast pins it.
	var acquired bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1::bigint)", key).Scan(&acquired); err != nil {
		conn.Close()
		return nil, false, fmt.Errorf("advisory lock: pg_try_advisory_lock: %w", err)
	}
	if !acquired {
		conn.Close()
		return nil, false, nil
	}

	// Keepalive: SELECT 1 on the same conn every 10s. A failed probe means
	// the session is dead; it closes lostCh so the supervisor can stop the
	// body and re-acquire. lostCh is buffered+single-close.
	keepAliveDone := make(chan struct{})
	lost := make(chan struct{})
	go s.advisoryLockKeepalive(conn, key, keepAliveDone, lost)

	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(keepAliveDone) // stop the keepalive goroutine
			// Best-effort unlock + close. Use pg_advisory_unlock (returns
			// void) not pg_try_advisory_unlock — the latter does not exist
			// in the PG 16 catalog (only the lock-side try_* functions are
			// defined; unlock is unconditional). pg_advisory_unlock raises
			// an error if the session doesn't hold the lock, which is fine
			// here: if the keepalive already detected loss the conn may be
			// dead, and PG releases session locks on disconnect regardless,
			// so a failed unlock is harmless. The error is swallowed.
			if _, err := conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1::bigint)", key); err != nil {
				// Swallow: either the lock was already released/cleared
				// (session death), or the conn is dead (PG frees the lock
				// on disconnect). Either way the lock does not persist.
				_ = err
			}
			conn.Close()
		})
	}

	return &lockHandle{release: release, lostCh: lost}, true, nil
}

// advisoryLockKeepalive probes the dedicated connection every
// advisoryLockKeepalive. A failed probe means the session is dead (network
// split, DB restart, conn closed externally); it closes lostCh so the
// supervisor can stop the body and re-acquire. The goroutine exits when
// keepAliveDone is closed (normal release path) or after a loss is signaled.
func (s *Store) advisoryLockKeepalive(conn *sql.Conn, key int64, keepAliveDone <-chan struct{}, lost chan<- struct{}) {
	ticker := time.NewTicker(advisoryLockKeepalive)
	defer ticker.Stop()
	for {
		select {
		case <-keepAliveDone:
			return
		case <-ticker.C:
			// SELECT 1 on the same conn: validates the session is alive. A
			// dead conn returns an error within the driver's socket timeout;
			// we bound the probe to the keepalive tick so a stalled probe
			// itself triggers loss within one window.
			probeCtx, cancel := context.WithTimeout(context.Background(), advisoryLockKeepalive)
			var n int
			if err := conn.QueryRowContext(probeCtx, "SELECT 1").Scan(&n); err != nil {
				cancel()
				close(lost)
				return
			}
			cancel()
		}
	}
}

// RunLoopSupervisor wraps a background loop so it runs on exactly one
// replica at a time on Postgres (advisory lock), and runs unchanged on
// SQLite (the lock is a no-op always-succeed). The wrapped loop body runs
// while the lock is held; on lock loss or ctx cancel it exits and the
// supervisor re-acquires after reAcquireDelay, until ctx is cancelled.
//
// The body runs under a sub-context derived from ctx. When the lock is lost
// (keepalive fails on PG) OR the parent ctx is cancelled (SIGTERM), the
// sub-context is cancelled so the body stops cleanly; the supervisor waits
// for the body goroutine to return before releasing and (on loss only)
// retrying acquisition. SIGTERM stops both the body and the supervisor.
//
// On SQLite the lock is always held and never lost, so the supervisor is a
// transparent wrapper: the body runs until the parent ctx is cancelled,
// identical to the pre-HA `go body(ctx)` it replaces.
//
// lockName is a human label for logs only; it does not affect the lock key.
func RunLoopSupervisor(ctx context.Context, st *Store, log *slog.Logger, key int64, lockName string, body func(ctx context.Context)) {
	for {
		if ctx.Err() != nil {
			return
		}
		h, ok, err := st.tryAdvisoryLock(ctx, key)
		if err != nil {
			log.Error("advisory lock acquire failed", "loop", lockName, "key", key, "err", err)
			if !sleepOrCancel(ctx, reAcquireDelay) {
				return
			}
			continue
		}
		if !ok {
			// Another replica holds the lock — standby. Wait and retry;
			// the loop simply does not run on this replica until it wins
			// the lock (leader death/failover clears the session lock).
			if !sleepOrCancel(ctx, reAcquireDelay) {
				return
			}
			continue
		}

		log.Info("acquired singleton lock", "loop", lockName, "key", key)

		// Body runs under a sub-context we cancel on loss/SIGTERM so the
		// body's own ctx.Done() path fires (every loop already respects
		// ctx — that's the SIGTERM shutdown contract).
		bodyCtx, bodyCancel := context.WithCancel(ctx)
		bodyDone := make(chan struct{})
		go func() {
			defer close(bodyDone)
			body(bodyCtx)
		}()

		// Three exit triggers:
		//  1. body returned on its own (ctx propagated a stop, or it chose
		//     to exit) → release, re-acquire if parent not done.
		//  2. keepalive signaled lock loss (PG only) → cancel body, wait,
		//     release, re-acquire.
		//  3. parent ctx cancelled (SIGTERM) → cancel body, wait, release,
		//     return.
		select {
		case <-bodyDone:
			release := h.release
			bodyCancel()
			release()
			if ctx.Err() != nil {
				return
			}
			continue
		case <-h.lostCh:
			log.Warn("singleton lock lost (keepalive failed); re-acquiring", "loop", lockName, "key", key)
			bodyCancel()
			<-bodyDone // wait for the body to drain
			h.release()
			if ctx.Err() != nil {
				return
			}
			if !sleepOrCancel(ctx, reAcquireDelay) {
				return
			}
			continue
		case <-ctx.Done():
			bodyCancel()
			<-bodyDone
			h.release()
			return
		}
	}
}

// reAcquireDelay is the pause between losing/failing-to-acquire a lock and
// the next attempt. 15s balances failover latency against lock-churn: the
// advisory lock is non-transactional, so a crashed holder's session lock
// clears within the PG connection-close window (sub-second once the server
// notices the dead session), and 15s is the retry cadence for the standby.
const reAcquireDelay = 15 * time.Second

// sleepOrCancel sleeps for d, returning false if ctx was cancelled during
// the sleep (so the caller can exit), true if the full duration elapsed.
func sleepOrCancel(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
