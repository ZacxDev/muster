package api

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
)

// TestAToldWaitIsNotPushedAgainAsAStall (round 3, 🟡a): a dispatched agent's
// wait has ONE owner, the outcome tracker, which pushes it (with the question)
// after its 60 s hold. Session state's pass, which turns an agent conversation
// stalled for the nudge idle into an episode and pushes it, stands down for
// that agent once its current wait was told or seen (outcomeOwnsWait): the stop
// is still an episode, drawn on the Asks page, but no second push.
//
//   - told: a tracked agent waiting, its wait told → an episode, NOT pushed;
//   - not yet told (inside its hold): pushed, as before (the positive control:
//     the same conversation, the same pass, only the told time differs);
//   - an untracked agent's conversation: pushed, as before.
//
// 🔴 RED AT 0443dc28 for the told case: the pass pushed it as a stall, a second
// push for a wait the owner had already been told about.
func TestAToldWaitIsNotPushedAgainAsAStall(t *testing.T) {
	f := newStateFixture(t, false)
	f.srv.nudgeReadHook = nil
	f.mirror.mu.Lock()
	f.mirror.windows = nil
	f.mirror.mu.Unlock()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	sys := authz.System(context.Background())
	mk := func(name string, outcome string, told bool) agents.Agent {
		t.Helper()
		a := f.agentFor(t, authz.OperatorUserID, name+"-"+suffix, name, "state-outcome-"+name+"-"+suffix)
		f.stallAgent(t, a, authz.OperatorUserID)
		if outcome != "" {
			if _, err := f.pool.Exec(sys, `UPDATE agents SET outcome = $2, outcome_at = now() - interval '20 minutes',
				outcome_told_at = CASE WHEN $3 THEN now() - interval '19 minutes' END WHERE id = $1`, a.ID, outcome, told); err != nil {
				t.Fatal(err)
			}
		}
		return a
	}
	told := mk("told", "waiting", true)
	untold := mk("untold", "waiting", false)
	plain := mk("plain", "", false)

	f.statePass(t)
	f.statePass(t)
	pushed := map[int64]bool{}
	opened := map[int64]bool{}
	for _, e := range f.episodes(t) {
		opened[e.AgentID] = true
		if !e.PushedAt.IsZero() {
			pushed[e.AgentID] = true
		}
	}
	if !opened[untold.ID] || !pushed[untold.ID] {
		t.Fatalf("instrument check: the not-yet-told wait opened=%v pushed=%v; want a pushed stall (the pass reaches agent conversations)",
			opened[untold.ID], pushed[untold.ID])
	}
	if !opened[plain.ID] || !pushed[plain.ID] {
		t.Errorf("an untracked agent's stall: opened=%v pushed=%v; want pushed, as before", opened[plain.ID], pushed[plain.ID])
	}
	if !opened[told.ID] {
		t.Errorf("a told wait's stop is not an episode at all; want one (drawn on the Asks page)")
	}
	if pushed[told.ID] {
		t.Errorf("a told wait was pushed again as a stall")
	}
}
