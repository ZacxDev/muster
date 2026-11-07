package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentoutcome"
	"github.com/ZacxDev/muster/internal/agents"
)

// hangingRuntime takes every turn and never answers: a first turn that outlives
// muster's budget while the runtime (may) still run it.
func hangingRuntime(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	return srv
}

// TestAnAbandonedFirstTurnIsClosedAndASilentAgentGoesStuck (round 2, 🔴1): a
// first turn that outlives its budget is not failed (the runtime may still be
// running it) but it is CLOSED, so the agent is judged by what it does next: a
// sweep past the no-reply threshold calls it stuck (one push), and an idle reply
// after that makes it waiting.
//
// 🔴 RED AT 4cca02e3 (reproduced by the round-2 audit): the turn stayed open for
// the life of the process, so the watchdog read the agent as working — 0 stuck
// two hours later — and its idle reply returned early, leaving it `running`.
func TestAnAbandonedFirstTurnIsClosedAndASilentAgentGoesStuck(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	a := o.dispatchGateway(t, "outlives its budget", "openrouter/acme/acme-v4.1-pro")
	// The budget also bounds the deliverer's own writes before the send, so it
	// is generous enough for a loaded host; the runtime never answers anyway.
	o.kickoffWithin(t, hangingRuntime(t), 2*time.Second, a)

	got := o.agent(t, a.ID)
	if !got.KickedOff {
		t.Fatalf("instrument check: the first turn was never sent (kickoff error %q)", got.KickoffError)
	}
	if got.Outcome != agentoutcome.Running || got.Status == agents.StatusError {
		t.Fatalf("after its budget: outcome %q status %q; want running, not failed", got.Outcome, got.Status)
	}
	if o.srv.agentOutcomes.busy(a.ID) {
		t.Fatal("the abandoned first turn is still open")
	}
	if n := len(o.outcomePushes(a.ID)); n != 0 {
		t.Fatalf("the budget pushed %d times; want none (not a failure)", n)
	}

	o.clock.Add(2 * time.Hour)
	if n := o.sweep(t); n != 1 {
		t.Fatalf("a sweep two hours later called %d agents stuck; want 1", n)
	}
	if got := o.agent(t, a.ID); got.Outcome != agentoutcome.Stuck {
		t.Fatalf("after the sweep: outcome %q; want stuck", got.Outcome)
	}
	if n := len(o.outcomePushes(a.ID)); n != 1 {
		t.Fatalf("stuck pushed %d times; want 1", n)
	}

	o.srv.outcomeAnswered(o.asOwner, o.agent(t, a.ID), "I am back. Which branch should I use?")
	if got := o.agent(t, a.ID); got.Outcome != agentoutcome.Waiting {
		t.Fatalf("an idle reply after stuck: outcome %q; want waiting", got.Outcome)
	}
}
