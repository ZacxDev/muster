package sendqueue

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE DISPATCHER AGAINST A SESSION UNDER A USAGE LIMIT, on the real store with
// a fake clock: the drain the operator reported ("when a session hits its
// limit, all the queued messages send immediately") is held, the message the
// limit rejected goes back to the head, and the queue resumes after the reset —
// each with its own control.

// cli is a fake Claude Code behind the send path: each message pasted into it
// ends its turn at once (its status goes idle with a NEWER time, as the real
// CLI's does), with the turn error reply names — nil for a real answer.
type cli struct {
	*fakeTarget
	c     *clock
	reply func(body string) *mirror.TurnError
}

func newCLI(c *clock, start time.Time, reply func(string) *mirror.TurnError) *cli {
	return &cli{fakeTarget: &fakeTarget{st: idleSince(start)}, c: c, reply: reply}
}

func (l *cli) send(ctx context.Context, it Item, st AgentState) Outcome {
	o := l.fakeTarget.send(ctx, it, st)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.st = AgentState{Idle: true, Since: l.st.Since + 700, Observed: l.c.now(), TurnError: l.reply(it.Body)}
	return o
}

// answer is a turn the person started by hand that the CLI answered for real
// (or with e): idle, newer, observed now.
func (l *cli) answer(e *mirror.TurnError) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.st = AgentState{Idle: true, Since: l.st.Since + 5000, Observed: l.c.now(), TurnError: e}
}

func limitDispatcher(s *PGStore, l *cli, c *clock) *Dispatcher {
	d := newDispatcher(s, l.fakeTarget, c)
	d.Send = l.send
	return d
}

// limited is the CLI's usage-limit rejection of body: the limit was its only
// reply, so the host names the prompt.
func limited(reset time.Time) func(string) *mirror.TurnError {
	return func(body string) *mirror.TurnError { return limitErr(reset, mirror.PromptDigest(body)) }
}

