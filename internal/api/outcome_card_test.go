package api

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agentoutcome"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/pins"
	"github.com/ZacxDev/muster/internal/ui"
)

// recordOutcome writes agent id's outcome through the outcome store, as the
// tracker does (Start, then the transition to outcome).
func recordOutcome(t *testing.T, pool *pgxpool.Pool, id int64, outcome, reason string) {
	t.Helper()
	st, ctx, now := agentoutcome.NewPG(pool), authz.System(context.Background()), time.Now()
	if err := st.Start(ctx, id, nil, now); err != nil {
		t.Fatal(err)
	}
	switch outcome {
	case agentoutcome.Failed:
		if _, err := st.Fail(ctx, id, reason, true, now); err != nil {
			t.Fatal(err)
		}
	case agentoutcome.Running:
	default:
		if ok, err := st.Transition(ctx, id, []string{agentoutcome.Running}, outcome, reason, now); err != nil || !ok {
			t.Fatalf("transition to %s: %v %v", outcome, ok, err)
		}
	}
}

// outcomeRow is an agent row as the store reads it back with outcome recorded:
// running, kicked off, nothing else going on.
func outcomeRow(outcome, reason string) agents.Agent {
	at := time.Now()
	a := agents.Agent{ID: 4242, Name: "keen-wren", DisplayName: "#7 · fix the build", Status: agents.StatusRunning,
		Kind: agents.KindGateway, KickedOff: true, Outcome: outcome, OutcomeReason: reason, OutcomeAt: &at, ActivityAt: &at}
	if outcome == agentoutcome.Failed {
		a.Status = agents.StatusError // Fail writes the status with the outcome
	}
	return a
}

// TestADispatchedAgentsCardReadsItsRecordedOutcome: the chief panel's card for
// a stuck or failed dispatched agent says stuck / failed with the reason the
// agent page shows, and a done one says done — from the recorded outcome
// (outcomeCardState wired to AgentOutcomeOf).
//
// 🔴 RED WITHOUT THE WIRING: #496 shipped outcomeCardState returning nothing,
// so a stuck agent read "waiting for you" and a failed one a bare "error".
func TestADispatchedAgentsCardReadsItsRecordedOutcome(t *testing.T) {
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	for _, c := range []struct {
		outcome, reason, state string
	}{
		{agentoutcome.Stuck, agentoutcome.ReasonNoReply, ui.DispatchStuck},
		{agentoutcome.Failed, "first turn failed: runtime turn failed: FailoverError: Unknown model: openai/Acme v9 ultra", ui.DispatchFailed},
		{agentoutcome.Done, "", ui.DispatchDone},
		{agentoutcome.Waiting, "Which repository should I work in?", ui.DispatchWaiting},
	} {
		state, detail, ok := s.outcomeCardState(outcomeRow(c.outcome, c.reason))
		if !ok || state != c.state || detail != c.reason {
			t.Errorf("%s: card (%q, %q, %v); want (%q, %q, true)", c.outcome, state, detail, ok, c.state, c.reason)
		}
	}
	for _, o := range []string{"", agentoutcome.Running} {
		if state, _, ok := s.outcomeCardState(outcomeRow(o, "")); ok {
			t.Errorf("outcome %q drew %q; want it left to the derived states", o, state)
		}
	}
	// The card's ONE function: a recorded outcome wins over what is derived.
	if state, _ := s.dispatchCardState(context.Background(), outcomeRow(agentoutcome.Stuck, "silent for 30 min"), agents.StatusRunning); state != ui.DispatchStuck {
		t.Errorf("dispatchCardState for a stuck agent = %q; want stuck", state)
	}
	// The reason is scrubbed of the agent's note, as on the page.
	a := outcomeRow(agentoutcome.Failed, "a turn failed: echoed the secret note text")
	a.PendingNote = "the secret note text"
	if _, detail, _ := s.outcomeCardState(a); strings.Contains(detail, "secret note") {
		t.Errorf("the card's detail carries the note: %q", detail)
	}
}

// TestTheCardAndThePageAgreeOnEveryOutcome is the consolidation guard (one rule
// for an agent's end): for EVERY outcome, the chief panel's card state and the
// agent page's view (cardViewIndexed → the strip's badge and notice) say the
// same thing — failed, stuck and done as themselves, running and untracked as
// neither — and the card adds no end-state of its own (a failed first turn or
// a task in review is the outcome's to say, not a card rule beside it).
func TestTheCardAndThePageAgreeOnEveryOutcome(t *testing.T) {
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: noClaimAgents{}})
	never := func(agents.Agent) bool { return false }
	for _, o := range []string{"", agentoutcome.Running, agentoutcome.Waiting, agentoutcome.Done, agentoutcome.Failed, agentoutcome.Stuck} {
		a := outcomeRow(o, "the reason for "+o)
		page := s.cardViewIndexed(a, nil, time.Time{}, 0, never, pins.Set{}, nil)
		card, detail, ok := s.outcomeCardState(a)
		switch o {
		case agentoutcome.Failed, agentoutcome.Stuck:
			if !ok || card != o || page.Outcome != o || detail != page.OutcomeReason {
				t.Errorf("%s: card %q (%q) page %q (%q): want both %q with one reason", o, card, detail, page.Outcome, page.OutcomeReason, o)
			}
		case agentoutcome.Done:
			if !ok || card != ui.DispatchDone || page.Outcome != agentoutcome.Done {
				t.Errorf("done: card %q page %q", card, page.Outcome)
			}
		case agentoutcome.Waiting:
			if !ok || card != ui.DispatchWaiting || page.Outcome != agentoutcome.Waiting || detail != page.OutcomeReason {
				t.Errorf("waiting: card %q (%q) page %q (%q)", card, detail, page.Outcome, page.OutcomeReason)
			}
		default:
			if ok || page.Outcome == agentoutcome.Failed || page.Outcome == agentoutcome.Stuck {
				t.Errorf("%q: card %q (ok %v) page %q; want neither to show an end", o, card, ok, page.Outcome)
			}
		}
	}
	// A failed first turn with NO recorded outcome is no longer a card rule of
	// its own: the card derives from the status, like the page.
	a := outcomeRow("", "")
	a.KickoffError = "turn failed"
	if state, _ := s.dispatchCardState(context.Background(), a, agents.StatusRunning); state == ui.DispatchError || state == ui.DispatchFailed {
		t.Errorf("an unrecorded first-turn failure drew %q on the card from a rule of its own", state)
	}
}

// noClaimAgents is a store whose agents hold no kickoff claim.
type noClaimAgents struct{ stubAgentsStore }

func (noClaimAgents) KickoffClaimHolder(context.Context, int64) (string, error) { return "", nil }
