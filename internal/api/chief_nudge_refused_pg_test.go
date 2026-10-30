package api

import (
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
)

// A STALL CHECK THE CHIEF REFUSES IS RETRIED, BOUNDED (chief_nudge_retry.go).
//
// ccd never reaps an idle operator lane and a check never stops one, so once
// the owner holds CCD_MAX_SESSIONS sessions with the chief every check gets 503
// capacity — for ever (and a ccd with CCD_MAX_SESSIONS=0 answers every check
// 503 not_ready, for ever). These pin what the sweep does about it: the
// classifier is not re-billed per pass, and past the bound the streak is
// recorded, shown and no longer asked about.

// refusedClock is a settable clock for f.srv.now.
type refusedClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *refusedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *refusedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// newRefusedFixture is a claude-code chief's lane fixture with the classifier
// wired and muster's clock settable, plus the server's log.
func newRefusedFixture(t *testing.T) (*nudgeFixture, *nudgeCCD, *jevFake, *refusedClock, *lockedBuf) {
	t.Helper()
	f, ccd := newLaneNudgeFixture(t, agents.KindClaudeCode)
	j := withJev(t, f)
	j.set(200, 0.05, 0.05, 0.90)
	clk := &refusedClock{at: time.Now()}
	f.srv.now = clk.now
	buf := &lockedBuf{}
	f.srv.logger = log.New(buf, "", 0)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server log:\n%s", buf.String())
		}
	})
	return f, ccd, j, clk, buf
}

func (j *jevFake) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.calls
}

// TestChiefNudgeARefusedCheckReusesItsClassification: two passes refused for
// capacity, then one the chief answers — the classifier (an OpenRouter call on
// the owner's key) is called ONCE for the streak, and the decision finally
// recorded carries that one result.
func TestChiefNudgeARefusedCheckReusesItsClassification(t *testing.T) {
	f, ccd, j, clk, _ := newRefusedFixture(t)
	ccd.setFull(true)
	for i := 0; i < 2; i++ {
		if n := f.pass(t); n != 0 {
			t.Fatalf("pass %d decided %d with ccd full, want 0", i, n)
		}
		clk.advance(ChiefNudgeInterval)
	}
	if got := len(ccd.seen()); got != 2 {
		t.Fatalf("chief turns %d, want 2 (one per refused pass)", got)
	}
	if got := j.count(); got != 1 {
		t.Fatalf("the classifier was called %d times across two refused passes of one streak, want 1 (the retry reuses its result)", got)
	}
	ccd.setFull(false)
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d once ccd had room, want 1", n)
	}
	if got := j.count(); got != 1 {
		t.Fatalf("the classifier was called %d times by the answered retry, want still 1", got)
	}
	if d := f.one(t); d.JevLabel != chiefnudge.LabelNeither {
		t.Fatalf("recorded classifier label %q, want %q (the kept result)", d.JevLabel, chiefnudge.LabelNeither)
	}
}

// TestChiefNudgeLanesFullPastTheBoundIsRecorded: a streak refused for capacity
// on every pass is retried silently up to chiefNudgeRefusedBound, then recorded
// suppressed:lanes_full with a detail that says why and what to do — and the
// next pass does not ask the chief about it again. The first refusal is logged
// once, not every pass.
func TestChiefNudgeLanesFullPastTheBoundIsRecorded(t *testing.T) {
	f, ccd, j, clk, buf := newRefusedFixture(t)
	ccd.setFull(true)
	for elapsed := time.Duration(0); elapsed < chiefNudgeRefusedBound; elapsed += 10 * time.Minute {
		if n := f.pass(t); n != 0 {
			t.Fatalf("decided %d at %s of refusals, want 0 (inside the bound)", n, elapsed)
		}
		clk.advance(10 * time.Minute)
	}
	if list := f.decisions(t); len(list) != 0 {
		t.Fatalf("decisions inside the bound: %+v, want none", list)
	}
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d past the bound, want 1", n)
	}
	d := f.one(t)
	if d.Outcome != chiefnudge.Suppressed(chiefnudge.GuardLanesFull) {
		t.Fatalf("outcome %q, want %q", d.Outcome, chiefnudge.Suppressed(chiefnudge.GuardLanesFull))
	}
	if !strings.Contains(d.Detail, "the chief's lanes were full for 30m0s") || !strings.Contains(d.Detail, "CCD_MAX_SESSIONS") {
		t.Fatalf("detail %q does not say why", d.Detail)
	}
	if d.JevLabel != chiefnudge.LabelNeither || j.count() != 1 {
		t.Fatalf("jev label %q after %d classifier calls, want %q after 1", d.JevLabel, j.count(), chiefnudge.LabelNeither)
	}
	turns := len(ccd.seen())
	clk.advance(ChiefNudgeInterval)
	if n := f.pass(t); n != 0 {
		t.Fatalf("decided %d on the pass after, want 0", n)
	}
	if got := len(ccd.seen()); got != turns {
		t.Fatalf("chief turns %d after the recorded streak's next pass, want %d (it is not asked about again)", got, turns)
	}
	if got := strings.Count(buf.String(), "the chief refused the check (lanes_full)"); got != 1 {
		t.Fatalf("the refusal was logged %d times, want once:\n%s", got, buf.String())
	}
	if body := f.page(t, "/attention?tab=nudges"); !strings.Contains(body, "the chief&#39;s lanes stayed full") {
		t.Fatalf("the Chief nudges tab does not show the lanes_full decision")
	}
}

