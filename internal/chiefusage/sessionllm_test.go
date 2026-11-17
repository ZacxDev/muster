package chiefusage

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestTheSessionBookIsTheOwnersAlone: bookings add up per (session, day); the
// day's spend is the owner's over every session; another owner reads none of
// it (row-level security and the owner filter).
func TestTheSessionBookIsTheOwnersAlone(t *testing.T) {
	s, pool := pgUsage(t)
	dbtest.OwnerExec(t, dbtest.DSN(t), `TRUNCATE chief_session_llm_usage`)
	day := time.Date(2031, 3, 14, 0, 0, 0, 0, time.UTC)
	for _, u := range []SessionLLMUsage{
		{SessionID: 811, Model: "openrouter/vendor-q/q1", Day: day, Input: 100, Output: 9, CostUSD: 0.0031},
		{SessionID: 811, Model: "openrouter/vendor-q/q1", Day: day, Input: 200, CacheRead: 50, Output: 11, CostUSD: 0.0047, Estimated: true},
		{SessionID: 812, Model: "openrouter/vendor-r/r2", Day: day, Input: 70, Output: 3, CostUSD: 0.0059},
	} {
		if err := s.BookSessionLLM(as(2), u); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.BookSessionLLM(as(3), SessionLLMUsage{SessionID: 813, Day: day, CostUSD: 0.0997}); err != nil {
		t.Fatal(err)
	}
	daySpent, err := s.DaySpent(as(2), day)
	if err != nil || math.Abs(daySpent-0.0137) > 1e-9 {
		t.Fatalf("owner 2's day = %v (%v), want 0.0137", daySpent, err)
	}
	sess, err := s.SessionSpent(as(2), 811)
	if err != nil || math.Abs(sess-0.0078) > 1e-9 {
		t.Fatalf("session 811 = %v (%v), want 0.0078", sess, err)
	}
	if other, err := s.SessionSpent(as(3), 811); err != nil || other != 0 {
		t.Fatalf("owner 3 reads owner 2's session as %v (%v), want 0", other, err)
	}
	var req, est, in, read int64
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT requests, estimated_requests, input_tokens, cache_read_tokens
		FROM chief_session_llm_usage WHERE session_id = 811`).Scan(&req, &est, &in, &read); err != nil {
		t.Fatal(err)
	}
	if req != 2 || est != 1 || in != 300 || read != 50 {
		t.Fatalf("session 811's row: %d requests (%d estimated), in %d, cache read %d", req, est, in, read)
	}
}

// TestTheSessionBookCannotBeDeletedOrDecreased: as the application role
// (muster_app, which holds DELETE on every table through approle.go's boot
// GRANT), a row of the spend book cannot be deleted, moved or decreased; the
// proxy's own upsert, which only adds, still works.
func TestTheSessionBookCannotBeDeletedOrDecreased(t *testing.T) {
	s, pool := pgUsage(t)
	dbtest.OwnerExec(t, dbtest.DSN(t), `TRUNCATE chief_session_llm_usage`)
	day := time.Date(2031, 3, 15, 0, 0, 0, 0, time.UTC)
	u := SessionLLMUsage{SessionID: 821, Model: "openrouter/vendor-s/s3", Day: day, Input: 41, Output: 5, CostUSD: 0.0061}
	if err := s.BookSessionLLM(as(2), u); err != nil {
		t.Fatal(err)
	}
	// Each refusal names its own branch, so the DELETE case is pinned to the
	// DELETE branch (not to the update branch, whose NULL NEW would also
	// refuse it).
	for q, want := range map[string]string{
		`DELETE FROM chief_session_llm_usage WHERE session_id = 821`:                            "cannot be deleted",
		`UPDATE chief_session_llm_usage SET cost_usd = cost_usd - 0.001 WHERE session_id = 821`: "cannot be moved",
		`UPDATE chief_session_llm_usage SET requests = requests - 1 WHERE session_id = 821`:     "cannot be moved",
		`UPDATE chief_session_llm_usage SET input_tokens = 0 WHERE session_id = 821`:            "cannot be moved",
		`UPDATE chief_session_llm_usage SET day = day - 1 WHERE session_id = 821`:               "cannot be moved",
	} {
		_, err := pool.Exec(as(2), q)
		if err == nil || !strings.Contains(err.Error(), "append-only") || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want the append-only refusal (%s)", q, err, want)
		}
	}
	if err := s.BookSessionLLM(as(2), u); err != nil {
		t.Fatalf("the adding upsert is refused: %v", err)
	}
	if got, err := s.SessionSpent(as(2), 821); err != nil || math.Abs(got-0.0122) > 1e-9 {
		t.Fatalf("session 821 = %v (%v), want both bookings, 0.0122", got, err)
	}
}

// TestReserveHoldsTheCapUnderConcurrency: 20 Reserves at once, each wanting
// $0.0037 while the owner's day has room for $0.011, admit exactly 2 (2 × 0.0037
// = 0.0074; a third would make 0.0111): the check and the reservation are one
// step under the owner's lock. A reservation past expires_at no longer counts,
// Release drops one, and Settle replaces one by the booked cost.
func TestReserveHoldsTheCapUnderConcurrency(t *testing.T) {
	s, pool := pgUsage(t)
	dbtest.OwnerExec(t, dbtest.DSN(t), `TRUNCATE chief_session_llm_usage, chief_session_llm_reservations`)
	day := time.Date(2031, 3, 16, 0, 0, 0, 0, time.UTC)
	const roomUSD, wantUSD = 0.011, 0.0037
	decide := func(sp SessionSpend) float64 {
		if sp.DaySpent+sp.DayReserved+wantUSD > roomUSD {
			return 0
		}
		return wantUSD
	}
	ids := make([]int64, 20)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, _, err := s.Reserve(as(2), 831, day, time.Minute, decide)
			if err != nil {
				t.Errorf("reserve %d: %v", i, err)
			}
			ids[i] = id
		}(i)
	}
	wg.Wait()
	var got []int64
	for _, id := range ids {
		if id > 0 {
			got = append(got, id)
		}
	}
	sp, err := s.Spend(as(2), 831, day)
	if err != nil || len(got) != 2 || math.Abs(sp.DayReserved-2*wantUSD) > 1e-9 || math.Abs(sp.SessionReserved-2*wantUSD) > 1e-9 {
		t.Fatalf("20 concurrent reserves: %d admitted, spend %+v (%v), want 2 holding $0.0074", len(got), sp, err)
	}
	// Settle the first: its reservation goes, its booked cost counts instead.
	if err := s.Settle(as(2), got[0], SessionLLMUsage{SessionID: 831, Day: day, CostUSD: 0.0029}); err != nil {
		t.Fatal(err)
	}
	// Release the second.
	if err := s.Release(as(2), got[1]); err != nil {
		t.Fatal(err)
	}
	if sp, err := s.Spend(as(2), 831, day); err != nil || sp.DayReserved != 0 || math.Abs(sp.DaySpent-0.0029) > 1e-9 {
		t.Fatalf("after settle and release: %+v (%v), want $0.0029 booked and nothing held", sp, err)
	}
	// An expired reservation does not count.
	if _, err := pool.Exec(authz.System(context.Background()), `INSERT INTO chief_session_llm_reservations (owner_id, session_id, amount_usd, expires_at)
		VALUES (2, 831, 0.0063, now() - interval '1 second')`); err != nil {
		t.Fatal(err)
	}
	if sp, err := s.Spend(as(2), 831, day); err != nil || sp.DayReserved != 0 {
		t.Fatalf("an expired reservation counts: %+v (%v)", sp, err)
	}
	// Another owner sees none of it.
	if sp, err := s.Spend(as(3), 831, day); err != nil || sp.DaySpent != 0 || sp.SessionSpent != 0 {
		t.Fatalf("owner 3 reads owner 2's spend: %+v (%v)", sp, err)
	}
}
