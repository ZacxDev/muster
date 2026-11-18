package officers_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE JUDGEMENT SESSION CAP IS PERSISTED (operator, #436 round 0 Q1): one row
// per (officer, UTC day) in officer_judgement_sessions holding the admitted
// item ids, shared by every muster-server replica, admitted by ONE atomic
// statement. A restart or a rollout overlap can neither reset nor split it.
// ---------------------------------------------------------------------------

// sessionStore is a PG session store on its own pool over the package's test
// database, the table emptied first. Each call is a fresh "server".
func sessionStore(t *testing.T, dsn string) *officers.PGSessionStore {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return officers.NewPGSessionStore(pool)
}

func freshSessions(t *testing.T) string {
	t.Helper()
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `TRUNCATE officer_judgement_sessions RESTART IDENTITY`)
	return dsn
}

const capOfficer = "gpu-health:fleet"

// t0 is the fixtures' clock: a moment in 2000 (any day string a test uses is
// a key only; staleness of a pending entry is judged against t0).
var t0 = time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)

// TestTheSessionCountSurvivesANewServer: two sessions admitted through one
// store are still counted by a NEW store on its own pool (a restarted server,
// or the other replica): at a cap of 2 a third item is refused, a counted
// item is re-admitted free.
func TestTheSessionCountSurvivesANewServer(t *testing.T) {
	dsn := freshSessions(t)
	ctx := dbtest.AsOperator(context.Background())
	day := "2000-01-01"
	first := sessionStore(t, dsn)
	for _, item := range []string{"a", "b"} {
		if a, err := first.AdmitSession(ctx, capOfficer, day, item, 2, t0); err != nil || !a.Admitted {
			t.Fatalf("admit %s: %+v %v", item, a, err)
		}
	}
	second := sessionStore(t, dsn)
	if a, err := second.AdmitSession(ctx, capOfficer, day, "c", 2, t0); err != nil || a.Admitted {
		t.Fatalf("a new server admitted the third session of a cap of 2: %+v %v", a, err)
	}
	if a, err := second.AdmitSession(ctx, capOfficer, day, "a", 2, t0); err != nil || !a.Admitted {
		t.Fatalf("a new server refused re-claiming counted item a: %+v %v", a, err)
	}
	if got, err := second.Sessions(ctx, capOfficer, day); err != nil || len(got) != 2 {
		t.Fatalf("sessions = %v (%v), want [a b]", got, err)
	}
}

// TestConcurrentAdmitsAtCapMinusOneAdmitExactlyOne: at cap-1, many admits of
// distinct items at once — each through its own store and pool, as separate
// replicas — admit exactly one. Positive control: under the cap, all admit.
func TestConcurrentAdmitsAtCapMinusOneAdmitExactlyOne(t *testing.T) {
	dsn := freshSessions(t)
	ctx := dbtest.AsOperator(context.Background())
	const n = 8
	race := func(day string, limit int, prefill []string) int {
		t.Helper()
		s := sessionStore(t, dsn)
		for _, item := range prefill {
			if a, err := s.AdmitSession(ctx, capOfficer, day, item, limit, t0); err != nil || !a.Admitted {
				t.Fatalf("prefill %s: %+v %v", item, a, err)
			}
		}
		stores := make([]*officers.PGSessionStore, n)
		for i := range stores {
			stores[i] = sessionStore(t, dsn)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		admitted := 0
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				a, err := stores[i].AdmitSession(ctx, capOfficer, day, fmt.Sprintf("race-%d", i), limit, t0)
				if err != nil {
					t.Errorf("admit race-%d: %v", i, err)
				}
				if a.Admitted {
					mu.Lock()
					admitted++
					mu.Unlock()
				}
			}(i)
		}
		close(start)
		wg.Wait()
		return admitted
	}
	if got := race("2000-01-02", 3, []string{"a", "b"}); got != 1 {
		t.Fatalf("at cap-1, %d concurrent admits were admitted, want exactly 1", got)
	}
	if got := race("2000-01-03", 100, nil); got != n {
		t.Fatalf("control: under the cap %d of %d concurrent admits were admitted", got, n)
	}
}

// TestAReleasedSessionFreesItsSlotAndTheDayRollsOver: releasing an item
// (HQ refused its claim) gives its slot back; a new UTC day starts at zero.
func TestAReleasedSessionFreesItsSlotAndTheDayRollsOver(t *testing.T) {
	dsn := freshSessions(t)
	ctx := dbtest.AsOperator(context.Background())
	s := sessionStore(t, dsn)
	day := "2000-01-04"
	if a, _ := s.AdmitSession(ctx, capOfficer, day, "a", 1, t0); !a.Admitted {
		t.Fatal("admit a")
	}
	if a, _ := s.AdmitSession(ctx, capOfficer, day, "b", 1, t0); a.Admitted {
		t.Fatal("b admitted past a cap of 1")
	}
	if err := s.ReleaseSession(ctx, capOfficer, day, "a"); err != nil {
		t.Fatal(err)
	}
	if a, err := s.AdmitSession(ctx, capOfficer, day, "b", 1, t0); err != nil || !a.Admitted {
		t.Fatalf("after releasing a, b: %+v %v", a, err)
	}
	if a, err := s.AdmitSession(ctx, capOfficer, "2000-01-05", "c", 1, t0); err != nil || !a.Admitted {
		t.Fatalf("the next day, c: %+v %v", a, err)
	}
}

