package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentoutcome"
	"github.com/ZacxDev/muster/internal/agents"
)

// refusingGateway refuses every ask with err before typing anything (a typed
// 409 busy, as ccd answers while another turn holds the session) and counts
// the asks. No stream, so no activity: exactly what the real transport does.
type refusingGateway struct {
	stubGateway
	err  error
	asks *int
}

func (g refusingGateway) ChatWithTools(context.Context, agents.Agent, string, string, string, []agents.ToolDef, agents.ToolDispatch, agents.StreamEmit) (string, error) {
	*g.asks++
	return "", g.err
}

// answeringGateway takes the turn and ends it with reply: with a stream read
// (streams), or with no sign before the answer itself (no 103, no stream).
type answeringGateway struct {
	stubGateway
	reply   string
	streams bool
}

func (g answeringGateway) ChatWithTools(ctx context.Context, _ agents.Agent, _, _, _ string, _ []agents.ToolDef, _ agents.ToolDispatch, _ agents.StreamEmit) (string, error) {
	if g.streams {
		agents.MarkTurnActivity(ctx)
	}
	return g.reply, nil
}

// TestARefusedAskIsNeitherActivityNorARearm: an ask the runtime refused before
// typing anything — #494's held message, asked again every poll for minutes
// while another turn holds the session — is not a turn of the agent. A waiting
// agent stays waiting (no re-arm, no second "waiting" push), its activity time
// does not move (the watchdog must not read the asks as life), and no turn is
// left open. A turn the runtime DOES take still re-arms it, so its idle end is a
// new "waiting" push.
//
// 🔴 RED BEFORE THE FIX: every refused ask re-armed the agent to running and
// recorded activity, so a held message made a quiet agent look alive.
func TestARefusedAskIsNeitherActivityNorARearm(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	a := o.dispatchGateway(t, "asked while busy", "openrouter/acme/acme-v4.1-pro")
	sess, err := agents.NewPG(o.pool).LatestOrCreateSession(o.asOwner, a.ID, a.Name)
	if err != nil {
		t.Fatal(err)
	}
	o.srv.outcomeAnswered(o.asOwner, o.agent(t, a.ID), "Which repository should I work in?")
	before := o.agent(t, a.ID)
	if before.Outcome != agentoutcome.Waiting || before.ActivityAt == nil {
		t.Fatalf("instrument check: outcome %q activity %v; want waiting with an activity time", before.Outcome, before.ActivityAt)
	}
	if n := len(o.outcomePushes(a.ID)); n != 1 {
		t.Fatalf("instrument check: %d pushes; want the one waiting push", n)
	}
	o.clock.Add(5 * time.Minute)

	asks := 0
	busy := &agents.RuntimeError{Status: http.StatusConflict, Type: "busy", Message: "a turn is running"}
	o.srv.ext.Gateway = refusingGateway{err: busy, asks: &asks}
	// The chat door's one ask, and #494's hold: asked again until its bound.
	_, _ = o.srv.chatTurn(o.asOwner, o.agent(t, a.ID), sess.SessionKey, "hello", nil)
	o.srv.chatBusyWait, o.srv.chatBusyPoll = 60*time.Millisecond, 5*time.Millisecond
	if _, err := o.srv.chatTurnWhenFree(o.asOwner, o.agent(t, a.ID), sess.SessionKey, "hello", nil); err == nil {
		t.Fatal("a message refused past the bound returned no error")
	}
	if asks < 3 {
		t.Fatalf("instrument check: the runtime was asked %d times; want the hold's repeated asks", asks)
	}
	got := o.agent(t, a.ID)
	if got.Outcome != agentoutcome.Waiting {
		t.Errorf("after %d refused asks: outcome %q; want still waiting", asks, got.Outcome)
	}
	if got.ActivityAt == nil || !got.ActivityAt.Equal(*before.ActivityAt) {
		t.Errorf("after %d refused asks: activity %v; want unchanged %v", asks, got.ActivityAt, before.ActivityAt)
	}
	if o.srv.agentOutcomes.busy(a.ID) {
		t.Error("a refused ask left a turn open")
	}

	// The runtime takes a turn: re-armed, and its idle end is a NEW waiting push.
	o.srv.ext.Gateway = answeringGateway{reply: "Still need the repository name.", streams: true}
	if _, err := o.srv.chatTurn(o.asOwner, o.agent(t, a.ID), sess.SessionKey, "hello", nil); err != nil {
		t.Fatal(err)
	}
	got = o.agent(t, a.ID)
	if got.Outcome != agentoutcome.Waiting || got.OutcomeReason != "Still need the repository name." ||
		got.ActivityAt == nil || !got.ActivityAt.After(*before.ActivityAt) {
		t.Fatalf("after a taken turn: outcome %q reason %q activity %v; want waiting on the new reply, activity moved",
			got.Outcome, got.OutcomeReason, got.ActivityAt)
	}
	if n := len(o.outcomePushes(a.ID)); n != 2 {
		t.Fatalf("pushes after a taken turn: %d; want 2 (the re-armed agent's new waiting)", n)
	}
	// A runtime that shows nothing before its answer (no 103, no stream read):
	// the answer itself is the turn taken, so it re-arms too.
	o.srv.ext.Gateway = answeringGateway{reply: "Or the second repository?"}
	if _, err := o.srv.chatTurn(o.asOwner, o.agent(t, a.ID), sess.SessionKey, "hello", nil); err != nil {
		t.Fatal(err)
	}
	if got := o.agent(t, a.ID); got.OutcomeReason != "Or the second repository?" || len(o.outcomePushes(a.ID)) != 3 {
		t.Fatalf("an answer with no earlier sign: reason %q, %d pushes; want re-armed to a third waiting push",
			got.OutcomeReason, len(o.outcomePushes(a.ID)))
	}
}

// TestOutcomeTurnKeepsTheCallerCallback: the outcome bracket's 103 callback
// CHAINS to the one the caller's context carries (markOwnerTurn's owner-turn
// marker, acceptedAfterQueued's "accepted"), and takes the turn: the 103 of an
// ask is its activity and its re-arm.
func TestOutcomeTurnKeepsTheCallerCallback(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	a := o.dispatchGateway(t, "accepted by its runtime", "openrouter/acme/acme-v4.1-pro")
	o.srv.outcomeAnswered(o.asOwner, o.agent(t, a.ID), "Which repository should I work in?")
	if got := o.agent(t, a.ID); got.Outcome != agentoutcome.Waiting {
		t.Fatalf("instrument check: outcome %q; want waiting", got.Outcome)
	}
	var outer []string
	ctx := agents.WithTurnAccepted(o.asOwner, func(acc agents.TurnAcceptance) { outer = append(outer, acc.Lane) })
	tctx, ended := o.srv.outcomeTurn(ctx, o.agent(t, a.ID))
	agents.MarkTurnAccepted(tctx, agents.TurnAcceptance{Lane: "lane-q7"})
	if len(outer) != 1 || outer[0] != "lane-q7" {
		t.Fatalf("the caller's own 103 callback saw %q; want it called once with the lane", outer)
	}
	if got := o.agent(t, a.ID); got.Outcome != agentoutcome.Running {
		t.Errorf("after the runtime's 103: outcome %q; want re-armed to running", got.Outcome)
	}
	ended("", context.Canceled)
	if o.srv.agentOutcomes.busy(a.ID) {
		t.Error("the bracket left its turn open")
	}
}
