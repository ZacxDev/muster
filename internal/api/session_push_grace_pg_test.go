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

// TestAHostBlipAnsweredThroughNeverPushes (regression, audit round 2 🟡-1):
// the window waits; its host drops 50 s in, the owner answers the prompt at
// the terminal while it is away, and it reconnects 25 s later. The first pass
// past the episode's own grace (61 s) finds the host offline for only 11 s, so
// it pushes nothing; once the host is back the stop ends 'owner'. Before the
// fix that pass pushed it: an offline host's held wait was pushed as soon as
// the EPISODE's grace had passed, however briefly the host had been away.
func TestAHostBlipAnsweredThroughNeverPushes(t *testing.T) {
	f := newStateFixture(t, false)
	f.statusSince(mirror.StatusWaiting, nudgeSince+500_000, 2*time.Second)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	e := f.oneEpisode(t)
	f.advance(50 * time.Second)
	f.setOnline(false)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.advance(11 * time.Second) // 61 s: past the episode's grace, the host away 11 s
	f.statePass(t)
	if got := f.buzzes(); len(got) != 0 {
		t.Fatalf("pushed 11 s into a host blip: %v", got)
	}
	f.advance(14 * time.Second) // 75 s: back, the prompt answered meanwhile
	f.statusSince(mirror.StatusBusy, nudgeSince+600_000, 0)
	f.setOnline(true)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.statePass(t)
	f.advance(30 * time.Second)
	f.statusSince(mirror.StatusIdle, nudgeSince+640_000, 0)
	f.statePass(t)
	if got := f.episodeByID(t, e.ID); got.EndedAt.IsZero() || got.EndReason != endOwner {
		t.Fatalf("after the blip: ended %v reason %q, want %q", got.EndedAt, got.EndReason, endOwner)
	}
	for i := 0; i < 4; i++ {
		f.advance(30 * time.Second)
		f.statePass(t)
	}
	if got := f.buzzes(); len(got) != 0 {
		t.Fatalf("buzzes for a prompt answered through a host blip: %v, want none", got)
	}
}

// TestALastWindowClosedInsideTheHoldNeverPushes (regression, audit round 2
// 🟡-1): the window waits, and 20 s in it is closed — the last one on its
// host, which stays connected and reporting status but now lists no windows
// at all. The host is connected, so this is no drop: no pass pushes the held
// wait. (Nor does the empty list end it — it is indistinguishable from a
// failed first read, TestAHostThatHasSentNoWindowListEndsNothing.) Before the
// fix an empty list counted as unread, so the held wait was pushed as if its
// host had dropped.
func TestALastWindowClosedInsideTheHoldNeverPushes(t *testing.T) {
	f := newStateFixture(t, false)
	f.status(mirror.StatusWaiting, 2*time.Second)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	e := f.oneEpisode(t)
	f.advance(20 * time.Second)
	f.mirror.mu.Lock()
	f.mirror.windows = nil
	f.mirror.mu.Unlock()
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	for i := 0; i < 4; i++ {
		f.advance(30 * time.Second)
		f.statePass(t)
	}
	if got := f.buzzes(); len(got) != 0 {
		t.Fatalf("pushed a window closed inside its hold: %v", got)
	}
	if got := f.episodeByID(t, e.ID); !got.PushedAt.IsZero() {
		t.Fatalf("the closed window's episode was recorded pushed at %v", got.PushedAt)
	}
}

// TestAnEmptyListThatWasAFailedReadOnlyDelaysThePush (control for holding the
// drop push on a connected host with an empty list): the list was in fact a
// failed read — the window is still waiting when the host's next list arrives
// 90 s in. Nothing is pushed while the list is empty, and the wait is pushed
// once, on its own episode, once the host lists it again: a delay, never a
// silence.
func TestAnEmptyListThatWasAFailedReadOnlyDelaysThePush(t *testing.T) {
	f := newStateFixture(t, false)
	f.status(mirror.StatusWaiting, 2*time.Second)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	e := f.oneEpisode(t)
	f.mirror.mu.Lock()
	kept := f.mirror.windows
	f.mirror.windows = nil
	f.mirror.mu.Unlock()
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	for i := 0; i < 3; i++ {
		f.advance(30 * time.Second)
		f.statePass(t)
	}
	if got := f.buzzes(); len(got) != 0 {
		t.Fatalf("pushed while its connected host listed no windows: %v", got)
	}
	f.mirror.mu.Lock()
	f.mirror.windows = kept
	f.mirror.mu.Unlock()
	f.status(mirror.StatusWaiting, 2*time.Second)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	for i := 0; i < 4; i++ {
		f.advance(30 * time.Second)
		f.statePass(t)
	}
	want := "attention attention-episode-" + strconv.FormatInt(e.ID, 10)
	if got := f.buzzes(); len(got) != 1 || got[0] != want {
		t.Fatalf("buzzes once the still-waiting window was listed again: %v, want [%s]", got, want)
	}
}

