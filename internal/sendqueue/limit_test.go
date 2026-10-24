package sendqueue

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE HOLD RULES (limit.go), as a pure function of the head, the target's state
// and the clock: Decide's own, so the composer's "Held — …" and the
// dispatcher's restraint are the same decision.

// limitErr is a usage limit resetting at resetsAt (zero: none named);
// rejected: it was its prompt's only reply.
func limitErr(resetsAt time.Time, rejected bool) *mirror.TurnError {
	e := &mirror.TurnError{Kind: mirror.TurnErrorLimit, Rejected: rejected, Quick: 1}
	if !resetsAt.IsZero() {
		e.ResetsAt = resetsAt.UnixMilli()
	}
	return e
}

func TestDecideHoldsOnAUsageLimitUntilItsResetPlusTheMargin(t *testing.T) {
	now := time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)
	reset := now.Add(90 * time.Minute)
	st := idleSince(now.Add(-time.Hour))
	st.TurnError = limitErr(reset, true)
	h := Head{Item: Item{State: Queued}}
	for _, c := range []struct {
		at   time.Time
		send bool
	}{
		{now, false},
		{reset.Add(-time.Second), false},
		{reset, false}, // the reset minute itself: not yet
		{reset.Add(LimitMargin - time.Second), false}, // inside the margin
		{reset.Add(LimitMargin), true},                // past it
		{reset.Add(24 * time.Hour), true},
	} {
		d := Decide(h, st, c.at, idle)
		if d.Send != c.send {
			t.Fatalf("at %s (reset %s): Decide = %+v; want send %t", c.at, reset, d, c.send)
		}
		if !c.send && (!d.Held || !d.ResumeAt.Equal(reset) || d.Why != LimitHeldText) {
			t.Fatalf("at %s: Decide = %+v; want held, resuming at the reset %s, saying %q", c.at, d, reset, LimitHeldText)
		}
	}
}

// TestTheHeldWordsCarryNoTime: the time a held queue resumes is ResumeAt,
// drawn in the reader's zone; the words never carry the server's rendering of
// it, which would disagree with the reader's.
func TestTheHeldWordsCarryNoTime(t *testing.T) {
	for _, s := range []string{LimitHeldText, LimitHeldNoReset, BreakerHeldText(&mirror.TurnError{Kind: "server_error", Quick: 3})} {
		if strings.ContainsAny(strings.ReplaceAll(s, "last 3", ""), "0123456789:") {
			t.Fatalf("%q carries a time", s)
		}
	}
}

// TestALimitWithNoResetTimeHoldsUntilARealAnswer: however long it lasts; the
// positive control is the same head once the host reports no turn error (a
// real answer came).
func TestALimitWithNoResetTimeHoldsUntilARealAnswer(t *testing.T) {
	now := time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)
	st := idleSince(now.Add(-time.Hour))
	st.TurnError = limitErr(time.Time{}, true)
	h := Head{Item: Item{State: Queued}}
	for _, after := range []time.Duration{0, time.Hour, 7 * 24 * time.Hour} {
		if d := Decide(h, st, now.Add(after), idle); d.Send || !d.Held || d.Why != LimitHeldNoReset || !d.ResumeAt.IsZero() {
			t.Fatalf("%s later: Decide = %+v; want held, saying %q", after, d, LimitHeldNoReset)
		}
	}
	st.TurnError = nil
	if d := Decide(h, st, now, idle); !d.Send {
		t.Fatalf("CONTROL, a real answer: Decide = %+v; want send", d)
	}
}

// TestTheBreakerBacksOff1_2_5Then15Minutes is the back-off schedule on an
// injected clock: BreakerAfter quick failures in a row hold for 1 minute from
// when muster saw the failure, each further one for 2, 5, then 15 — and 15
// again after that; one second before each the queue is held, saying when it
// retries; at it, it sends.
func TestTheBreakerBacksOff1_2_5Then15Minutes(t *testing.T) {
	seen := time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)
	h := Head{Item: Item{State: Queued}}
	for _, c := range []struct {
		quick int
		hold  time.Duration
	}{
		{BreakerAfter, time.Minute},
		{BreakerAfter + 1, 2 * time.Minute},
		{BreakerAfter + 2, 5 * time.Minute},
		{BreakerAfter + 3, 15 * time.Minute},
		{BreakerAfter + 4, 15 * time.Minute},
		{BreakerAfter + 9, 15 * time.Minute},
	} {
		e := &mirror.TurnError{Kind: "authentication_failed", Quick: c.quick}
		st := AgentState{Idle: true, Since: 1, Observed: seen, TurnError: e}
		before := Decide(h, st, seen.Add(c.hold-time.Second), 0)
		if before.Send || !before.Held || !before.ResumeAt.Equal(seen.Add(c.hold)) || before.Why != BreakerHeldText(e) {
			t.Fatalf("quick %d, a second before %s: Decide = %+v; want held until %s", c.quick, c.hold, before, seen.Add(c.hold))
		}
		if at := Decide(h, st, seen.Add(c.hold), 0); !at.Send {
			t.Fatalf("quick %d, at %s: Decide = %+v; want send (the back-off has run)", c.quick, c.hold, at)
		}
	}
}

