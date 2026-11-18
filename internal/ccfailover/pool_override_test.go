package ccfailover

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/ccpool"
)

// THE POOL SUBMISSION'S FAILOVER (AccountFailedForPool): a pool worker's pin
// does not stop it; nothing else changes.

// markNorth marks north rate-limited, as the gateway does before a failover.
func (f *fixture) markNorth(t *testing.T) {
	t.Helper()
	if err := f.pool.MarkFailure(context.Background(), 1, "north", ccpool.FailureRateLimited, "You've hit your limit"); err != nil {
		t.Fatal(err)
	}
}

// TestAPoolSubmissionOverridesAPoolWorkersPinOnly: a worker of pool
// "reviewers" pinned to north (no opt-in) is moved by AccountFailedForPool and
// still NOT by AccountFailed (its own failed turn). An agent in no pool, pinned
// the same way, is moved by neither, and says why. With failover off, not even
// the pool worker moves.
func TestAPoolSubmissionOverridesAPoolWorkersPinOnly(t *testing.T) {
	ctx := context.Background()
	const why = `the agent is pinned to account "north" and has not opted in to failover`
	t.Run("pinned pool worker", func(t *testing.T) {
		f := newFixture(t, true, "north", "south")
		f.ags.row.CCAccountPinned, f.ags.row.WorkerPool = true, "reviewers"
		f.markNorth(t)
		if res := f.svc.AccountFailed(ctx, agentID, "north", ccpool.FailureRateLimited, "x"); res.Moved || res.Why != why {
			t.Fatalf("AccountFailed on a pinned pool worker = %+v, want not moved (%q)", res, why)
		}
		res := f.svc.AccountFailedForPool(ctx, agentID, "north", ccpool.FailureRateLimited, "x")
		if !res.Rebound || res.To != "south" {
			t.Fatalf("AccountFailedForPool on a pinned pool worker = %+v, want moved to south", res)
		}
	})
	t.Run("pinned agent in no pool", func(t *testing.T) {
		f := newFixture(t, true, "north", "south")
		f.ags.row.CCAccountPinned = true
		f.markNorth(t)
		res := f.svc.AccountFailedForPool(ctx, agentID, "north", ccpool.FailureRateLimited, "x")
		if res.Moved || res.Why != why || f.rb.count() != 0 {
			t.Fatalf("AccountFailedForPool on a pinned non-pool agent = %+v (%d rebinds), want not moved (%q)", res, f.rb.count(), why)
		}
	})
	t.Run("failover off", func(t *testing.T) {
		f := newFixture(t, false, "north", "south")
		f.ags.row.CCAccountPinned, f.ags.row.WorkerPool = true, "reviewers"
		f.markNorth(t)
		res := f.svc.AccountFailedForPool(ctx, agentID, "north", ccpool.FailureRateLimited, "x")
		if res.Moved || !strings.Contains(res.Why, "MUSTER_AGENT_CC_FAILOVER=off") || f.rb.count() != 0 {
			t.Fatalf("AccountFailedForPool with failover off = %+v, want not moved", res)
		}
	})
}

// TestRebindingIsTheAgentsLatestMoveForAWhile: a job about to run on a
// just-moved agent asks Rebinding, which answers the move (and the instance it
// replaced) for memoTTL, then nothing.
func TestRebindingIsTheAgentsLatestMoveForAWhile(t *testing.T) {
	f := newFixture(t, true, "north", "south")
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	f.svc.cfg.Now = func() time.Time { return now }
	if _, ok := f.svc.Rebinding(agentID); ok {
		t.Fatal("Rebinding answered before any move")
	}
	f.fail(t, "north", ccpool.FailureRateLimited, "You've hit your limit")
	got, ok := f.svc.Rebinding(agentID)
	if !ok || got.To != "south" || got.AgentName != "keen-wren" || got.PrevInstance != "pod-old" {
		t.Fatalf("Rebinding = %+v, %v; want the move to south replacing pod-old", got, ok)
	}
	now = now.Add(memoTTL)
	if _, ok := f.svc.Rebinding(agentID); ok {
		t.Fatal("Rebinding still answered memoTTL after the move")
	}
}
