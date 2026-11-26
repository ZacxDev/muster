package api

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/fleet"
	"github.com/ZacxDev/muster/internal/push"
)

// A WITHDRAWAL NEVER OVERTAKES WHAT IT WITHDRAWS (push_order.go). goNotify
// sends each notification on its own goroutine; a `resolved` that lands
// before the alert it follows closes nothing, and the alert then stays.
// These pin the two guards on the fleet push: in this process the sends of
// one tag go one at a time, in order; and a withdrawal is re-checked after one
// fan-out and sent again while nothing should show (the cross-replica case).
// Names only identifiers that exist before the change (the scheduler seam is
// the episodes' clearRetry.after), so each is red there for a behavioural
// reason.

// gatedPush holds the first fleet alert at the device until gate closes.
type gatedPush struct {
	*morningPush
	gate chan struct{}
	held atomic.Bool
}

func (g *gatedPush) SendTo(ctx context.Context, owner int64, p push.Payload) push.Report {
	if p.Tag == "fleet-proposals" && p.Type != push.TypeResolved && g.held.CompareAndSwap(false, true) {
		<-g.gate
	}
	return g.morningPush.SendTo(ctx, owner, p)
}

// capturedAfter is the clearRetry.after seam: each scheduled re-check, held
// until the test runs it.
type capturedAfter struct {
	mu sync.Mutex
	fs []func()
}

func (c *capturedAfter) after(_ time.Duration, f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fs = append(c.fs, f)
}

// runAll runs every re-check scheduled so far, once.
func (c *capturedAfter) runAll() int {
	c.mu.Lock()
	fs := c.fs
	c.fs = nil
	c.mu.Unlock()
	for _, f := range fs {
		f()
	}
	return len(fs)
}

// TestAWithdrawalWaitsForTheAlertItFollows: the alert is held at the device;
// the owner approves the proposal meanwhile, which withdraws the push. The
// withdrawal is delivered AFTER the alert, never before it — else the alert
// would land last and stay up with nothing left to withdraw it.
//
// Red at the branch's first commit: the withdrawal went out on its own
// goroutine while the alert was held, and the alert landed last.
func TestAWithdrawalWaitsForTheAlertItFollows(t *testing.T) {
	f := newProposeFixture(t)
	f.srv.clearRetry.after = (&capturedAfter{}).after // no real timers
	mp := &morningPush{}
	g := &gatedPush{morningPush: mp, gate: make(chan struct{})}
	f.srv.ext.Push = g
	s, _, _ := f.stopped(t, f.owner)
	id := f.propose(t, s, fleet.ClassContinue)
	f.pass()
	f.card(t, fresh(f.asOwner), id, "approve", nil)
	// Give a withdrawal that does not wait its turn every chance to land first.
	for deadline := time.Now().Add(500 * time.Millisecond); len(mp.to(f.owner)) == 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	close(g.gate)
	f.srv.WaitForPushes()
	var order []string
	for _, p := range mp.to(f.owner) {
		if p.Tag == "fleet-proposals" {
			order = append(order, p.Type)
		}
	}
	if len(order) != 2 || order[0] == push.TypeResolved || order[1] != push.TypeResolved {
		t.Fatalf("the fleet push's sends reached the device as %v; want the alert, then its withdrawal", order)
	}
}

// TestAWithdrawalIsSentAgainAfterOneFanOut: once the last proposal is decided
// the push is withdrawn, and a re-check is scheduled; when it runs (one
// fan-out later) and still nothing should show, the withdrawal is sent again —
// so an alert another replica had in flight cannot outlive it.
//
// Red at the branch's first commit: nothing was scheduled; one withdrawal.
func TestAWithdrawalIsSentAgainAfterOneFanOut(t *testing.T) {
	f, cp := newRetractFixture(t)
	later := &capturedAfter{}
	f.srv.clearRetry.after = later.after
	s, _, _ := f.stopped(t, f.owner)
	id := f.propose(t, s, fleet.ClassContinue)
	f.pass()
	f.card(t, fresh(f.asOwner), id, "approve", nil)
	wantWithdrawn(t, f.fleetPushes(cp), 2)
	if n := later.runAll(); n == 0 {
		t.Fatal("no re-check was scheduled after the withdrawal")
	}
	wantWithdrawn(t, f.fleetPushes(cp), 3)
}
