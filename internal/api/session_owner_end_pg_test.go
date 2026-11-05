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

// THE OWNER ACTING ENDS AN EPISODE (sessionstate.OwnerActed, read by
// episodeOwnerActed), end to end on a real Postgres: the owner goes to the
// session and answers it — a message of their own after the stop, or the
// waiting prompt answered — and the episode ends on the next pass, its push
// withdrawn and its Stalled / waiting row gone, even though the session then
// works for far less than EndAfter. A chief nudge and the owner's message from
// before the stop do not end it. (The owner OPENING the session in muster ends
// it too, 'viewed': session_viewed_pg_test.go.)
//
// 🔴 These tests name only what exists at the base too (the end reason is the
// literal "owner", not the new constant), so each REGRESSION test is shown red
// at the base by running it there.

// endOwner is sessionstate.EndOwner, spelled as the column holds it.
const endOwner = "owner"

// statusSince puts the fixture's window in status as Claude Code wrote it at
// since (host ms) — a NEW status time is a new streak, as a real turn makes —
// observed for d before the clock.
func (f *stateFixture) statusSince(status string, since int64, d time.Duration) {
	f.mu.Lock()
	now := f.clock
	f.mu.Unlock()
	f.mirror.mu.Lock()
	defer f.mirror.mu.Unlock()
	f.mirror.windows[0].Status, f.mirror.windows[0].StatusSince = status, since
	f.mirror.agentState = &mirror.AgentState{Runtime: mirror.RuntimeClaude, Status: status, Since: since,
		Observed: now.Add(-d), Session: nudgeSID}
}

// say appends m to the session's transcript.
func (f *stateFixture) say(m chiefnudge.Msg) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = append(f.script, m)
}

// clearedAt reports whether episode id's notification was recorded closed.
func (f *stateFixture) cleared(t *testing.T, id int64) bool {
	t.Helper()
	var ok bool
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT cleared_at IS NOT NULL FROM session_episodes WHERE id = $1`, id).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// episodeByID is episode id, open or ended (episodes).
func (f *stateFixture) episodeByID(t *testing.T, id int64) sessionstate.Episode {
	t.Helper()
	for _, e := range f.episodes(t) {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("no episode %d", id)
	return sessionstate.Episode{}
}

// openedAt is episode id's opened_at, as the database wrote it.
func (f *stateFixture) openedAt(t *testing.T, id int64) time.Time {
	t.Helper()
	var at time.Time
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT opened_at FROM session_episodes WHERE id = $1`, id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// pushedStall opens and pushes the fixture's idle stop (no chief judges it, so
// it pushes at once) and returns it with its notification tag.
func (f *stateFixture) pushedStall(t *testing.T) (id int64, tag string) {
	t.Helper()
	f.statePass(t)
	e := f.oneEpisode(t)
	tag = "attention-episode-" + strconv.FormatInt(e.ID, 10)
	if got := f.pushes(); len(got) != 1 || got[0] != "attention "+tag {
		t.Fatalf("the stop's push: %v", got)
	}
	return e.ID, tag
}

// aShortTurn is the session working for 40 s (well under EndAfter) on a new
// streak, then idle again on another: two passes.
func (f *stateFixture) aShortTurn(t *testing.T) {
	t.Helper()
	f.advance(10 * time.Second)
	f.statusSince(mirror.StatusBusy, nudgeSince+600_000, 0)
	f.statePass(t)
	f.advance(30 * time.Second)
	f.statusSince(mirror.StatusIdle, nudgeSince+640_000, 0)
	f.statePass(t)
}

// TestTheOwnerAnsweringAStallAtTheTerminalEndsItAndClearsThePush (regression):
// the operator's report. A stall is pushed; the owner goes to the window and
// types an answer (a prompt of their own, written after the stop opened);
// Claude works 40 s and stops. The episode ends "owner" on that pass, its
// notification is withdrawn (a resolved push for its tag, cleared_at set), and
// the Asks page's Stalled / waiting row is gone. Red at the base: a 40 s turn
// never ended an episode, so the push and the row stayed.
func TestTheOwnerAnsweringAStallAtTheTerminalEndsItAndClearsThePush(t *testing.T) {
	f := newStateFixture(t, false)
	id, tag := f.pushedStall(t)
	f.say(chiefnudge.Msg{Role: "user", Text: "yes, merge it", ID: "line:9000", At: f.openedAt(t, id).Add(3 * time.Second)})
	f.aShortTurn(t)
	e := f.episodeByID(t, id)
	if e.EndedAt.IsZero() || e.EndReason != endOwner {
		t.Fatalf("after the owner answered: ended %v reason %q, want %q", e.EndedAt, e.EndReason, endOwner)
	}
	if got := f.pushes(); len(got) != 2 || got[1] != "resolved "+tag {
		t.Fatalf("pushes: %v, want the push then its withdrawal", got)
	}
	if !f.cleared(t, id) {
		t.Fatal("cleared_at not set")
	}
	if sec := f.srv.episodeSectionHTML(asUser(authz.OperatorUserID), time.Now()); sec != "" {
		t.Fatalf("the Stalled / waiting row is still drawn:\n%s", sec)
	}
	if list := f.episodes(t); len(list) != 1 {
		t.Fatalf("a new stop opened: %+v", list)
	}
}

// TestTheOwnerAnsweringAWaitingPromptEndsItsEpisode (regression): a window
// waiting on a permission prompt is pushed once its grace has passed; the
// owner approves it at the terminal and Claude works 40 s. The episode ends "owner" and its
// push is withdrawn. Red at the base: a permission let through was a blip
// that never ended one.
func TestTheOwnerAnsweringAWaitingPromptEndsItsEpisode(t *testing.T) {
	f := newStateFixture(t, false)
	f.statusSince(mirror.StatusWaiting, nudgeSince+500_000, 5*time.Second)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	e := f.oneEpisode(t)
	tag := "attention-episode-" + strconv.FormatInt(e.ID, 10)
	f.pastGrace(t)
	if got := f.pushes(); len(got) != 1 || got[0] != "attention "+tag {
		t.Fatalf("the waiting push: %v", got)
	}
	f.aShortTurn(t)
	got := f.episodeByID(t, e.ID)
	if got.EndedAt.IsZero() || got.EndReason != endOwner {
		t.Fatalf("after the prompt was answered: ended %v reason %q", got.EndedAt, got.EndReason)
	}
	if p := f.pushes(); len(p) != 2 || p[1] != "resolved "+tag || !f.cleared(t, e.ID) {
		t.Fatalf("pushes: %v (cleared %v)", p, f.cleared(t, e.ID))
	}
}

// TestAWaitingPromptStillWaitingIsNotEnded — an INVARIANT GUARD: an owner
// message newer than the opening does not end an episode whose window still
// waits (the session needs the owner whatever the transcript says). Green at
// the base too.
func TestAWaitingPromptStillWaitingIsNotEnded(t *testing.T) {
	f := newStateFixture(t, false)
	f.statusSince(mirror.StatusWaiting, nudgeSince+500_000, 5*time.Second)
	f.statePass(t)
	e := f.oneEpisode(t)
	f.say(chiefnudge.Msg{Role: "user", Text: "go ahead", ID: "line:9000", At: f.openedAt(t, e.ID).Add(3 * time.Second)})
	f.advance(30 * time.Second)
	f.statusSince(mirror.StatusWaiting, nudgeSince+530_000, 0)
	f.statePass(t)
	if got := f.oneEpisode(t); !got.Open() {
		t.Fatalf("a window still waiting was ended: %+v", got)
	}
}

// TestAChiefNudgeIsNotTheOwnerActing — an INVARIANT GUARD (green at the base,
// where nothing short ended an episode): the chief's nudge, pasted into the
// window after the stop, starts a short turn — that is muster acting, not the
// owner, so the episode and its push stay.
func TestAChiefNudgeIsNotTheOwnerActing(t *testing.T) {
	f := newStateFixture(t, false)
	id, _ := f.pushedStall(t)
	quoted := chiefnudge.Msg{Role: "user", Text: nudgeOwnerText, ID: nudgeOwnerID, At: time.Now().Add(-10 * time.Minute)}
	f.say(chiefnudge.Msg{Role: "user", Text: chiefnudge.NudgeText("Operator", quoted, "keep going until merged", time.UTC),
		ID: "line:9000", At: f.openedAt(t, id).Add(3 * time.Second)})
	f.aShortTurn(t)
	if e := f.oneEpisode(t); e.ID != id || !e.Open() {
		t.Fatalf("a chief nudge ended the episode: %+v", e)
	}
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes: %v", got)
	}
}

