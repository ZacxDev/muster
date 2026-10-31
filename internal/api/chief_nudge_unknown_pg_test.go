package api

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// 🔴 AN UNREADABLE SESSION STATE IS UNKNOWN, NOT MOVED ON (nudgeStateNow).
// Before this, a host's muster-mirror losing its websocket for about a minute
// was enough: a sweep inside that window read every one of its windows as
// "moved on" and withdrew every open ask on that host, though no session had
// moved. Each live step that consults
// the streak is driven here with the host disconnected (or not reporting
// status) and must WAIT; a state that was read and differs still counts as
// moved on.

// nudgeOtherSID is a Claude conversation that is not nudgeSID.
const nudgeOtherSID = "7c1e9f04-2b8d-4a65-b3f0-91d2e6a4c5b8"

// hostDrops is the host's connection dropping: offline in the host list, and
// AgentState refusing as the hub does for a host it has no connection for.
func (f *nudgeFixture) hostDrops() {
	f.mirror.mu.Lock()
	defer f.mirror.mu.Unlock()
	f.mirror.online = false
	f.mirror.agentState.Unknown = fmt.Errorf("%w (host %q)", mirror.ErrHostOffline, "ws")
}

// hostReturns is the host back, its window exactly as it was.
func (f *nudgeFixture) hostReturns() {
	f.mirror.mu.Lock()
	defer f.mirror.mu.Unlock()
	f.mirror.online = true
	f.mirror.agentState.Unknown = nil
}

func (f *nudgeFixture) askStatus(t *testing.T, id int64) attention.Status {
	t.Helper()
	a, err := f.attn.Get(asUser(authz.OperatorUserID), id)
	if err != nil {
		t.Fatal(err)
	}
	return a.Status
}

// TestChiefNudgeAnOpenAskSurvivesAnUnreadableState is the incident: an open
// ask, then the host's connection drops (and, separately, a connected host
// that stops reporting status). The reconcile must leave the ask OPEN and the
// decision asked; once the host is back and the session is where it was, the
// ask is still there to answer.
func TestChiefNudgeAnOpenAskSurvivesAnUnreadableState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		unread func(*nudgeFixture)
	}{
		{"host disconnected", (*nudgeFixture).hostDrops},
		{"host reports no status", func(f *nudgeFixture) {
			f.state(func(s *mirror.AgentState) { s.Unknown = fmt.Errorf("%w (host %q)", mirror.ErrNoStatusReport, "ws") })
		}},
		{"idle with no status time", func(f *nudgeFixture) {
			f.state(func(s *mirror.AgentState) { s.Since = 0 })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNudgeFixture(t, true)
			f.gw.set(nudgeAskReply)
			f.pass(t)
			d := f.one(t)
			if d.Outcome != chiefnudge.OutcomeAsked || d.AskID == 0 {
				t.Fatalf("live ask %+v", d)
			}
			tc.unread(f)
			f.pass(t)
			f.pass(t)
			if got := f.one(t); got.Outcome != chiefnudge.OutcomeAsked {
				t.Fatalf("with the state unreadable the ask was closed: outcome %q detail %q", got.Outcome, got.Detail)
			}
			if st := f.askStatus(t, d.AskID); st != attention.Open {
				t.Fatalf("with the state unreadable the ask is %q, want open", st)
			}
			f.hostReturns()
			f.state(func(s *mirror.AgentState) { s.Unknown, s.Since = nil, nudgeSince })
			f.pass(t)
			if got := f.one(t); got.Outcome != chiefnudge.OutcomeAsked || f.askStatus(t, d.AskID) != attention.Open {
				t.Fatalf("back, unmoved: %+v", got)
			}
		})
	}
}

