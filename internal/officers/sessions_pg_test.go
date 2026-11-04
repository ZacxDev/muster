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
		if ok, err := first.AdmitSession(ctx, capOfficer, day, item, 2); err != nil || !ok {
			t.Fatalf("admit %s: %v %v", item, ok, err)
		}
	}
	second := sessionStore(t, dsn)
	if ok, err := second.AdmitSession(ctx, capOfficer, day, "c", 2); err != nil || ok {
		t.Fatalf("a new server admitted the third session of a cap of 2: %v %v", ok, err)
	}
	if ok, err := second.AdmitSession(ctx, capOfficer, day, "a", 2); err != nil || !ok {
		t.Fatalf("a new server refused re-claiming counted item a: %v %v", ok, err)
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
			if ok, err := s.AdmitSession(ctx, capOfficer, day, item, limit); err != nil || !ok {
				t.Fatalf("prefill %s: %v %v", item, ok, err)
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
				ok, err := stores[i].AdmitSession(ctx, capOfficer, day, fmt.Sprintf("race-%d", i), limit)
				if err != nil {
					t.Errorf("admit race-%d: %v", i, err)
				}
				if ok {
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
	if ok, _ := s.AdmitSession(ctx, capOfficer, day, "a", 1); !ok {
		t.Fatal("admit a")
	}
	if ok, _ := s.AdmitSession(ctx, capOfficer, day, "b", 1); ok {
		t.Fatal("b admitted past a cap of 1")
	}
	if err := s.ReleaseSession(ctx, capOfficer, day, "a"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AdmitSession(ctx, capOfficer, day, "b", 1); err != nil || !ok {
		t.Fatalf("after releasing a, b: %v %v", ok, err)
	}
	if ok, err := s.AdmitSession(ctx, capOfficer, "2000-01-05", "c", 1); err != nil || !ok {
		t.Fatalf("the next day, c: %v %v", ok, err)
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
	if ok, _ := s.AdmitSession(ctx, capOfficer, day, "a", 2); !ok {
		t.Fatal("admit a")
	}
	if ok, _ := s.AdmitSession(ctx, capOfficer, day, "pending:1", 2); !ok {
		t.Fatal("admit the pending token")
	}
	if ok, _ := s.AdmitSession(ctx, capOfficer, day, "b", 2); ok {
		t.Fatal("the pending token did not hold its slot")
	}
	if err := s.ResolveSession(ctx, capOfficer, day, "pending:1", "a"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Sessions(ctx, capOfficer, day); len(got) != 1 || got[0] != "a" {
		t.Fatalf("resolved to a counted item: %v, want [a]", got)
	}
	_, _ = s.AdmitSession(ctx, capOfficer, day, "pending:2", 2)
	if err := s.ResolveSession(ctx, capOfficer, day, "pending:2", "c"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Sessions(ctx, capOfficer, day); len(got) != 2 || got[1] != "c" {
		t.Fatalf("resolved to a new item: %v, want [a c]", got)
	}
	if ok, _ := s.AdmitSession(ctx, capOfficer, day, "d", 2); ok {
		t.Fatal("d admitted past the cap after the resolve")
	}
}