// TestTheOwnersMessageBeforeTheStopKeepsItOpen — an INVARIANT GUARD: the
// session works a short turn of its own (a background task's notice) with the
// owner's newest message the one from BEFORE the stop: nothing the owner did
// since, so the episode stays open (the 120 s rule still governs it).
func TestTheOwnersMessageBeforeTheStopKeepsItOpen(t *testing.T) {
	f := newStateFixture(t, false)
	id, _ := f.pushedStall(t)
	f.say(chiefnudge.Msg{Role: "user", Text: "and then tag it", ID: "line:8000", At: f.openedAt(t, id).Add(-time.Second)})
	f.aShortTurn(t)
	if e := f.oneEpisode(t); e.ID != id || !e.Open() {
		t.Fatalf("an owner message from before the stop ended it: %+v", e)
	}
	if f.readCount() == 0 {
		t.Fatal("the owner-acted read never ran: this test checked nothing")
	}
}

// TestTheOwnerAnsweringAnAgentEndsItsStop (regression): an agent
// conversation's stop; the owner sends it a message (after the stop opened)
// and it replies 10 s later. The stop ends "owner". Red at the base: a quick
// exchange stayed one stop.
func TestTheOwnerAnsweringAnAgentEndsItsStop(t *testing.T) {
	f := newStateFixture(t, false)
	f.srv.nudgeReadHook = nil
	f.mirror.mu.Lock()
	f.mirror.windows = nil
	f.mirror.mu.Unlock()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	worker := f.agentFor(t, authz.OperatorUserID, "stateowner-"+suffix, "worker", "state-owner-tok-"+suffix)
	sess, _ := f.stallAgent(t, worker, authz.OperatorUserID)
	store := agents.NewPG(f.pool)
	ctx := asUser(authz.OperatorUserID)
	f.statePass(t)
	var ep int64
	for _, e := range f.episodes(t) {
		if e.AgentID == worker.ID {
			ep = e.ID
		}
	}
	if ep == 0 {
		t.Fatal("the agent's stop did not open")
	}
	opened := f.openedAt(t, ep)
	for i, m := range []agents.ChatMessage{
		{AgentID: worker.ID, SessionID: sess.ID, Role: "user", Content: "main"},
		{AgentID: worker.ID, SessionID: sess.ID, Role: "assistant", Content: "Merged into main. Delete the branch too?"},
	} {
		got, err := store.AddChatMessage(ctx, m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE chat_messages SET created_at = $2 WHERE id = $1`,
			got.ID, opened.Add(time.Duration(5+10*i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	f.advance(30 * time.Second)
	f.statePass(t)
	e := f.episodeByID(t, ep)
	if e.EndedAt.IsZero() || e.EndReason != endOwner {
		t.Fatalf("after the owner answered the agent: ended %v reason %q", e.EndedAt, e.EndReason)
	}
}
