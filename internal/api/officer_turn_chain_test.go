package api

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestRecordingATurnKeepsTheFreshPoolsAcceptance (round 1's Y3): for an
// instance of a fresh_session_per_item pool, OfficerTurnContext both records
// the turn's prompt row once ccd accepts it (registerOfficerTurn) AND keeps
// the pool's own acceptance callback (officerConversationAccepted): the
// recording CHAINS to it. Played here by a ccd that accepts the turn without
// naming a new conversation — the pool's callback marks the instance stale.
func TestRecordingATurnKeepsTheFreshPoolsAcceptance(t *testing.T) {
	r := newFreshOfficerRig(t)
	j1 := r.addNamed(offTJudge+"1", 982)
	if r.s.officerRT.isStale(j1.Name) {
		t.Fatal("control: the instance is stale before any turn")
	}
	tctx := r.s.OfficerTurnContext(agents.WithTurnInputRow(context.Background(), 4242), j1)
	if r.s.turnInputs.only(j1.ID) != 0 {
		t.Fatal("the turn is recorded before ccd accepted it")
	}
	agents.MarkTurnAccepted(tctx, agents.TurnAcceptance{}) // accepted, no new conversation named
	if got := r.s.turnInputs.only(j1.ID); got != 4242 {
		t.Fatalf("after ccd's acceptance the turn's record is %d, want its prompt row 4242", got)
	}
	if !r.s.officerRT.isStale(j1.Name) {
		t.Fatal("the fresh pool's acceptance callback did not run: recording the turn replaced it instead of chaining to it")
	}
}
