package officers

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// A BUILTIN OFFICER'S DAILY BUDGET AND ROUTINE BATCHING
//
// Two settings on a builtin officer's declaration, both off by default:
//
//   - `daily_budget_usd`: once the officer's spend for the UTC day reaches it,
//     the wake loop stops waking the officer for the rest of the day and the
//     claim refuses it new work, EXCEPT an item its owner enqueued
//     (Item.Origin == OriginOwner) whose kind is not routine. The spend is the
//     officer's own booked usage, officer_llm_usage summed over its pools and
//     instances for the day (builtin.Backend.SpentToday): the estimate the
//     usage collector already books from the instances' transcripts, or the
//     proxy's reported cost for a pool on the LLM proxy. The owner is told
//     once per UTC day when the cap is reached (Config.BudgetAlert).
//
//   - `routine_kinds` with `routine_wake_at` ("HH:MM", UTC): an item of a
//     listed kind is enqueued with not_before at the next routine_wake_at
//     (Officer.RoutineDue, applied by the built-in store), so it neither wakes
//     the officer nor can be claimed until then, and every routine item queued
//     during the day comes due at the same minute: one wake drains them. The
//     set is explicit configuration, never a pattern: a kind is routine when it
//     is listed, exactly.
//
// 🔴 THE CAP STOPS WAKES, IT DOES NOT KILL A TURN. An instance that holds a
// claim when the cap is reached keeps it and finishes; the spend it adds is
// over the cap. The spend is also only as fresh as the usage collector's last
// push (usage.DefaultInterval), so a running instance can overshoot by what it
// spends between two pushes. The cap bounds how many times the officer is
// WOKEN past it: once it reads as reached, zero, until the UTC day ends.
//
// 🔴 ROUTINE KINDS NEVER BYPASS THE CAP, whoever enqueued them: a routine item
// is the work the cap exists to stop.
// ---------------------------------------------------------------------------

// OriginOwner is Item.Origin for an item the officer's owner enqueued.
const OriginOwner = "owner"

// MaxRoutineKinds bounds routine_kinds.
const MaxRoutineKinds = 32

// Budget is an officer's `daily_budget_usd`, `routine_kinds` and
// `routine_wake_at`. The zero value caps nothing and batches nothing.
type Budget struct {
	// DailyUSD is the spend cap per UTC day; 0 is none.
	DailyUSD float64
	// RoutineKinds is the item kinds batched into the daily routine wake,
	// sorted and distinct; empty batches nothing.
	RoutineKinds []string
	// WakeAt is the routine wake's time of day, from UTC midnight. Set exactly
	// when RoutineKinds is.
	WakeAt time.Duration
}

// parseBudget reads d's budget fields for officer o (its backend already
// parsed). Every field is refused on an officer that is not builtin: the
// spend it reads and the queue it defers are the built-in store's.
func parseBudget(o Officer, d declared) (Budget, error) {
	var b Budget
	wake := strings.TrimSpace(d.RoutineWakeAt)
	if !o.IsBuiltin() && (d.DailyBudgetUSD != 0 || len(d.RoutineKinds) > 0 || wake != "") {
		return b, fieldErr("backend", fmt.Errorf(`officer %s: daily_budget_usd, routine_kinds and routine_wake_at need "backend": "builtin" (the spend and the queue they read are the built-in store's)`, o.ID))
	}
	// 🔴 NOT ON A HOST OFFICER. Its instances are host-spawned windows: no pod,
	// so the usage collector books nothing and the spend reads $0 for ever;
	// and the window holds the host's shared hook token, with which it could
	// enqueue an owner item to itself past its own cap.
	if o.OnHost() && d.DailyBudgetUSD != 0 {
		return b, fieldErr("daily_budget_usd", fmt.Errorf(`officer %s: daily_budget_usd is refused on a "runtime": "host" officer (its spend is not booked, so a cap would never be reached, and its instances hold the hook token that enqueues as the owner)`, o.ID))
	}
	if v := d.DailyBudgetUSD; math.IsNaN(v) || v < 0 || v > MaxLLMDailyBudgetUSD {
		return b, fieldErr("daily_budget_usd", fmt.Errorf("officer %s: daily_budget_usd %v is not in [0, %v] (0 is no cap)", o.ID, v, MaxLLMDailyBudgetUSD))
	}
	b.DailyUSD = d.DailyBudgetUSD
	if len(d.RoutineKinds) > MaxRoutineKinds {
		return b, fieldErr("routine_kinds", fmt.Errorf("officer %s: routine_kinds lists %d kinds, over %d", o.ID, len(d.RoutineKinds), MaxRoutineKinds))
	}
	seen := map[string]bool{}
	for _, k := range d.RoutineKinds {
		k = strings.TrimSpace(k)
		if k == "" || len(k) > 100 || strings.ContainsAny(k, " \t\r\n,") {
			return b, fieldErr("routine_kinds", fmt.Errorf("officer %s: routine_kinds entry %q is not an item kind (1-100 characters, no spaces or commas)", o.ID, k))
		}
		if !seen[k] {
			seen[k] = true
			b.RoutineKinds = append(b.RoutineKinds, k)
		}
	}
	sort.Strings(b.RoutineKinds)
	switch {
	case len(b.RoutineKinds) > 0 && wake == "":
		return b, fieldErr("routine_wake_at", fmt.Errorf("officer %s: routine_kinds needs routine_wake_at (\"HH:MM\", UTC): the one time of day its routine items come due", o.ID))
	case len(b.RoutineKinds) == 0 && wake != "":
		return b, fieldErr("routine_wake_at", fmt.Errorf("officer %s: routine_wake_at without routine_kinds batches nothing; list the kinds or remove it", o.ID))
	case wake != "":
		t, err := time.Parse("15:04", wake)
		if err != nil {
			return b, fieldErr("routine_wake_at", fmt.Errorf("officer %s: routine_wake_at %q is not \"HH:MM\" (UTC, 24-hour)", o.ID, d.RoutineWakeAt))
		}
		b.WakeAt = time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute
	}
	return b, nil
}

