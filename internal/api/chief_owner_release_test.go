package api

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// turnsStore answers LatestUserTurns and KickoffClaimHolder with fixed values,
// whatever it is asked: a store whose own filter has drifted.
type turnsStore struct {
	agents.Store
	turns  agents.UserTurns
	holder string
}

func (s turnsStore) LatestUserTurns(context.Context, int64, int64) (agents.UserTurns, error) {
	return s.turns, nil
}
func (s turnsStore) KickoffClaimHolder(context.Context, int64) (string, error) { return s.holder, nil }

// TestOwnerRequestRechecksTheRowNotOnlyTheQuery: ownerRequest re-reads WHO typed
// the row and WHOSE chat it is in, rather than trusting the store's WHERE. A
// store that hands back an untyped row, another user's, or another agent's as
// "the owner's" releases nothing; the control — the owner's own row, newest,
// fresh, no claim — is accepted. Row ids, agent ids and user ids are pairwise
// distinct.
func TestOwnerRequestRechecksTheRowNotOnlyTheQuery(t *testing.T) {
	s := everyExtensionOn(t)
	chief := agents.Agent{ID: 31, Name: "chief", OwnerID: 7}
	now := time.Now()
	row := func(id, agent, by int64) agents.ChatMessage {
		return agents.ChatMessage{ID: id, AgentID: agent, Role: "user", Content: "do it", TypedBy: by, CreatedAt: now}
	}
	for _, c := range []struct {
		name string
		m    agents.ChatMessage
		want string
	}{
		{"an untyped row (a plugin delivery)", row(501, 31, 0), chiefReleaseNoOwnerMessage},
		{"another user's row", row(502, 31, 8), chiefReleaseNoOwnerMessage},
		{"another agent's row", row(503, 32, 7), chiefReleaseNoOwnerMessage},
		{"the owner's own row (control)", row(504, 31, 7), ""},
	} {
		s.ext.Agents = turnsStore{turns: agents.UserTurns{Latest: c.m, Typed: c.m}}
		got, why := s.ownerRequest(context.Background(), chief)
		if why != c.want {
			t.Errorf("%s: refusal %q, want %q", c.name, why, c.want)
		}
		if c.want == "" && got.ID != c.m.ID {
			t.Errorf("%s: resolved message %d, want %d", c.name, got.ID, c.m.ID)
		}
	}
	// A store without LatestUserTurns cannot vouch for anything.
	s.ext.Agents = listAgents{}
	if _, why := s.ownerRequest(context.Background(), chief); why != chiefReleaseUnverifiable {
		t.Errorf("a store without the read: %q, want %q", why, chiefReleaseUnverifiable)
	}
}
