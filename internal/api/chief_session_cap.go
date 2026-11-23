package api

import (
	"context"
	"encoding/json"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ui"
)

// AN OPENROUTER SESSION AT ITS BUDGET IS ANSWERED WITH A CARD, NOT AN ERROR
// (the operator's decision): the chat shows what was spent, the cap, when it
// resets (00:00 UTC), and "Continue in a new session" — onto a Claude model,
// whose turns do not spend the chief sessions' key.
//
// Two moments, one card (ui.ChatKindSpendCap, a stored assistant part):
//
//   - BEFORE the turn (chiefSessionCapBefore): the caps leave no room for a
//     minimal request (CheckChiefSessionSpend), so nothing is sent at all;
//   - AFTER a turn that FAILED (chiefSessionCapAfter): the proxy's 400 ended
//     it mid-way (the door's refusal reaches muster as the turn's error), and
//     the caps read as spent now. Any other failure stays the failure it was.
//
// A spend that cannot be read is no card: the turn goes on (the proxy door
// decides each request itself, by reservation), and a failed turn keeps its
// own error.

// chiefSessionCapBefore is the card for sess's next turn when its budget is
// spent; ok false for any session not on opencode, or with room.
func (s *Server) chiefSessionCapBefore(ctx context.Context, sess agents.ChatSession) ([]agents.TurnPart, bool) {
	if sess.Runtime != agents.SessionRuntimeOpencode || s.chiefSessions == nil {
		return nil, false
	}
	sp, err := s.CheckChiefSessionSpend(ctx, sess.ID)
	if err != nil {
		s.logger.Printf("chief sessions: session %d's spend before its turn: %v (the turn goes on; the proxy decides each request)", sess.ID, err)
		return nil, false
	}
	if sp.Refusal == "" {
		return nil, false
	}
	return []agents.TurnPart{chiefSessionCapPart(sp)}, true
}

// chiefSessionCapAfter is the card for sess's turn that failed with err, when
// the budget now reads as spent.
func (s *Server) chiefSessionCapAfter(ctx context.Context, sess agents.ChatSession, err error) ([]agents.TurnPart, bool) {
	if err == nil || ctx.Err() != nil {
		return nil, false
	}
	return s.chiefSessionCapBefore(ctx, sess)
}

// chiefSessionCapPart is the card's stored part: ui.SpendCapCard's fields as
// JSON, under ui.ChatKindSpendCap.
func chiefSessionCapPart(sp ChiefSessionSpend) agents.TurnPart {
	c := ui.SpendCapCard{DaySpentUSD: sp.DaySpentUSD, DayReservedUSD: sp.DayReservedUSD, DailyCapUSD: sp.DailyCapUSD,
		SessionSpentUSD: sp.SessionSpentUSD, SessionCapUSD: sp.SessionCapUSD,
		ResetsAt: sp.Day.AddDate(0, 0, 1).UTC(), Refusal: sp.Refusal}
	b, _ := json.Marshal(c)
	return agents.TurnPart{Kind: ui.ChatKindSpendCap, Content: string(b)}
}