// TestChiefNudgeAnOpenAskIsWithdrawnWhenTheSessionWasReadAndMovedOn keeps the
// withdrawal for every state that WAS read and differs: a newer idle streak,
// not idle, another conversation in the window, the window gone from a
// connected host.
func TestChiefNudgeAnOpenAskIsWithdrawnWhenTheSessionWasReadAndMovedOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		move func(*mirror.AgentState)
	}{
		{"a newer streak", func(s *mirror.AgentState) { s.Since = nudgeSince + 45_000 }},
		{"busy", func(s *mirror.AgentState) { s.Status = mirror.StatusBusy; s.Since = nudgeSince + 46_000 }},
		{"another conversation", func(s *mirror.AgentState) { s.Session = nudgeOtherSID }},
		{"window gone from a connected host", func(s *mirror.AgentState) {
			s.Unknown = fmt.Errorf("%w (host %q, session %q, window %q)", mirror.ErrWindowGone, "ws", "main", "@1")
		}},
		{"session gone from a connected host", func(s *mirror.AgentState) {
			s.Unknown = fmt.Errorf("%w (host %q, session %q)", mirror.ErrSessionGone, "ws", "main")
		}},
		{"no Claude Code in the window", func(s *mirror.AgentState) {
			s.Unknown = fmt.Errorf("%w (host %q, session %q)", mirror.ErrNotClaude, "ws", "main")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNudgeFixture(t, true)
			f.gw.set(nudgeAskReply)
			f.pass(t)
			d := f.one(t)
			if d.Outcome != chiefnudge.OutcomeAsked {
				t.Fatalf("live ask %+v", d)
			}
			// Any new streak the pass judges is answered "leave": its row is not
			// the one checked here.
			f.gw.set(`{"decision":"leave","reason":"nothing to do"}`)
			f.state(tc.move)
			f.pass(t)
			got, err := f.nudges.Get(asUser(authz.OperatorUserID), d.ID)
			if err != nil || got.Outcome != chiefnudge.OutcomeAskClosed || got.Detail != "the session moved on; the ask was withdrawn" {
				t.Fatalf("after moving on: %+v %v", got, err)
			}
			if st := f.askStatus(t, d.AskID); st == attention.Open {
				t.Fatal("the ask is still open")
			}
		})
	}
}

// TestChiefNudgeTheSendQueueGateHoldsAnUnreadableRow: a live nudge is queued,
// then the session's state becomes unreadable. The gate answers
// SendQueueNudgeWait (never stale, never moved — a disconnected host's windows
// are just absent from the list), and the delivery hands the row BACK to
// queued (Superseded), so it is retried rather than failed.
func TestChiefNudgeTheSendQueueGateHoldsAnUnreadableRow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		unread func(*nudgeFixture)
	}{
		{"host disconnected", (*nudgeFixture).hostDrops},
		{"host reports no status", func(f *nudgeFixture) {
			f.state(func(s *mirror.AgentState) { s.Unknown = fmt.Errorf("%w (host %q)", mirror.ErrNoStatusReport, "ws") })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNudgeFixture(t, true)
			f.pass(t)
			d := f.one(t)
			if d.Outcome != chiefnudge.OutcomeQueued || d.SendQueueID == 0 {
				t.Fatalf("live nudge %+v", d)
			}
			octx := f.srv.queuedPrincipal(context.Background(), authz.OperatorUserID)
			it, err := f.queue.Get(octx, d.SendQueueID)
			if err != nil {
				t.Fatal(err)
			}
			tc.unread(f)
			if why := f.srv.sendQueueNudgeGate(octx, it); why != SendQueueNudgeWait {
				t.Fatalf("gate with the state unreadable: %q, want %q", why, SendQueueNudgeWait)
			}
			if o := f.srv.sendQueueDeliver(octx, it); !o.Superseded || o.Sent || o.MayHaveDelivered || o.Detail != SendQueueNudgeWait {
				t.Fatalf("delivery with the state unreadable: %+v, want handed back to queued", o)
			}
			if p := f.pastes(); len(p) != 0 {
				t.Fatalf("pasted while unreadable: %+v", p)
			}
		})
	}
}