// TestAPendingClaimNextResolvesToItsItem: a claim-next holds a session under a
// pending token until HQ names the item: resolved to a new item it counts that
// item; to an item already counted, or to none, it frees the token's slot.
func TestAPendingClaimNextResolvesToItsItem(t *testing.T) {
	dsn := freshSessions(t)
	ctx := dbtest.AsOperator(context.Background())
	s := sessionStore(t, dsn)
	day := "2000-01-06"
	p1, p2 := officers.PendingToken(t0), officers.PendingToken(t0)
	if a, _ := s.AdmitSession(ctx, capOfficer, day, "a", 2, t0); !a.Admitted {
		t.Fatal("admit a")
	}
	if a, _ := s.AdmitSession(ctx, capOfficer, day, p1, 2, t0); !a.Admitted {
		t.Fatal("admit the pending token")
	}
	if a, _ := s.AdmitSession(ctx, capOfficer, day, "b", 2, t0); a.Admitted {
		t.Fatal("the pending token did not hold its slot")
	}
	if err := s.ResolveSession(ctx, capOfficer, day, p1, "a"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Sessions(ctx, capOfficer, day); len(got) != 1 || got[0] != "a" {
		t.Fatalf("resolved to a counted item: %v, want [a]", got)
	}
	_, _ = s.AdmitSession(ctx, capOfficer, day, p2, 2, t0)
	if err := s.ResolveSession(ctx, capOfficer, day, p2, "c"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Sessions(ctx, capOfficer, day); len(got) != 2 || got[1] != "c" {
		t.Fatalf("resolved to a new item: %v, want [a c]", got)
	}
	if a, _ := s.AdmitSession(ctx, capOfficer, day, "d", 2, t0); a.Admitted {
		t.Fatal("d admitted past the cap after the resolve")
	}
}

// TestTwoClaimsOfOneItemTakeOneSlot (round 1 Y1, Y2): many concurrent admits
// of the SAME item, each through its own pool, below the cap: every one is
// admitted (a re-claim is free, never the cap's refusal), exactly ONE reports
// it inserted the item (so only that caller may undo it), and the item takes
// one slot.
func TestTwoClaimsOfOneItemTakeOneSlot(t *testing.T) {
	dsn := freshSessions(t)
	ctx := dbtest.AsOperator(context.Background())
	day := "2000-01-07"
	if a, err := sessionStore(t, dsn).AdmitSession(ctx, capOfficer, day, "a", 3, t0); err != nil || !a.Admitted {
		t.Fatalf("prefill: %+v %v", a, err)
	}
	const n = 8
	stores := make([]*officers.PGSessionStore, n)
	for i := range stores {
		stores[i] = sessionStore(t, dsn)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted, inserted := 0, 0
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			a, err := stores[i].AdmitSession(ctx, capOfficer, day, "x", 3, t0)
			if err != nil {
				t.Errorf("admit x (%d): %v", i, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if a.Admitted {
				admitted++
			}
			if a.Inserted {
				inserted++
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if admitted != n || inserted != 1 {
		t.Fatalf("%d concurrent claims of one item: %d admitted, %d inserted; want %d and exactly 1", n, admitted, inserted, n)
	}
	if got, _ := stores[0].Sessions(ctx, capOfficer, day); len(got) != 2 || got[0] != "a" || got[1] != "x" {
		t.Fatalf("sessions = %v, want [a x]", got)
	}
}

// TestAStalePendingEntryNoLongerBlocks (round 1 Y3): a claim-next's pending
// entry older than officers.PendingTTL (a server died before resolving it)
// stops counting: at a cap of 1 it refuses a new item while fresh, and no
// longer does once stale — for admission and for the wake loop's count.
func TestAStalePendingEntryNoLongerBlocks(t *testing.T) {
	dsn := freshSessions(t)
	ctx := dbtest.AsOperator(context.Background())
	s := sessionStore(t, dsn)
	day := "2000-01-08"
	tok := officers.PendingToken(t0)
	if a, err := s.AdmitSession(ctx, capOfficer, day, tok, 1, t0); err != nil || !a.Admitted {
		t.Fatalf("admit the pending token: %+v %v", a, err)
	}
	if a, _ := s.AdmitSession(ctx, capOfficer, day, "a", 1, t0.Add(10*time.Second)); a.Admitted {
		t.Fatal("a fresh pending entry did not hold its slot")
	}
	entries, _ := s.Sessions(ctx, capOfficer, day)
	if n, _ := officers.CountSessions(entries, t0.Add(10*time.Second)); n != 1 {
		t.Fatalf("fresh: counted %d, want 1", n)
	}
	later := t0.Add(officers.PendingTTL + time.Second)
	if n, _ := officers.CountSessions(entries, later); n != 0 {
		t.Fatalf("stale: counted %d, want 0", n)
	}
	if a, err := s.AdmitSession(ctx, capOfficer, day, "a", 1, later); err != nil || !a.Admitted {
		t.Fatalf("after the pending entry went stale, a: %+v %v", a, err)
	}
}
