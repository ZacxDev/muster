package signinreq

import (
	"context"
	"errors"
	"io"
	"log"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/users"
)

// rig is a migrated database with one fresh member user, the approver.
type rig struct {
	s    *PGStore
	pool *pgxpool.Pool
	uid  int64
}

func newRig(t *testing.T) rig {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(dbtest.AsOperator(ctx), pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	r := rig{s: NewPG(pool), pool: pool}
	if err := pool.QueryRow(authz.System(ctx), `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`,
		"qr"+strconv.FormatInt(time.Now().UnixNano(), 36)).Scan(&r.uid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM signin_requests WHERE approved_by = $1`, `DELETE FROM audit_log WHERE user_id = $1`,
			`DELETE FROM users WHERE id = $1`} {
			_, _ = pool.Exec(bg, q, r.uid)
		}
	})
	return r
}

func (r rig) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := r.pool.Exec(authz.System(context.Background()), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (r rig) actor() users.Actor { return users.Actor{UserID: r.uid, Via: authz.ViaSession} }

// start creates a request and returns its code and binding.
func (r rig) start(t *testing.T) (string, string) {
	t.Helper()
	code, binding, exp, err := r.s.Create(context.Background(), "192.0.2.10", "Firefox on Linux")
	if err != nil {
		t.Fatal(err)
	}
	if !CodeShapeOK(code) || !BindingShapeOK(binding) || code[len(CodePrefix):] == binding {
		t.Fatalf("secrets: code %q binding %q", code, binding)
	}
	if d := time.Until(exp); d < TTL-10*time.Second || d > TTL+10*time.Second {
		t.Fatalf("expires in %v; want about %v", d, TTL)
	}
	return code, binding
}

func (r rig) approve(t *testing.T, code string) {
	t.Helper()
	if _, err := r.s.Decide(context.Background(), code, true, r.actor(), time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("approve: %v", err)
	}
}

func (r rig) state(t *testing.T, binding string) State {
	t.Helper()
	req, err := r.s.Status(context.Background(), binding)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	return req.State
}

// TestTheWholeFlowAndItsAudit: pending -> scanned -> approved -> redeemed, the
// details the phone is shown, the approver handed to the redeem, and one audit
// row per decision and redeem, as the approver.
func TestTheWholeFlowAndItsAudit(t *testing.T) {
	r := newRig(t)
	code, binding := r.start(t)
	if s := r.state(t, binding); s != StatePending {
		t.Fatalf("new: %s", s)
	}
	req, err := r.s.Open(context.Background(), code)
	if err != nil || req.State != StateScanned || req.RequesterIP != "192.0.2.10" || req.RequesterAgent != "Firefox on Linux" {
		t.Fatalf("open: %+v %v", req, err)
	}
	if _, err := r.s.Open(context.Background(), code); err != nil {
		t.Fatalf("a second open (a reload) is refused: %v", err)
	}
	if s := r.state(t, binding); s != StateScanned {
		t.Fatalf("after open: %s", s)
	}
	r.approve(t, code)
	if s := r.state(t, binding); s != StateApproved {
		t.Fatalf("after approve: %s", s)
	}
	got, err := r.s.Redeem(context.Background(), code, binding)
	if err != nil || got.ApprovedBy != r.uid || got.State != StateRedeemed {
		t.Fatalf("redeem: %+v %v", got, err)
	}
	if s := r.state(t, binding); s != StateRedeemed {
		t.Fatalf("after redeem: %s", s)
	}
	rows, err := r.pool.Query(authz.System(context.Background()), `SELECT action, via, object FROM audit_log WHERE user_id = $1 ORDER BY id`, r.uid)
	if err != nil {
		t.Fatal(err)
	}
	var audit []string
	for rows.Next() {
		var a, v, o string
		if err := rows.Scan(&a, &v, &o); err != nil {
			t.Fatal(err)
		}
		audit = append(audit, a+" "+v+" "+o)
	}
	rows.Close()
	obj := Object(req.ID)
	want := []string{"signin.app.approve session " + obj, "signin.app.redeem session " + obj}
	if len(audit) != len(want) || audit[0] != want[0] || audit[1] != want[1] {
		t.Fatalf("audit = %q; want %q", audit, want)
	}
}

// TestRedeemIsSingleUse: the second redeem of one approval is refused, also
// when two race.
func TestRedeemIsSingleUse(t *testing.T) {
	r := newRig(t)
	code, binding := r.start(t)
	r.approve(t, code)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = r.s.Redeem(context.Background(), code, binding)
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrNotFound):
			t.Errorf("a losing redeem: %v; want ErrNotFound", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d of %d concurrent redeems succeeded; want exactly 1", ok, len(errs))
	}
	if _, err := r.s.Redeem(context.Background(), code, binding); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a later redeem: %v; want ErrNotFound", err)
	}
}

// TestRedeemNeedsThisRequestsBinding: the code with no binding, with another
// request's binding, or with a malformed one, redeems nothing — and the
// approval is still there for the right browser afterwards.
func TestRedeemNeedsThisRequestsBinding(t *testing.T) {
	r := newRig(t)
	code, binding := r.start(t)
	_, other := r.start(t)
	r.approve(t, code)
	for name, b := range map[string]string{"none": "", "another request's": other, "malformed": "not-a-binding", "the code itself": code} {
		if _, err := r.s.Redeem(context.Background(), code, b); !errors.Is(err, ErrNotFound) {
			t.Errorf("redeem with %s binding: %v; want ErrNotFound", name, err)
		}
	}
	if s := r.state(t, binding); s != StateApproved {
		t.Fatalf("after the refused redeems: %s; want still approved", s)
	}
	if _, err := r.s.Redeem(context.Background(), code, binding); err != nil {
		t.Fatalf("the right browser's redeem: %v", err)
	}
}

// TestExpiry: past expires_at, nothing moves — open, decide and redeem are
// refused whatever the stored state — and Status says expired. Measured on the
// database's clock at both sides of the boundary.
func TestExpiry(t *testing.T) {
	r := newRig(t)
	expire := func(code string) {
		r.exec(t, `UPDATE signin_requests SET expires_at = now() - interval '1 second' WHERE code_sha256 = $1`, hash(code))
	}
	// pending, expired: no open, no decision.
	code, binding := r.start(t)
	expire(code)
	if s := r.state(t, binding); s != StateExpired {
		t.Fatalf("status: %s", s)
	}
	if _, err := r.s.Open(context.Background(), code); !errors.Is(err, ErrNotFound) {
		t.Errorf("open: %v", err)
	}
	if _, err := r.s.Decide(context.Background(), code, true, r.actor(), time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("decide: %v", err)
	}
	// approved, then expired: no redeem.
	code, binding = r.start(t)
	r.approve(t, code)
	expire(code)
	if s := r.state(t, binding); s != StateExpired {
		t.Fatalf("approved then expired: status %s", s)
	}
	if _, err := r.s.Redeem(context.Background(), code, binding); !errors.Is(err, ErrNotFound) {
		t.Fatalf("redeem after expiry: %v; want ErrNotFound", err)
	}
	// The other side of the boundary: a second left, it redeems.
	code, binding = r.start(t)
	r.approve(t, code)
	r.exec(t, `UPDATE signin_requests SET expires_at = now() + interval '1 second' WHERE code_sha256 = $1`, hash(code))
	if _, err := r.s.Redeem(context.Background(), code, binding); err != nil {
		t.Fatalf("redeem a second before expiry: %v", err)
	}
}

// TestDenyAndDecideOnce: a denial is final and redeems nothing; a decided
// request cannot be decided again either way.
func TestDenyAndDecideOnce(t *testing.T) {
	r := newRig(t)
	code, binding := r.start(t)
	if _, err := r.s.Decide(context.Background(), code, false, r.actor(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if s := r.state(t, binding); s != StateDenied {
		t.Fatalf("status: %s", s)
	}
	if _, err := r.s.Redeem(context.Background(), code, binding); !errors.Is(err, ErrDenied) {
		t.Fatalf("redeem a denial: %v; want ErrDenied", err)
	}
	if _, err := r.s.Decide(context.Background(), code, true, r.actor(), time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("approve after a denial: %v; want ErrNotFound", err)
	}
	code, _ = r.start(t)
	r.approve(t, code)
	if _, err := r.s.Decide(context.Background(), code, false, r.actor(), time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deny after an approval: %v; want ErrNotFound", err)
	}
}

// TestRedeemBeforeApproval: refused as not approved, and the request is left
// as it was.
func TestRedeemBeforeApproval(t *testing.T) {
	r := newRig(t)
	code, binding := r.start(t)
	if _, err := r.s.Redeem(context.Background(), code, binding); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("redeem a pending request: %v; want ErrNotApproved", err)
	}
	r.approve(t, code)
	if _, err := r.s.Redeem(context.Background(), code, binding); err != nil {
		t.Fatalf("redeem once approved: %v", err)
	}
}

// TestAnApprovalDiesWithTheApproversSession: a disable, a session cutoff after
// the approving sign-in, or a Sign out everywhere after it refuses the redeem;
// a cutoff BEFORE the approving sign-in does not (the control).
func TestAnApprovalDiesWithTheApproversSession(t *testing.T) {
	r := newRig(t)
	signin := time.Now().Add(-time.Hour)
	approved := func() (string, string) {
		code, binding := r.start(t)
		if _, err := r.s.Decide(context.Background(), code, true, r.actor(), signin); err != nil {
			t.Fatal(err)
		}
		return code, binding
	}
	reset := `UPDATE users SET disabled_at = NULL, sessions_not_before = NULL, signed_out_at = NULL WHERE id = $1`
	for name, q := range map[string]string{
		"disabled":                    `UPDATE users SET disabled_at = now() WHERE id = $1`,
		"a cutoff after the sign-in":  `UPDATE users SET sessions_not_before = now() WHERE id = $1`,
		"signed out everywhere since": `UPDATE users SET signed_out_at = now() WHERE id = $1`,
		"a cutoff in the same second": `UPDATE users SET sessions_not_before = $2 WHERE id = $1`,
	} {
		code, binding := approved()
		if name == "a cutoff in the same second" {
			r.exec(t, q, r.uid, signin)
		} else {
			r.exec(t, q, r.uid)
		}
		if _, err := r.s.Redeem(context.Background(), code, binding); !errors.Is(err, ErrApproverEnded) {
			t.Errorf("%s: redeem %v; want ErrApproverEnded", name, err)
		}
		r.exec(t, reset, r.uid)
	}
	code, binding := approved()
	r.exec(t, `UPDATE users SET sessions_not_before = $2 WHERE id = $1`, r.uid, signin.Add(-time.Minute))
	if _, err := r.s.Redeem(context.Background(), code, binding); err != nil {
		t.Fatalf("control, a cutoff before the approving sign-in: %v", err)
	}
}

// TestDecideNeedsAnApprover: no approver or no sign-in time is refused before
// any row moves.
func TestDecideNeedsAnApprover(t *testing.T) {
	r := newRig(t)
	code, binding := r.start(t)
	if _, err := r.s.Decide(context.Background(), code, true, users.Actor{}, time.Now()); !errors.Is(err, ErrNoApprover) {
		t.Errorf("no approver: %v", err)
	}
	if _, err := r.s.Decide(context.Background(), code, true, r.actor(), time.Time{}); !errors.Is(err, ErrNoApprover) {
		t.Errorf("no sign-in time: %v", err)
	}
	if s := r.state(t, binding); s != StatePending {
		t.Fatalf("after the refusals: %s", s)
	}
}

// TestTheDetailsAreClipped: the User-Agent summary and address are bounded
// and lose control characters (they are drawn on the phone).
func TestTheDetailsAreClipped(t *testing.T) {
	if got := clip("a\x00b\nc\x7fd", 10); got != "abcd" {
		t.Fatalf("clip = %q", got)
	}
	long := ""
	for i := 0; i < 200; i++ {
		long += "é"
	}
	if got := []rune(clip(long, MaxAgentLen)); len(got) != MaxAgentLen {
		t.Fatalf("clip to %d runes gave %d", MaxAgentLen, len(got))
	}
}
