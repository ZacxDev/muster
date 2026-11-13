package api

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// NO FALSE 'gone', AND NO SECOND PUSH FOR A STOP BACK FROM ONE
// (session_state.go: episodeGoneMisses, listedUnkeyed; the backstop,
// sessionstate.resumedReasons). A connected host's listing that leaves a
// window out for a pass, or lists it before its session key, ends nothing: an
// episode ends gone only once its window has been missing on
// episodeGoneMisses consecutive passes of a connected host, counted on the
// episode so a restart neither loses the count nor starts it over. A stop that
// does end gone and comes back on its unchanged streak keeps its push; one
// that ended gone unpushed is pushed once.
//
// These tests use only names that exist at the base too, so each is run there
// to show which are red before the fix.

// sentEpisodes is the episode pushes SENT so far (not their clears), as
// "attention attention-episode-<id>" lines.
func (f *stateFixture) sentEpisodes() []string {
	var out []string
	for _, p := range f.pushes() {
		if strings.HasPrefix(p, "attention attention-episode-") {
			out = append(out, p)
		}
	}
	return out
}

// withdrawn reports whether episode id's notification was withdrawn (a
// "resolved" sent under its tag).
func (f *stateFixture) withdrawn(id int64) bool {
	for _, p := range f.pushes() {
		if p == "resolved attention-episode-"+strconv.FormatInt(id, 10) {
			return true
		}
	}
	return false
}

// listing sets the fixture host's online state and window list.
func (f *stateFixture) listing(online bool, ws []mirror.Window) {
	f.mirror.mu.Lock()
	defer f.mirror.mu.Unlock()
	f.mirror.online = online
	f.mirror.windows = ws
}

// windowsNow is a copy of the fixture host's window list.
func (f *stateFixture) windowsNow() []mirror.Window {
	f.mirror.mu.Lock()
	defer f.mirror.mu.Unlock()
	return append([]mirror.Window(nil), f.mirror.windows...)
}

// shellOnly is a reporting host's listing that no longer holds the session's
// window.
var shellOnly = []mirror.Window{{Session: "main", ID: "@2", Index: 2, Label: "shell"}}

// nextPass advances the clock one SessionStateInterval and runs a pass.
func (f *stateFixture) nextPass(t *testing.T) {
	t.Helper()
	f.advance(SessionStateInterval)
	f.statePass(t)
}

