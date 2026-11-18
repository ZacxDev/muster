package api

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// Session state's audit round 2 (#426): an agent's turn seen mid-turn, the
// owner's chief off, reclassifying once the classifier comes on, the backoff
// cap, and an older host's Hermes window, which neither stall read lists.

// TestAnAgentTurnSeenMidTurnStillEndsTheStop (N1): a pass lands while the
// owner's prompt is the conversation's newest message (a turn no turn gate
// saw); the reply then lands 2m17s after the prompt. The stop's anchor was not
// moved onto the prompt, so the turn is timed from it and the stop ends.
func TestAnAgentTurnSeenMidTurnStillEndsTheStop(t *testing.T) {
	f := newStateFixture(t, false)
	f.srv.nudgeReadHook = nil
	f.mirror.mu.Lock()
	f.mirror.windows = nil
	f.mirror.mu.Unlock()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	worker := f.agentFor(t, authz.OperatorUserID, "statemid-"+suffix, "worker", "state-mid-tok-"+suffix)
	sess, _ := f.stallAgent(t, worker, authz.OperatorUserID)
	store := agents.NewPG(f.pool)
	add := func(role, text string, ago time.Duration) {
		t.Helper()
		m, err := store.AddChatMessage(asUser(authz.OperatorUserID), agents.ChatMessage{AgentID: worker.ID, SessionID: sess.ID, Role: role, Content: text})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE chat_messages SET created_at = $2 WHERE id = $1`,
			m.ID, time.Now().Add(-ago)); err != nil {
			t.Fatal(err)
		}
	}
	mine := func() []sessionstate.Episode {
		var out []sessionstate.Episode
		for _, e := range f.episodes(t) {
			if e.AgentID == worker.ID {
				out = append(out, e)
			}
		}
		return out
	}
	f.statePass(t)
	first := mine()
	if len(first) != 1 || !first[0].Open() {
		t.Fatalf("the agent's stop: %+v", first)
	}
	add("user", "main, then cut the release", 14*time.Minute)
	f.statePass(t) // mid-turn: the prompt is the newest message
	if got := mine(); len(got) != 1 || !got[0].Open() || got[0].StreakKey != first[0].StreakKey {
		t.Fatalf("mid-turn, the stop's anchor moved: %+v", got)
	}
	add("assistant", "Merged into main and released 1.2.3.", 14*time.Minute-137*time.Second)
	f.statePass(t)
	got := mine()
	if len(got) < 1 || got[len(got)-1].ID != first[0].ID || got[len(got)-1].Open() || got[len(got)-1].EndReason != sessionstate.EndWorked {
		t.Fatalf("a 2m17s reply seen mid-turn did not end the stop: %+v", got)
	}
}

// TestTheClassifierDoesNotRunForAnOwnerWhoseChiefIsOff (N2): the nudges and
// the classifier are configured, and the owner turned their chief off — no
// classifier call, no transcript read, and the stop is labelled as asking. The
// positive control, chief on, is one call and one read.
func TestTheClassifierDoesNotRunForAnOwnerWhoseChiefIsOff(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run("chief on="+strconv.FormatBool(on), func(t *testing.T) {
			f := newStateFixture(t, true)
			j := withJev(t, f.nudgeFixture)
			j.set(200, 0.03, 0.02, 0.95)
			prefs := &memChiefPrefs{}
			if err := prefs.SetEnabled(context.Background(), authz.OperatorUserID, on); err != nil {
				t.Fatal(err)
			}
			f.srv.ext.ChiefPrefs = prefs
			f.statePass(t)
			want := 0
			if on {
				want = 1
			}
			if c, r := f.jevCalls(j), f.readCount(); c != want || r != want {
				t.Fatalf("classifier calls %d, reads %d; want %d each", c, r, want)
			}
			e := f.oneEpisode(t)
			if !on && (e.Jev.Err != jevNotConfigured || !e.Asked) {
				t.Fatalf("chief off: %+v", e)
			}
		})
	}
}

// TestAStreakIsReclassifiedOnceTheClassifierComesOn (N3): a stop opened with
// no classifier (not_configured) is classified on the first pass that has
// one — once, for the same streak.
func TestAStreakIsReclassifiedOnceTheClassifierComesOn(t *testing.T) {
	f := newStateFixture(t, true)
	f.statePass(t) // the nudges are on, the classifier not yet
	if e := f.oneEpisode(t); e.Jev.Err != jevNotConfigured || !e.Asked {
		t.Fatalf("with no classifier: %+v", e)
	}
	j := withJev(t, f.nudgeFixture)
	j.set(200, 0.03, 0.02, 0.95)
	f.statePass(t)
	f.statePass(t)
	e := f.oneEpisode(t)
	if e.Jev.Err != "" || e.Jev.Label != chiefnudge.LabelNeither || e.Asked || f.jevCalls(j) != 1 {
		t.Fatalf("after the classifier came on: %+v (calls %d)", e, f.jevCalls(j))
	}
}

// TestNeitherStallReadListsAnOlderHostsHermesWindow: an older muster-mirror
// may still name a window Runtime "hermes"; muster no longer knows that
// runtime, so neither the nudge sweep's stall facts nor session state's list
// it (no episode, no nudge). The Claude Code window beside it is the control.
func TestNeitherStallReadListsAnOlderHostsHermesWindow(t *testing.T) {
	f := newStateFixture(t, true)
	f.mirror.mu.Lock()
	f.mirror.windows = append(f.mirror.windows, mirror.Window{Session: "main", ID: "@5", Index: 5, Label: "hermes",
		Runtime: "hermes", AgentSession: "h-80", Status: mirror.StatusIdle, StatusSince: nudgeSince})
	f.mirror.mu.Unlock()
	octx := f.srv.attentionPrincipal(context.Background(), authz.OperatorUserID)
	nudges, state := f.srv.stallInputs(octx, nil), f.srv.stallFacts(octx, nil, f.mirror.Hosts(octx)).sessions
	for _, cs := range [][]nudgeCandidate{nudges, state} {
		for _, c := range cs {
			if c.Window == "@5" || c.Runtime != "" {
				t.Fatalf("a stall read lists the older host's Hermes window: %+v", c.Session)
			}
		}
	}
	if len(nudges) == 0 || len(state) == 0 {
		t.Fatalf("CONTROL: the Claude Code window is not listed: nudges %d, state %d", len(nudges), len(state))
	}
}

// TestAFailingReadBacksOffToTheCap (N3): the backoff doubles from
// stateReadBackoff and stops at stateReadBackoffMax.
func TestAFailingReadBacksOffToTheCap(t *testing.T) {
	var r nudgeRetries
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute,
		30 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, w := range want {
		r.readFailed("k", now)
		e := r.m["k"]
		if got := e.readAfter.Sub(now); got != w {
			t.Fatalf("failure %d: next read after %s, want %s", i+1, got, w)
		}
		if r.readDue("k", now.Add(w-time.Second)) || !r.readDue("k", now.Add(w)) {
			t.Fatalf("failure %d: due at the wrong time", i+1)
		}
	}
}