// IsRoutineKind reports whether kind is one of o's routine_kinds: listed,
// exactly.
func (o Officer) IsRoutineKind(kind string) bool {
	i := sort.SearchStrings(o.Budget.RoutineKinds, kind)
	return i < len(o.Budget.RoutineKinds) && o.Budget.RoutineKinds[i] == kind
}

// RoutineDue is the not_before an item of kind enqueued for o at now is
// given: nil for a kind that is not routine; for a routine kind, the next
// routine_wake_at — today's when now is before it, else tomorrow's. Every
// routine item enqueued between two wakes thus gets the SAME not_before.
func (o Officer) RoutineDue(kind string, now time.Time) *time.Time {
	if !o.IsRoutineKind(kind) {
		return nil
	}
	day := now.UTC().Truncate(24 * time.Hour)
	at := day.Add(o.Budget.WakeAt)
	if !now.Before(at) {
		at = at.AddDate(0, 0, 1)
	}
	return &at
}

// BypassesBudget reports whether item it is work for o even with its daily
// budget spent: its owner enqueued it, and its kind is not routine. THE rule;
// the built-in claim-next spells the same in SQL (builtin.overBudgetClaimable).
func (o Officer) BypassesBudget(it Item) bool {
	return it.Origin == OriginOwner && !o.IsRoutineKind(it.Kind)
}

// OverBudget reports whether spent has reached o's daily budget. An officer
// with no budget is never over it.
func (o Officer) OverBudget(spent float64) bool {
	return o.Budget.DailyUSD > 0 && spent >= o.Budget.DailyUSD
}

// WithRaisedCap is o with its daily budget for one UTC day raised to usd (its
// owner's "Raise cap for today", builtin.Store.RaiseCapToday). THE rule for a
// raise: it only ever RAISES — usd at or below the declared budget changes
// nothing, and an officer with no budget (0, uncapped) is never given one.
// Every reader of a raise (the wake loop, the claim, the pages) applies it
// through here, then asks OverBudget.
func (o Officer) WithRaisedCap(usd float64) Officer {
	if o.Budget.DailyUSD > 0 && usd > o.Budget.DailyUSD {
		o.Budget.DailyUSD = usd
	}
	return o
}

// RaisedCapBackend is a backend whose officer's daily budget its owner may
// raise for one UTC day (builtin.Backend): the raised cap for now's UTC day,
// 0 for none.
type RaisedCapBackend interface {
	RaisedCapToday(ctx context.Context, now time.Time) (float64, error)
}

// raisedCapReader is the part of the loop's HQ that reads a raise
// (Backends.RaisedCapToday), asserted so an HQ without it raises nothing.
type raisedCapReader interface {
	RaisedCapToday(ctx context.Context, o Officer, now time.Time) (float64, error)
}

// RaisedCapToday is o's raised cap for now's UTC day through its backend; 0
// when the backend has none or raises nothing.
func (h *Backends) RaisedCapToday(ctx context.Context, o Officer, now time.Time) (float64, error) {
	rb, ok := h.Backend(o).(RaisedCapBackend)
	if !ok {
		return 0, nil
	}
	return rb.RaisedCapToday(ctx, now)
}

// BudgetBackend is a backend that can say what its officer has spent on
// now's UTC day (builtin.Backend). A backend without it has no budget.
type BudgetBackend interface {
	SpentToday(ctx context.Context, now time.Time) (float64, error)
}

// spendReader is the part of the loop's HQ that reads an officer's spend
// (Backends.SpentToday), asserted so an HQ without it (every test fake that
// does not opt in) caps nothing.
type spendReader interface {
	SpentToday(ctx context.Context, o Officer, now time.Time) (float64, bool, error)
}

// SpentToday is o's spend on now's UTC day through its backend; served false
// when the backend has no BudgetBackend.
func (h *Backends) SpentToday(ctx context.Context, o Officer, now time.Time) (spent float64, served bool, err error) {
	bb, ok := h.Backend(o).(BudgetBackend)
	if !ok {
		return 0, false, nil
	}
	spent, err = bb.SpentToday(ctx, now)
	return spent, true, err
}

// BudgetAlert is the once-per-UTC-day notice that an officer reached its
// daily budget (Config.BudgetAlert).
type BudgetAlert struct {
	// Unreadable: not "the budget is reached" but "the spend could not be
	// read" — the cap's work is held back (fail closed) until it can be;
	// Spent and ResetAt are then zero. Told once per UTC day, apart from the
	// reached notice.
	Unreadable bool
	// Spent and Budget are the day's spend as read and the cap, in USD.
	Spent, Budget float64
	// Waiting is the items held back by the cap when it was reached.
	Waiting int
	// ResetAt is when the cap resets: the next UTC midnight.
	ResetAt time.Time
}
