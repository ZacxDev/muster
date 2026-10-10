package agentkickoff

import (
	"strings"
	"testing"
	"time"

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

// TestARuntimeNotReadyFirstTurnIsRetriedNotFailed: ccd answers a turn that
// arrives before the CLI's SessionStart with a typed `503 not_ready` and pastes
// nothing. That one post-stamp failure is un-stamped and retried; the next tick
// delivers. An UNTYPED 503 (the control) is not read as "nothing sent".
func TestARuntimeNotReadyFirstTurnIsRetriedNotFailed(t *testing.T) {
	h := newHarness(t, []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 0)}, ownedRow())
	h.gw.err = &agents.RuntimeError{Status: 503, Type: "not_ready", Message: "no SessionStart yet"}
	log := h.tick(t)
	if r := h.store.row(7301); !agents.KickoffOwed(r) || agents.KickoffFailed(r) ||
		!strings.HasPrefix(r.KickoffError, RuntimeNotReadyReason) {
		t.Fatalf("after not_ready: owed=%t failed=%t error=%q, want owed, not failed, the retry reason.%s",
			agents.KickoffOwed(r), agents.KickoffFailed(r), r.KickoffError, transcript(log))
	}
	h.gw.err = nil
	h.tick(t)
	if h.gw.callCount() != 2 {
		t.Fatalf("the retry did not run: %d turn(s)", h.gw.callCount())
	}
	if r := h.store.row(7301); !r.KickedOff || agents.KickoffOwed(r) {
		t.Fatalf("after the retry: kicked_off=%t owed=%t", r.KickedOff, agents.KickoffOwed(r))
	}

	// Control: an untyped 503 is an ordinary post-stamp failure, never retried.
	h2 := newHarness(t, []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 0)}, ownedRow())
	h2.gw.err = &agents.RuntimeError{Status: 503}
	h2.tick(t)
	h2.tick(t)
	if h2.gw.callCount() != 1 || !agents.KickoffFailed(h2.store.row(7301)) {
		t.Fatalf("an untyped 503 was retried (%d turns) or not recorded as failed", h2.gw.callCount())
	}
}
