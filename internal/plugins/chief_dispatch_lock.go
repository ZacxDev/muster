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

// chiefDispatchLockPoll is the first and the longest pause between attempts
// (doubling from the first, with jitter so waiters do not retry in step).
const (
	chiefDispatchLockPollMin = 2 * time.Millisecond
	chiefDispatchLockPollMax = 50 * time.Millisecond
)

// errChiefDispatchLockHeld is the attempt's own signal that the lock was held:
// the transaction is rolled back (returning its connection) and retried.
var errChiefDispatchLockHeld = errors.New("plugins: chief-dispatch lock held")

// inChiefDispatchLock runs fn in a transaction holding owner's chief-dispatch
// lock (a transaction advisory lock: released at commit or rollback, in
// Postgres, so it serialises every muster replica, not one process). It is THE
// way that lock is taken — by ReserveChiefDispatch and both releases.
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
	deadline := time.Now().Add(chiefDispatchLockWait)
	pause := chiefDispatchLockPollMin
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
		wait := pause/2 + rand.N(pause/2+1)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return errors.Join(ErrChiefDispatchBusy, ctx.Err())
		case <-t.C:
		}
		pause = min(2*pause, chiefDispatchLockPollMax)
	}
}
