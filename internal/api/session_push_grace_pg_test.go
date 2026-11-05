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
