package sendqueue

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// listed is items as "state:body#id", in List's order.
func listed(items []Item) string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, string(it.State)+":"+it.Body+"#"+strconv.FormatInt(it.ID, 10))
	}
	return strings.Join(out, " ")
}

// TestListShowsOnlyWhatStillNeedsSomething: List draws what is queued or being
// sent, and a failure nobody has handled yet — never a sent or a cancelled
// message (they are done; the operator had to Delete each by hand), nor a
// failure already retried (its retry row carries it). Every row's body is its
// own, so a row in the wrong part, or a missing one, names itself.
func TestListShowsOnlyWhatStillNeedsSomething(t *testing.T) {
	s, _ := pgStore(t)
	ctx := context.Background()

	sent := mustEnqueue(t, s, 2, mirrorItem("alpha sent"))
	if _, ok, err := s.Claim(ctx, sent.ID, time.Now(), claimFor("t", time.Now())); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.Finish(ctx, sent.ID, "t", Outcome{Sent: true, Detail: "Sent."}, time.Now()); err != nil {
		t.Fatal(err)
	}
	cancelled := mustEnqueue(t, s, 2, mirrorItem("bravo cancelled"))
	if _, err := s.Cancel(as(2), cancelled.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	retried := mustEnqueue(t, s, 2, mirrorItem("charlie failed then retried"))
	failRow(t, s, retried.ID, false)
	again, err := s.Retry(as(2), retried.ID, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	failed := mustEnqueue(t, s, 2, mirrorItem("delta failed"))
	failed2 := mustEnqueue(t, s, 2, mirrorItem("echo failed later"))
	failRow(t, s, failed.ID, true)
	failRow(t, s, failed2.ID, false)
	sending := mustEnqueue(t, s, 2, mirrorItem("foxtrot sending"))
	if _, ok, err := s.Claim(ctx, sending.ID, time.Now(), claimFor("live", time.Now())); err != nil || !ok {
		t.Fatal(ok, err)
	}
	queued := mustEnqueue(t, s, 2, mirrorItem("golf queued"))
	// Another owner's failure at the same address, and the caller's own at
	// another window: neither is the caller's list for win.
	theirs := mustEnqueue(t, s, 3, mirrorItem("hotel user 3 failed"))
	failRow(t, s, theirs.ID, false)
	elsewhere := mirrorItem("india another window failed")
	elsewhere.Target.Window = "@8"
	failRow(t, s, mustEnqueue(t, s, 2, elsewhere).ID, false)

	items, err := s.List(as(2), win)
	if err != nil {
		t.Fatal(err)
	}
	// Open oldest first (the retry row was queued before the sending one),
	// then the unretried failures newest first.
	want := strings.Join([]string{
		"queued:charlie failed then retried#" + strconv.FormatInt(again.ID, 10),
		"sending:foxtrot sending#" + strconv.FormatInt(sending.ID, 10),
		"queued:golf queued#" + strconv.FormatInt(queued.ID, 10),
		"failed:echo failed later#" + strconv.FormatInt(failed2.ID, 10),
		"failed:delta failed#" + strconv.FormatInt(failed.ID, 10),
	}, " ")
	if got := listed(items); got != want {
		t.Fatalf("user 2's list:\n got %s\nwant %s", got, want)
	}
	// CONTROL: the hidden rows are still there (List hides; it deletes nothing).
	for _, id := range []int64{sent.ID, cancelled.ID, retried.ID} {
		if _, err := s.Get(as(2), id); err != nil {
			t.Fatalf("hidden row %d is gone: %v", id, err)
		}
	}
}

// TestListLeavesOutAFailureHandedBackToTheChief: a failed ROUTED row and a
// failed CHIEF NUDGE have no Retry here — their outcome goes back to the
// chief, which is their handling — so neither is listed (left in, each sat in
// the list until deleted by hand, the backlog this list exists to avoid). A
// person's own failure, beside them, is.
func TestListLeavesOutAFailureHandedBackToTheChief(t *testing.T) {
	s, _ := pgStore(t)
	routed := mustEnqueue(t, s, 2, Item{Target: win, Body: "lima routed", StepUpAt: time.Now(), RouteID: "rt_0123456789abcdef01234567"})
	failRow(t, s, routed.ID, false)
	nudge := mustEnqueue(t, s, 2, Item{Target: win, Body: "mike chief nudge", StepUpAt: time.Now(), ChiefNudgeID: 4242})
	failRow(t, s, nudge.ID, false)
	own := mustEnqueue(t, s, 2, mirrorItem("november own failure"))
	failRow(t, s, own.ID, false)
	items, err := s.List(as(2), win)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := listed(items), "failed:november own failure#"+strconv.FormatInt(own.ID, 10); got != want {
		t.Fatalf("user 2's list:\n got %s\nwant %s", got, want)
	}
	// CONTROL: the fixture really made them a routed and a nudge failure.
	for _, c := range []struct {
		id    int64
		check func(Item) bool
	}{{routed.ID, func(it Item) bool { return it.RouteID != "" }}, {nudge.ID, func(it Item) bool { return it.ChiefNudgeID == 4242 }}} {
		if it, err := s.Get(as(2), c.id); err != nil || it.State != Failed || !c.check(it) {
			t.Fatalf("fixture row %d: %+v %v", c.id, it, err)
		}
	}
}

// TestFailedShownCoversAWholeQueue pins the bound itself: a whole queue that
// failed (MaxOpenPerTarget messages, literally 20) is listed whole. The bound
// test below derives its fixture from FailedShown, so a smaller FailedShown
// would pass it.
func TestFailedShownCoversAWholeQueue(t *testing.T) {
	if MaxOpenPerTarget != 20 || FailedShown < MaxOpenPerTarget {
		t.Fatalf("FailedShown = %d, MaxOpenPerTarget = %d; want at least 20, so a whole failed queue is shown", FailedShown, MaxOpenPerTarget)
	}
}

// TestListBoundsTheFailures: a target that failed more than FailedShown times
// lists the FailedShown newest failures, newest first, and its open rows
// whatever the bound.
func TestListBoundsTheFailures(t *testing.T) {
	s, _ := pgStore(t)
	n := FailedShown + 3
	for i := range n {
		it := mustEnqueue(t, s, 2, mirrorItem(fmt.Sprintf("failure %02d", i)))
		failRow(t, s, it.ID, false)
	}
	open := mustEnqueue(t, s, 2, mirrorItem("still queued"))
	items, err := s.List(as(2), win)
	if err != nil {
		t.Fatal(err)
	}
	// 21: the literal, not FailedShown+1 — the expectation is not derived from
	// the constant it checks.
	if len(items) != 21 || items[0].ID != open.ID {
		t.Fatalf("list of %d failures and 1 queued: %d rows, first %s; want 21, the queued one first",
			n, len(items), listed(items[:1]))
	}
	for i, it := range items[1:] {
		if want := fmt.Sprintf("failure %02d", n-1-i); it.State != Failed || it.Body != want {
			t.Fatalf("failure row %d: %s:%s; want failed:%s", i, it.State, it.Body, want)
		}
	}
}