// TestChiefNudgeAHeldQueuedNudgeIsRetriedNotFailed is the same hold through
// the dispatcher: the probe saw the window idle, the host dropped before the
// gate read it (the race a real disconnect makes). The row is queued again,
// the decision stays queued, and once the host is back the next pass pastes it.
func TestChiefNudgeAHeldQueuedNudgeIsRetriedNotFailed(t *testing.T) {
	f := newNudgeFixture(t, true)
	f.pass(t)
	d := f.one(t)
	if d.Outcome != chiefnudge.OutcomeQueued || d.SendQueueID == 0 {
		t.Fatalf("live nudge %+v", d)
	}
	f.hostDrops()
	f.srv.sendQueueClock = func() time.Time { return time.Now().Add(time.Hour) }
	disp := f.srv.newSendQueueDispatcher()
	disp.Probe = func(context.Context, sendqueue.Head) sendqueue.AgentState {
		// What the probe read a moment before the connection dropped.
		return sendqueue.AgentState{Idle: true, Since: nudgeSince, Observed: time.Now().Add(-20 * time.Minute), Session: nudgeSID}
	}
	if _, err := disp.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	it, err := f.queue.Get(asUser(authz.OperatorUserID), d.SendQueueID)
	if err != nil || it.State != sendqueue.Queued {
		t.Fatalf("the held row %+v %v, want queued again", it, err)
	}
	f.pass(t) // the reconcile: still in flight
	if got := f.one(t); got.Outcome != chiefnudge.OutcomeQueued {
		t.Fatalf("decision after the hold: outcome %q detail %q", got.Outcome, got.Detail)
	}
	f.hostReturns()
	f.tick(t)
	if p := f.pastes(); len(p) != 1 || p[0].text != d.Message {
		t.Fatalf("pastes after the host returned %+v", p)
	}
	f.pass(t)
	if got := f.one(t); got.Outcome != chiefnudge.OutcomeSent {
		t.Fatalf("after the paste: %+v", got)
	}
}

// TestChiefNudgeTheGateStillRefusesAReadMove keeps the gate's final answers
// for a state that was read: another conversation in the window is
// SendQueueNudgeMoved, a newer streak of the same one SendQueueNudgeStale.
func TestChiefNudgeTheGateStillRefusesAReadMove(t *testing.T) {
	for _, tc := range []struct {
		name string
		move func(*nudgeFixture)
		want string
	}{
		{"another conversation", func(f *nudgeFixture) {
			f.state(func(s *mirror.AgentState) { s.Session = nudgeOtherSID })
			f.mirror.mu.Lock()
			f.mirror.windows[0].ClaudeSession = nudgeOtherSID
			f.mirror.mu.Unlock()
		}, SendQueueNudgeMoved},
		{"a newer streak", func(f *nudgeFixture) {
			f.state(func(s *mirror.AgentState) { s.Since = nudgeSince + 47_000 })
		}, SendQueueNudgeStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNudgeFixture(t, true)
			f.pass(t)
			d := f.one(t)
			octx := f.srv.queuedPrincipal(context.Background(), authz.OperatorUserID)
			it, err := f.queue.Get(octx, d.SendQueueID)
			if err != nil {
				t.Fatal(err)
			}
			tc.move(f)
			if why := f.srv.sendQueueNudgeGate(octx, it); why != tc.want {
				t.Fatalf("gate: %q, want %q", why, tc.want)
			}
		})
	}
}

// TestChiefNudgeANudgeOrAskIsHeldWhenTheStateGoesUnreadableMidDecision: the
// host drops while the chief is thinking. The nudge (or ask) the chief chose
// is neither sent nor recorded suppressed:moved_on — nothing is recorded, so
// the streak is judged again — and once the host is back the next pass acts.
func TestChiefNudgeANudgeOrAskIsHeldWhenTheStateGoesUnreadableMidDecision(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
		want  string
	}{
		{"nudge", "", chiefnudge.OutcomeQueued},
		{"ask", nudgeAskReply, chiefnudge.OutcomeAsked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNudgeFixture(t, true)
			if tc.reply != "" {
				f.gw.set(tc.reply)
			}
			f.gw.mu.Lock()
			f.gw.during = f.hostDrops
			f.gw.mu.Unlock()
			f.pass(t)
			if list := f.decisions(t); len(list) != 0 {
				t.Fatalf("with the state unreadable at the send, decisions %+v, want none (held)", list)
			}
			if n := f.queuedRows(t); n != 0 {
				t.Fatalf("queued %d rows while unreadable", n)
			}
			f.gw.mu.Lock()
			f.gw.during = nil
			f.gw.mu.Unlock()
			f.hostReturns()
			f.pass(t)
			if got := f.one(t); got.Outcome != tc.want {
				t.Fatalf("back: outcome %q detail %q, want %q", got.Outcome, got.Detail, tc.want)
			}
		})
	}
}

