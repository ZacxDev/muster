package devices

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// MIGRATION 0189: a phone with a step-up key marks its account
// (users.had_strong_factor_at) in the SAME statement as it is paired and as it
// is deleted — whoever deletes it, under whatever principal.

func (r rig) marked(t *testing.T, uid int64) bool {
	t.Helper()
	var at *time.Time
	if err := r.pool.QueryRow(authz.System(context.Background()), `SELECT had_strong_factor_at FROM users WHERE id = $1`, uid).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at != nil
}

func (r rig) unmark(t *testing.T, uid int64) {
	t.Helper()
	if _, err := r.pool.Exec(authz.System(context.Background()), `UPDATE users SET had_strong_factor_at = NULL WHERE id = $1`, uid); err != nil {
		t.Fatal(err)
	}
}

func (r rig) devicesOf(t *testing.T, uid int64) int {
	t.Helper()
	var n int
	if err := r.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM devices WHERE owner_id = $1`, uid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// refuseMarks makes any write of uid's mark fail (a CHECK constraint on that
// one row), until the test ends.
func refuseMarks(t *testing.T, uid int64) {
	t.Helper()
	dsn := dbtest.DSN(t)
	name := fmt.Sprintf("test_refuse_mark_%d", uid)
	dbtest.OwnerExec(t, dsn, fmt.Sprintf(`ALTER TABLE users ADD CONSTRAINT %s CHECK (had_strong_factor_at IS NULL OR id <> %d) NOT VALID`, name, uid))
	t.Cleanup(func() { dbtest.OwnerExec(t, dsn, `ALTER TABLE users DROP CONSTRAINT IF EXISTS `+name) })
}

// TestAStepUpPairingMarksItsAccountOrDoesNotHappen: the keyed insert marks the
// account; when the mark cannot be written the pairing rolls back — no
// device, and the code unspent (a retry pairs). A read-only phone marks
// nothing.
func TestAStepUpPairingMarksItsAccountOrDoesNotHappen(t *testing.T) {
	r := newRig(t)
	keyed := func() PairIn {
		return PairIn{Name: "Pixel", AppVersion: "1.2.3", StepUpKey: p256SPKI(t), KeyType: KeyBiometric, KeyLevel: LevelStrongBox}
	}
	r.pair(t, r.b, time.Now(), PairIn{Name: "read-only", AppVersion: "1"})
	if r.marked(t, r.b) {
		t.Fatal("a read-only phone marked its account")
	}
	r.pair(t, r.a, time.Now(), keyed())
	if !r.marked(t, r.a) {
		t.Fatal("a step-up pairing left its account unmarked")
	}
	// The atomicity: the mark refused, the pairing refused with it.
	r.unmark(t, r.a)
	before := r.devicesOf(t, r.a)
	refuseMarks(t, r.a)
	code, _, err := r.s.CreatePairingCode(as(r.a), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.s.Redeem(context.Background(), code, keyed()); err == nil {
		t.Fatal("a step-up pairing whose mark could not be written succeeded")
	}
	if n := r.devicesOf(t, r.a); n != before {
		t.Fatalf("a refused pairing left %d device(s), want %d", n, before)
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), fmt.Sprintf(`ALTER TABLE users DROP CONSTRAINT test_refuse_mark_%d`, r.a))
	if _, _, err := r.s.Redeem(context.Background(), code, keyed()); err != nil {
		t.Fatalf("the retry on the same code: %v (a rolled-back pairing spent it)", err)
	}
	if !r.marked(t, r.a) {
		t.Fatal("the retried pairing left its account unmarked")
	}
}

// TestRemovingAStepUpPhoneMarksItsAccountUnderItsOwnersPrincipal: Remove runs
// as the OWNER, who under row-level security cannot write their own users
// row: the trigger still marks it. With the mark refused, the removal is
// refused and the phone kept.
func TestRemovingAStepUpPhoneMarksItsAccountUnderItsOwnersPrincipal(t *testing.T) {
	r := newRig(t)
	_, d := r.pair(t, r.a, time.Now(), PairIn{Name: "Pixel", AppVersion: "1.2.3", StepUpKey: p256SPKI(t), KeyType: KeyBiometric, KeyLevel: LevelStrongBox})
	r.unmark(t, r.a) // as a phone paired before 0189 and never marked
	refuseMarks(t, r.a)
	if ok, err := r.s.Remove(as(r.a), d.ID); err == nil || ok {
		t.Fatalf("a removal whose mark could not be written: %v %v", ok, err)
	}
	if r.devicesOf(t, r.a) != 1 {
		t.Fatal("a refused removal deleted the phone")
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), fmt.Sprintf(`ALTER TABLE users DROP CONSTRAINT test_refuse_mark_%d`, r.a))
	if ok, err := r.s.Remove(as(r.a), d.ID); err != nil || !ok {
		t.Fatalf("the removal: %v %v", ok, err)
	}
	if !r.marked(t, r.a) {
		t.Fatal("removing an unmarked account's step-up phone left it unmarked")
	}
}