// TestAFreshProcessWaitsTheGraceForAHostItHasNotSeen (regression, audit round
// 2 🟡-1): the hook holds a wait; muster restarts, and the fresh process's hub
// has no hosts yet (they have not reconnected). Its first pass, past the
// episode's grace, pushes nothing — it has never seen the host, so it has not
// known it offline for any time at all — and the wait is pushed once that
// process has gone the grace without reading the host: a delay, never a
// silence. Before the fix the fresh process pushed it on its first pass.
func TestAFreshProcessWaitsTheGraceForAHostItHasNotSeen(t *testing.T) {
	f := newStateFixture(t, false)
	f.status(mirror.StatusWaiting, 2*time.Second)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	e := f.oneEpisode(t)
	f.restart()
	f.mirror.mu.Lock()
	f.mirror.noHosts = true
	f.mirror.mu.Unlock()
	f.advance(61 * time.Second)
	f.statePass(t)
	if got := f.buzzes(); len(got) != 0 {
		t.Fatalf("a fresh process pushed for a host it has not seen: %v", got)
	}
	f.advance(30 * time.Second)
	f.statePass(t)
	if got := f.buzzes(); len(got) != 0 {
		t.Fatalf("pushed 30 s after the fresh process first missed the host: %v", got)
	}
	f.advance(31 * time.Second)
	f.statePass(t)
	want := "attention attention-episode-" + strconv.FormatInt(e.ID, 10)
	if got := f.buzzes(); len(got) != 1 || got[0] != want {
		t.Fatalf("pushes once the fresh process went the grace without the host: %v, want [%s]", got, want)
	}
	for i := 0; i < 4; i++ {
		f.advance(30 * time.Second)
		f.statePass(t)
	}
	if got := f.buzzes(); len(got) != 1 {
		t.Fatalf("buzzes after more passes: %v, want one", got)
	}
}

// TestAnAnsweredUnknownStateWaitIsNotPushedOnADropAndTheNextOneIs
// (regression, audit round 2 🟡-2): a window whose host reports no StatusSince
// waits, and the owner answers it 20 s in. Half an hour later the host drops:
// nothing buzzes — the stop ended when the pass read it busy. The host comes
// back with the window waiting on a NEW prompt nobody answers: it buzzes once,
// within five minutes. Before the fix the answered stop never ended, so the
// drop pushed it, and the new prompt found that episode still open and got no
// push of its own.
func TestAnAnsweredUnknownStateWaitIsNotPushedOnADropAndTheNextOneIs(t *testing.T) {
	f := newStateFixture(t, false)
	f.unknownStatus(mirror.StatusWaiting)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	first := f.oneEpisode(t)
	f.advance(20 * time.Second)
	f.unknownStatus(mirror.StatusBusy)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.advance(30 * time.Second)
	f.statePass(t)
	f.advance(30 * time.Second)
	f.unknownStatus(mirror.StatusIdle)
	f.statePass(t)
	f.advance(30 * time.Minute)
	f.setOnline(false)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	for i := 0; i < 6; i++ {
		f.advance(30 * time.Second)
		f.statePass(t)
	}
	if got := f.buzzes(); len(got) != 0 {
		t.Fatalf("buzzes on a drop half an hour after the prompt was answered: %v, want none", got)
	}
	if got := f.episodeByID(t, first.ID); got.EndedAt.IsZero() || got.EndReason != endOwner {
		t.Fatalf("the answered unknown-state stop: ended %v reason %q, want %q", got.EndedAt, got.EndReason, endOwner)
	}
	f.setOnline(true)
	f.unknownStatus(mirror.StatusWaiting)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	for i := 0; i < 10; i++ { // five minutes, unanswered
		f.advance(30 * time.Second)
		f.statePass(t)
	}
	if got := f.buzzes(); len(got) != 1 {
		t.Fatalf("buzzes for a new prompt left unanswered for five minutes: %v, want exactly one", got)
	}
}

// TestAnUnknownStateWaitAnsweredJustBeforeItsHostDropsIsNotPushed (guard for
// the streak check in the drop push, audit round 2 🟡-2): the window's host
// reports no StatusSince; the owner answers 20 s in and the host drops before
// any pass reads it busy. The episode is still open and was waiting when last
// seen, but its turn was never read, so the drop does not push it: such a wait
// is pushed only from its host listing it waiting. The accepted cost: an
// unknown-state wait left UNanswered when its host drops is not pushed until
// the host lists it again.
func TestAnUnknownStateWaitAnsweredJustBeforeItsHostDropsIsNotPushed(t *testing.T) {
	f := newStateFixture(t, false)
	f.unknownStatus(mirror.StatusWaiting)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.oneEpisode(t)
	f.advance(20 * time.Second)
	f.unknownStatus(mirror.StatusBusy)
	f.setOnline(false)
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	for i := 0; i < 6; i++ {
		f.advance(30 * time.Second)
		f.statePass(t)
	}
	if got := f.buzzes(); len(got) != 0 {
		t.Fatalf("pushed an unknown-state wait on its host's drop: %v", got)
	}
}

// TestAHeldIdleStopWhoseHostDropsIsNotPushedAsAWait (guard for the ep.Waiting
// check in the drop push, audit round 2 🟢): an idle stall held for the live
// chief's decision, whose host then drops for two minutes. The drop push is
// for a WAITING stop only: it must not settle this idle one on waiting facts
// (which would push it as "needs you" past the grace).
func TestAHeldIdleStopWhoseHostDropsIsNotPushedAsAWait(t *testing.T) {
	f := newStateFixture(t, true)
	f.live()
	f.statePass(t)
	e := f.oneEpisode(t)
	if e.Waiting {
		t.Fatal("the fixture's idle stop opened as waiting")
	}
	if got := f.episodePushes(); len(got) != 0 {
		t.Fatalf("the held idle stop pushed before the drop: %v", got)
	}
	f.setOnline(false)
	for i := 0; i < 4; i++ {
		f.advance(30 * time.Second)
		f.statePass(t)
	}
	if got := f.episodePushes(); len(got) != 0 {
		t.Fatalf("an idle stop whose host dropped was pushed as a wait: %v", got)
	}
}
