package plugins

import (
	"context"
	"errors"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/db"
)

// ErrChiefDispatchBusy: the owner's chief-dispatch lock stayed held by another
// reservation or release for chiefDispatchLockWait. Nothing was read or
// written; the call may be retried.
var ErrChiefDispatchBusy = errors.New("plugins: the owner's chief-dispatch lock is busy; nothing was reserved")

// chiefDispatchLockWait bounds how long inChiefDispatchLock waits for the
// owner's lock: one statement timeout, which is what bounded the blocking wait
// this replaced. A variable for tests only.
var chiefDispatchLockWait = db.StatementTimeout

// chiefDispatchPause is the jittered pause before a waiter's next attempt,
// having waited waited: uniform in [0.5 ms, hi], where hi starts at 5 ms and
// SHRINKS by 1 ms per 20 ms waited, to 1 ms — so the longer a caller has
// waited, the more often it polls, and a release goes preferentially to the
// oldest waiters.
//
// 🔴 THE PAUSE MUST NEVER GROW WITH THE WAIT. A doubling backoff (2 ms up to
// 50 ms, before this) made the callers that had waited longest poll the
// slowest, so under sustained contention newcomers took each released lock
// and a long waiter could run out its whole bound (ErrChiefDispatchBusy).
// Measured with TestALongWaiterIsNotStarvedBySustainedContention (8 callers,
// 20 ms holds, 6 s, a 4 s bound): doubling p99 1.6-1.8 s, max 1.7 s up to the
// bound (5/5 runs, GOMAXPROCS=2); this, p99 0.54-0.83 s, max 0.63-1.07 s
// (15/15, 2 and 8 CPUs). A constant 1-5 ms range sat between (max up to
// 1.8 s): a caller arriving as the lock is released tries at once, a waiter
// only every few ms, so the old waiter must poll faster than that.
//
// The cost is churn: one attempt is about five round trips (BEGIN, the try,
// ROLLBACK, plus the pool's per-acquire role/principal setup and reset), every
// 0.5-5 ms per waiter while the lock is held. Contention on ONE owner's
// dispatch lock is rare and short (a reservation is a few reads and one
// insert), so that is at most a few thousand cheap statements a second per
// contended owner, only while a holder holds — against a start refused busy
// after waiting out the bound.
func chiefDispatchPause(waited time.Duration) time.Duration {
	const lo, top, floor = 500 * time.Microsecond, 5 * time.Millisecond, time.Millisecond
	hi := max(floor, top-waited/20)
	return lo + rand.N(hi-lo+1)
}

// errChiefDispatchLockHeld is the attempt's own signal that the lock was held:
// the transaction is rolled back (returning its connection) and retried.
var errChiefDispatchLockHeld = errors.New("plugins: chief-dispatch lock held")

// inChiefDispatchLock runs fn in a transaction holding owner's chief-dispatch
// lock (a transaction advisory lock: released at commit or rollback, in
// Postgres, so it serialises every muster replica, not one process). It is THE
// way that lock is taken — by ReserveChiefDispatch, both releases and
// EndHostDispatch (whose release of a host dispatch's start is the third).
//
// 🔴 A WAITER NEVER HOLDS A POOL CONNECTION WHILE IT WAITS. The lock is TRIED
// (pg_try_advisory_xact_lock); when another transaction holds it, this one
// rolls back, returning its connection, sleeps, and tries again. The holder's
// work reads through OTHER pool connections — ReserveChiefDispatch's check runs
// the caller's own reads (the owner's turn, the task, a hold) through the
// stores they belong to, which cannot share this transaction. With a blocking
// pg_advisory_xact_lock every waiter sat on a connection, and once waiters
// filled the pool the holder could not get one for its check: nothing moved
// until statement_timeout cancelled the waiters (SQLSTATE 57014; measured: 6
// concurrent agent_dispatch starts on a 2-connection pool, 50 s, 5 of 6
// failed). Polling, the holder needs at most one connection beyond its own
// while a waiter's attempt is in flight for one round trip.
//
// A wait past chiefDispatchLockWait, or ctx ending, returns ErrChiefDispatchBusy
// (wrapping ctx's error when that is why): fn did not run.
func (s *PGStore) inChiefDispatchLock(ctx context.Context, owner int64, fn func(pgx.Tx) error) error {
	key := strconv.FormatInt(owner, 10)
	start := time.Now()
	deadline := start.Add(chiefDispatchLockWait)
	for {
		err := s.inTx(ctx, func(tx pgx.Tx) error {
			var locked bool
			if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('muster.chief_dispatch:' || $1::text, 0))`,
				key).Scan(&locked); err != nil {
				return err
			}
			if !locked {
				return errChiefDispatchLockHeld
			}
			return fn(tx)
		})
		if !errors.Is(err, errChiefDispatchLockHeld) {
			return err
		}
		if time.Now().After(deadline) {
			return ErrChiefDispatchBusy
		}
		t := time.NewTimer(chiefDispatchPause(time.Since(start)))
		select {
		case <-ctx.Done():
			t.Stop()
			return errors.Join(ErrChiefDispatchBusy, ctx.Err())
		case <-t.C:
		}
	}
}
