package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE OWNER OPENING A SESSION ENDS ITS EPISODE (session_viewed.go), THE
// OWNER-ACTED READ COVERS THE BACKLOG, AND MIGRATION 0079 ENDS THE BACKLOG ONCE
// — end to end on a real Postgres.
//
// 🔴 These tests name only what exists at the commit before this change too
// (end reasons as the column holds them, no helper of the change), so each
// REGRESSION test is shown red there by running it there.

// The end reasons this change adds, spelled as the column holds them.
const (
	endViewed = "viewed"
	endReset  = "reset"
)

// viewPath is the fixture window's rendered view.
func viewPath() string { return ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"}.ViewPath() }

// restart swaps the fixture's server for a fresh process over the same
// database and extensions, on the fixture's clock and transcript.
func (f *stateFixture) restart() {
	s := f.restarted()
	s.now, s.nudgeReadHook = f.srv.now, f.srv.nudgeReadHook
	f.srv, f.h = s, s.Handler()
}

// get is a GET of path as the operator, with extra headers.
func (f *stateFixture) get(t *testing.T, path string, extra map[string]string) int {
	t.Helper()
	return f.call(t, http.MethodGet, path, nil, merge(f.operator(), extra)).Code
}

// handledTwoPassesLater runs two more passes, a minute apart in all, and
// reports whether episode id is still the only one, still ended for reason,
// with no push since the withdrawal: a stop the owner handled is not opened
// and pushed again while the session sits on the same streak.
func (f *stateFixture) handledTwoPassesLater(t *testing.T, id int64, reason string, pushes int) {
	t.Helper()
	f.advance(30 * time.Second)
	f.statePass(t)
	f.advance(30 * time.Second)
	f.statePass(t)
	list := f.episodes(t)
	if len(list) != 1 || list[0].ID != id || list[0].EndReason != reason {
		t.Fatalf("after the stop was handled (%s), the same stop opened again: %+v", reason, list)
	}
	if got := f.episodePushes(); len(got) != pushes {
		t.Fatalf("pushes after the stop was handled: %v, want %d", got, pushes)
	}
}

// TestOpeningTheSessionsViewEndsItsEpisodeAndClearsThePush (regression, Q1):
// a stall is pushed; the owner opens the window's rendered view in muster (as
// the push's tap lands there) and does nothing else. The episode ends
// 'viewed', its push is withdrawn and cleared_at set, its Stalled / waiting
// row is gone, and the passes after it do not open the same stop again. Red
// before: opening the view ended nothing (TestViewingASessionDoesNotEndItsEpisode
// pinned that).
func TestOpeningTheSessionsViewEndsItsEpisodeAndClearsThePush(t *testing.T) {
	f := newStateFixture(t, false)
	id, tag := f.pushedStall(t)
	if code := f.get(t, viewPath(), nil); code != http.StatusOK {
		t.Fatalf("GET %s: %d", viewPath(), code)
	}
	e := f.episodeByID(t, id)
	if e.EndedAt.IsZero() || e.EndReason != endViewed {
		t.Fatalf("after the owner opened the session: ended %v reason %q, want %q", e.EndedAt, e.EndReason, endViewed)
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
	f.handledTwoPassesLater(t, id, endViewed, 2)
}

// TestOpeningTheSessionsTerminalEndsItsEpisode (regression): the window's
// terminal page is the session's own page too.
func TestOpeningTheSessionsTerminalEndsItsEpisode(t *testing.T) {
	f := newStateFixture(t, false)
	id, _ := f.pushedStall(t)
	path := ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"}.TerminalPath()
	if code := f.get(t, path, nil); code != http.StatusOK {
		t.Fatalf("GET %s: %d", path, code)
	}
	if e := f.episodeByID(t, id); e.EndReason != endViewed {
		t.Fatalf("after the owner opened the terminal: reason %q, want %q", e.EndReason, endViewed)
	}
}

// TestOpeningAWaitingWindowEndsItAndTheHookDoesNotReopenIt (regression): a
// window waiting on a prompt is pushed from the hook; the owner opens it and
// leaves the prompt as it is. The episode ends 'viewed', and neither the hook
// (on the next mirror change, and after a restart, when it sees every waiting
// window as new) nor a pass opens the same wait again; a new wait is a new
// stop.
func TestOpeningAWaitingWindowEndsItAndTheHookDoesNotReopenIt(t *testing.T) {
	f := newStateFixture(t, false)
	f.statusSince(mirror.StatusWaiting, nudgeSince+500_000, 5*time.Second)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	e := f.oneEpisode(t)
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("the waiting push: %v", got)
	}
	if code := f.get(t, viewPath(), nil); code != http.StatusOK {
		t.Fatalf("GET view: %d", code)
	}
	if got := f.episodeByID(t, e.ID); got.EndReason != endViewed {
		t.Fatalf("after the owner opened the waiting window: reason %q", got.EndReason)
	}
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	// A restart: the hook sees every waiting window as new (first seen by
	// this process) — the same wait is still not opened again.
	f.restart()
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.handledTwoPassesLater(t, e.ID, endViewed, 2)
	// The session moving on — a NEW wait — is a new stop.
	f.advance(30 * time.Second)
	f.statusSince(mirror.StatusWaiting, nudgeSince+900_000, 5*time.Second)
	f.statePass(t)
	if list := f.episodes(t); len(list) != 2 || !list[0].Open() {
		t.Fatalf("a new wait opened no new stop: %+v", list)
	}
}

// TestOpeningAnAgentConversationEndsItsStop (regression): an agent
// conversation's stop; the owner opens that conversation's page. It ends
// 'viewed'.
func TestOpeningAnAgentConversationEndsItsStop(t *testing.T) {
	f := newStateFixture(t, false)
	f.srv.nudgeReadHook = nil
	f.mirror.mu.Lock()
	f.mirror.windows = nil
	f.mirror.mu.Unlock()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	worker := f.agentFor(t, authz.OperatorUserID, "stateview-"+suffix, "worker", "state-view-tok-"+suffix)
	sess, _ := f.stallAgent(t, worker, authz.OperatorUserID)
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
	if code := f.get(t, ui.AgentSessionHref(worker.Name, sess.ID), nil); code != http.StatusOK {
		t.Fatalf("GET the conversation: %d", code)
	}
	if e := f.episodeByID(t, ep); e.EndedAt.IsZero() || e.EndReason != endViewed {
		t.Fatalf("after the owner opened the conversation: ended %v reason %q", e.EndedAt, e.EndReason)
	}
}

// TestRenderingListsPrefetchesAndTheTapAloneDoNotEndIt — the view rule's other
// side: the bar, the Agents list, the Asks page (which draws the stop's own
// Stalled / waiting row), the tap route on its own (it only redirects), a
// prefetch of the view and a HEAD of it are not the owner opening the
// session. The episode stays open and its push stays shown, across passes.
func TestRenderingListsPrefetchesAndTheTapAloneDoNotEndIt(t *testing.T) {
	f := newStateFixture(t, false)
	id, _ := f.pushedStall(t)
	for _, c := range []struct {
		path string
		hdr  map[string]string
		code int
	}{
		{ui.NavAgentsPath, nil, http.StatusOK},
		{ui.HomeFallbackPath, nil, 0},
		{attentionPath, nil, http.StatusOK},
		{attentionPath + "/items", nil, http.StatusOK},
		{"/attention/open/episode-" + strconv.FormatInt(id, 10), nil, http.StatusSeeOther},
		{viewPath(), map[string]string{"Sec-Purpose": "prefetch"}, http.StatusOK},
		{viewPath(), map[string]string{"Purpose": "prefetch"}, http.StatusOK},
	} {
		if code := f.get(t, c.path, c.hdr); c.code != 0 && code != c.code {
			t.Fatalf("GET %s %v: %d, want %d", c.path, c.hdr, code, c.code)
		}
		if e := f.episodeByID(t, id); !e.Open() {
			t.Fatalf("GET %s %v ended the episode: %+v", c.path, c.hdr, e)
		}
	}
	if w := f.call(t, http.MethodHead, viewPath(), nil, f.operator()); w.Code != http.StatusOK {
		t.Fatalf("HEAD view: %d", w.Code)
	}
	f.advance(30 * time.Second)
	f.statePass(t)
	if e := f.oneEpisode(t); e.ID != id || !e.Open() {
		t.Fatalf("after the lists, prefetches and the tap: %+v", e)
	}
	if got := f.episodePushes(); len(got) != 1 {
		t.Fatalf("pushes: %v", got)
	}
}

// TestOpeningAnotherSessionDoesNotEndThisOne: two windows, each stopped; the
// owner opens the second. Only the second's episode ends.
func TestOpeningAnotherSessionDoesNotEndThisOne(t *testing.T) {
	f := newStateFixture(t, false)
	const otherSID = "6a1c2b3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	f.mirror.mu.Lock()
	f.mirror.windows = append(f.mirror.windows, mirror.Window{Session: "main", ID: "@2", Index: 2, Label: "Gold:2", Claude: true,
		ClaudeSession: otherSID, Status: mirror.StatusIdle, StatusSince: nudgeSince})
	f.mirror.mu.Unlock()
	f.statePass(t)
	byKey := map[string]int64{}
	for _, e := range f.episodes(t) {
		byKey[e.Key] = e.ID
	}
	if byKey[nudgeSID] == 0 || byKey[otherSID] == 0 {
		t.Fatalf("both windows' stops: %v", byKey)
	}
	path := ui.MirrorRef{Host: "ws", Session: "main", Window: "@2"}.ViewPath()
	if code := f.get(t, path, nil); code != http.StatusOK {
		t.Fatalf("GET %s: %d", path, code)
	}
	if e := f.episodeByID(t, byKey[otherSID]); e.EndReason != endViewed {
		t.Fatalf("the opened window's stop: reason %q, want %q (the control)", e.EndReason, endViewed)
	}
	if e := f.episodeByID(t, byKey[nudgeSID]); !e.Open() {
		t.Fatalf("opening another window ended this one: %+v", e)
	}
}

// TestAnotherUserOpeningTheSessionDoesNotEndIt — an INVARIANT GUARD (nothing
// ended an episode on a view before): a member asking for the owner's window
// ends nothing of the owner's. The page itself refuses them (the window is
// not on a host of theirs); the episode rule's own owner scoping is pinned
// below the page in session_viewed_scope_pg_test.go.
func TestAnotherUserOpeningTheSessionDoesNotEndIt(t *testing.T) {
	f := newStateFixture(t, false)
	id, _ := f.pushedStall(t)
	w := httptest.NewRecorder()
	f.srv.handleMirrorViewPage(w, httptest.NewRequest(http.MethodGet, viewPath(), nil).WithContext(asUser(f.member)))
	if e := f.episodeByID(t, id); !e.Open() {
		t.Fatalf("another user asking for the session (HTTP %d) ended the owner's stop: %+v", w.Code, e)
	}
}

// TestAnEpisodeWhoseOwnerRepliedBeforeThisCodeIsReadOnce (regression, the
// round-0 backlog finding): an open episode whose owner already answered, and
// whose session's new streak an earlier pass already wrote onto it (as every
// pass's Observe does — here, a pass of the code before the owner-acted
// check), is read on the next pass and ends 'owner'. And a session that then
// sits still is not read again on every pass. Red before: a read was owed only
// when the session's streak differed from the stored one, so it never was.
func TestAnEpisodeWhoseOwnerRepliedBeforeThisCodeIsReadOnce(t *testing.T) {
	f := newStateFixture(t, false)
	f.statePass(t)
	ep := f.oneEpisode(t)
	// Before this code: the owner answered, the session worked briefly and
	// stopped on a new streak, and the old pass observed it.
	f.say(chiefnudge.Msg{Role: "user", Text: "yes, merge it", ID: "line:9000", At: f.openedAt(t, ep.ID).Add(3 * time.Second)})
	f.advance(40 * time.Second)
	f.statusSince(mirror.StatusIdle, nudgeSince+640_000, 0)
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE session_episodes SET streak_key = $2 WHERE id = $1`,
		ep.ID, "since:"+strconv.FormatInt(nudgeSince+640_000, 10)); err != nil {
		t.Fatal(err)
	}
	// This code, freshly started.
	f.restart()
	f.advance(30 * time.Second)
	f.statePass(t)
	if e := f.episodeByID(t, ep.ID); e.EndedAt.IsZero() || e.EndReason != endOwner {
		t.Fatalf("the backlog episode the owner already answered: ended %v reason %q, want %q", e.EndedAt, e.EndReason, endOwner)
	}
}

// TestASessionSittingStillIsReadOnceNotEveryPass — the bound the backlog fix
// keeps: an open episode with no owner action is read once after it opens (or
// after a restart), then not again while its session sits still.
func TestASessionSittingStillIsReadOnceNotEveryPass(t *testing.T) {
	f := newStateFixture(t, false)
	f.statePass(t)
	ep := f.oneEpisode(t)
	f.restart()
	for i := 0; i < 4; i++ {
		f.advance(30 * time.Second)
		f.statePass(t)
	}
	if n := f.readCount(); n != 1 {
		t.Fatalf("reads across four still passes after a restart: %d, want 1", n)
	}
	if e := f.oneEpisode(t); e.ID != ep.ID || !e.Open() {
		t.Fatalf("a still session's stop: %+v", e)
	}
}

// TestMigration0079EndsAPushedOpenEpisodeAndTheNextPassWithdrawsIt
// (regression, the operator's backlog decision): a stop pushed before 0079
// applies is ended 'reset' by it; the next pass withdraws its push (cleared_at
// set, a resolved push for its tag) and drops its row, and the session, still
// on the same streak, is not opened and pushed again. The migration is applied
// as the migrator would, scoped to the operator's rows so the package's
// shared database keeps other tests' episodes. Red before: 0079 only widened
// the check, so the episode stayed open and pushed.
func TestMigration0079EndsAPushedOpenEpisodeAndTheNextPassWithdrawsIt(t *testing.T) {
	f := newStateFixture(t, false)
	id, tag := f.pushedStall(t)
	f.applyReset(t)
	if e := f.episodeByID(t, id); e.EndedAt.IsZero() || e.EndReason != endReset {
		t.Fatalf("after 0079: ended %v reason %q, want %q", e.EndedAt, e.EndReason, endReset)
	}
	if f.cleared(t, id) {
		t.Fatal("cleared before any pass: the withdrawal is the pass's")
	}
	f.advance(30 * time.Second)
	f.statePass(t)
	if !f.cleared(t, id) {
		t.Fatal("cleared_at not set after the next pass")
	}
	if got := f.pushes(); len(got) != 2 || got[1] != "resolved "+tag {
		t.Fatalf("pushes: %v, want the push then its withdrawal", got)
	}
	if sec := f.srv.episodeSectionHTML(asUser(authz.OperatorUserID), time.Now()); sec != "" {
		t.Fatalf("the Stalled / waiting row is still drawn:\n%s", sec)
	}
	f.handledTwoPassesLater(t, id, endReset, 2)
}
