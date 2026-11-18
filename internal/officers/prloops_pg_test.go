package officers_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
)

// A draft officer's review-loop announcements (prloops.go): one contract,
// asserted on the memory store here and on officer_pr_loops (migration 0119)
// with a database — where the record also survives a new store on its own
// pool (a restarted server, or the other replica).

const (
	prLoopOfficer = "investigate:example-infra"
	prLoopRepo    = "example-org/example-infra"
	prLoopHead    = "5e1f0c3a9b7d2e4f6a8c0b1d3e5f7a9c2b4d6e8f"
)

func prLoopContract(t *testing.T, ctx context.Context, st officers.PRLoopStore, reopen func() officers.PRLoopStore) {
	t.Helper()
	state := func(st officers.PRLoopStore, pr int) string {
		t.Helper()
		s, err := st.PRLoopState(ctx, prLoopOfficer, prLoopRepo, pr)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	settle := func(pr int, to string) {
		t.Helper()
		if err := st.PRLoopSettle(ctx, prLoopOfficer, prLoopRepo, pr, to); err != nil {
			t.Fatal(err)
		}
	}
	if s := state(st, 31); s != "" {
		t.Fatalf("never announced: %q", s)
	}
	if err := st.PRLoopAnnounced(ctx, prLoopOfficer, "Example-Org/Example-Infra", 31, prLoopHead); err != nil {
		t.Fatal(err)
	}
	if err := st.PRLoopAnnounced(ctx, prLoopOfficer, prLoopRepo, 32, prLoopHead); err != nil {
		t.Fatal(err)
	}
	if s := state(st, 31); s != "sending" {
		t.Fatalf("announced: %q, want sending — the announcing process's claim (repo compared without case)", s)
	}
	settle(31, officers.PRLoopInLoop)
	// A second announcement keeps the state: a pull request HQ acknowledged
	// never goes back to being sent.
	if err := st.PRLoopAnnounced(ctx, prLoopOfficer, prLoopRepo, 31, prLoopHead); err != nil {
		t.Fatal(err)
	}
	if s := state(st, 31); s != "in_loop" {
		t.Fatalf("after a repeat announcement: %q, want in_loop kept", s)
	}
	// A 'sending' row (#32: its answer never recorded) is never claimed.
	notBefore := time.Now().Add(-time.Hour)
	if c, e, err := st.PRLoopsClaim(ctx, prLoopOfficer, notBefore); err != nil || len(c)+len(e) != 0 {
		t.Fatalf("claim with only sending and in_loop rows: claimed %+v expired %+v %v, want none", c, e, err)
	}
	// Pending (an outage recorded): claimed ONCE — a second claimer (another
	// replica, a later sweep) gets nothing — and moved to sending.
	settle(32, officers.PRLoopPending)
	settle(34, officers.PRLoopPending)
	c, e, err := st.PRLoopsClaim(ctx, prLoopOfficer, notBefore)
	if err != nil || len(c) != 2 || len(e) != 0 {
		t.Fatalf("claim: claimed %+v expired %+v %v, want #32 and #34", c, e, err)
	}
	for _, r := range c {
		if (r.PR != 32 && r.PR != 34) || r.State != "sending" || r.AnnouncedAt.IsZero() || (r.PR == 32 && r.HeadSHA != prLoopHead) {
			t.Fatalf("claimed %+v: want #32 (at its head) or #34, sending, with an announced-at time", r)
		}
	}
	if s := state(st, 32); s != "sending" {
		t.Fatalf("#32 after its claim: %q, want sending", s)
	}
	if c, e, _ := st.PRLoopsClaim(ctx, prLoopOfficer, notBefore); len(c)+len(e) != 0 {
		t.Fatalf("a second claim: claimed %+v expired %+v, want nothing", c, e)
	}
	// Past the age bound: failed, returned as expired, never claimed.
	settle(34, officers.PRLoopPending)
	c, e, err = st.PRLoopsClaim(ctx, prLoopOfficer, time.Now().Add(time.Hour))
	if err != nil || len(c) != 0 || len(e) != 1 || e[0].PR != 34 || e[0].State != "failed" || state(st, 34) != "failed" {
		t.Fatalf("an old pending row: claimed %+v expired %+v %v (state %q), want #34 expired and failed", c, e, err, state(st, 34))
	}
	// Another officer's rows are not its to claim.
	settle(35, officers.PRLoopPending)
	if c, e, _ := st.PRLoopsClaim(ctx, "draft:example-web", notBefore); len(c)+len(e) != 0 {
		t.Fatalf("another officer's claim: claimed %+v expired %+v, want nothing", c, e)
	}
	// Two claimers at once share the pending rows, never one row twice.
	for pr := 40; pr < 60; pr++ {
		settle(pr, officers.PRLoopPending)
	}
	other := st
	if reopen != nil {
		other = reopen()
	}
	got := make([][]officers.PRLoop, 2)
	var wg sync.WaitGroup
	for i, s := range []officers.PRLoopStore{st, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _, err := s.PRLoopsClaim(ctx, prLoopOfficer, notBefore)
			if err != nil {
				t.Error(err)
			}
			got[i] = c
		}()
	}
	wg.Wait()
	seen := map[int]int{}
	for _, c := range got {
		for _, r := range c {
			seen[r.PR]++
		}
	}
	if len(seen) != 21 { // #35 and #40..#59
		t.Fatalf("two concurrent claims took %d distinct rows (%d + %d), want all 21", len(seen), len(got[0]), len(got[1]))
	}
	for pr, n := range seen {
		if n != 1 {
			t.Fatalf("#%d was claimed %d times by two concurrent claimers, want once", pr, n)
		}
	}
	// In a loop: #31 only; another officer's: none.
	in, err := st.PRLoopsInLoop(ctx, prLoopOfficer)
	if err != nil || len(in) != 1 || in[0].PR != 31 || in[0].State != "in_loop" {
		t.Fatalf("in loop %+v %v, want only #31", in, err)
	}
	if in, _ := st.PRLoopsInLoop(ctx, "draft:example-web"); len(in) != 0 {
		t.Fatalf("another officer's in loop: %+v", in)
	}
	settle(31, officers.PRLoopClosed)
	if in, _ := st.PRLoopsInLoop(ctx, prLoopOfficer); len(in) != 0 {
		t.Fatalf("in loop after #31 closed: %+v, want none", in)
	}
	if reopen != nil {
		again := reopen()
		if s := state(again, 31); s != "closed" {
			t.Fatalf("a new store reads #31 %q, want closed (the record survives a restart)", s)
		}
		if s := state(again, 32); s != "sending" {
			t.Fatalf("a new store reads #32 %q, want sending (a claim survives a restart)", s)
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