// TestChiefNudgeTheGateLabelsAMoveFromItsOneRead: the AgentState read says the
// same conversation moved to a newer streak, while the host list no longer
// shows the window (the connection dropping between two reads). The row is
// SendQueueNudgeStale, from the one read the decision was made on — never
// SendQueueNudgeMoved from a second read that saw the host half gone.
func TestChiefNudgeTheGateLabelsAMoveFromItsOneRead(t *testing.T) {
	f := newNudgeFixture(t, true)
	f.pass(t)
	d := f.one(t)
	octx := f.srv.queuedPrincipal(context.Background(), authz.OperatorUserID)
	it, err := f.queue.Get(octx, d.SendQueueID)
	if err != nil {
		t.Fatal(err)
	}
	f.state(func(s *mirror.AgentState) { s.Since = nudgeSince + 48_000 })
	f.mirror.mu.Lock()
	f.mirror.online = false
	f.mirror.mu.Unlock()
	if why := f.srv.sendQueueNudgeGate(octx, it); why != SendQueueNudgeStale {
		t.Fatalf("gate: %q, want %q", why, SendQueueNudgeStale)
	}
}

// recentFails is the agent store with ListRecentChatMessages failing for one
// conversation while fail is set: a database read of the agent's conversation
// that errors, which is unknown, not moved on.
type recentFails struct {
	agents.Store
	mu      sync.Mutex
	session int64
	fail    bool
}

func (r *recentFails) ListRecentChatMessages(ctx context.Context, sessionID int64, limit int) ([]agents.ChatMessage, error) {
	r.mu.Lock()
	fail := r.fail && sessionID == r.session
	r.mu.Unlock()
	if fail {
		return nil, errors.New("recentFails: the database is unreachable")
	}
	return r.Store.ListRecentChatMessages(ctx, sessionID, limit)
}

func (r *recentFails) set(fail bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail = fail
}

// failRecent wraps f's agent store so that session's recent-messages read
// fails while set.
func (f *nudgeFixture) failRecent(session int64) *recentFails {
	r := &recentFails{Store: f.srv.ext.Agents, session: session, fail: true}
	f.srv.ext.Agents = r
	return r
}

// TestChiefNudgeAnAgentsOpenAskSurvivesAFailedConversationRead: an open ask on
// an agent's conversation, and the read of that conversation's latest message
// fails. The ask stays open and the decision asked.
func TestChiefNudgeAnAgentsOpenAskSurvivesAFailedConversationRead(t *testing.T) {
	f := newNudgeFixture(t, true)
	d, stalled, _ := liveAgentAskOpen(t, f)
	r := f.failRecent(stalled.ID)
	f.pass(t)
	if got := f.one(t); got.Outcome != chiefnudge.OutcomeAsked {
		t.Fatalf("with the conversation unreadable: outcome %q detail %q", got.Outcome, got.Detail)
	}
	if st := f.askStatus(t, d.AskID); st != attention.Open {
		t.Fatalf("the ask is %q, want open", st)
	}
	r.set(false)
	f.pass(t)
	if got := f.one(t); got.Outcome != chiefnudge.OutcomeAsked {
		t.Fatalf("readable again, unmoved: %+v", got)
	}
}

// TestChiefNudgeAnAgentRelayWaitsOutAFailedConversationRead: the owner
// answered an agent's ask; the read of the conversation fails when the relay
// is due. sendNudgeTurn holds it (relay_queued, no turn) instead of recording
// it moved on, and relays it once the read works.
func TestChiefNudgeAnAgentRelayWaitsOutAFailedConversationRead(t *testing.T) {
	f := newNudgeFixture(t, true)
	d, stalled, _, answer := liveAgentAsk(t, f)
	if d.Outcome != chiefnudge.OutcomeRelayQueued {
		t.Fatalf("claimed: %+v", d)
	}
	r := f.failRecent(stalled.ID)
	f.pass(t)
	f.pass(t)
	if _, turns := f.gw.seen(); len(turns) != 0 {
		t.Fatalf("relayed while unreadable: %v", turns)
	}
	if got := f.one(t); got.Outcome != chiefnudge.OutcomeRelayQueued {
		t.Fatalf("with the conversation unreadable: outcome %q detail %q, want relay_queued", got.Outcome, got.Detail)
	}
	r.set(false)
	f.pass(t)
	waitFor(t, func() bool { _, turns := f.gw.seen(); return len(turns) == 1 })
	if got := f.one(t); got.Outcome != chiefnudge.OutcomeRelayed {
		t.Fatalf("after the read works: %+v", got)
	}
	waitFor(t, func() bool {
		got := f.sessionMessages(t, stalled.ID, "user")
		return len(got) > 0 && got[len(got)-1] == answer
	})
}
