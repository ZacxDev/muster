package plugins

import (
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestACapacityRefusalLeavesTheMessageQueuedAndItIsDeliveredOnceALaneFrees
// (PR #609 round 1 F1): a chief whose primary is a ccd lane, with every lane
// mid-turn, is refused with ccd's typed 503 capacity — answered before any CLI
// is started or anything pasted (cmd/ccd TestACapacityRefusalPastesNothing).
// The message stays QUEUED, not settled unconfirmed (which is never sent
// again), and the next attempt, once a lane frees, delivers it exactly once.
func TestACapacityRefusalLeavesTheMessageQueuedAndItIsDeliveredOnceALaneFrees(t *testing.T) {
	s, _, gw, d, p, chief := deliverFixture(t)
	m, _, _ := s.Submit(plugin(2), p.ID, msg("cap-1"))
	gw.err = &agents.RuntimeError{Status: 503, Type: agents.FailureCapacity, Message: "every Claude Code session ccd may run at once is mid-turn"}
	if r := d.Deliver(plugin(2), p.ID, m.ID, chief); r.Outcome != OutcomePending || r.Reason != agents.FailureCapacity {
		t.Fatalf("a capacity refusal: %+v; want pending/%s", r, agents.FailureCapacity)
	}
	if got, _ := s.Message(plugin(2), m.ID); got.Status != StatusQueued || got.AgentID != nil {
		t.Fatalf("after a capacity refusal: %+v; want queued, no chief named", got)
	}
	gw.err = nil
	before := len(gw.sent)
	if r := d.Deliver(plugin(2), p.ID, m.ID, chief); r.Outcome != OutcomeDelivered {
		t.Fatalf("once a lane frees: %+v; want delivered", r)
	}
	if r := d.Deliver(plugin(2), p.ID, m.ID, chief); r.Outcome != OutcomeSettled || len(gw.sent) != before+1 {
		t.Fatalf("delivered %d time(s) (%+v); want exactly once", len(gw.sent)-before, r)
	}
}
