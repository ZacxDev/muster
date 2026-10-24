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

func limitErr(resetsAt time.Time, prompt string) *mirror.TurnError {
	e := &mirror.TurnError{Kind: mirror.TurnErrorLimit, Prompt: prompt, Quick: 1}
	if !resetsAt.IsZero() {
		e.ResetsAt = resetsAt.UnixMilli()
	}
	return e
}

func TestDecideHoldsOnAUsageLimitUntilItsResetPlusTheMargin(t *testing.T) {
	now := time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)
	reset := now.Add(90 * time.Minute)
	st := idleSince(now.Add(-time.Hour))
	st.TurnError = limitErr(reset, "")
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
		if !c.send && (!d.Held || !d.ResumeAt.Equal(reset) || !strings.HasPrefix(d.Why, LimitHeldPrefix)) {
			t.Fatalf("at %s: Decide = %+v; want held, resuming at the reset %s, saying %q…", c.at, d, reset, LimitHeldPrefix)
		}
	}
}

// TestALimitWithNoResetTimeHoldsUntilARealAnswer: however long it lasts; the
// positive control is the same head once the host reports no turn error (a
// real answer came).
func TestALimitWithNoResetTimeHoldsUntilARealAnswer(t *testing.T) {
	now := time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)
	st := idleSince(now.Add(-time.Hour))
	st.TurnError = limitErr(time.Time{}, "")
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

func TestTheCircuitBreakerHoldsOnQuickFailuresInARow(t *testing.T) {
	now := time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC)
	h := Head{Item: Item{State: Queued}}
	for _, c := range []struct {
		name string
		e    *mirror.TurnError
		held bool
	}{
		{"no error", nil, false},
		{"one quick failure", &mirror.TurnError{Kind: "authentication_failed", Quick: 1}, false},
		{"BreakerAfter quick failures", &mirror.TurnError{Kind: "authentication_failed", Quick: BreakerAfter}, true},
		{"more", &mirror.TurnError{Kind: "server_error", Quick: BreakerAfter + 3}, true},
		{"an error that was not quick", &mirror.TurnError{Kind: "server_error"}, false},
		// A limit that has RESET is not held by the breaker: those quick turns
		// were the limit's.
		{"a reset limit's quick turns", &mirror.TurnError{Kind: mirror.TurnErrorLimit, ResetsAt: now.Add(-time.Hour).UnixMilli(), Quick: 5}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := idleSince(now.Add(-time.Hour))
			st.TurnError = c.e
			d := Decide(h, st, now, idle)
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
	e := limitErr(now.Add(time.Hour), "")
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

// TestRejected is the one rule for "a usage limit rejected this sent message
// unprocessed", each condition's own case against the case that passes.
func TestRejected(t *testing.T) {
	body := "please run the migration"
	ok := Item{State: Sent, Body: body, AfterSince: 1000}
	okSt := AgentState{Idle: true, Since: 2000, TurnError: limitErr(time.Time{}, mirror.PromptDigest(body))}
	if !Rejected(ok, okSt) {
		t.Fatal("CONTROL: the rejected case is not Rejected")
	}
	mut := func(f func(*Item, *AgentState)) (Item, AgentState) {
		it, st := ok, okSt
		e := *okSt.TurnError
		st.TurnError = &e
		f(&it, &st)
		return it, st
	}
	for name, f := range map[string]func(*Item, *AgentState){
		"not sent (failed)":                    func(it *Item, _ *AgentState) { it.State = Failed },
		"not idle":                             func(_ *Item, st *AgentState) { st.Idle = false },
		"no status newer than its paste":       func(_ *Item, st *AgentState) { st.Since = 1000 },
		"no turn error (a real answer)":        func(_ *Item, st *AgentState) { st.TurnError = nil },
		"not a usage limit":                    func(_ *Item, st *AgentState) { st.TurnError.Kind = "authentication_failed" },
		"processed first (no prompt named)":    func(_ *Item, st *AgentState) { st.TurnError.Prompt = "" },
		"another prompt (a person's own Send)": func(_ *Item, st *AgentState) { st.TurnError.Prompt = mirror.PromptDigest("something else") },
	} {
		if it, st := mut(f); Rejected(it, st) {
			t.Errorf("%s: Rejected; want not", name)
		}
	}
}