// TestChiefNudgeNotReadyForeverIsRecorded: a chief whose ccd answers every
// check 503 not_ready (CCD_MAX_SESSIONS=0) is handled the same way, under its
// own guard and detail.
func TestChiefNudgeNotReadyForeverIsRecorded(t *testing.T) {
	f, ccd, _, clk, _ := newRefusedFixture(t)
	ccd.setNotReady(true)
	if n := f.pass(t); n != 0 {
		t.Fatalf("decided %d on the first refusal, want 0", n)
	}
	clk.advance(chiefNudgeRefusedBound)
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d past the bound, want 1", n)
	}
	d := f.one(t)
	if d.Outcome != chiefnudge.Suppressed(chiefnudge.GuardChiefNotReady) || !strings.Contains(d.Detail, "CCD_MAX_SESSIONS=0") {
		t.Fatalf("outcome %q detail %q, want %q naming CCD_MAX_SESSIONS=0", d.Outcome, d.Detail, chiefnudge.Suppressed(chiefnudge.GuardChiefNotReady))
	}
	turns := len(ccd.seen())
	clk.advance(ChiefNudgeInterval)
	f.pass(t)
	if got := len(ccd.seen()); got != turns {
		t.Fatalf("chief turns %d after, want %d", got, turns)
	}
}

// TestChiefNudgeRefusalClocksStartForTheCandidatesWaiting: with two stalled
// sessions and the lanes full, the second never reaches the chief (the pass
// stops at the first refusal) — yet its clock starts with the first's, so past
// the bound BOTH are recorded in one pass, not one bound after the other.
func TestChiefNudgeRefusalClocksStartForTheCandidatesWaiting(t *testing.T) {
	f, ccd, _, clk, _ := newRefusedFixture(t)
	f.mirror.mu.Lock()
	f.mirror.windows = append(f.mirror.windows, mirror.Window{Session: "main", ID: "@2", Index: 2, Label: "Gold:2", Claude: true,
		ClaudeSession: "5c1e0a3b-7d2f-4e88-b1a6-9f3c2d4e5a60", Status: mirror.StatusIdle, StatusSince: nudgeSince})
	f.mirror.mu.Unlock()
	ccd.setFull(true)
	if n := f.pass(t); n != 0 {
		t.Fatalf("decided %d, want 0", n)
	}
	if got := len(ccd.seen()); got != 1 {
		t.Fatalf("chief turns %d on the first pass, want 1 (the pass stops at the refusal)", got)
	}
	clk.advance(chiefNudgeRefusedBound)
	if n := f.pass(t); n != 2 {
		t.Fatalf("decided %d past the bound, want 2: the waiting candidate's clock started with the first's", n)
	}
	for _, d := range f.decisions(t) {
		if d.Outcome != chiefnudge.Suppressed(chiefnudge.GuardLanesFull) {
			t.Fatalf("outcome %q, want lanes_full", d.Outcome)
		}
	}
}
