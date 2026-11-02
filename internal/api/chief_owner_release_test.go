package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// turnsStore is an agents store whose owner-turn marker, chat rows and session
// claim are fixed values, whatever it is asked — so ownerTurnFor's own checks
// are what is measured — and which records every Set/Clear.
type turnsStore struct {
	agents.Store
	turn   agents.OwnerTurn
	msgs   map[int64]agents.ChatMessage
	holder string
	sets   []int64 // message ids marked
	clears []int64 // message ids cleared
}

func (s *turnsStore) SetOwnerTurn(_ context.Context, _, _, messageID int64, _ time.Duration) error {
	s.sets = append(s.sets, messageID)
	return nil
}
func (s *turnsStore) ClearOwnerTurn(_ context.Context, _, messageID int64) error {
	s.clears = append(s.clears, messageID)
	return nil
}
func (s *turnsStore) OwnerTurnInFlight(context.Context, int64) (agents.OwnerTurn, error) {
	return s.turn, nil
}
func (s *turnsStore) ChatMessage(_ context.Context, id int64) (agents.ChatMessage, error) {
	m, ok := s.msgs[id]
	if !ok {
		return agents.ChatMessage{}, errors.New("no such row")
	}
	return m, nil
}
func (s *turnsStore) KickoffClaimHolder(context.Context, int64) (string, error) { return s.holder, nil }

// TestOwnerTurnForChecksEveryCondition: ownerTurnFor releases only on a marker
// naming the chief's OWNER, pointing at the chief's own user row, with no
// muster-started turn holding the claim — each failure its own sentence. Agent,
// user and row ids are pairwise distinct.
func TestOwnerTurnForChecksEveryCondition(t *testing.T) {
	s := everyExtensionOn(t)
	chief := agents.Agent{ID: 31, Name: "chief", OwnerID: 7}
	msgs := map[int64]agents.ChatMessage{
		501: {ID: 501, AgentID: 31, Role: "user", Content: "dispatch it"},
		502: {ID: 502, AgentID: 32, Role: "user", Content: "another agent's"},
		503: {ID: 503, AgentID: 31, Role: "assistant", Content: "a reply"},
	}
	for _, c := range []struct {
		name   string
		turn   agents.OwnerTurn
		holder string
		want   string
	}{
		{"no marker (a turn the owner did not type, or one the runtime refused)", agents.OwnerTurn{}, "", chiefReleaseNoOwnerTurn},
		{"a marker typed by someone else", agents.OwnerTurn{By: 8, MessageID: 501}, "", chiefReleaseNotOwnersTurn},
		{"a marker naming another agent's row", agents.OwnerTurn{By: 7, MessageID: 502}, "", chiefReleaseUnverifiable},
		{"a marker naming an assistant row", agents.OwnerTurn{By: 7, MessageID: 503}, "", chiefReleaseUnverifiable},
		{"a marker naming no row", agents.OwnerTurn{By: 7, MessageID: 599}, "", chiefReleaseUnverifiable},
		{"a muster-started turn in flight too", agents.OwnerTurn{By: 7, MessageID: 501}, "plugin-delivery/x", chiefReleaseOutsideTurn},
		{"the owner's turn alone (control)", agents.OwnerTurn{By: 7, MessageID: 501}, "", ""},
	} {
		s.ext.Agents = &turnsStore{turn: c.turn, msgs: msgs, holder: c.holder}
		got, why := s.ownerTurnFor(context.Background(), chief)
		if why != c.want {
			t.Errorf("%s: refusal %q, want %q", c.name, why, c.want)
		}
		if c.want == "" && got.ID != 501 {
			t.Errorf("%s: resolved message %d, want 501", c.name, got.ID)
		}
	}
	// A store without the marker cannot vouch for anything.
	s.ext.Agents = listAgents{}
	if _, why := s.ownerTurnFor(context.Background(), chief); why != chiefReleaseUnverifiable {
		t.Errorf("a store without the marker: %q, want %q", why, chiefReleaseUnverifiable)
	}
}

// TestMarkOwnerTurnMarksOnlyAnAcceptedOwnersTurn: the marker is set only when
// the typer IS the agent's owner AND the runtime accepted the text; a runtime
// that refused it (no MarkTurnAccepted) never marks; the clear always names
// the turn's own row.
func TestMarkOwnerTurnMarksOnlyAnAcceptedOwnersTurn(t *testing.T) {
	s := everyExtensionOn(t)
	a := agents.Agent{ID: 31, Name: "chief", OwnerID: 7}
	msg := agents.ChatMessage{ID: 501, AgentID: 31, Role: "user"}

	st := &turnsStore{}
	s.ext.Agents = st
	ctx, clear := s.markOwnerTurn(context.Background(), a, msg, 7)
	if len(st.sets) != 0 {
		t.Fatalf("marked before the runtime accepted: %v", st.sets)
	}
	agents.MarkTurnAccepted(ctx)
	agents.MarkTurnAccepted(ctx) // once only
	clear()
	if len(st.sets) != 1 || st.sets[0] != 501 || len(st.clears) != 1 || st.clears[0] != 501 {
		t.Fatalf("accepted owner turn: sets %v clears %v; want [501] [501]", st.sets, st.clears)
	}

	refused := &turnsStore{}
	s.ext.Agents = refused
	_, clear = s.markOwnerTurn(context.Background(), a, msg, 7) // the runtime never accepts
	clear()
	if len(refused.sets) != 0 {
		t.Fatalf("a refused turn marked: %v", refused.sets)
	}

	for _, by := range []int64{0, 8} { // nobody typed it; someone else typed it
		other := &turnsStore{}
		s.ext.Agents = other
		ctx, clear := s.markOwnerTurn(context.Background(), a, msg, by)
		agents.MarkTurnAccepted(ctx)
		clear()
		if len(other.sets) != 0 {
			t.Errorf("typed by %d: marked %v; only the owner's own turn marks", by, other.sets)
		}
	}
}
