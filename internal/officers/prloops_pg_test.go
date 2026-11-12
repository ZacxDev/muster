package officers_test

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
)

// A draft officer's review-loop announcements (prloops.go): one contract,
// asserted on the memory store here and on officer_pr_loops (migration 0118)
// with a database — where the record also survives a new store on its own
// pool (a restarted server, or the other replica).

const (
	prLoopOfficer = "investigate:example-infra"
	prLoopRepo    = "example-org/example-infra"
	prLoopHead    = "5e1f0c3a9b7d2e4f6a8c0b1d3e5f7a9c2b4d6e8f"
)

func prLoopContract(t *testing.T, ctx context.Context, st officers.PRLoopStore, reopen func() officers.PRLoopStore) {
	t.Helper()
	if s, err := st.PRLoopState(ctx, prLoopOfficer, prLoopRepo, 31); err != nil || s != "" {
		t.Fatalf("never announced: %q %v", s, err)
	}
	if err := st.PRLoopAnnounced(ctx, prLoopOfficer, "Example-Org/Example-Infra", 31, prLoopHead); err != nil {
		t.Fatal(err)
	}
	if err := st.PRLoopAnnounced(ctx, prLoopOfficer, prLoopRepo, 32, prLoopHead); err != nil {
		t.Fatal(err)
	}
	if s, _ := st.PRLoopState(ctx, prLoopOfficer, prLoopRepo, 31); s != officers.PRLoopPending {
		t.Fatalf("announced: %q, want pending (repo compared without case)", s)
	}
	if err := st.PRLoopSettle(ctx, prLoopOfficer, prLoopRepo, 31, officers.PRLoopInLoop); err != nil {
		t.Fatal(err)
	}
	// A second announcement keeps the state: a pull request HQ acknowledged
	// never goes back to pending.
	if err := st.PRLoopAnnounced(ctx, prLoopOfficer, prLoopRepo, 31, prLoopHead); err != nil {
		t.Fatal(err)
	}
	if s, _ := st.PRLoopState(ctx, prLoopOfficer, prLoopRepo, 31); s != officers.PRLoopInLoop {
		t.Fatalf("after a repeat announcement: %q, want in_loop kept", s)
	}
	pending, err := st.PRLoopsPending(ctx, prLoopOfficer)
	if err != nil || len(pending) != 1 || pending[0].PR != 32 || pending[0].HeadSHA != prLoopHead {
		t.Fatalf("pending %+v %v, want only #32 at its head", pending, err)
	}
	if other, _ := st.PRLoopsPending(ctx, "draft:example-web"); len(other) != 0 {
		t.Fatalf("another officer's pending: %+v", other)
	}
	if pending[0].AnnouncedAt.IsZero() {
		t.Fatal("a pending announcement carries no announced-at time")
	}
	if any, err := st.PRLoopsAnyInLoop(ctx, prLoopOfficer); err != nil || !any {
		t.Fatalf("any in loop: %v %v, want true", any, err)
	}
	if any, err := st.PRLoopsAnyInLoop(ctx, "draft:example-web"); err != nil || any {
		t.Fatalf("another officer's any in loop: %v %v, want false", any, err)
	}
	if err := st.PRLoopSettle(ctx, prLoopOfficer, prLoopRepo, 32, officers.PRLoopFailed); err != nil {
		t.Fatalf("settle failed: %v", err)
	}
	if reopen != nil {
		again := reopen()
		if s, _ := again.PRLoopState(ctx, prLoopOfficer, prLoopRepo, 31); s != officers.PRLoopInLoop {
			t.Fatalf("a new store reads %q, want in_loop (the record survives a restart)", s)
		}
	}
}

func TestTheMemoryLoopStoreKeepsTheContract(t *testing.T) {
	prLoopContract(t, context.Background(), officers.NewMemPRLoopStore(), nil)
}

func TestTheDatabaseLoopStoreKeepsTheContract(t *testing.T) {
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `TRUNCATE officer_pr_loops RESTART IDENTITY`)
	open := func() officers.PRLoopStore {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		pool, err := db.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return officers.NewPGPRLoopStore(pool)
	}
	prLoopContract(t, dbtest.AsOperator(context.Background()), open(), open)
}
