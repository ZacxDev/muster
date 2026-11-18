package api

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// #635 audit round 1: a pane that runs no agent is gone, not "unkeyed"; a
// chief's decision anywhere in a resumed stop holds; a stop back from gone
// that was pushed has its notification shown again, silently.

// shellIn is the fixture's window as a pane running no agent: Claude gone
// from it (no Claude flag, no session id), a shell in its place.
func shellIn(w mirror.Window, command string) mirror.Window {
	w.Claude, w.ClaudeSession, w.Status, w.StatusSince, w.Pending, w.Command = false, "", "", 0, nil, command
	return w
}

// TestAWindowWhoseAgentExitedEndsGone (regression, audit round 1 🔴1): the
// window stays listed, but its pane runs no agent any more — Claude exited
// (Ctrl-C, /exit) and left the shell, or a tmux restart reused the window id
// for an unrelated shell. Its episode ends gone at the threshold, not before,
// and its notification is withdrawn — and stays ended through an hour of the
// shell sitting there. RED at the head before the fix: listed without a key,
// it counted as present forever.
func TestAWindowWhoseAgentExitedEndsGone(t *testing.T) {
	for _, c := range []struct {
		name string
		mut  func(mirror.Window) mirror.Window
	}{
		{"claude exited, the shell left in the pane", func(w mirror.Window) mirror.Window { return shellIn(w, "zsh") }},
		{"tmux restarted, the window id reused for another shell", func(w mirror.Window) mirror.Window {
			w = shellIn(w, "bash")
			w.Index, w.Label = 4, ""
			return w
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newStateFixture(t, false)
			e := pushedStop(t, f)
			ws := f.windowsNow()
			ws[0] = c.mut(ws[0])
			f.listing(true, ws)
			for i := 1; i < episodeGoneMisses; i++ {
				f.nextPass(t)
			}
			f.openStill(t, e.ID, "one pass short of the threshold")
			f.nextPass(t)
			if got := f.oneOf(t, e.ID); got.Open() || got.EndReason != sessionstate.EndGone {
				t.Fatalf("its agent gone from the pane: %+v, want ended gone", got)
			}
			if !f.withdrawn(e.ID) {
				t.Fatal("the gone stop's notification was not withdrawn")
			}
			for i := 0; i < 120; i++ {
				f.nextPass(t)
			}
			if list := f.episodes(t); len(list) != 1 || list[0].Open() {
				t.Fatalf("an hour of the shell: %+v, want the one episode, ended", list)
			}
		})
	}
}

// TestAnUnkeyedOpencodePaneIsPresent: the rule's other runtime. An opencode
// window's stop; the host then lists the window still running opencode but
// without its session id. Present, not missing: nothing ends.
func TestAnUnkeyedOpencodePaneIsPresent(t *testing.T) {
	f := newStateFixture(t, false)
	ws := f.windowsNow()
	ws[0].Claude, ws[0].ClaudeSession = false, ""
	ws[0].Runtime, ws[0].AgentSession = mirror.RuntimeOpenCode, "ses_round1probe"
	f.listing(true, ws)
	f.statePass(t)
	e := f.oneEpisode(t)
	if e.Key != "opencode:ses_round1probe" {
		t.Fatalf("the opencode stop: %+v", e)
	}
	ws[0].AgentSession = ""
	f.listing(true, ws)
	for i := 0; i < 2*episodeGoneMisses; i++ {
		f.nextPass(t)
	}
	f.openStill(t, e.ID, "an opencode pane listed without its session id")
}

// TestAHeldWaitWhoseClaudeExitedIsNotPushedOnADrop (regression, audit round 1
// 🔴1): a waiting stop is HELD in its grace when its Claude is Ctrl-C'd (the
// shell stays in the pane), then its host drops for minutes. Nothing is
// pushed: the session is gone, not waiting. RED at the head before the fix:
// the episode stayed open as "unkeyed", and the host-drop rule pushed a
// "needs you" for a dead session.
func TestAHeldWaitWhoseClaudeExitedIsNotPushedOnADrop(t *testing.T) {
	f := newStateFixture(t, false)
	f.status(mirror.StatusWaiting, 5*time.Second)
	f.statePass(t)
	first := f.oneEpisode(t)
	if !first.Waiting || !first.PushedAt.IsZero() {
		t.Fatalf("a held wait: %+v", first)
	}
	ws := f.windowsNow()
	ws[0] = shellIn(ws[0], "zsh")
	f.listing(true, ws)
	for i := 0; i < episodeGoneMisses; i++ {
		f.advance(5 * time.Second)
		f.statePass(t)
	}
	f.listing(false, ws)
	for i := 0; i < 10; i++ {
		f.nextPass(t)
	}
	if got := f.sentEpisodes(); len(got) != 0 {
		t.Fatalf("pushes: %v — a held wait whose Claude exited was pushed on a host drop", got)
	}
}

