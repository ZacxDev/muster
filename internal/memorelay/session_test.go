package memorelay

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestAMemoToTheChiefGoesToItsPrimary (PR #609 round 1 F7): with SetSession —
// cmd/muster-server passes api.Server.DeliverySession — a memo turn runs in the
// session that hook picks (the chief's PRIMARY chat), never the agent's
// "latest", which can be a side chat. Without it, the latest, as before.
func TestAMemoToTheChiefGoesToItsPrimary(t *testing.T) {
	r := &Relay{agents: &fakeAgents{}}
	chief := agents.Agent{ID: 7, Name: "chief"}
	if s, err := r.sessionFor(context.Background(), chief); err != nil || s.ID != 70 {
		t.Fatalf("control: with no hook the memo goes to %d (%v), want the latest 70", s.ID, err)
	}
	r.SetSession(func(_ context.Context, a agents.Agent) (agents.ChatSession, error) {
		return agents.ChatSession{ID: 900 + a.ID, AgentID: a.ID, SessionKey: "primary", IsDefault: true}, nil
	})
	if s, err := r.sessionFor(context.Background(), chief); err != nil || s.ID != 907 {
		t.Errorf("with the hook the memo goes to %d (%v), want the primary 907", s.ID, err)
	}
}
