package officers_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// HQ's task ordering marks an open item whose task waits on unfinished tasks
// `blocked: true` (with `blocked_by`), and refuses to claim it. Such an item is
// not work: waking an instance for it starts a pod that can claim nothing.

func TestABlockedOnlyQueueWakesNobody(t *testing.T) {
	r := newRig(t, 2, 30*time.Minute)
	a := r.hq.AddItem(relID, "engineering.task", "task:a")
	b := r.hq.AddItem(relID, "engineering.task", "task:b")
	r.hq.SetBlocked(a, "T1")
	r.hq.SetBlocked(b, "T1", "T2")
	res := r.check(t)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.Decision.Claimable != 0 || !res.Decision.Idle || len(res.Decision.Start) != 0 || r.fleet.upCount() != 0 {
		t.Fatalf("only blocked items open: claimable %d, idle %v, started %v; want 0, true, none", res.Decision.Claimable, res.Decision.Idle, res.Decision.Start)
	}
	// Unblocked, the same item is work again.
	r.hq.SetBlocked(a)
	if res := r.check(t); res.Decision.Claimable != 1 || r.fleet.upCount() != 1 {
		t.Fatalf("one item unblocked: claimable %d, up %d; want 1, 1", res.Decision.Claimable, r.fleet.upCount())
	}
}

func TestABlockedOnlyQueueDoesNotKeepAnInstanceUp(t *testing.T) {
	r := newRig(t, 1, 30*time.Minute)
	r.fleet.up[1], r.fleet.exists[1] = true, true
	id := r.hq.AddItem(relID, "engineering.task", "task:a")
	r.hq.SetBlocked(id, "T1")
	if res := r.check(t); !res.Decision.Idle || len(res.Decision.Reprompt) != 0 {
		t.Fatalf("an up instance, only a blocked item: idle %v, reprompt %v; want idle, no prompt", res.Decision.Idle, res.Decision.Reprompt)
	}
	r.clk.Advance(30 * time.Minute)
	if res := r.check(t); len(res.Decision.Stop) != 1 || res.Decision.Stop[0] != 1 {
		t.Fatalf("idle window over with only a blocked item open: stop %v, want [1]", res.Decision.Stop)
	}
}

func TestABlockedItemBesideAnUnblockedOneWakesOne(t *testing.T) {
	r := newRig(t, 3, 30*time.Minute)
	blocked := r.hq.AddItem(relID, "engineering.task", "task:a")
	r.hq.AddItem(relID, "engineering.task", "task:b")
	r.hq.SetBlocked(blocked, "T1")
	res := r.check(t)
	if res.Decision.Claimable != 1 || r.fleet.upCount() != 1 {
		t.Fatalf("one blocked + one claimable, max 3: claimable %d, up %d; want 1, 1", res.Decision.Claimable, r.fleet.upCount())
	}
}

func TestAnExpiredLeaseOnABlockedItemIsNotWork(t *testing.T) {
	r := newRig(t, 1, 30*time.Minute)
	id := r.hq.AddItem(relID, "engineering.task", "task:a")
	r.hq.SetClaim(id, "gone-session", r.clk.Now().Add(-time.Minute))
	r.hq.SetBlocked(id, "T1")
	if res := r.check(t); res.Decision.Claimable != 0 || r.fleet.upCount() != 0 {
		t.Fatalf("expired lease on a blocked item: claimable %d, up %d; want 0, 0", res.Decision.Claimable, r.fleet.upCount())
	}
}

// An HQ without task ordering (an older release) sends no `blocked` field at
// all: every open item is work, as before.
func TestNoBlockedFieldMeansEveryItemIsWork(t *testing.T) {
	r := newRig(t, 3, 30*time.Minute)
	r.hq.AddItem(relID, "engineering.task", "task:a")
	r.hq.AddItem(relID, "engineering.task", "task:b")
	page, err := officers.NewClient(r.o, nil).Items(context.Background(), officers.ItemsQuery{Status: officers.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(page.Raw, []byte("blocked")) {
		t.Fatalf("the fixture must serve NO blocked field: %s", page.Raw)
	}
	if res := r.check(t); res.Decision.Claimable != 2 || r.fleet.upCount() != 2 {
		t.Fatalf("no blocked field: claimable %d, up %d; want 2, 2", res.Decision.Claimable, r.fleet.upCount())
	}
}
