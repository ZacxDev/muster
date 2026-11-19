package builtin_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// THE OWNER'S RAISE OF A DAILY BUDGET FOR ONE UTC DAY (cap_raise.go): the
// claim honours it on its day and not the next, a raise is one more day's
// worth and bounded, its audit row commits with it, and row-level security
// keeps it the owner's own.

func capRaiseStore(t *testing.T) *builtin.Store {
	t.Helper()
	s := budgetStore(t)
	dsn := dbtest.DSN(t)
	wipe := func() {
		dbtest.OwnerExec(t, dsn, `DELETE FROM officer_cap_raises WHERE officer = 'budgetdemo:main'`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action = '`+builtin.CapRaiseAuditAction+`'`)
	}
	wipe()
	t.Cleanup(wipe)
	return s
}

// TestARaiseLetsTheClaimTakeWorkOnItsDayOnly: $10.49 of a $10 budget refuses
// the plugin's item; the owner's raise ($10 -> $20) lets the SAME claim take
// it; the next UTC day, $10.49 that day is refused again — the raise is not
// that day's, and nothing was cleaned up.
func TestARaiseLetsTheClaimTakeWorkOnItsDayOnly(t *testing.T) {
	s := capRaiseStore(t)
	now := time.Date(2000, 3, 4, 12, 0, 0, 0, time.UTC)
	o := budgetOfficerDecl(t)
	b := builtin.NewBackend(s, o, func() time.Time { return now }, dbtest.AsOperator)
	plugin := enqueueAs(t, s, "plugin.reply", "p1", "", now)
	bookSpend(t, s, "2000-03-04", "judgement", 1, 10.49)
	if raw, err := b.ClaimNext(context.Background(), "officer-budgetdemo-main-1", 300, ""); err != nil || claimedID(t, raw) != "" {
		t.Fatalf("control: over budget, claim-next took %s (%v); want nothing", raw, err)
	}
	r, err := s.RaiseCapToday(ctxOp(), o, "session", now)
	if err != nil || r.Old != 10 || r.New != 20 || !r.Until.Equal(time.Date(2000, 3, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("the raise: %+v, %v; want $10 -> $20 until 2000-03-05 00:00 UTC", r, err)
	}
	if v, err := b.RaisedCapToday(context.Background(), now); err != nil || v != 20 {
		t.Fatalf("RaisedCapToday = %v, %v; want 20", v, err)
	}
	raw, err := b.ClaimNext(context.Background(), "officer-budgetdemo-main-1", 300, "")
	if err != nil || claimedID(t, raw) != plugin {
		t.Fatalf("after the raise, claim-next took %s (%v); want the plugin item %s", raw, err, plugin)
	}

	next := time.Date(2000, 3, 5, 1, 0, 0, 0, time.UTC)
	bn := builtin.NewBackend(s, o, func() time.Time { return next }, dbtest.AsOperator)
	enqueueAs(t, s, "plugin.reply", "p2", "", next)
	bookSpend(t, s, "2000-03-05", "judgement", 1, 10.49)
	if v, err := bn.RaisedCapToday(context.Background(), next); err != nil || v != 0 {
		t.Fatalf("the next day's RaisedCapToday = %v, %v; want 0", v, err)
	}
	if raw, err := bn.ClaimNext(context.Background(), "officer-budgetdemo-main-1", 300, ""); err != nil || claimedID(t, raw) != "" {
		t.Fatalf("the next UTC day over $10, claim-next took %s (%v); want nothing (the raise was yesterday's)", raw, err)
	}
}

// TestARaiseIsBoundedAuditedAndTheOwnersOwn: each raise adds the declared
// $10 and writes one audit row; the fourth in a day is refused (ErrInvalid)
// and writes neither; another user's principal cannot write user 1's raise
// (row-level security), and an officer with no budget has nothing to raise.
func TestARaiseIsBoundedAuditedAndTheOwnersOwn(t *testing.T) {
	s := capRaiseStore(t)
	now := time.Date(2000, 3, 4, 12, 0, 0, 0, time.UTC)
	o := budgetOfficerDecl(t)
	count := func() (rows int, audits int) {
		t.Helper()
		if err := s.Pool().QueryRow(ctxOp(), `SELECT coalesce(max(daily_usd), 0)::int FROM officer_cap_raises WHERE officer = $1`, budgetOfficer).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if err := s.Pool().QueryRow(ctxOp(), `SELECT count(*) FROM audit_log WHERE action = $1`, builtin.CapRaiseAuditAction).Scan(&audits); err != nil {
			t.Fatal(err)
		}
		return rows, audits
	}
	other := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 987311, Role: authz.RoleMember, Via: authz.ViaSession})
	if _, err := s.RaiseCapToday(other, o, "session", now); err == nil {
		t.Fatal("another user's principal raised user 1's officer")
	}
	if usd, audits := count(); usd != 0 || audits != 0 {
		t.Fatalf("another user's refused raise left $%d and %d audit row(s)", usd, audits)
	}
	for i, want := range []float64{20, 30, 40} {
		r, err := s.RaiseCapToday(ctxOp(), o, "session", now)
		if err != nil || r.New != want || r.Old != want-10 {
			t.Fatalf("raise %d: %+v, %v; want $%v -> $%v", i+1, r, err, want-10, want)
		}
	}
	if _, err := s.RaiseCapToday(ctxOp(), o, "session", now); !errors.Is(err, builtin.ErrInvalid) {
		t.Fatalf("a fourth raise in a day: %v, want ErrInvalid", err)
	}
	if usd, audits := count(); usd != 40 || audits != 3 {
		t.Fatalf("after three raises and a refused fourth: $%d and %d audit row(s), want $40 and 3", usd, audits)
	}
	uncapped := o
	uncapped.Budget = officers.Budget{}
	if _, err := s.RaiseCapToday(ctxOp(), uncapped, "session", now); !errors.Is(err, builtin.ErrNoBudget) {
		t.Fatalf("a raise of an officer with no budget: %v, want ErrNoBudget", err)
	}
	many, err := s.RaisedCapsOn(ctxOp(), []int64{1}, now)
	if err != nil || many[builtin.OfficerRef{Owner: 1, Officer: budgetOfficer}] != 40 {
		t.Fatalf("RaisedCapsOn = %v, %v; want $40 for %s", many, err, budgetOfficer)
	}
	if got, err := s.RaisedCapsOn(other, []int64{1}, now); err != nil || len(got) != 0 {
		t.Fatalf("another user's RaisedCapsOn of user 1 = %v, %v; want nothing (row-level security)", got, err)
	}
}
