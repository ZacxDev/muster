package api

import (
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE OPERATOR'S REPORT, through the real server against a real Postgres:
// "when a session hits its limit, all the queued messages send immediately".
//
// Claude Code under a usage limit rejects each message at once with a record
// it writes itself and goes back to its prompt — idle, with a NEWER status
// time, which is all the queue waited for. The fake host here plays exactly
// that: each paste ends its turn at once, and the host reports how
// (limitRejects, which names the prompt the limit answered).
//
// 🔴 RED ON origin/main: there the queue drains — all three pasted in as many
// passes (the host's report does not exist, so limitRejects reports nothing;
// send_queue_limit_shim_test.go is the one file swapped to run this there).
func TestAUsageLimitHoldsTheQueueInsteadOfDrainingIt(t *testing.T) {
	f := newSQFixture(t)
	var mu sync.Mutex
	now := time.Now()
	f.srv.sendQueueClock = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	reset := now.Add(2 * time.Hour)
	limited := true
	f.mirror.agentState = &mirror.AgentState{Status: mirror.StatusIdle, Since: now.Add(-time.Hour).UnixMilli(), Observed: now.Add(-time.Hour)}
	first := f.enqueue(t, "the first, queued", time.Minute, 0)
	second := f.enqueue(t, "the second, queued", time.Minute, 0)
	third := f.enqueue(t, "the third, queued", time.Minute, 0)
	// onPaste runs under f.mirror.mu, with f.mirror.v2.mu held by the same
	// paste: the paste just recorded is the last one.
	f.mirror.onPaste = func() {
		ps := f.mirror.v2.pastes
		text := ps[len(ps)-1].text
		prev := f.mirror.agentState.Since
		st := mirror.AgentState{Status: mirror.StatusIdle, Since: prev + 700, Observed: time.Now().Add(-time.Hour), Pasted: prev}
		if limited {
			limitRejects(&st, text, reset)
		}
		f.mirror.agentState = &st
	}

	for i := 0; i < 6; i++ {
		f.tick(t)
	}
	if got := f.pasted(); len(got) != 1 || got[0] != first.Body {
		t.Fatalf("pasted %q into a session under a usage limit; want the first message only", got)
	}
	if r := f.row(t, first.ID); r.State != sendqueue.Queued || returnsOf(r) != 1 {
		t.Fatalf("the message the limit rejected: %s, returned %d; want queued again (returned once), not counted as sent", r.State, returnsOf(r))
	}
	for _, it := range []sendqueue.Item{second, third} {
		if r := f.row(t, it.ID); r.State != sendqueue.Queued {
			t.Fatalf("%q: %s; want still queued", it.Body, r.State)
		}
	}
	// The composer's list says it is held, and when it resumes, in reserved
	// space; the rejected message says it came back.
	_, page := mirrorGet(t, f.srv, f.ts, ui.SendQueuePath+"?"+url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}}.Encode())
	for _, want := range []string{`data-queue-hold="limit"`, limitHeldPrefix, `data-clock="` + strconv.FormatInt(reset.UnixMilli(), 10) + `"`, `data-queue-returned`} {
		if !strings.Contains(page, want) {
			t.Fatalf("the queue's list does not contain %q:\n%s", want, page)
		}
	}

	// The reset passes (plus the margin): the returned message is sent again —
	// a NEW delivery, not taken by the send path for the one the limit
	// refused — and, the limit lifted, the rest follow one per turn.
	limited = false
	mu.Lock()
	now = reset.Add(limitMargin)
	mu.Unlock()
	for i := 0; i < 6; i++ {
		f.tick(t)
	}
	want := []string{first.Body, first.Body, second.Body, third.Body}
	if got := f.pasted(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("after the reset, pasted %q; want %q", got, want)
	}
	if r := f.row(t, first.ID); r.State != sendqueue.Sent || r.Detail != "Sent." {
		t.Fatalf("the returned message, resent: %s %q; want sent, pasted (not \"already delivered\")", r.State, r.Detail)
	}
}
