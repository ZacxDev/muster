package api

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// A WAITING WINDOW'S GRACE (session_push.go, EpisodeWaitingGrace): a window
// waiting on a prompt muster cannot answer is HELD for its grace (60 s) and
// pushed by the state pass only if its episode is still open
// once the grace has passed. The operator's report: "needs you" pushes for
// sessions that are actually active — measured, nearly every such prompt was
// answered at the terminal within a minute, and each one buzzed the phone.
//
// 🔴 The regression tests below use only names that exist at the base too
// (the fixture, the hub hook, the pass, the clock, a restarted server), and
// spell the grace as a literal, so each is shown RED against the base by
// running it there: the base pushes from the hook at once.

// TestAWaitingPromptAnsweredInsideItsGraceNeverPushes (regression): the
// window starts waiting; the hook opens its episode and pushes nothing; the
// owner answers the prompt at the terminal 10 s later. The episode ends
// 'owner', was never pushed (no pushed_at, no notification), and the grace
// passing afterwards pushes nothing either.
func TestAWaitingPromptAnsweredInsideItsGraceNeverPushes(t *testing.T) {
	f := newStateFixture(t, false)
	f.statusSince(mirror.StatusWaiting, nudgeSince+500_000, 2*time.Second)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	e := f.oneEpisode(t)
	if got := f.pushes(); len(got) != 0 {
		t.Fatalf("pushed from the hook at once: %v", got)
	}
	f.statePass(t) // still inside the grace
	if got := f.pushes(); len(got) != 0 {
		t.Fatalf("pushed by a pass inside the grace: %v", got)
	}
	f.aShortTurn(t) // the owner approves at the terminal: 10 s busy, then idle 30 s later
	got := f.episodeByID(t, e.ID)
	if got.EndedAt.IsZero() || got.EndReason != endOwner {
		t.Fatalf("after the owner answered: ended %v reason %q, want %q", got.EndedAt, got.EndReason, endOwner)
	}
	f.advance(2 * time.Minute)
	f.statePass(t)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	if p := f.pushes(); len(p) != 0 {
		t.Fatalf("pushes: %v, want none — the prompt was answered inside its grace", p)
	}
	if got := f.episodeByID(t, e.ID); !got.PushedAt.IsZero() {
		t.Fatalf("the answered stop was recorded as pushed at %v", got.PushedAt)
	}
}

// TestAWaitingWindowStillWaitingAfterItsGracePushesOnce (regression): the
// window waits and nobody answers. The hook and a pass 30 s in push nothing,
// and the hold writes nothing (no quiet: the row is a plain waiting row); the
// first pass past 60 s pushes it, exactly once — later passes and hook nudges
// add nothing.
func TestAWaitingWindowStillWaitingAfterItsGracePushesOnce(t *testing.T) {
	f := newStateFixture(t, false)
	f.status(mirror.StatusWaiting, 2*time.Second)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	e := f.oneEpisode(t)
	f.advance(30 * time.Second)
	f.statePass(t)
	if got := f.pushes(); len(got) != 0 {
		t.Fatalf("pushed inside the grace: %v", got)
	}
	if q := f.quietOf(t, e.ID); q != "" {
		t.Fatalf("quiet %q inside the grace, want none (the hold is not persisted)", q)
	}
	if sec := f.srv.episodeSectionHTML(asUser(authz.OperatorUserID), f.srv.now()); strings.Contains(sec, "data-attention-episode-quiet") {
		t.Fatalf("the held row is drawn quiet:\n%s", sec)
	}
	f.advance(31 * time.Second)
	f.statePass(t)
	want := "attention attention-episode-" + strconv.FormatInt(e.ID, 10)
	if got := f.pushes(); len(got) != 1 || got[0] != want {
		t.Fatalf("pushes past the grace: %v, want [%s]", got, want)
	}
	for i := 0; i < 3; i++ {
		f.advance(30 * time.Second)
		f.statePass(t)
		f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	}
	if got := f.pushes(); len(got) != 1 {
		t.Fatalf("pushes after more passes: %v", got)
	}
}

