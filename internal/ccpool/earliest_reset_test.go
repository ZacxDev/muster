package ccpool

import (
	"context"
	"testing"
	"time"
)

// earliestNow is the pool's clock in the EarliestReset tests.
var earliestNow = time.Date(2026, time.November, 21, 0, 0, 0, 0, time.UTC)

// TestEarliestResetThroughThePoolsStates reads the earliest reset of a pool
// through the pool itself — real marks, the CLI's limit messages, the pool's
// clock and cooldown, Pool.States — rather than hand-built states. The three
// accounts reset at pairwise distinct times, and the one marked LAST (alpha,
// the account a failover tries last) resets LATEST, so neither "the newest
// mark" nor "the latest reset" can pass for the earliest.
func TestEarliestResetThroughThePoolsStates(t *testing.T) {
	const (
		alphaLimit = "You've hit your weekly limit · resets Nov 24, 2pm (UTC)"
		betaLimit  = "You've hit your weekly limit · resets Nov 23, 1am (UTC)"
		gammaLimit = "You've hit your weekly limit · resets Nov 23, 6:30pm (UTC)"
	)
	marked := func(at time.Time, detail string) Mark {
		return Mark{RateLimitedAt: at, RateLimitedDetail: detail}
	}
	exhausted := func() map[string]Mark {
		return map[string]Mark{
			"alpha": marked(earliestNow.Add(-1*time.Hour), alphaLimit),
			"beta":  marked(earliestNow.Add(-3*time.Hour), betaLimit),
			"gamma": marked(earliestNow.Add(-2*time.Hour), gammaLimit),
		}
	}
	for _, tc := range []struct {
		name   string
		marks  func() map[string]Mark
		want   time.Time
		wantOK bool
	}{
		{"every account cooling: the earliest stated reset", exhausted,
			time.Date(2026, time.November, 23, 1, 0, 0, 0, time.UTC), true},
		{"an account never marked: not exhausted", func() map[string]Mark {
			m := exhausted()
			delete(m, "gamma")
			return m
		}, time.Time{}, false},
		{"an account whose stated reset is already past: not exhausted", func() map[string]Mark {
			m := exhausted()
			m["gamma"] = marked(time.Date(2026, time.November, 19, 12, 0, 0, 0, time.UTC),
				"You've hit your weekly limit · resets Nov 20, 1am (UTC)")
			return m
		}, time.Time{}, false},
		{"an auth_failed account is skipped", func() map[string]Mark {
			m := exhausted()
			m["beta"] = Mark{AuthFailedAt: earliestNow.Add(-time.Hour), AuthFailedToken: Fingerprint(tokBeta), AuthFailedDetail: "401"}
			return m
		}, time.Date(2026, time.November, 23, 18, 30, 0, 0, time.UTC), true},
		{"the earliest cooldown is the configured one after an unreadable message: no reset to quote", func() map[string]Mark {
			m := exhausted()
			m["gamma"] = marked(earliestNow.Add(-10*time.Minute), "You've hit your limit")
			return m
		}, time.Time{}, false},
		{"no marks at all", func() map[string]Mark { return map[string]Mark{} }, time.Time{}, false},
		{"every account auth_failed", func() map[string]Mark {
			m := map[string]Mark{}
			for n, tok := range tokens {
				m[n] = Mark{AuthFailedAt: earliestNow.Add(-time.Hour), AuthFailedToken: Fingerprint(tok), AuthFailedDetail: "401"}
			}
			return m
		}, time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPool(t, &fakeStore{marks: tc.marks()})
			p.now = func() time.Time { return earliestNow }
			states, err := p.States(context.Background(), opOwner)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := EarliestReset(states, earliestNow)
			if ok != tc.wantOK || !got.Equal(tc.want) {
				t.Fatalf("EarliestReset = %s, %v; want %s, %v (states %+v)", got, ok, tc.want, tc.wantOK, states)
			}
		})
	}
}

