package memorelay

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestAMemoToTheChiefGoesToItsPrimary (PR #609 round 1 F7): with SetSession —
// cmd/muster-server passes api.Server.DeliveryRoute — a memo turn runs in the
// session that hook picks (the chief's PRIMARY chat), never the agent's
// "latest", which can be a side chat. Without it, the latest, as before.
func TestAMemoToTheChiefGoesToItsPrimary(t *testing.T) {
	r := &Relay{agents: &fakeAgents{}}
	chief := agents.Agent{ID: 7, Name: "chief"}
	if _, s, err := r.sessionFor(context.Background(), chief); err != nil || s.ID != 70 {
		t.Fatalf("control: with no hook the memo goes to %d (%v), want the latest 70", s.ID, err)
	}
	r.SetSession(func(ctx context.Context, a agents.Agent) (context.Context, agents.ChatSession, error) {
		return ctx, agents.ChatSession{ID: 900 + a.ID, AgentID: a.ID, SessionKey: "primary", IsDefault: true}, nil
	})
	if _, s, err := r.sessionFor(context.Background(), chief); err != nil || s.ID != 907 {
		t.Errorf("with the hook the memo goes to %d (%v), want the primary 907", s.ID, err)
	}
}

// TestAMemoIsSentUnderItsSessionsRoute (PR #609 round 2 N3): the memo turn is
// sent under the context SetSession's hook returns — the session's ROUTE. ccd
// routes a turn by its X-Muster-CC-Session header, never by the body's session
// key, so a memo sent on the bare ctx went to ccd's original CLI: after a fresh
// primary, the archived conversation. Here the hook routes ada's memo to a
// pinned lane, as api.Server.DeliveryRoute does for a chief's lane primary.
func TestAMemoIsSentUnderItsSessionsRoute(t *testing.T) {
	w := newWorld(agent(1, "ada", "example/widgets"))
	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
	r := w.relay(t)
	r.SetSession(func(ctx context.Context, a agents.Agent) (context.Context, agents.ChatSession, error) {
		h := http.Header{}
		h.Set(agents.HeaderCCSession, "agent:ada:webchat:primary")
		h.Set(agents.HeaderCCPinned, "1")
		key := fmt.Sprintf("sess-%d", a.ID)
		return agents.WithTurnRoute(ctx, agents.TurnRoute{Runtime: agents.SessionRuntimeClaudeCode, Header: h}),
			agents.ChatSession{ID: 900, AgentID: a.ID, SessionKey: key, IsDefault: true}, nil
	})
	tick(t, r)
	sent := w.gw.sent()
	if len(sent) != 1 {
		t.Fatalf("%d memo turns, want 1", len(sent))
	}
	if sent[0].lane != "agent:ada:webchat:primary" || !sent[0].pinned {
		t.Errorf("the memo was sent on lane %q pinned=%v; want the primary's pinned lane", sent[0].lane, sent[0].pinned)
	}
}