func row(t *testing.T, s *PGStore, id int64) Item {
	t.Helper()
	it, err := s.Get(as(2), id)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

// TestAUsageLimitHoldsTheQueueReturnsTheRejectedMessageAndResumesAfterTheReset
// is the operator's report end to end. Three messages queued; the session hits
// its limit. Before the fix every pass sent the next one (the CONTROL further
// down: the same CLI with no turn error drains). Now: the first is sent, the
// limit rejects it, it goes back to the head marked returned — not counted as
// sent — and nothing else is sent until the reset plus the margin; then the
// first is sent again, answered for real, and the rest follow one per turn.
func TestAUsageLimitHoldsTheQueueReturnsTheRejectedMessageAndResumesAfterTheReset(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	reset := t0.Add(2 * time.Hour)
	l := newCLI(c, t0.Add(-time.Hour), limited(reset))
	d := limitDispatcher(s, l, c)
	one := mustEnqueue(t, s, 2, mirrorItem("one"))
	two := mustEnqueue(t, s, 2, mirrorItem("two"))
	three := mustEnqueue(t, s, 2, mirrorItem("three"))

	for i := 1; i <= 20; i++ { // ten minutes of passes, each past IdleFor
		c.set(t0.Add(time.Duration(i) * idle))
		tick(t, d)
	}
	wantSent(t, l.fakeTarget, "under the limit", "one")
	got := row(t, s, one.ID)
	if got.State != Queued || got.Returns != 1 || got.Detail != ReturnedLimitDetail || got.MayHaveDelivered || !got.FinishedAt.IsZero() {
		t.Fatalf("the rejected message: %+v; want queued again, returned once, not delivered, not finished", got)
	}
	for _, id := range []int64{two.ID, three.ID} {
		if r := row(t, s, id); r.State != Queued || r.Returns != 0 {
			t.Fatalf("message %d: %s, returns %d; want queued, never sent", id, r.State, r.Returns)
		}
	}
	h, ok, err := s.HeadOf(as(2), win)
	if err != nil || !ok || h.ID != one.ID {
		t.Fatalf("head = %+v, %v, %v; want the returned message back at the head", h, ok, err)
	}
	if dec := d.Decide(h, l.probe(context.Background(), h)); dec.Send || !dec.Held || !dec.ResumeAt.Equal(reset) {
		t.Fatalf("Decide = %+v; want held until the reset %s", dec, reset)
	}

	// Just inside the margin: still held.
	c.set(reset.Add(LimitMargin - time.Second))
	tick(t, d)
	wantSent(t, l.fakeTarget, "inside the margin", "one")

	// Past it: the returned message is sent again — and this time answered.
	l.reply = func(string) *mirror.TurnError { return nil }
	c.set(reset.Add(LimitMargin))
	tick(t, d)
	wantSent(t, l.fakeTarget, "after the reset", "one", "one")
	for i := 1; i <= 6; i++ {
		c.set(reset.Add(LimitMargin + time.Duration(i)*idle))
		tick(t, d)
	}
	wantSent(t, l.fakeTarget, "the limit lifted", "one", "one", "two", "three")
	if r := row(t, s, one.ID); r.State != Sent || r.Returns != 1 {
		t.Fatalf("the returned message, answered: %s returns %d; want sent, once returned", r.State, r.Returns)
	}
}

// TestWithoutATurnErrorTheSameCLIDrainsTheQueue is the control the test above
// needs: the very same CLI (each message ends its turn at once, idle, newer)
// with the host reporting nothing about how the turn ended — what every host
// sent before this — and the queue goes out one message per pass. This is the
// drain the operator saw; it is what a host without the turn error still does.
func TestWithoutATurnErrorTheSameCLIDrainsTheQueue(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	l := newCLI(c, t0.Add(-time.Hour), func(string) *mirror.TurnError { return nil })
	d := limitDispatcher(s, l, c)
	for _, b := range []string{"one", "two", "three"} {
		mustEnqueue(t, s, 2, mirrorItem(b))
	}
	for i := 1; i <= 6; i++ {
		c.set(t0.Add(time.Duration(i) * idle))
		tick(t, d)
	}
	wantSent(t, l.fakeTarget, "no turn error reported", "one", "two", "three")
}

// TestALimitHitMidTurnHoldsButReturnsNothing: the host names no prompt when the
// limit came after real work (the message WAS processed). It stays sent — a
// re-send would run it twice — and the queue still holds.
func TestALimitHitMidTurnHoldsButReturnsNothing(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	l := newCLI(c, t0.Add(-time.Hour), func(string) *mirror.TurnError { return limitErr(t0.Add(time.Hour), "") })
	d := limitDispatcher(s, l, c)
	one := mustEnqueue(t, s, 2, mirrorItem("one"))
	mustEnqueue(t, s, 2, mirrorItem("two"))
	for i := 1; i <= 6; i++ {
		c.set(t0.Add(time.Duration(i) * idle))
		tick(t, d)
	}
	wantSent(t, l.fakeTarget, "a limit mid-turn", "one")
	if r := row(t, s, one.ID); r.State != Sent || r.Returns != 0 {
		t.Fatalf("the processed message: %s returns %d; want sent, not returned", r.State, r.Returns)
	}
}

// TestALimitThatAnsweredAnotherPromptReturnsNothing: a person's own Send got in
// after the queued message and the limit answered THAT: the latest turn's
// prompt is not the queued message's text, so the queue cannot tell what
// became of its message, and leaves it sent.
func TestALimitThatAnsweredAnotherPromptReturnsNothing(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	l := newCLI(c, t0.Add(-time.Hour), func(string) *mirror.TurnError {
		return limitErr(t0.Add(time.Hour), mirror.PromptDigest("typed by hand"))
	})
	d := limitDispatcher(s, l, c)
	one := mustEnqueue(t, s, 2, mirrorItem("one"))
	for i := 1; i <= 4; i++ {
		c.set(t0.Add(time.Duration(i) * idle))
		tick(t, d)
	}
	if r := row(t, s, one.ID); r.State != Sent || r.Returns != 0 {
		t.Fatalf("message: %s returns %d; want sent, not returned", r.State, r.Returns)
	}
}

// TestARejectedMessageIsReturnedOncePerSend: two dispatchers (two pods during a
// rolling update) both see the rejection; the row comes back ONCE — Return
// moves only a row still sent — and is not sent twice.
func TestARejectedMessageIsReturnedOncePerSend(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	l := newCLI(c, t0.Add(-time.Hour), limited(t0.Add(time.Hour)))
	d := limitDispatcher(s, l, c)
	one := mustEnqueue(t, s, 2, mirrorItem("one"))
	c.set(t0.Add(idle))
	tick(t, d) // sent, rejected
	other := limitDispatcher(s, l, c)
	other.Instance = "the-other-pod"
	var wg sync.WaitGroup
	for _, x := range []*Dispatcher{d, other, d, other} {
		wg.Add(1)
		go func(x *Dispatcher) { defer wg.Done(); x.returnRejected(context.Background()) }(x)
	}
	wg.Wait()
	if r := row(t, s, one.ID); r.State != Queued || r.Returns != 1 {
		t.Fatalf("after four concurrent passes: %s returns %d; want queued, returned exactly once", r.State, r.Returns)
	}
	if ok, err := s.Return(context.Background(), one.ID, ReturnedLimitDetail); err != nil || ok {
		t.Fatalf("Return of a queued row = %v, %v; want refused", ok, err)
	}
	wantSent(t, l.fakeTarget, "returned", "one")
}

// TestARealAnswerLiftsTheLimitAtOnce: the reset is hours away, but a message
// the person typed by hand was answered for real — the limit has lifted — so
// the returned message goes at the next idle stretch, not at the reset.
func TestARealAnswerLiftsTheLimitAtOnce(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	l := newCLI(c, t0.Add(-time.Hour), limited(t0.Add(5*time.Hour)))
	d := limitDispatcher(s, l, c)
	mustEnqueue(t, s, 2, mirrorItem("one"))
	for i := 1; i <= 4; i++ {
		c.set(t0.Add(time.Duration(i) * idle))
		tick(t, d)
	}
	wantSent(t, l.fakeTarget, "held", "one")
	l.reply = func(string) *mirror.TurnError { return nil }
	l.answer(nil)
	c.set(c.now().Add(idle))
	tick(t, d)
	wantSent(t, l.fakeTarget, "after a real answer", "one", "one")
}

// TestTheCircuitBreakerStopsAnImmediateFailureLoop: "Not logged in" fails each
// message at once (the host counts the quick failures in a row). BreakerAfter
// messages go — the first failure alone is not a loop — and then the queue
// holds, saying why; those messages are not returned (they were not a limit).
// Send now still sends exactly ONE.
func TestTheCircuitBreakerStopsAnImmediateFailureLoop(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	quick := 0
	l := newCLI(c, t0.Add(-time.Hour), func(string) *mirror.TurnError {
		quick++
		return &mirror.TurnError{Kind: "authentication_failed", Quick: quick}
	})
	d := limitDispatcher(s, l, c)
	var items []Item
	for _, b := range []string{"m1", "m2", "m3", "m4", "m5"} {
		items = append(items, mustEnqueue(t, s, 2, mirrorItem(b)))
	}
	for i := 1; i <= 12; i++ {
		c.set(t0.Add(time.Duration(i) * idle))
		tick(t, d)
	}
	wantSent(t, l.fakeTarget, "the breaker", "m1", "m2")
	h, ok, err := s.HeadOf(as(2), win)
	if err != nil || !ok || h.Body != "m3" {
		t.Fatalf("head = %+v; want m3", h)
	}
	dec := d.Decide(h, l.probe(context.Background(), h))
	if dec.Send || !dec.Held || dec.Why != BreakerHeldText(&mirror.TurnError{Kind: "authentication_failed", Quick: BreakerAfter}) {
		t.Fatalf("Decide = %+v; want held by the breaker", dec)
	}
	for _, it := range items[:2] {
		if r := row(t, s, it.ID); r.State != Sent || r.Returns != 0 {
			t.Fatalf("%s: %s returns %d; want sent (an error that is not a limit is not returned)", it.Body, r.State, r.Returns)
		}
	}
	// Send now: the one message tapped, and only it.
	if _, o, err := d.SendNow(as(2), d.AsOwner(context.Background(), 2), items[4].ID, l.probe(context.Background(), h).Since, l.send); err != nil || !o.Sent {
		t.Fatalf("Send now = %+v, %v", o, err)
	}
	for i := 13; i <= 16; i++ {
		c.set(t0.Add(time.Duration(i) * idle))
		tick(t, d)
	}
	wantSent(t, l.fakeTarget, "a Send now while held", "m1", "m2", "m5")
}

// TestSendNowDuringALimitSendsOneAndARejectionComesBack: the override sends the
// one message tapped, at once, held or not; the limit rejects it; it goes
// back to the queue like any other rejected send, and nothing else is sent.
func TestSendNowDuringALimitSendsOneAndARejectionComesBack(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	l := newCLI(c, t0.Add(-time.Hour), limited(t0.Add(time.Hour)))
	l.st.TurnError = limitErr(t0.Add(time.Hour), mirror.PromptDigest("earlier, by hand"))
	d := limitDispatcher(s, l, c)
	a := mustEnqueue(t, s, 2, mirrorItem("a"))
	b := mustEnqueue(t, s, 2, mirrorItem("b"))
	c.set(t0.Add(idle))
	tick(t, d)
	wantSent(t, l.fakeTarget, "held from the start", nil...)
	if _, o, err := d.SendNow(as(2), d.AsOwner(context.Background(), 2), b.ID, l.probe(context.Background(), Head{}).Since, l.send); err != nil || !o.Sent {
		t.Fatalf("Send now = %+v, %v", o, err)
	}
	for i := 2; i <= 6; i++ {
		c.set(t0.Add(time.Duration(i) * idle))
		tick(t, d)
	}
	wantSent(t, l.fakeTarget, "one Send now under the limit", "b")
	if r := row(t, s, b.ID); r.State != Queued || r.Returns != 1 {
		t.Fatalf("the sent-now message: %s returns %d; want returned to the queue", r.State, r.Returns)
	}
	if r := row(t, s, a.ID); r.State != Queued {
		t.Fatalf("a: %s; want still queued", r.State)
	}
}

// TestRecentSentIsEachTargetsLatestClaimOnly: a target whose latest claimed
// message FAILED has nothing to return, though an older one was sent; a send
// older than the window is not read.
func TestRecentSentIsEachTargetsLatestClaimOnly(t *testing.T) {
	s, _ := pgStore(t)
	now := time.Now()
	x := mustEnqueue(t, s, 2, mirrorItem("x"))
	y := mustEnqueue(t, s, 2, mirrorItem("y"))
	for i, id := range []int64{x.ID, y.ID} {
		if _, ok, err := s.Claim(context.Background(), id, now.Add(time.Duration(i)*time.Second), claimFor("p", now)); err != nil || !ok {
			t.Fatal(ok, err)
		}
		o := Outcome{Sent: true}
		if id == y.ID {
			o = Outcome{Detail: "Not sent: x"}
		}
		if err := s.Finish(context.Background(), id, "p", o, now); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.RecentSent(context.Background(), now.Add(-time.Minute))
	if err != nil || len(got) != 0 {
		t.Fatalf("RecentSent = %v, %v; want none (the latest claim failed)", bodies(got), err)
	}
	z := mustEnqueue(t, s, 2, mirrorItem("z"))
	if _, ok, err := s.Claim(context.Background(), z.ID, now.Add(5*time.Second), claimFor("p", now)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.Finish(context.Background(), z.ID, "p", Outcome{Sent: true}, now); err != nil {
		t.Fatal(err)
	}
	if got, err := s.RecentSent(context.Background(), now.Add(-time.Minute)); err != nil || len(got) != 1 || got[0].ID != z.ID {
		t.Fatalf("RecentSent = %v, %v; want z", bodies(got), err)
	}
	if got, err := s.RecentSent(context.Background(), now.Add(time.Minute)); err != nil || len(got) != 0 {
		t.Fatalf("RecentSent after the window = %v, %v; want none", bodies(got), err)
	}
}