// TestStatesSaysWhetherACooldownIsAStatedReset: ResetStated is true for a
// cooldown the limit message stated, false for the configured cooldown after
// an unreadable one, and false (with a zero CoolingUntil) once it is over.
func TestStatesSaysWhetherACooldownIsAStatedReset(t *testing.T) {
	p := newPool(t, &fakeStore{marks: map[string]Mark{
		"alpha": {RateLimitedAt: earliestNow.Add(-time.Hour), RateLimitedDetail: "You've hit your weekly limit · resets Nov 23, 1am (UTC)"},
		"beta":  {RateLimitedAt: earliestNow.Add(-10 * time.Minute), RateLimitedDetail: "You've hit your limit"},
		"gamma": {RateLimitedAt: earliestNow.Add(-2 * time.Hour), RateLimitedDetail: "You've hit your limit"},
	}})
	p.now = func() time.Time { return earliestNow }
	st, err := p.States(context.Background(), opOwner)
	if err != nil {
		t.Fatal(err)
	}
	if a := st["alpha"]; !a.ResetStated || !a.CoolingUntil.Equal(time.Date(2026, time.November, 23, 1, 0, 0, 0, time.UTC)) {
		t.Errorf("alpha = %+v; want cooling until Nov 23 01:00 UTC, stated", a)
	}
	if b := st["beta"]; b.ResetStated || !b.CoolingUntil.Equal(earliestNow.Add(50*time.Minute)) {
		t.Errorf("beta = %+v; want cooling until mark+1h (the default cooldown), not stated", b)
	}
	if g := st["gamma"]; g.ResetStated || !g.CoolingUntil.IsZero() {
		t.Errorf("gamma = %+v; want not cooling, not stated", g)
	}
}

// TestEarliestResetIsNeverInThePast: a CoolingUntil at or before now (a
// stale reading) is an account no longer cooling, never a reset to quote.
func TestEarliestResetIsNeverInThePast(t *testing.T) {
	future := earliestNow.Add(48 * time.Hour)
	for _, past := range []time.Time{earliestNow, earliestNow.Add(-time.Minute)} {
		states := map[string]AccountState{
			"alpha": {CoolingUntil: future, ResetStated: true},
			"beta":  {CoolingUntil: past, ResetStated: true},
		}
		if got, ok := EarliestReset(states, earliestNow); ok {
			t.Errorf("beta cooling until %s at now %s: EarliestReset = %s, true; want false", past, earliestNow, got)
		}
	}
}

// TestResetPhraseAtIsTheCLIsShape pins the phrase literally, and checks that
// the pool's own reader (ResetPhrase, statedReset) reads back exactly the time
// it was written from.
func TestResetPhraseAtIsTheCLIsShape(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("no zoneinfo: %v", err)
	}
	for _, tc := range []struct {
		at   time.Time
		want string
	}{
		{time.Date(2026, time.November, 23, 1, 0, 0, 0, time.UTC), "resets Nov 23, 1am (UTC)"},
		{time.Date(2026, time.November, 24, 14, 0, 0, 0, time.UTC), "resets Nov 24, 2pm (UTC)"},
		{time.Date(2026, time.November, 23, 21, 45, 0, 0, time.UTC), "resets Nov 23, 9:45pm (UTC)"},
		{time.Date(2026, time.November, 23, 0, 0, 0, 0, time.UTC), "resets Nov 23, 12am (UTC)"},
		{time.Date(2026, time.November, 23, 12, 5, 0, 0, time.UTC), "resets Nov 23, 12:05pm (UTC)"},
		// Between minutes: rounded UP, never told back before it is.
		{time.Date(2026, time.November, 23, 1, 0, 30, 0, time.UTC), "resets Nov 23, 1:01am (UTC)"},
		{time.Date(2026, time.November, 23, 23, 59, 1, 0, time.UTC), "resets Nov 24, 12am (UTC)"},
		// Another zone is stated in UTC.
		{time.Date(2026, time.November, 23, 2, 0, 0, 0, berlin), "resets Nov 23, 1am (UTC)"},
	} {
		got := ResetPhraseAt(tc.at)
		if got != tc.want {
			t.Errorf("ResetPhraseAt(%s) = %q; want %q", tc.at, got, tc.want)
			continue
		}
		if back := ResetPhrase("You've hit your weekly limit · " + got); back != got {
			t.Errorf("ResetPhrase does not read %q back: %q", got, back)
		}
		rounded := tc.at.UTC().Truncate(time.Minute)
		if !rounded.Equal(tc.at) {
			rounded = rounded.Add(time.Minute)
		}
		if r, ok := statedReset(got, tc.at.Add(-time.Hour)); !ok || !r.Equal(rounded) {
			t.Errorf("statedReset(%q) = %s, %v; want %s", got, r, ok, rounded)
		}
	}
}