// TestADecisionOnAResumedEpisodeHoldsForTheNextResume (regression, audit
// round 1 🟡2): stop A goes missing before the chief judges it (gone); it
// comes back as B (linked to A) and the chief LEAVES B; B goes gone and comes
// back as C (linked to A too). B's leave holds for C — one stop: no push, and
// the chief is not asked again. RED at the head before the fix: C compared
// the decision's episode (B) with its own link (A) and ignored it.
func TestADecisionOnAResumedEpisodeHoldsForTheNextResume(t *testing.T) {
	f, _ := newNeedsYouFixture(t, jevNothing, true)
	f.gw.set(`{"decision":"leave","reason":"it is done"}`)
	f.statePass(t) // A opens; held for the chief's wait, never judged
	a := f.oneEpisode(t)
	if f.checks() != 0 || len(f.sentEpisodes()) != 0 {
		t.Fatalf("precondition: %d chief call(s), pushes %v", f.checks(), f.sentEpisodes())
	}
	b := f.goneAndBack(t, a, nil)
	if b.ResumedFrom != a.ID {
		t.Fatalf("B: %+v, want resumed from A (%d)", b, a.ID)
	}
	f.pass(t) // the chief leaves B
	f.statePass(t)
	if q := f.quietOf(t, b.ID); q != sessionstate.QuietLeft || f.checks() != 1 {
		t.Fatalf("B judged: quiet %q, %d chief call(s)", q, f.checks())
	}
	full := f.windowsNow()
	f.listing(true, shellOnly)
	for i := 0; i < episodeGoneMisses; i++ {
		f.nextPass(t)
	}
	f.listing(true, full)
	f.nextPass(t)
	list := f.episodes(t)
	if len(list) != 3 || !list[0].Open() || list[0].ResumedFrom != a.ID {
		t.Fatalf("C: %+v, want a third, open episode resumed from A (%d)", list, a.ID)
	}
	c := list[0]
	for _, d := range []time.Duration{0, episodeChiefWait + time.Minute, time.Hour} {
		f.advance(d)
		f.pass(t)
		f.statePass(t)
	}
	if got := f.sentEpisodes(); len(got) != 0 {
		t.Fatalf("pushes: %v — B's leave did not hold for C", got)
	}
	if f.checks() != 1 {
		t.Fatalf("the chief was asked %d times, want once", f.checks())
	}
	if q := f.quietOf(t, c.ID); q != sessionstate.QuietLeft {
		t.Fatalf("C: quiet %q, want %q", q, sessionstate.QuietLeft)
	}
}

// TestAPushedStopBackFromGoneIsReshownSilently (regression, audit round 1
// 🟡3): a pushed stop goes missing past the threshold (ended gone, its
// notification withdrawn) and comes back on its streak. Its notification is
// shown again ONCE, under the new episode's tag, marked silent — and it is
// no new push: nothing alerts again, and the episode keeps the first push's
// time (the repush floor reads that). RED at the head before the fix: no
// re-show — the notification stayed withdrawn.
func TestAPushedStopBackFromGoneIsReshownSilently(t *testing.T) {
	f := newStateFixture(t, false)
	first := pushedStop(t, f)
	pushedAt := f.oneOf(t, first.ID).PushedAt
	back := f.goneAndBack(t, first, nil)
	if !f.withdrawn(first.ID) {
		t.Fatal("the gone stop's notification was not withdrawn")
	}
	for i := 0; i < 10; i++ {
		f.nextPass(t)
	}
	if got := f.reshows(); len(got) != 1 || got[0] != "attention-episode-"+itoa(back.ID) {
		t.Fatalf("silent re-shows: %v, want one, under episode %d's tag", got, back.ID)
	}
	if got := f.sentEpisodes(); len(got) != 1 || got[0] != episodeTag(first.ID) {
		t.Fatalf("alerting pushes: %v, want only the first stop's", got)
	}
	if got := f.oneOf(t, back.ID); !got.PushedAt.Equal(pushedAt) {
		t.Fatalf("the stop back: pushed_at %v, want the first push's %v", got.PushedAt, pushedAt)
	}
}

// TestAnUnpushedStopBackFromGoneIsNotReshown: the re-show is of a notification
// that was SHOWN. A stop that ended gone unpushed has nothing to put back; its
// return is pushed (alerting) once, as any stop — never a silent stand-in.
func TestAnUnpushedStopBackFromGoneIsNotReshown(t *testing.T) {
	f := newStateFixture(t, false)
	f.status(mirror.StatusWaiting, 5*time.Second)
	f.statePass(t)
	first := f.oneEpisode(t)
	full := f.windowsNow()
	f.listing(true, shellOnly)
	for i := 0; i < episodeGoneMisses; i++ {
		f.advance(5 * time.Second)
		f.statePass(t)
	}
	f.listing(true, full)
	f.advance(5 * time.Second)
	f.statePass(t)
	f.pastGrace(t)
	if got := f.reshows(); len(got) != 0 {
		t.Fatalf("silent re-shows: %v, want none for a stop never pushed", got)
	}
	if got := f.sentEpisodes(); len(got) != 1 || got[0] == episodeTag(first.ID) {
		t.Fatalf("alerting pushes: %v, want the returned stop pushed once", got)
	}
}