// TestARestartMidHoldStillPushesOnce (regression): the hook holds the stop;
// muster restarts (a fresh process: nothing in memory), and once the grace
// has passed TWO replicas run their passes at the same moment. The stop is
// pushed exactly once — what is held is the row, and the push is claimed on
// pushed_at.
func TestARestartMidHoldStillPushesOnce(t *testing.T) {
	f := newStateFixture(t, false)
	f.status(mirror.StatusWaiting, 2*time.Second)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	e := f.oneEpisode(t)
	if got := f.pushes(); len(got) != 0 {
		t.Fatalf("pushed from the hook at once: %v", got)
	}
	f.restart()
	other := f.restarted()
	other.now, other.nudgeReadHook = f.srv.now, f.srv.nudgeReadHook
	f.advance(61 * time.Second)
	var wg sync.WaitGroup
	for _, s := range []*Server{f.srv, other} {
		wg.Add(1)
		go func(s *Server) {
			defer wg.Done()
			if err := s.SessionStatePass(context.Background()); err != nil {
				t.Errorf("pass: %v", err)
			}
		}(s)
	}
	wg.Wait()
	other.WaitForPushes()
	want := "attention attention-episode-" + strconv.FormatInt(e.ID, 10)
	if got := f.pushes(); len(got) != 1 || got[0] != want {
		t.Fatalf("pushes after a restart and two replicas: %v, want [%s]", got, want)
	}
}

// TestAWaitingWindowWhoseStateIsUnknownStillPushesOnce (regression, audit
// round 1 F1): the host lists the window waiting but reports no StatusSince
// (an older muster-mirror or Claude Code), so the hub's AgentState is Unknown
// and the pass sees no status for it. The hook opens and holds its episode;
// the pass must still push it once the grace has passed — from the host's own
// listing of the window — exactly once, however many passes and hook nudges
// follow. Before the fix it was held by the hook and never pushed at all.
func TestAWaitingWindowWhoseStateIsUnknownStillPushesOnce(t *testing.T) {
	f := newStateFixture(t, false)
	f.unknownStatus(mirror.StatusWaiting)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	e := f.oneEpisode(t)
	f.advance(30 * time.Second)
	f.statePass(t)
	if got := f.pushes(); len(got) != 0 {
		t.Fatalf("pushed inside the grace: %v", got)
	}
	f.advance(31 * time.Second)
	f.statePass(t)
	want := "attention attention-episode-" + strconv.FormatInt(e.ID, 10)
	if got := f.pushes(); len(got) != 1 || got[0] != want {
		t.Fatalf("pushes past the grace for a window whose state is unknown: %v, want [%s]", got, want)
	}
	for i := 0; i < 6; i++ {
		f.advance(30 * time.Second)
		f.statePass(t)
		f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	}
	if got := f.pushes(); len(got) != 1 {
		t.Fatalf("pushes after more passes and hook nudges: %v, want one", got)
	}
}

// TestAnUnknownStateWindowNoLongerWaitingIsNotPushed (control for the F1 fix):
// the same unknown-state window, but by the time the grace has passed the host
// lists it busy (the prompt was answered at the terminal). The pass reads the
// host's listing, so it pushes nothing for it: the fix pushes a wait the host
// still shows, not every held episode.
func TestAnUnknownStateWindowNoLongerWaitingIsNotPushed(t *testing.T) {
	f := newStateFixture(t, false)
	f.unknownStatus(mirror.StatusWaiting)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.oneEpisode(t)
	f.advance(20 * time.Second)
	f.unknownStatus(mirror.StatusBusy)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	for i := 0; i < 4; i++ {
		f.advance(30 * time.Second)
		f.statePass(t)
	}
	if got := f.pushes(); len(got) != 0 {
		t.Fatalf("pushed a window the host lists busy: %v", got)
	}
}

