package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentoutcome"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/jobs"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// TestAWaitIsToldOnlyWhenTheOwnerIsAway (round 2, 🟡3): every turn of a
// conversation ends waiting on the owner, so a wait is told — one push, one
// chief note — only past the waiting hold (60 s, session state's rule) and only
// if the owner neither replied nor opened the agent's page meanwhile.
//
//   - present: three turns, the owner answering each inside the hold and then
//     looking at the page: no push, no chief note;
//   - away: one turn and nobody comes: one push and one note, and a later sweep
//     tells nothing more;
//   - with session state on, its episode already pushes "needs you" for the
//     conversation: the wait is noted to the chief once and not pushed again.
//
// 🔴 RED AT 4cca02e3: each of the three turns pushed "is waiting for you" and
// noted the chief, at the moment it ended.
func TestAWaitIsToldOnlyWhenTheOwnerIsAway(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	turn := func(a agents.Agent, reply string) {
		t.Helper()
		sess, err := agents.NewPG(o.pool).LatestOrCreateSession(o.asOwner, a.ID, a.Name)
		if err != nil {
			t.Fatal(err)
		}
		o.srv.ext.Gateway = answeringGateway{reply: reply, streams: true}
		if _, err := o.srv.chatTurn(o.asOwner, o.agent(t, a.ID), sess.SessionKey, "go on", nil); err != nil {
			t.Fatal(err)
		}
		if got := o.agent(t, a.ID); got.Outcome != agentoutcome.Waiting {
			t.Fatalf("instrument check: after a turn ending %q: outcome %q; want waiting", reply, got.Outcome)
		}
	}

	// Present: the owner answers inside the hold, twice, then opens the page.
	p := o.dispatchGateway(t, "a conversation", "openrouter/acme/acme-v4.1-pro")
	for _, reply := range []string{"Which repository?", "Which branch?", "Shall I open the PR now?"} {
		turn(p, reply)
		o.clock.Add(20 * time.Second)
		if n := o.sweep(t); n != 0 {
			t.Fatalf("a sweep inside the hold called %d stuck", n)
		}
	}
	// The owner opens the agent's page (its real handler, as the owner).
	psess, err := agents.NewPG(o.pool).LatestOrCreateSession(o.asOwner, p.ID, p.Name)
	if err != nil {
		t.Fatal(err)
	}
	page := httptest.NewRequest(http.MethodGet, "/agents/"+p.Name+"?session="+strconv.FormatInt(psess.ID, 10), nil).WithContext(o.asOwner)
	page.SetPathValue("name", p.Name)
	rec := httptest.NewRecorder()
	o.srv.handleAgentDetail(rec, page)
	if rec.Code != http.StatusOK {
		t.Fatalf("instrument check: the agent page answered %d", rec.Code)
	}
	if got := o.agent(t, p.ID); got.OutcomeToldAt == nil {
		t.Fatal("opening the agent's page did not mark its wait seen")
	}
	o.tellWaits(t)
	o.tellWaits(t)
	if n := len(o.outcomePushes(p.ID)); n != 0 {
		t.Errorf("owner present: %d waiting pushes; want 0", n)
	}
	if n := o.chiefNotes(t, p.Name); len(n) != 0 {
		t.Errorf("owner present: chief notes %q; want none", n)
	}

	// Away: one turn, nobody replies or looks.
	w := o.dispatchGateway(t, "nobody comes", "openrouter/acme/acme-v4.1-pro")
	turn(w, "Which repository should I work in?")
	o.clock.Add(EpisodeWaitingGrace - time.Second)
	o.sweep(t)
	if n := len(o.outcomePushes(w.ID)); n != 0 {
		t.Fatalf("inside the hold: %d pushes; want 0", n)
	}
	o.tellWaits(t)
	if n := len(o.outcomePushes(w.ID)); n != 1 {
		t.Fatalf("owner away past the hold: %d pushes; want 1", n)
	}
	if n := o.chiefNotes(t, w.Name); len(n) != 1 {
		t.Fatalf("owner away: chief notes %q; want one", n)
	}
	o.tellWaits(t)
	if np, nn := len(o.outcomePushes(w.ID)), len(o.chiefNotes(t, w.Name)); np != 1 || nn != 1 {
		t.Fatalf("a later sweep told again: %d pushes, %d notes; want 1 and 1", np, nn)
	}

	// Session state on: its episode pushes the conversation; the wait is noted once.
	o.srv.ext.Episodes = sessionstate.NewPG(o.pool)
	defer func() { o.srv.ext.Episodes = nil }()
	e := o.dispatchGateway(t, "session state pushes it", "openrouter/acme/acme-v4.1-pro")
	turn(e, "Which environment?")
	o.tellWaits(t)
	if n := len(o.outcomePushes(e.ID)); n != 0 {
		t.Errorf("with session state on: %d outcome pushes beside its episode; want 0", n)
	}
	if n := o.chiefNotes(t, e.Name); len(n) != 1 {
		t.Errorf("with session state on: chief notes %q; want one", n)
	}
}

// heldJobGateway answers a job's turn (Chat), recording whether the agent's turn was
// held open while it ran.
type heldJobGateway struct {
	stubGateway
	srv    *Server
	heldIn *bool
}

func (g heldJobGateway) Chat(_ context.Context, a agents.Agent, _, _ string, _ func(string)) (string, error) {
	*g.heldIn = g.srv.agentOutcomes.busy(a.ID)
	return "Job done: 3 files changed.", nil
}

// TestAJobTurnIsHeldAndJudgedAsNothing (round 2, 🟢4): a scheduled job's turn
// on a dispatched agent is held open while it runs (the watchdog reads it as
// working, never silent) and judged as nothing: its reply is the job's result,
// not the agent waiting on its owner, so the outcome is unchanged and nothing
// is pushed; the turn is closed afterwards.
func TestAJobTurnIsHeldAndJudgedAsNothing(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	a := o.dispatchGateway(t, "runs a job", "openrouter/acme/acme-v4.1-pro")
	sess, err := agents.NewPG(o.pool).LatestOrCreateSession(o.asOwner, a.ID, a.Name)
	if err != nil {
		t.Fatal(err)
	}
	held := false
	o.srv.ext.Gateway = heldJobGateway{srv: o.srv, heldIn: &held}
	out := o.srv.runJobTurn(o.asOwner, o.agent(t, a.ID), sess, jobs.Job{ID: 9, AgentID: a.ID, Agent: a.Name, Prompt: "run the nightly check"})
	if out.Status != jobs.StatusSucceeded {
		t.Fatalf("instrument check: the job ended %q (%s)", out.Status, out.Reason+" "+out.Error)
	}
	if !held {
		t.Error("the job's turn was not held open while it ran")
	}
	if o.srv.agentOutcomes.busy(a.ID) {
		t.Error("the job's turn is still open after it ended")
	}
	if got := o.agent(t, a.ID); got.Outcome != agentoutcome.Running {
		t.Errorf("after a job: outcome %q; want unchanged (running)", got.Outcome)
	}
	o.tellWaits(t)
	if n := len(o.outcomePushes(a.ID)); n != 0 {
		t.Errorf("a job pushed %d outcomes; want 0", n)
	}
}