func TestTheCircuitBreakerHoldsOnQuickFailuresInARow(t *testing.T) {
	now := time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)
	h := Head{Item: Item{State: Queued}}
	for _, c := range []struct {
		name string
		e    *mirror.TurnError
		held bool
	}{
		{"no error (a real answer resets it)", nil, false},
		{"one quick failure", &mirror.TurnError{Kind: "authentication_failed", Quick: 1}, false},
		{"BreakerAfter quick failures", &mirror.TurnError{Kind: "authentication_failed", Quick: BreakerAfter}, true},
		{"more", &mirror.TurnError{Kind: "server_error", Quick: BreakerAfter + 3}, true},
		{"an error that was not quick", &mirror.TurnError{Kind: "server_error"}, false},
		// A limit that has RESET is not held by the breaker: those quick turns
		// were the limit's.
		{"a reset limit's quick turns", &mirror.TurnError{Kind: mirror.TurnErrorLimit, ResetsAt: now.Add(-time.Hour).UnixMilli(), Quick: 5}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := AgentState{Idle: true, Since: 1, Observed: now.Add(-30 * time.Second), TurnError: c.e}
			d := Decide(h, st, now, idle/2)
			if d.Send == c.held || d.Held != c.held {
				t.Fatalf("Decide = %+v; want held %t", d, c.held)
			}
			if c.held && d.Why != BreakerHeldText(c.e) {
				t.Fatalf("Why = %q; want %q", d.Why, BreakerHeldText(c.e))
			}
		})
	}
}

// TestTheHoldComesAfterTheTurnChecks: a limit does not hide that the window is
// working, or that the previous message's turn has not ended — those say what
// is true now.
func TestTheHoldComesAfterTheTurnChecks(t *testing.T) {
	now := time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)
	e := limitErr(now.Add(time.Hour), true)
	busy := AgentState{Why: "Claude Code is working.", TurnError: e}
	if d := Decide(Head{Item: Item{State: Queued}}, busy, now, idle); d.Held || d.Why != busy.Why {
		t.Fatalf("busy: %+v", d)
	}
	st := idleSince(now.Add(-time.Hour))
	st.TurnError = e
	if d := Decide(Head{Item: Item{State: Queued}, TurnAfter: st.Since}, st, now, idle); d.Held || !strings.Contains(d.Why, "previous message") {
		t.Fatalf("turn not ended: %+v", d)
	}
}

// TestBounced is the one rule for "a usage limit refused this sent message
// before Claude Code worked on it", each condition's own case against the case
// that passes.
func TestBounced(t *testing.T) {
	ok := Item{State: Sent, Body: "please run the migration", AfterSince: 1000}
	okSt := AgentState{Idle: true, Since: 2000, Pasted: 1000, TurnError: limitErr(time.Time{}, true)}
	if !Bounced(ok, okSt) {
		t.Fatal("CONTROL: the bounced case is not Bounced")
	}
	for name, f := range map[string]func(*Item, *AgentState){
		"not sent (failed)":              func(it *Item, _ *AgentState) { it.State = Failed },
		"not idle":                       func(_ *Item, st *AgentState) { st.Idle = false },
		"no status newer than its paste": func(_ *Item, st *AgentState) { st.Since = 1000 },
		"a paste through muster since":   func(_ *Item, st *AgentState) { st.Pasted = 1500 },
		"no turn error (a real answer)":  func(_ *Item, st *AgentState) { st.TurnError = nil },
		"not a usage limit":              func(_ *Item, st *AgentState) { st.TurnError.Kind = "authentication_failed" },
		"processed first (mid-turn)":     func(_ *Item, st *AgentState) { st.TurnError.Rejected = false },
	} {
		it, st := ok, okSt
		e := *okSt.TurnError
		st.TurnError = &e
		f(&it, &st)
		if Bounced(it, st) {
			t.Errorf("%s: Bounced; want not", name)
		}
	}
}
