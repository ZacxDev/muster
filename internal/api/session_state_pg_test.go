package api

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// SESSION STATE AND EPISODES end to end on a real Postgres (session_state.go):
// a stopped session opens one episode, claimed once; waiting is needs_you at
// once; an answerable ask opens none; only sustained work ends one; a host
// that dropped ends nothing; the classifier decides needs_you vs stalled and
// its failure counts as asked; and all of it runs with the chief's nudges off.

type stateFixture struct {
	*nudgeFixture
	eps   *sessionstate.PG
	mu    sync.Mutex
	clock time.Time
}

// newStateFixture is the nudge fixture with episodes on, its window idle for
// 20 minutes on the fixture's clock; nudges false turns the chief's nudges
// (and with them the classifier) off.
func newStateFixture(t *testing.T, nudges bool) *stateFixture {
	t.Helper()
	f := &stateFixture{nudgeFixture: newNudgeFixture(t, false), clock: time.Now()}
	f.eps = sessionstate.NewPG(f.pool)
	f.srv.ext.Episodes = f.eps
	if !nudges {
		f.srv.ext.ChiefNudge, f.srv.ext.ChiefNudges = nil, nil
	}
	f.srv.now = func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.clock
	}
	f.status(mirror.StatusIdle, 20*time.Minute)
	t.Cleanup(func() {
		if _, err := f.pool.Exec(authz.System(context.Background()), `DELETE FROM session_episodes WHERE owner_id IN ($1, $2)`,
			authz.OperatorUserID, f.member); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	return f
}

// status puts the fixture's window in status, observed for d before the clock.
func (f *stateFixture) status(status string, d time.Duration) {
	f.mu.Lock()
	now := f.clock
	f.mu.Unlock()
	f.mirror.mu.Lock()
	defer f.mirror.mu.Unlock()
	f.mirror.windows[0].Status = status
	f.mirror.agentState = &mirror.AgentState{Runtime: mirror.RuntimeClaude, Status: status, Since: nudgeSince,
		Observed: now.Add(-d), Session: nudgeSID}
}

func (f *stateFixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = f.clock.Add(d)
}

func (f *stateFixture) statePass(t *testing.T) {
	t.Helper()
	if err := f.srv.SessionStatePass(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
}

func (f *stateFixture) episodes(t *testing.T) []sessionstate.Episode {
	t.Helper()
	list, err := f.eps.List(asUser(authz.OperatorUserID), 50)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func (f *stateFixture) oneEpisode(t *testing.T) sessionstate.Episode {
	t.Helper()
	list := f.episodes(t)
	if len(list) != 1 {
		t.Fatalf("episodes: %d, want 1: %+v", len(list), list)
	}
	return list[0]
}

// TestAStalledSessionOpensOneEpisodeWithTheChiefsNudgesOff: with
// MUSTER_CHIEF_NUDGES off (no classifier, no chief call) a window idle past
// the threshold opens ONE stalled episode, counted as asked, its
// classification stored as not_configured for its streak — and a second pass
// opens no second one. Where it is (host, tmux session, window index) is on
// the row. No chief was asked.
func TestAStalledSessionOpensOneEpisodeWithTheChiefsNudgesOff(t *testing.T) {
	f := newStateFixture(t, false)
	f.statePass(t)
	f.statePass(t)
	e := f.oneEpisode(t)
	if e.Kind != sessionstate.KindMirror || e.Key != nudgeSID || e.State != sessionstate.StateStalled || !e.Asked || e.Waiting ||
		!e.Open() || e.Seq != 1 || e.Host != "ws" || e.TmuxSession != "main" || e.Index != 1 {
		t.Fatalf("episode: %+v", e)
	}
	if e.ClassifiedStreak == "" || e.Jev.Err != jevNotConfigured {
		t.Fatalf("classification: %+v", e)
	}
	if checks, _ := f.gw.seen(); len(checks) != 0 {
		t.Fatalf("the chief was asked: %v", checks)
	}
}

// TestAWindowIdleUnderTheThresholdOpensNothing: the guard's other side.
func TestAWindowIdleUnderTheThresholdOpensNothing(t *testing.T) {
	f := newStateFixture(t, false)
	f.status(mirror.StatusIdle, 3*time.Minute)
	f.statePass(t)
	if list := f.episodes(t); len(list) != 0 {
		t.Fatalf("episodes: %+v", list)
	}
}

// TestAStallEndingOnTheOwnersWordOpensNothing: the owner spoke last, so the
// session has not stopped on its own word (the nudge sweep's rule).
func TestAStallEndingOnTheOwnersWordOpensNothing(t *testing.T) {
	f := newStateFixture(t, false)
	f.mu.Lock()
	f.script = f.script[:1]
	f.mu.Unlock()
	f.statePass(t)
	if list := f.episodes(t); len(list) != 0 {
		t.Fatalf("episodes: %+v", list)
	}
}

// TestAWaitingWindowIsNeedsYouAtOnce: a window waiting on a prompt muster
// cannot answer is needs_you at once — no idle threshold.
func TestAWaitingWindowIsNeedsYouAtOnce(t *testing.T) {
	f := newStateFixture(t, false)
	f.status(mirror.StatusWaiting, 5*time.Second)
	f.statePass(t)
	e := f.oneEpisode(t)
	if e.State != sessionstate.StateNeedsYou || !e.Waiting {
		t.Fatalf("episode: %+v", e)
	}
}

// TestAHeldQuestionOpensNoEpisode: a waiting window whose AskUserQuestion
// muster holds is an answerable ask with its own push, never an episode.
func TestAHeldQuestionOpensNoEpisode(t *testing.T) {
	f := newStateFixture(t, false)
	f.status(mirror.StatusWaiting, 5*time.Second)
	if _, err := f.srv.Questions().Open(askq.Key{Owner: 1, Source: askSourceMirror, Scope: "ws", Session: nudgeSID, ToolUseID: "toolu_state1"},
		[]askq.Question{{Question: "Which database?", Options: []askq.Option{{Label: "postgres"}, {Label: "sqlite"}}}}, askq.NewID()); err != nil {
		t.Fatal(err)
	}
	f.statePass(t)
	if list := f.episodes(t); len(list) != 0 {
		t.Fatalf("episodes: %+v", list)
	}
}

// TestABusyBlipKeepsTheEpisodeAndSustainedWorkEndsIt: one stop is one
// episode. Waiting, then let through and busy for under EndAfter, then idle:
// the SAME episode (no second one). Busy, seen past EndAfter: it ends
// (worked). The next stop is seq 2.
func TestABusyBlipKeepsTheEpisodeAndSustainedWorkEndsIt(t *testing.T) {
	f := newStateFixture(t, false)
	f.status(mirror.StatusWaiting, 5*time.Second)
	f.statePass(t)
	first := f.oneEpisode(t)

	f.advance(10 * time.Second)
	f.status(mirror.StatusBusy, 0)
	f.statePass(t)
	f.advance(sessionstate.EndAfter - 31*time.Second)
	f.statePass(t)
	f.advance(20 * time.Second)
	f.status(mirror.StatusIdle, 0)
	f.statePass(t)
	if e := f.oneEpisode(t); e.ID != first.ID || !e.Open() || !e.BusySince.IsZero() {
		t.Fatalf("after a blip: %+v", e)
	}

	f.status(mirror.StatusBusy, 0)
	f.statePass(t)
	f.advance(sessionstate.EndAfter + 9*time.Second)
	f.statePass(t)
	if e := f.oneEpisode(t); e.ID != first.ID || e.Open() || e.EndReason != sessionstate.EndWorked {
		t.Fatalf("after sustained work: %+v", e)
	}

	f.advance(time.Minute)
	f.status(mirror.StatusIdle, 20*time.Minute)
	f.statePass(t)
	list := f.episodes(t)
	if len(list) != 2 || list[0].Seq != 2 || !list[0].Open() {
		t.Fatalf("the next stop: %+v", list)
	}
}

// TestOnlyAReportingHostCanEndAnEpisodeAsGone: a host that dropped ends
// nothing (a failure never quiets a session); the window gone from a host
// that is online and reporting status ends it as gone.
func TestOnlyAReportingHostCanEndAnEpisodeAsGone(t *testing.T) {
	f := newStateFixture(t, false)
	f.statePass(t)
	first := f.oneEpisode(t)

	f.mirror.mu.Lock()
	f.mirror.online = false
	f.mirror.mu.Unlock()
	f.statePass(t)
	if e := f.oneEpisode(t); !e.Open() {
		t.Fatalf("an offline host ended it: %+v", e)
	}

	f.mirror.mu.Lock()
	f.mirror.online = true
	f.mirror.windows = []mirror.Window{{Session: "main", ID: "@2", Index: 2, Label: "shell"}}
	f.mirror.mu.Unlock()
	f.statePass(t)
	if e := f.oneEpisode(t); e.ID != first.ID || e.Open() || e.EndReason != sessionstate.EndGone {
		t.Fatalf("the window left a reporting host: %+v", e)
	}
}

// TestTheClassifierDecidesNeedsYouAndItsFailureCountsAsAsked: with the
// classifier configured, an ask score at the line is needs_you (asked), a
// clear "neither" is stalled and NOT asked, and a failed call is stalled but
// counted as asked — and the nudge sweep then reuses the pass's answer
// instead of billing the owner's key again.
func TestTheClassifierDecidesNeedsYouAndItsFailureCountsAsAsked(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		probs  [3]float64
		state  string
		asked  bool
		errSet bool
	}{
		{"asked", 200, [3]float64{0.81, 0.10, 0.09}, sessionstate.StateNeedsYou, true, false},
		{"neither", 200, [3]float64{0.03, 0.02, 0.95}, sessionstate.StateStalled, false, false},
		{"failed", 500, [3]float64{}, sessionstate.StateStalled, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newStateFixture(t, true)
			j := withJev(t, f.nudgeFixture)
			j.set(c.status, c.probs[0], c.probs[1], c.probs[2])
			f.statePass(t)
			e := f.oneEpisode(t)
			if e.State != c.state || e.Asked != c.asked || (e.Jev.Err != "") != c.errSet || e.ClassifiedStreak == "" {
				t.Fatalf("episode: %+v", e)
			}
			j.mu.Lock()
			calls := j.calls
			j.mu.Unlock()
			if calls != 1 {
				t.Fatalf("classifier calls after the state pass: %d", calls)
			}
			if c.status == 200 {
				f.pass(t) // the nudge sweep, same streak
				j.mu.Lock()
				calls = j.calls
				j.mu.Unlock()
				if calls != 1 {
					t.Fatalf("the nudge sweep classified the streak again: %d calls", calls)
				}
			}
		})
	}
}

// TestAnAgentConversationsNewMessageEndsItsStop: a dispatched agent's
// conversation that stopped on its own question opens an episode (read from
// the real transcript, no test hook); a new message in it — a turn that ran —
// ends that stop as worked.
func TestAnAgentConversationsNewMessageEndsItsStop(t *testing.T) {
	f := newStateFixture(t, false)
	f.srv.nudgeReadHook = nil
	f.mirror.mu.Lock()
	f.mirror.windows = nil
	f.mirror.mu.Unlock()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	worker := f.agentFor(t, authz.OperatorUserID, "stateworker-"+suffix, "worker", "state-worker-tok-"+suffix)
	sess, _ := f.stallAgent(t, worker, authz.OperatorUserID)
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
	got := mine()
	if len(got) != 1 || got[0].Kind != sessionstate.KindAgent || got[0].ChatSessionID != sess.ID || !got[0].Open() ||
		got[0].State != sessionstate.StateStalled {
		t.Fatalf("the agent's stop: %+v", got)
	}
	if _, err := agents.NewPG(f.pool).AddChatMessage(asUser(authz.OperatorUserID),
		agents.ChatMessage{AgentID: worker.ID, SessionID: sess.ID, Role: "assistant", Content: "Merged into main."}); err != nil {
		t.Fatal(err)
	}
	f.statePass(t)
	got = mine()
	if len(got) != 1 || got[0].Open() || got[0].EndReason != sessionstate.EndWorked {
		t.Fatalf("after a new message: %+v", got)
	}
}

// TestRacingStatePassesOpenOneEpisode: passes running at once (two replicas
// during a rolling deploy) claim one episode between them.
func TestRacingStatePassesOpenOneEpisode(t *testing.T) {
	f := newStateFixture(t, false)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f.srv.SessionStatePass(context.Background())
		}()
	}
	wg.Wait()
	f.oneEpisode(t)
}
