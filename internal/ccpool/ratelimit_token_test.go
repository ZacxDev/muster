package ccpool

import (
	"context"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// A RATE-LIMIT MARK IS ABOUT ONE TOKEN. MarkFailure records the limited
// token's fingerprint; a mark whose fingerprint is not the account's CURRENT
// token does not apply (forToken), so re-issuing the token ends the cooldown
// the way it already clears an auth_failed mark. A mark with no fingerprint
// (written before the column existed) is honoured whatever the token.
// ---------------------------------------------------------------------------

// weekly is a rate-limit message whose stated reset is days after t0, so the
// mark is cooling at every clock these tests use unless something clears it.
const weekly = "You've hit your weekly limit · resets Jan 3, 9am (UTC)"

// coolingEverywhere is what every reader of a mark says about alpha: whether
// Choose ranks it after beta, ChooseFailover refuses it, States reports it
// cooling, and Pool.Cooldown reports it cooling (with the mark's time).
type coolingEverywhere struct {
	choose, failover, states, cooldown bool
	markedAt                           time.Time
}

func readersSay(t *testing.T, mark Mark) coolingEverywhere {
	t.Helper()
	now := t0.Add(time.Hour)
	two := []string{"alpha", "beta"}
	// beta is never rate-limited; alpha only loses to it while alpha is cooling
	// — EXCEPT that alpha, rate-limited and not cooling, ranks after a
	// never-limited beta too. So beta gets an older, expired mark: alpha then
	// wins the ranking exactly when its own mark does not apply.
	marks := map[string]Mark{
		"alpha": mark,
		"beta":  {RateLimitedAt: t0.Add(-48 * time.Hour), RateLimitedDetail: "You've hit your limit", RateLimitedToken: Fingerprint(tokBeta)},
	}
	var got coolingEverywhere
	c, err := Choose(now, testCooldown, two, tokens, marks, nil)
	if err != nil {
		t.Fatal(err)
	}
	got.choose = c != "alpha"
	_, ok := ChooseFailover(now, testCooldown, []string{"alpha", "gamma"}, tokens, marks, "gamma")
	got.failover = !ok

	st := &fakeStore{marks: marks}
	src, err := NewStatic(tokens)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewWithLimits(src, st, Limits{Cooldown: testCooldown})
	if err != nil {
		t.Fatal(err)
	}
	p.now = func() time.Time { return now }
	states, err := p.States(context.Background(), opOwner)
	if err != nil {
		t.Fatal(err)
	}
	got.states = !states["alpha"].CoolingUntil.IsZero()
	cd, err := p.Cooldown(context.Background(), opOwner, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	got.cooldown, got.markedAt = !cd.Until.IsZero(), cd.MarkedAt
	if until, _ := p.CoolingUntil(context.Background(), opOwner, "alpha"); !until.Equal(cd.Until) {
		t.Fatalf("CoolingUntil = %s, Cooldown.Until = %s: two answers to one question", until, cd.Until)
	}
	return got
}

// TestARateLimitMarkUnderAReplacedTokenIsIgnored (d): alpha's weekly limit was
// hit under a token that is no longer alpha's — the token was re-issued — so
// every reader treats alpha as not cooling. The control: the same mark under
// alpha's CURRENT token is honoured by every reader.
func TestARateLimitMarkUnderAReplacedTokenIsIgnored(t *testing.T) {
	stale := Mark{RateLimitedAt: t0, RateLimitedDetail: weekly, RateLimitedToken: Fingerprint("sk-ant-oat01-FAKE-alpha-the-OLD-token")}
	if got := readersSay(t, stale); got != (coolingEverywhere{}) {
		t.Fatalf("a mark under a replaced token: readers say %+v, want none cooling (the token changed)", got)
	}
	current := Mark{RateLimitedAt: t0, RateLimitedDetail: weekly, RateLimitedToken: Fingerprint(tokAlpha)}
	want := coolingEverywhere{choose: true, failover: true, states: true, cooldown: true, markedAt: t0}
	if got := readersSay(t, current); got != want {
		t.Fatalf("a mark under the current token: readers say %+v, want %+v", got, want)
	}
}

// TestARateLimitMarkWithNoFingerprintIsHonoured (e): a row written before the
// fingerprint was recorded has an empty rate_limited_token — it is honoured by every
// reader, whatever the token is now, exactly as before.
func TestARateLimitMarkWithNoFingerprintIsHonoured(t *testing.T) {
	old := Mark{RateLimitedAt: t0, RateLimitedDetail: weekly}
	want := coolingEverywhere{choose: true, failover: true, states: true, cooldown: true, markedAt: t0}
	if got := readersSay(t, old); got != want {
		t.Fatalf("an old mark with no fingerprint: readers say %+v, want %+v", got, want)
	}
}

// TestClearRateLimitIsACompareAndClear: the pool hands the store the mark's
// time it was given and clears only that mark; a zero time clears nothing and
// never reaches the store.
func TestClearRateLimitIsACompareAndClear(t *testing.T) {
	st := &fakeStore{marks: map[string]Mark{"alpha": {RateLimitedAt: t0, RateLimitedDetail: weekly, RateLimitedToken: Fingerprint(tokAlpha)}}}
	p := newPool(t, st)
	ctx := context.Background()
	if ok, err := p.ClearRateLimit(ctx, opOwner, "alpha", time.Time{}); ok || err != nil || len(st.calls) != 0 {
		t.Fatalf("zero markedAt: cleared=%v err=%v calls=%v, want nothing", ok, err, st.calls)
	}
	if ok, err := p.ClearRateLimit(ctx, opOwner, "alpha", t0.Add(-time.Second)); ok || err != nil || !st.marks["alpha"].RateLimitedAt.Equal(t0) {
		t.Fatalf("an older markedAt: cleared=%v err=%v mark=%+v, want the mark kept", ok, err, st.marks["alpha"])
	}
	if ok, err := p.ClearRateLimit(ctx, opOwner, "alpha", t0); !ok || err != nil || st.marks["alpha"] != (Mark{}) {
		t.Fatalf("the mark's own time: cleared=%v err=%v mark=%+v, want it cleared", ok, err, st.marks["alpha"])
	}
}
