package agentkickoff

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// TestAClaudeCodeFirstTurnOutlastsItsRequestBudget: a first turn is bounded by
// a context; for claude-code that context must outlast the 35m request budget
// (agents.ClaudeCodeTurnTimeout), or the kickoff would cancel a turn its own
// HTTP client is still willing to wait for. The gateway kind keeps the
// configured turn exactly.
func TestAClaudeCodeFirstTurnOutlastsItsRequestBudget(t *testing.T) {
	d := &Deliverer{turn: 15 * time.Minute}
	if got := d.turnFor(agents.Agent{}); got != 15*time.Minute {
		t.Fatalf("gateway (unset kind) turn = %s, want the configured 15m", got)
	}
	if got := d.turnFor(agents.Agent{Kind: agents.KindGateway}); got != 15*time.Minute {
		t.Fatalf("gateway turn = %s, want 15m", got)
	}
	if got := d.turnFor(agents.Agent{Kind: agents.KindClaudeCode}); got != 37*time.Minute {
		t.Fatalf("claude-code turn = %s, want 37m (35m request + 2m slack)", got)
	}
	// A configured turn already longer than that wins.
	long := &Deliverer{turn: 90 * time.Minute}
	if got := long.turnFor(agents.Agent{Kind: agents.KindClaudeCode}); got != 90*time.Minute {
		t.Fatalf("claude-code turn with a longer configured turn = %s, want 90m", got)
	}
}

// notReadyGateway answers typed `not_ready` for the first n sends, then succeeds.
type notReadyGateway struct {
	*fakeGateway
	n int
}

func (g *notReadyGateway) Send(ctx context.Context, t agentgateway.Target, key, msg string, emit func(string)) (string, error) {
	g.mu.Lock()
	left := g.n
	if g.n > 0 {
		g.n--
	}
	g.mu.Unlock()
	reply, err := g.fakeGateway.Send(ctx, t, key, msg, emit)
	if left > 0 {
		return "", &agents.RuntimeError{Status: 503, Type: "not_ready", Message: "no SessionStart yet"}
	}
	return reply, err
}

func notReadyHarness(t *testing.T, n int) (*harness, *notReadyGateway) {
	h := newHarness(t, []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 0)}, ownedRow())
	g := &notReadyGateway{fakeGateway: h.gw, n: n}
	h.d.gw = g
	h.d.notReadyWait, h.d.notReadyPoll = 50*time.Millisecond, 5*time.Millisecond
	return h, g
}

// TestARuntimeNotReadyFirstTurnIsResentWithinOneDelivery: ccd's typed
// `not_ready` (nothing pasted) is re-sent inside the SAME delivery — one stamp,
// one transcript row, one attempt — and the turn then lands.
func TestARuntimeNotReadyFirstTurnIsResentWithinOneDelivery(t *testing.T) {
	h, _ := notReadyHarness(t, 2)
	log := h.tick(t)
	if h.gw.callCount() != 3 {
		t.Fatalf("sends = %d, want 3 (two not_ready, then the turn).%s", h.gw.callCount(), transcript(log))
	}
	if n := countPrefix(log, "SetKickedOff(7301,true)"); n != 1 {
		t.Fatalf("stamped %d time(s), want 1.%s", n, transcript(log))
	}
	if n := countPrefix(log, "RecordKickoffDelivery("); n != 1 {
		t.Fatalf("delivery recorded %d time(s), want 1.%s", n, transcript(log))
	}
	if r := h.store.row(7301); !r.KickedOff || agents.KickoffFailed(r) {
		t.Fatalf("kicked_off=%t failed=%t, want delivered", r.KickedOff, agents.KickoffFailed(r))
	}
}

// TestALastingNotReadyEndsAsAFailedKickoffNotALoop: a runtime that answers
// not_ready for ever (a TUI stuck on a login screen) is bounded: the delivery
// ends as an ordinary recorded failure, and the next tick does NOT send again.
// An untyped 503 is not re-sent at all (the control).
func TestALastingNotReadyEndsAsAFailedKickoffNotALoop(t *testing.T) {
	h, _ := notReadyHarness(t, 1<<30)
	h.tick(t)
	sent := h.gw.callCount()
	if sent < 2 {
		t.Fatalf("control: not_ready was not re-sent at all (%d send)", sent)
	}
	h.tick(t)
	if h.gw.callCount() != sent {
		t.Fatalf("a second tick sent again (%d -> %d): the kickoff loops", sent, h.gw.callCount())
	}
	if r := h.store.row(7301); !agents.KickoffFailed(r) {
		t.Fatalf("a lasting not_ready is not recorded as a failed kickoff: %+v", r.KickoffError)
	}

	h2 := newHarness(t, []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 0)}, ownedRow())
	h2.d.notReadyWait, h2.d.notReadyPoll = 50*time.Millisecond, 5*time.Millisecond
	h2.gw.err = &agents.RuntimeError{Status: 503}
	h2.tick(t)
	h2.tick(t)
	if h2.gw.callCount() != 1 || !agents.KickoffFailed(h2.store.row(7301)) {
		t.Fatalf("an untyped 503 was re-sent (%d sends) or not recorded as failed", h2.gw.callCount())
	}
}
