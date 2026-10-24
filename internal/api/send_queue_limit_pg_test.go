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

// THE BUG, through the real server against a real Postgres: a session hit its
// usage limit, and the queue sent every message it held into that limit.
//
// Claude Code under a usage limit refuses each message at once with a record
// it writes itself and goes back to its prompt — idle, with a NEWER status
// time, which is all the queue waited for. The fake host here plays exactly
// that: each paste ends its turn at once, and the host reports how (a
// mirror.TurnError: a usage limit, the prompt's only reply).
//
// 🔴 RED ON origin/main, where the host's report does not exist: drop the
// TurnError assignment in onPaste below (the one line main cannot compile) and
// the queue drains — all three pasted in as many passes.
func TestAUsageLimitHoldsTheQueueAndFailsOnlyTheBouncedMessage(t *testing.T) {
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
	// onPaste runs under f.mirror.mu after the paste was recorded.
	f.mirror.onPaste = func() {
		prev := f.mirror.agentState.Since
		st := mirror.AgentState{Status: mirror.StatusIdle, Since: prev + 700, Observed: time.Now().Add(-time.Hour), Pasted: prev}
		if limited {
			st.TurnError = &mirror.TurnError{Kind: mirror.TurnErrorLimit, At: time.Now().UnixMilli(), ResetsAt: reset.UnixMilli(), Rejected: true, Quick: 1}
		}
		f.mirror.agentState = &st
	}

	for i := 0; i < 6; i++ {
		f.tick(t)
	}
	if got := f.pasted(); len(got) != 1 || got[0] != first.Body {
		t.Fatalf("pasted %q into a session under a usage limit; want the first message only", got)
	}
	if r := f.row(t, first.ID); r.State != sendqueue.Failed || r.Detail != sendqueue.RejectedLimitDetail || r.MayHaveDelivered {
		t.Fatalf("the message the limit bounced: %s %q (may have been sent: %t); want failed, %q", r.State, r.Detail, r.MayHaveDelivered, sendqueue.RejectedLimitDetail)
	}
	for _, it := range []sendqueue.Item{second, third} {
		if r := f.row(t, it.ID); r.State != sendqueue.Queued {
			t.Fatalf("%q: %s; want still queued", it.Body, r.State)
		}
	}
	// The composer's list: the queue held, saying until when (in the reader's
	// zone, the server's own rendering in UTC); the bounced row with its
	// reason and a plain Retry.
	_, page := mirrorGet(t, f.srv, f.ts, ui.SendQueuePath+"?"+url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}}.Encode())
	for _, want := range []string{`data-queue-hold`, sendqueue.LimitHeldText, `data-clock="` + strconv.FormatInt(reset.UnixMilli(), 10) + `"`,
		reset.UTC().Format("15:04") + " UTC", sendqueue.RejectedLimitDetail, `action="` + ui.SendQueueRetryPath(first.ID) + `"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("the queue's list does not contain %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, `action="`+ui.SendQueueRetryPath(first.ID)+`" data-retry="" data-confirm`) {
		t.Fatalf("the bounced row's Retry asks whether it may already have been sent; it was not:\n%s", page)
	}

	// The reset passes (plus the margin), the limit lifted: the rest go, one
	// per turn — and the bounced message is not re-sent on its own.
	limited = false
	mu.Lock()
	now = reset.Add(sendqueue.LimitMargin)
	mu.Unlock()
	for i := 0; i < 6; i++ {
		f.tick(t)
	}
	want := []string{first.Body, second.Body, third.Body}
	if got := f.pasted(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("after the reset, pasted %q; want %q (the bounced one only by a Retry)", got, want)
	}
	if r := f.row(t, first.ID); r.State != sendqueue.Failed {
		t.Fatalf("the bounced message after the reset: %s; want still failed, waiting for a Retry by hand", r.State)
	}
}