// oneOf is episode id as the episodes read lists it.
func (f *stateFixture) oneOf(t *testing.T, id int64) sessionstate.Episode {
	t.Helper()
	for _, e := range f.episodes(t) {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("no episode %d", id)
	return sessionstate.Episode{}
}

// openStill fails unless episode id is still open, the only episode, and its
// notification was never withdrawn.
func (f *stateFixture) openStill(t *testing.T, id int64, when string) {
	t.Helper()
	list := f.episodes(t)
	if len(list) != 1 || list[0].ID != id || !list[0].Open() {
		t.Fatalf("%s: episodes %+v, want episode %d alone and open", when, list, id)
	}
	if f.withdrawn(id) {
		t.Fatalf("%s: episode %d's notification was withdrawn", when, id)
	}
}

func episodeTag(id int64) string { return "attention attention-episode-" + strconv.FormatInt(id, 10) }

// pushedStop runs the first pass: a stalled window, pushed at once (the
// chief's nudges off). It returns the episode.
func pushedStop(t *testing.T, f *stateFixture) sessionstate.Episode {
	t.Helper()
	f.statePass(t)
	e := f.oneEpisode(t)
	if got := f.sentEpisodes(); len(got) != 1 || got[0] != episodeTag(e.ID) {
		t.Fatalf("the stop's push: %v", got)
	}
	return e
}

// TestAOnePassOmissionEndsNothing (regression): the host drops, reconnects,
// and its first listing leaves the window out for ONE pass; the next lists it
// again. Nothing ends, the notification is never withdrawn, and nothing is
// pushed again — for an hour of passes.
func TestAOnePassOmissionEndsNothing(t *testing.T) {
	f := newStateFixture(t, false)
	e := pushedStop(t, f)
	full := f.windowsNow()
	f.listing(false, full)
	f.nextPass(t)
	f.listing(true, shellOnly)
	f.nextPass(t)
	f.openStill(t, e.ID, "after a one-pass omission")
	f.listing(true, full)
	for i := 0; i < 120; i++ {
		f.nextPass(t)
	}
	f.openStill(t, e.ID, "an hour later")
	if got := f.sentEpisodes(); len(got) != 1 {
		t.Fatalf("pushes: %v, want only the first", got)
	}
}

// TestAWindowListedWithoutItsKeyIsPresent (regression): the window is missing
// for one pass short of the threshold, then the host lists it, at its place,
// but without its Claude session id (the stall facts skip such a window). For
// many passes: present, not missing — nothing ends, and the count starts over,
// so missing again one pass short still ends nothing.
func TestAWindowListedWithoutItsKeyIsPresent(t *testing.T) {
	f := newStateFixture(t, false)
	e := pushedStop(t, f)
	unkeyed := f.windowsNow()
	unkeyed[0].ClaudeSession = ""
	f.listing(true, shellOnly)
	for i := 1; i < episodeGoneMisses; i++ {
		f.nextPass(t)
	}
	f.listing(true, unkeyed)
	for i := 0; i < 2*episodeGoneMisses+1; i++ {
		f.nextPass(t)
	}
	f.openStill(t, e.ID, "listed without its key")
	if got := f.oneOf(t, e.ID); got.MissedPasses != 0 {
		t.Fatalf("missed passes %d, want 0: an unkeyed window is present", got.MissedPasses)
	}
	f.listing(true, shellOnly)
	for i := 1; i < episodeGoneMisses; i++ {
		f.nextPass(t)
	}
	f.openStill(t, e.ID, "missing again, one pass short, after the unkeyed listing")
}

// TestAWindowHoldingAnotherConversationIsMissing: the window is listed at its
// place under ANOTHER session key (a new conversation in it). That is not the
// episode's session: it counts as missing and ends gone at the threshold.
func TestAWindowHoldingAnotherConversationIsMissing(t *testing.T) {
	f := newStateFixture(t, false)
	e := pushedStop(t, f)
	other := f.windowsNow()
	other[0].ClaudeSession = "5c1d9e0a-2b3f-4a6d-8e7c-1f0a9b8c7d6e"
	f.listing(true, other)
	for i := 0; i < episodeGoneMisses; i++ {
		f.nextPass(t)
	}
	if got := f.oneOf(t, e.ID); got.Open() || got.EndReason != sessionstate.EndGone {
		t.Fatalf("its window holding another conversation: %+v, want ended gone", got)
	}
}

// TestAnOmissionOfTwoPassesEndsNothing pins the threshold's VALUE with
// literals (every other test here counts in episodeGoneMisses, so they move
// with it): two consecutive missing passes — a one-pass omission with room
// for a second — end nothing; the third, about 90 s in, ends the episode.
func TestAnOmissionOfTwoPassesEndsNothing(t *testing.T) {
	f := newStateFixture(t, false)
	e := pushedStop(t, f)
	f.listing(true, shellOnly)
	f.nextPass(t)
	f.nextPass(t)
	f.openStill(t, e.ID, "after two missing passes")
	f.nextPass(t)
	if got := f.oneOf(t, e.ID); got.Open() || got.EndReason != sessionstate.EndGone {
		t.Fatalf("after three missing passes: %+v, want ended gone", got)
	}
	if got := episodeGoneMisses * SessionStateInterval; got != 90*time.Second {
		t.Fatalf("the gone threshold is %v, want 90s (3 passes of 30 s)", got)
	}
}

// TestAWindowMissingForTheThresholdEndsGone: missing on fewer than
// episodeGoneMisses passes ends nothing — and passes with the host OFFLINE in
// between count for nothing (they neither end it nor reset the count); the
// pass that makes it episodeGoneMisses ends it gone and withdraws its
// notification.
func TestAWindowMissingForTheThresholdEndsGone(t *testing.T) {
	f := newStateFixture(t, false)
	e := pushedStop(t, f)
	f.listing(true, shellOnly)
	for i := 1; i < episodeGoneMisses; i++ {
		f.nextPass(t)
	}
	f.openStill(t, e.ID, "one pass short of the threshold")
	f.listing(false, shellOnly)
	for i := 0; i < 5; i++ {
		f.nextPass(t)
	}
	f.openStill(t, e.ID, "with the host offline")
	f.listing(true, shellOnly)
	f.nextPass(t)
	if got := f.oneOf(t, e.ID); got.Open() || got.EndReason != sessionstate.EndGone {
		t.Fatalf("missing for the threshold: %+v, want ended gone", got)
	}
	if !f.withdrawn(e.ID) {
		t.Fatal("the gone stop's notification was not withdrawn")
	}
}

// TestAWindowBackBeforeTheThresholdContinues (regression): missing for one
// pass short of the threshold, then listed again: the SAME episode continues
// and its count starts over — missing again one pass short ends nothing.
func TestAWindowBackBeforeTheThresholdContinues(t *testing.T) {
	f := newStateFixture(t, false)
	e := pushedStop(t, f)
	full := f.windowsNow()
	for round := 0; round < 2; round++ {
		f.listing(true, shellOnly)
		for i := 1; i < episodeGoneMisses; i++ {
			f.nextPass(t)
		}
		f.listing(true, full)
		f.nextPass(t)
		f.openStill(t, e.ID, "back before the threshold")
		if got := f.oneOf(t, e.ID); got.MissedPasses != 0 {
			t.Fatalf("round %d: missed passes %d after it was listed again, want 0", round, got.MissedPasses)
		}
	}
	if got := f.sentEpisodes(); len(got) != 1 {
		t.Fatalf("pushes: %v, want only the first", got)
	}
}

// TestARestartMidCountNeitherLosesNorRestartsIt (regression): one pass short
// of the threshold, muster restarts (its in-memory state gone) after ten
// minutes down. The first pass after the restart is ONE more missing pass —
// the downtime counts for nothing — so it ends the episode exactly at the
// threshold, not before (a count derived from time would end it at once)
// and not a whole threshold later (an in-memory count would start over).
func TestARestartMidCountNeitherLosesNorRestartsIt(t *testing.T) {
	f := newStateFixture(t, false)
	e := pushedStop(t, f)
	f.listing(true, shellOnly)
	for i := 1; i < episodeGoneMisses-1; i++ {
		f.nextPass(t)
	}
	// The restart: everything this process held in memory, gone.
	f.srv.hostsUnread = hostUnreads{}
	f.srv.ownerChecked = episodeChecks{}
	f.srv.attention.mu.Lock()
	f.srv.attention.episodeWaiting = nil
	f.srv.attention.mu.Unlock()
	f.advance(10 * time.Minute)
	f.statePass(t)
	f.openStill(t, e.ID, "the first pass after a restart, one short of the threshold")
	f.nextPass(t)
	if got := f.oneOf(t, e.ID); got.Open() || got.EndReason != sessionstate.EndGone {
		t.Fatalf("at the threshold after a restart: %+v, want ended gone", got)
	}
}

// TestAStopBackFromGoneOnItsStreakIsNotPushedAgain (regression; the
// backstop): the window really is missing past the threshold (its episode
// ends gone), then is listed again on the SAME streak. The new episode carries
// the first one's push and nothing more is sent — not at once, not after the
// repush floor, not an hour later.
func TestAStopBackFromGoneOnItsStreakIsNotPushedAgain(t *testing.T) {
	f := newStateFixture(t, false)
	first := pushedStop(t, f)
	full := f.windowsNow()
	f.listing(true, shellOnly)
	for i := 0; i < episodeGoneMisses; i++ {
		f.nextPass(t)
	}
	if got := f.oneOf(t, first.ID); got.Open() || got.EndReason != sessionstate.EndGone {
		t.Fatalf("missing past the threshold: %+v, want ended gone", got)
	}
	f.listing(true, full)
	f.nextPass(t)
	list := f.episodes(t)
	if len(list) != 2 || !list[0].Open() || list[0].StreakKey != first.StreakKey {
		t.Fatalf("the window back on its streak: %+v, want a second open episode on streak %q", list, first.StreakKey)
	}
	for _, d := range []time.Duration{episodeRepushFloor + time.Second, episodeChiefWait, time.Hour} {
		f.advance(d)
		f.statePass(t)
	}
	if got := f.sentEpisodes(); len(got) != 1 {
		t.Fatalf("pushes: %v, want only the first stop's — the same stop back from gone was pushed again", got)
	}
}

// TestAHeldStopAcrossADropStillPushesOnce: a waiting window's stop is HELD in
// its grace (not pushed) when its window goes missing past the threshold
// (ended gone, unpushed). Listed back, still waiting on the same streak, its
// new episode carries no push — the hold delays, it never silences — and it is
// pushed exactly once after its grace.
func TestAHeldStopAcrossADropStillPushesOnce(t *testing.T) {
	f := newStateFixture(t, false)
	f.status(mirror.StatusWaiting, 5*time.Second)
	f.statePass(t)
	first := f.oneEpisode(t)
	if !first.Waiting || !first.PushedAt.IsZero() || len(f.sentEpisodes()) != 0 {
		t.Fatalf("a held wait: %+v, pushes %v", first, f.sentEpisodes())
	}
	full := f.windowsNow()
	f.listing(true, shellOnly)
	for i := 0; i < episodeGoneMisses; i++ {
		f.advance(5 * time.Second) // inside the grace
		f.statePass(t)
	}
	if got := f.oneOf(t, first.ID); got.Open() || got.EndReason != sessionstate.EndGone {
		t.Fatalf("missing past the threshold: %+v, want ended gone", got)
	}
	if got := f.sentEpisodes(); len(got) != 0 {
		t.Fatalf("pushes while it was missing: %v", got)
	}
	f.advance(5 * time.Second)
	f.listing(true, full)
	f.statePass(t)
	list := f.episodes(t)
	if len(list) != 2 || !list[0].Open() || !list[0].PushedAt.IsZero() || list[0].StreakKey != first.StreakKey {
		t.Fatalf("the held stop back: %+v, want an open, unpushed episode on streak %q", list, first.StreakKey)
	}
	f.pastGrace(t)
	f.advance(time.Hour)
	f.statePass(t)
	if got := f.sentEpisodes(); len(got) != 1 || got[0] != episodeTag(list[0].ID) {
		t.Fatalf("pushes: %v, want the held stop pushed once, as its new episode", got)
	}
}

// goneAndBack takes the fixture's window missing past the threshold (its
// episode e ends gone) and lists it again; the state there is then set by
// back (nil: unchanged — the same streak) before the pass that reopens it.
// It returns the new episode.
func (f *stateFixture) goneAndBack(t *testing.T, e sessionstate.Episode, back func(*mirror.AgentState)) sessionstate.Episode {
	t.Helper()
	full := f.windowsNow()
	f.listing(true, shellOnly)
	for i := 0; i < episodeGoneMisses; i++ {
		f.nextPass(t)
	}
	if got := f.oneOf(t, e.ID); got.Open() || got.EndReason != sessionstate.EndGone {
		t.Fatalf("missing past the threshold: %+v, want ended gone", got)
	}
	f.listing(true, full)
	if back != nil {
		f.state(back)
	}
	f.nextPass(t)
	list := f.episodes(t)
	if len(list) != 2 || !list[0].Open() || list[0].ID == e.ID {
		t.Fatalf("the window back: %+v, want a second, open episode", list)
	}
	return list[0]
}

// TestAQuietedStopBackFromGoneStaysQuiet (regression): the chief left a stop
// that asked nothing (quiet, never pushed); its window goes missing past the
// threshold (gone) and comes back on the SAME streak. The chief's decision on
// that stop holds for its return: no push — not after the chief's wait, not
// an hour later — the chief is not asked again, and the row stays quiet.
func TestAQuietedStopBackFromGoneStaysQuiet(t *testing.T) {
	f, _ := newNeedsYouFixture(t, jevNothing, true)
	f.gw.set(`{"decision":"leave","reason":"it is done"}`)
	f.judge(t)
	first := f.oneEpisode(t)
	if q := f.quietOf(t, first.ID); q != sessionstate.QuietLeft || f.checks() != 1 || len(f.sentEpisodes()) != 0 {
		t.Fatalf("precondition: quiet %q, %d chief call(s), pushes %v", q, f.checks(), f.sentEpisodes())
	}
	back := f.goneAndBack(t, first, nil)
	if back.StreakKey != first.StreakKey {
		t.Fatalf("back on streak %q, want %q", back.StreakKey, first.StreakKey)
	}
	for _, d := range []time.Duration{0, episodeChiefWait + time.Minute, time.Hour} {
		f.advance(d)
		f.pass(t)
		f.statePass(t)
	}
	if got := f.sentEpisodes(); len(got) != 0 {
		t.Fatalf("pushes: %v — a stop the chief left was pushed when it came back from gone", got)
	}
	if f.checks() != 1 {
		t.Fatalf("the chief was asked %d times, want once", f.checks())
	}
	if q := f.quietOf(t, back.ID); q != sessionstate.QuietLeft {
		t.Fatalf("the stop back: quiet %q, want %q", q, sessionstate.QuietLeft)
	}
}

// TestAQuietedStopBackOnANewStreakIsJudgedAfresh: the same, but the session
// ran in between (a new streak): a new stop, which the chief judges afresh.
func TestAQuietedStopBackOnANewStreakIsJudgedAfresh(t *testing.T) {
	f, _ := newNeedsYouFixture(t, jevNothing, true)
	f.gw.set(`{"decision":"leave","reason":"it is done"}`)
	f.judge(t)
	first := f.oneEpisode(t)
	back := f.goneAndBack(t, first, func(s *mirror.AgentState) {
		s.Status, s.Since, s.Observed = mirror.StatusIdle, nudgeSince+60_000, f.srv.now().Add(-20*time.Minute)
	})
	if back.StreakKey == first.StreakKey || back.ResumedFrom != 0 {
		t.Fatalf("back on a new streak: %+v", back)
	}
	f.pass(t)
	if f.checks() != 2 {
		t.Fatalf("the chief was asked %d times, want twice (the new stop judged afresh)", f.checks())
	}
}
