package builtin

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE BUILT-IN SIDE OF A BUILTIN OFFICER'S DAILY BUDGET AND ROUTINE BATCHING
// (internal/officers budget.go has the rules):
//
//   - SpentToday is the spend the cap is measured against: the officer's
//     officer_llm_usage rows for the UTC day, every pool and instance summed —
//     what PutUsage books from the instances' transcripts and the LLM proxy
//     books from the provider. It is the wake loop's read (officers.
//     BudgetBackend) and the claim's.
//   - Claim and ClaimNext refuse an officer over its budget any item that does
//     not bypass it, so an instance woken for its owner's item cannot drain
//     the rest of the queue on the same spent day.
//   - The store gives a routine item its not_before at enqueue (deferRoutine).
// ---------------------------------------------------------------------------

var _ officers.BudgetBackend = (*Backend)(nil)

// rowQuerier is what spentToday reads through: the pool or a transaction.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// spentTodaySQL is THE sum the daily budget is measured against.
const spentTodaySQL = `SELECT coalesce(sum(cost_usd), 0)::float8 FROM officer_llm_usage
	WHERE owner_id = $1 AND officer = $2 AND day = $3::date`

func (b *Backend) spentToday(ctx context.Context, q rowQuerier, now time.Time) (float64, error) {
	var spent float64
	err := q.QueryRow(ctx, spentTodaySQL, b.officer.Owner, b.officer.ID, now.UTC().Format("2006-01-02")).Scan(&spent)
	return spent, err
}

// SpentToday implements officers.BudgetBackend: the officer's spend on now's
// UTC day, in USD, read as its owner.
func (b *Backend) SpentToday(ctx context.Context, now time.Time) (float64, error) {
	spent, err := b.spentToday(b.scope(ctx), b.store.pool, now)
	if err != nil {
		return 0, outage("usage", err)
	}
	return spent, nil
}

// budgetGate is what a claim at now may take under the officer's daily
// budget: held is true when the budget is spent OR the spend cannot be read
// (unreadable) — then only an item that bypasses the budget (its owner's,
// non-routine) is claimable, so an instance woken for its owner's item can
// still take it while nothing else is handed out (fail closed for everything
// the cap holds back, never for the owner). An officer with no budget reads
// nothing and holds nothing.
//
// 🔴 READ BEFORE THE CLAIM'S TRANSACTION, NEVER IN IT: a failed statement
// aborts a Postgres transaction, so a spend read that failed inside it would
// fail the claim itself instead of narrowing it.
func (b *Backend) budgetGate(ctx context.Context, now time.Time) (held, unreadable bool, spent float64) {
	if b.officer.Budget.DailyUSD <= 0 {
		return false, false, 0
	}
	if b.faultSpendRead {
		return true, true, 0
	}
	spent, err := b.spentToday(b.scope(ctx), b.store.pool, now)
	if err != nil {
		return true, true, 0
	}
	return b.officer.OverBudget(spent), false, spent
}

// overBudgetClaimable narrows claim-next's `claimable` for an officer over
// its daily budget: Officer.BypassesBudget in SQL — the owner enqueued it,
// and its kind is not one of $7 (the officer's routine_kinds).
const overBudgetClaimable = ` AND origin = '` + officers.OriginOwner + `' AND NOT (kind = ANY($7::text[]))`

// overBudgetRefusal is the claim's 409 for an item a spent (or unreadable)
// budget holds back.
func overBudgetRefusal(o officers.Officer, unreadable bool, spent float64, now time.Time) error {
	if unreadable {
		return refuse(409, "over_budget", "the officer's spend for its daily budget cannot be read: only its owner's own non-routine items are claimable until it can",
			map[string]any{"daily_budget_usd": o.Budget.DailyUSD, "spend_unreadable": true})
	}
	return refuse(409, "over_budget", "the officer's daily budget is spent: only its owner's own non-routine items are claimable until it resets",
		map[string]any{"spent_usd": spent, "daily_budget_usd": o.Budget.DailyUSD, "resets_at": officers.CapResetAt(now)})
}

// asItem is the part of it the budget rules read.
func (it itemRow) asItem() officers.Item {
	return officers.Item{Kind: it.Kind, Origin: it.Origin}
}

// RoutinePolicy answers which officer (its Budget) an item for owner's
// officer id is enqueued for; ok false for an officer the roster does not
// declare (then nothing is deferred). internal/api sets it to the live
// registry (officers.Registry.ForOwner).
type RoutinePolicy func(owner int64, officer string) (officers.Officer, bool)

// SetRoutinePolicy installs p: from then on, an item of one of its officer's
// routine_kinds enqueued with no not_before of its own is given the next
// routine_wake_at (officers.Officer.RoutineDue). Safe to call while the
// store is in use.
func (s *Store) SetRoutinePolicy(p RoutinePolicy) {
	s.routine.Store(&p)
}

// deferRoutine is n with its routine not_before, when n's officer batches
// n's kind and n names no not_before of its own (an explicit one is the
// caller's to choose).
func (s *Store) deferRoutine(owner int64, n NewItem, now time.Time) NewItem {
	p := s.routine.Load()
	if p == nil || *p == nil || n.NotBefore != nil {
		return n
	}
	o, ok := (*p)(owner, n.Officer)
	if !ok {
		return n
	}
	n.NotBefore = o.RoutineDue(n.Kind, now)
	return n
}

// routinePolicyHolder is the Store's field type for its RoutinePolicy.
type routinePolicyHolder = atomic.Pointer[RoutinePolicy]
