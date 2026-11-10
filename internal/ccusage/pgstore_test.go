package ccusage

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestPGStoreRoundTripsAndKeepsOwnersApart runs the store against real
// Postgres (migration 0106): a row round-trips (windows, nil times, the
// streak), an upsert replaces it, and a member's principal neither reads nor
// writes the operator's rows.
func TestPGStoreRoundTripsAndKeepsOwnersApart(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 60*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sys := authz.System(context.Background())
	for _, q := range []string{`DELETE FROM cc_account_usage`, `DELETE FROM jobs`, `DELETE FROM agents`} {
		if _, err := pool.Exec(sys, q); err != nil {
			t.Fatal(err)
		}
	}
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member') ON CONFLICT (id) DO NOTHING`)
	member := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 2, Role: authz.RoleMember, Via: authz.ViaSession})
	st := NewPG(pool)

	reset := time.Date(2026, 11, 10, 20, 0, 0, 0, time.UTC)
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	in := Row{Account: "alpha", ReadAt: at, CheckedAt: at, TokenFP: "0123456789ab",
		Windows: map[string]Window{FiveHour: {Utilization: f(42.5), ResetsAt: &reset}, SevenDay: {Utilization: f(3)}}}
	if err := st.Save(ctx, 1, in); err != nil {
		t.Fatal(err)
	}
	got, err := st.Latest(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	g := got["alpha"]
	if *g.Windows[FiveHour].Utilization != 42.5 || !g.Windows[FiveHour].ResetsAt.Equal(reset) || g.Windows[SevenDay].ResetsAt != nil ||
		!g.ReadAt.Equal(at) || !g.NextAt.IsZero() || g.TokenFP != in.TokenFP {
		t.Fatalf("round trip: %+v", g)
	}
	next := at.Add(time.Hour)
	if err := st.Save(ctx, 1, Row{Account: "alpha", Windows: g.Windows, ReadAt: at, CheckedAt: next, ErrorKind: KindRateLimited,
		Error: "429", TokenFP: in.TokenFP, RateLimitStreak: 2, NextAt: next}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Latest(ctx, 1)
	if g := got["alpha"]; g.ErrorKind != KindRateLimited || g.RateLimitStreak != 2 || !g.NextAt.Equal(next) || len(got) != 1 {
		t.Fatalf("after the upsert: %+v (%d rows)", g, len(got))
	}
	if err := st.Save(member, 1, Row{Account: "forged", CheckedAt: at}); err == nil {
		t.Fatal("a member's principal wrote a row for owner 1")
	}
	if mm, err := st.Latest(member, 1); err != nil || len(mm) != 0 {
		t.Fatalf("a member's principal read owner 1's rows: %v %v", mm, err)
	}

	// Attribution reads: live agents count as active now, a stopped one at its
	// stop time; jobs are counted by account and requester over the windows.
	as := agents.NewPG(pool)
	stamp := time.Now().Format("150405.000000")
	mk := func(name, account, status string) int64 {
		t.Helper()
		a, err := as.Create(ctx, agents.Agent{Name: name + "-" + stamp, Namespace: "ns", Kind: agents.KindClaudeCode, CCAccount: account, Status: status})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return a.ID
	}
	mk("live", "alpha", agents.StatusRunning)
	recent := mk("recent", "alpha", agents.StatusStopped)
	old := mk("old", "alpha", agents.StatusStopped)
	now := time.Now().UTC().Truncate(time.Microsecond) // Postgres keeps microseconds
	for id, ago := range map[int64]time.Duration{recent: 2 * time.Hour, old: 8 * Day} {
		if _, err := pool.Exec(sys, `UPDATE agents SET stopped_at = $2 WHERE id = $1`, id, now.Add(-ago)); err != nil {
			t.Fatal(err)
		}
	}
	uses, err := st.AgentsSince(ctx, 1, now.Add(-Week), now)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]time.Time{}
	for _, u := range uses {
		seen[u.Name[:len(u.Name)-len(stamp)-1]] = u.LastActive
	}
	if len(seen) != 2 || seen["live"].Sub(now).Abs() > time.Millisecond || seen["recent"].Sub(now.Add(-2*time.Hour)).Abs() > time.Second {
		t.Fatalf("AgentsSince = %v, want live (now) and recent (2h ago), not old", seen)
	}
	for _, j := range []struct {
		account, by string
		ago         time.Duration
	}{{"alpha", "pr-review", time.Hour}, {"alpha", "pr-review", 3 * Day}, {"alpha", "pr-review", 9 * Day}, {"beta", "", time.Minute}} {
		if _, err := pool.Exec(ctx, `INSERT INTO jobs (agent_name, prompt, timeout_s, requested_by, cc_account, created_at)
			VALUES ('w', 'p', 60, $1, $2, $3)`, j.by, j.account, now.Add(-j.ago)); err != nil {
			t.Fatal(err)
		}
	}
	counts, err := st.JobCounts(ctx, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]JobCount{}
	for _, c := range counts {
		byKey[c.CCAccount+"/"+c.RequestedBy] = c
	}
	if c := byKey["alpha/pr-review"]; c.Last24h != 1 || c.Last7d != 2 {
		t.Fatalf("alpha/pr-review = %+v, want 1 in 24h and 2 in 7d", c)
	}
	if c := byKey["beta/"]; c.Last24h != 1 || c.Last7d != 1 || len(byKey) != 2 {
		t.Fatalf("counts = %v", byKey)
	}
	if mj, err := st.JobCounts(member, 1, now); err != nil || len(mj) != 0 {
		t.Fatalf("a member's principal counted owner 1's jobs: %v %v", mj, err)
	}
}
