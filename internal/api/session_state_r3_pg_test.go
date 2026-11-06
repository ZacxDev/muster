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
// cap, Hermes, and the nudges never judging Hermes.

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

// TestAHermesStallReadsNoTranscript (N3): an idle Hermes stop, with the
// classifier configured, is never read or classified — no_transcript, counted
// as asked.
func TestAHermesStallReadsNoTranscript(t *testing.T) {
	f := newStateFixture(t, true)
	j := withJev(t, f.nudgeFixture)
	f.mirror.mu.Lock()
	f.mirror.windows = []mirror.Window{{Session: "main", ID: "@5", Index: 5, Label: "hermes", Runtime: mirror.RuntimeHermes,
		AgentSession: "h-78", Status: mirror.StatusIdle, StatusSince: nudgeSince}}
	f.mirror.agentState = &mirror.AgentState{Runtime: mirror.RuntimeHermes, Status: mirror.StatusIdle, Since: nudgeSince,
		Observed: f.srv.now().Add(-20 * time.Minute)}
	f.mirror.mu.Unlock()
	f.statePass(t)
	e := f.oneEpisode(t)
	if e.Jev.Err != jevNoTranscript || !e.Asked || e.State != sessionstate.StateStalled || f.readCount() != 0 || f.jevCalls(j) != 0 {
		t.Fatalf("episode %+v, reads %d, calls %d", e, f.readCount(), f.jevCalls(j))
	}
}

// TestAHermesApprovalIsAnEpisodeAnswerableOrNot (N3, as the operator changed
// it after #463): a Hermes window waiting on an approval opens an episode
// whether or not its host answers it from muster — an answerable ask's push
// waits the grace like any wait's. Until then one with a prompt id its host
// answers opened none, and pushed at once.
func TestAHermesApprovalIsAnEpisodeAnswerableOrNot(t *testing.T) {
	for _, prompt := range []string{"p1", ""} {
		t.Run("prompt="+prompt, func(t *testing.T) {
			f := newStateFixture(t, false)
			f.mirror.mu.Lock()
			f.mirror.windows = []mirror.Window{{Session: "main", ID: "@5", Index: 5, Label: "hermes", Runtime: mirror.RuntimeHermes,
				AgentSession: "h-79", Status: mirror.StatusWaiting, StatusSince: nudgeSince,
				Pending: &mirror.Pending{Kind: mirror.PendingApproval, Summary: "terminal: make", Tool: "terminal", PromptID: prompt, Choices: "once deny"}}}
			f.mirror.agentState = &mirror.AgentState{Runtime: mirror.RuntimeHermes, Status: mirror.StatusWaiting, Since: nudgeSince,
				Observed: f.srv.now().Add(-time.Minute)}
			f.mirror.mu.Unlock()
			f.srv.ext.Mirror = &hermesAskMirror{fakeMirror: f.mirror, answers: true}
			f.statePass(t)
			if n := len(f.episodes(t)); n != 1 {
				t.Fatalf("episodes: %d, want 1", n)
			}
		})
	}
}

// TestTheStallNudgesNeverJudgeAHermesWindow (N3): the nudge sweep's stall
// facts list no Hermes window — only session state's do.
func TestTheStallNudgesNeverJudgeAHermesWindow(t *testing.T) {
	f := newStateFixture(t, true)
	f.mirror.mu.Lock()
	f.mirror.windows = append(f.mirror.windows, mirror.Window{Session: "main", ID: "@5", Index: 5, Label: "hermes",
		Runtime: mirror.RuntimeHermes, AgentSession: "h-80", Status: mirror.StatusIdle, StatusSince: nudgeSince})
	f.mirror.mu.Unlock()
	octx := f.srv.attentionPrincipal(context.Background(), authz.OperatorUserID)
	for _, c := range f.srv.stallInputs(octx, nil) {
		if c.Runtime == mirror.RuntimeHermes {
			t.Fatalf("the nudges' stall facts list a Hermes window: %+v", c.Session)
		}
	}
	hermes := 0
	for _, c := range f.srv.stallFacts(octx, nil, f.mirror.Hosts(octx), true).sessions {
		if c.Runtime == mirror.RuntimeHermes {
			hermes++
		}
	}
	if hermes != 1 {
		t.Fatalf("session state's stall facts list %d Hermes windows, want 1", hermes)
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
