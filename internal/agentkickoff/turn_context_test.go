package agentkickoff

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// turnCtxKey marks a context derived by Config.TurnContext.
type turnCtxKey struct{}

// TestAFirstTurnIsSentUnderTheConfiguredTurnContext: Config.TurnContext is
// handed the agent being kicked off and the context its first turn is sent
// under, and the turn is sent under what it returns. This is how an officer
// instance of a fresh_session_per_item pool gets its kickoff in a NEW
// conversation (internal/api OfficerTurnContext sets the route header there).
// The value carries the agent's NAME, so a TurnContext handed the wrong agent
// cannot pass.
func TestAFirstTurnIsSentUnderTheConfiguredTurnContext(t *testing.T) {
	row := ownedRow()
	h := newHarness(t, []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 2)}, row)
	var seen []string
	d, err := New(Config{
		Store: h.store, Instances: h.insts, Gateway: h.gw,
		NamespacePrefix: testPrefix, Owner: ownerID, Now: func() time.Time { return fixedNow },
		TurnContext: func(ctx context.Context, a agents.Agent) context.Context {
			seen = append(seen, a.Name)
			return context.WithValue(ctx, turnCtxKey{}, "route-for-"+a.Name)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.d = d
	log := h.tick(t)
	if h.gw.callCount() != 1 {
		t.Fatalf("the gateway was called %d time(s), want one first turn.%s", h.gw.callCount(), transcript(log))
	}
	if got, _ := h.gw.sendCtxs[0].Value(turnCtxKey{}).(string); got != "route-for-lively-newt" {
		t.Fatalf("the first turn was sent under %q, want the TurnContext's value %q (TurnContext saw %v)", got, "route-for-lively-newt", seen)
	}
}