// TestAHeldWaitWhoseHostDropsPushesAfterItsGrace (regression, audit round 1
// F2): the host drops 20 s into the hold and stays offline. The window was
// waiting when it was last seen, so once the grace has passed the pass pushes
// it — within the grace and one pass of it opening — and exactly once, also
// when the host comes back still showing it waiting. Before the fix it stayed
// unpushed for as long as the host was offline.
func TestAHeldWaitWhoseHostDropsPushesAfterItsGrace(t *testing.T) {
	f := newStateFixture(t, false)
	f.status(mirror.StatusWaiting, 2*time.Second)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	e := f.oneEpisode(t)
	f.advance(20 * time.Second)
	f.setOnline(false)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.advance(30 * time.Second) // 50 s: inside the grace
	f.statePass(t)
	if got := f.pushes(); len(got) != 0 {
		t.Fatalf("pushed inside the grace: %v", got)
	}
	f.advance(30 * time.Second) // 80 s: the first pass past the grace
	f.statePass(t)
	want := "attention attention-episode-" + strconv.FormatInt(e.ID, 10)
	if got := f.pushes(); len(got) != 1 || got[0] != want {
		t.Fatalf("pushes 80 s in, the host offline since 20 s: %v, want [%s]", got, want)
	}
	for i := 0; i < 10; i++ {
		f.advance(30 * time.Second)
		f.statePass(t)
	}
	f.setOnline(true)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.advance(30 * time.Second)
	f.statePass(t)
	// The hook clears the window's own waiting tag when its host comes back (a
	// "resolved", not a buzz — it does so with no fix too): count the buzzes.
	if got := f.buzzes(); len(got) != 1 {
		t.Fatalf("buzzes after the host stayed offline and came back: %v, want one", got)
	}
}

// TestAWaitThatEndsBeforeItsHostDropsIsNotPushed (control for the F2 fix): the
// owner answers inside the grace (the episode ends 'owner'), then the host
// drops. Nothing is pushed: only a wait still open is pushed for an offline
// host.
func TestAWaitThatEndsBeforeItsHostDropsIsNotPushed(t *testing.T) {
	f := newStateFixture(t, false)
	f.statusSince(mirror.StatusWaiting, nudgeSince+500_000, 2*time.Second)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	e := f.oneEpisode(t)
	f.aShortTurn(t)
	if got := f.episodeByID(t, e.ID); got.EndReason != endOwner {
		t.Fatalf("after the owner answered: reason %q, want %q", got.EndReason, endOwner)
	}
	f.setOnline(false)
	for i := 0; i < 6; i++ {
		f.advance(30 * time.Second)
		f.statePass(t)
	}
	if got := f.pushes(); len(got) != 0 {
		t.Fatalf("pushed an answered wait once its host dropped: %v", got)
	}
}

// buzzes is f.pushes without the clears ("resolved …"): what reached the
// operator's phone as a notification.
func (f *stateFixture) buzzes() []string {
	var out []string
	for _, p := range f.pushes() {
		if !strings.HasPrefix(p, "resolved ") {
			out = append(out, p)
		}
	}
	return out
}

// unknownStatus lists the fixture's window in status with no StatusSince, so
// the hub's AgentState for it is Unknown (mirror.ErrNoStatusSince).
func (f *stateFixture) unknownStatus(status string) {
	f.mirror.mu.Lock()
	defer f.mirror.mu.Unlock()
	f.mirror.windows[0].Status, f.mirror.windows[0].StatusSince = status, 0
	f.mirror.agentState = &mirror.AgentState{Runtime: mirror.RuntimeClaude, Unknown: mirror.ErrNoStatusSince, Session: nudgeSID}
}

// setOnline drops the fixture's host (stamped offline now, as the hub does) or
// brings it back.
func (f *stateFixture) setOnline(on bool) {
	f.mu.Lock()
	now := f.clock
	f.mu.Unlock()
	f.mirror.mu.Lock()
	defer f.mirror.mu.Unlock()
	f.mirror.online, f.mirror.offlineSince = on, now
}
