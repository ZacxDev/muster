package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// AN OWNER'S "RAISE CAP FOR TODAY" (officer_cap_raises, 0157)
//
// The owner of a builtin officer held by its daily budget may raise that
// budget for the CURRENT UTC DAY only, from the officer's page. The raise is a
// row keyed by the UTC day; every reader asks for today's row and nothing
// else, so it ends at the next UTC midnight by construction (no cleanup job).
// Readers apply it through officers.Officer.WithRaisedCap, which only ever
// raises.
//
//   - RaiseCapToday writes it: one more day's worth (the declared budget) on
//     top of today's cap, at most MaxCapRaisesPerDay times a day, and the
//     audit_log row (who, which officer, old and new cap, until when) in the
//     SAME transaction — a raise without its audit row never commits.
//   - Backend.RaisedCapToday is the wake loop's and the claim's read
//     (officers.RaisedCapBackend), as the officer's owner.
//   - RaisedCapsOn is the pages' batched read.
//
// 🔴 PERSONAL: owner_id is the officer's owner, under owner_isolation, so only
// the owner's own principal reads or writes it. A team's officer is never
// raised here (its owner is a team, no person's principal).
// ---------------------------------------------------------------------------

// MaxCapRaisesPerDay bounds how many raises one officer takes in one UTC day:
// at most that many extra days' worth of budget.
const MaxCapRaisesPerDay = 3

// CapRaiseAuditAction is the audit_log action of a raise.
const CapRaiseAuditAction = "officer.cap_raise"

// ErrNoBudget: the officer has no daily budget, so there is nothing to raise.
var ErrNoBudget = errors.New("builtin: the officer has no daily budget")

// CapRaise is a raise as written: the cap before and after, and when it ends
// (the next UTC midnight).
type CapRaise struct {
	Old, New float64
	Until    time.Time
}

// raisedCapSQL is today's raise of one officer.
const raisedCapSQL = `SELECT daily_usd FROM officer_cap_raises WHERE owner_id = $1 AND officer = $2 AND day = $3::date`

// raisedCap is owner's officer's raised cap on now's UTC day, 0 for none.
func raisedCap(ctx context.Context, q rowQuerier, owner int64, officer string, now time.Time) (float64, error) {
	var v float64
	err := q.QueryRow(ctx, raisedCapSQL, owner, officer, spendDay(now)).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

// RaiseCapToday raises o's daily budget for now's UTC day by one more day's
// worth (o's declared daily_budget_usd), as ctx's principal — who must be o's
// owner (row-level security refuses anyone else's row) — and appends the
// audit_log row naming them (via) in the same transaction. A raise past
// MaxCapRaisesPerDay is ErrInvalid; an officer with no budget ErrNoBudget.
func (s *Store) RaiseCapToday(ctx context.Context, o officers.Officer, via string, now time.Time) (CapRaise, error) {
	step := o.Budget.DailyUSD
	if step <= 0 {
		return CapRaise{}, ErrNoBudget
	}
	until := officers.CapResetAt(now)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CapRaise{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// Atomic under concurrent raises: the upsert adds to whatever is there.
	var raised float64
	if err := tx.QueryRow(ctx, `INSERT INTO officer_cap_raises (owner_id, officer, day, daily_usd, raised_at) VALUES ($1, $2, $3::date, $4::float8 * 2, $5)
		ON CONFLICT (owner_id, officer, day) DO UPDATE SET daily_usd = greatest(officer_cap_raises.daily_usd, $4::float8) + $4::float8, raised_at = EXCLUDED.raised_at
		RETURNING daily_usd`, o.Owner, o.ID, spendDay(now), step, now).Scan(&raised); err != nil {
		return CapRaise{}, classify(err)
	}
	if raised > step*float64(1+MaxCapRaisesPerDay)+1e-9 {
		return CapRaise{}, fmt.Errorf("%w: today's budget was already raised %d times; it resets at %s", ErrInvalid, MaxCapRaisesPerDay, until.Format("15:04 UTC"))
	}
	r := CapRaise{Old: raised - step, New: raised, Until: until}
	detail, _ := json.Marshal(map[string]any{"officer": o.ID, "officer_owner": o.Owner, "old_daily_usd": r.Old, "new_daily_usd": r.New,
		"declared_daily_usd": step, "day": spendDay(now), "until": until.UTC().Format(time.RFC3339)})
	if _, err := tx.Exec(ctx, `INSERT INTO audit_log (user_id, via, action, object, detail) VALUES ($1, $2, $3, $4, $5::jsonb)`,
		o.Owner, via, CapRaiseAuditAction, "officer_def:"+o.ID, detail); err != nil {
		return CapRaise{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CapRaise{}, err
	}
	return r, nil
}

// RaisedCapsOn is, in ONE query, the raised cap on now's UTC day of every
// officer of owners that has one; an officer absent from the map has none.
// Read under ctx's principal (row-level security: only its own).
func (s *Store) RaisedCapsOn(ctx context.Context, owners []int64, now time.Time) (map[OfficerRef]float64, error) {
	out := map[OfficerRef]float64{}
	if len(owners) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT owner_id, officer, daily_usd FROM officer_cap_raises WHERE owner_id = ANY($1::bigint[]) AND day = $2::date`,
		owners, spendDay(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r OfficerRef
		var v float64
		if err := rows.Scan(&r.Owner, &r.Officer, &v); err != nil {
			return nil, err
		}
		out[r] = v
	}
	return out, rows.Err()
}

var _ officers.RaisedCapBackend = (*Backend)(nil)

// RaisedCapToday implements officers.RaisedCapBackend: the officer's raised
// cap on now's UTC day (0: none), read as its owner.
func (b *Backend) RaisedCapToday(ctx context.Context, now time.Time) (float64, error) {
	if b.officer.Budget.DailyUSD <= 0 {
		return 0, nil
	}
	v, err := raisedCap(b.scope(ctx), b.store.pool, b.officer.Owner, b.officer.ID, now)
	if err != nil {
		return 0, outage("cap raise", err)
	}
	return v, nil
}
