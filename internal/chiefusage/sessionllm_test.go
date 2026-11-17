package chiefusage

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestATurnsCostIsWrittenAndNullWhenNone: chief_turn_usage.cost_usd holds the
// turn's booked cost when the turn path gives one, and is NULL — not 0 — when
// it gives none.
func TestATurnsCostIsWrittenAndNullWhenNone(t *testing.T) {
	s, pool := pgUsage(t)
	cost := 0.0123
	for _, tu := range []Turn{
		{Chief: 61, Trigger: Trigger{Kind: TriggerOwnerChat}, Outcome: OutcomeOther, SessionID: 901, CostUSD: &cost},
		{Chief: 61, Trigger: Trigger{Kind: TriggerOwnerChat}, Outcome: OutcomeOther, SessionID: 902},
	} {
		if err := s.RecordTurn(as(2), tu); err != nil {
			t.Fatal(err)
		}
	}
	got := map[int64]*float64{}
	rows, err := pool.Query(authz.System(context.Background()), `SELECT session_id, cost_usd::float8 FROM chief_turn_usage`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		var c *float64
		if err := rows.Scan(&id, &c); err != nil {
			t.Fatal(err)
		}
		got[id] = c
	}
	rows.Close()
	if c := got[901]; c == nil || math.Abs(*c-cost) > 1e-9 {
		t.Errorf("the costed turn's cost_usd = %v, want %v", c, cost)
	}
	if c, ok := got[902]; !ok || c != nil {
		t.Errorf("the uncosted turn's cost_usd = %v (present %v), want NULL", c, ok)
	}
}

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
