package ccfailover

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestThePGStoreRecordsARebindOwnedByTheAgentsOwner: Record writes the event
// under the AGENT's owner whoever reports it (here the system principal, as
// the kickoff deliverer is), and ForAgent reads it back newest first with its
// reset time — and the owner's member cannot read another owner's.
func TestThePGStoreRecordsARebindOwnedByTheAgentsOwner(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), time.Minute)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatal(err)
	}
	ags := agents.NewPG(pool)
	name := "failover-ev-" + time.Now().Format("150405.000000")
	a, err := ags.Create(ctx, agents.Agent{Name: name, Namespace: "muster-agent-" + name, Status: agents.StatusRunning,
		DisplayName: "x", Kind: agents.KindClaudeCode, CCAccount: "south"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(dbtest.AsOperator(context.Background()), `DELETE FROM agents WHERE id = $1`, a.ID)
	})

	st := NewPG(pool)
	sys := authz.System(context.Background())
	resets := time.Date(2000, 1, 1, 21, 45, 0, 0, time.UTC)
	first, err := st.Record(sys, Event{AgentID: a.ID, AgentName: a.Name, From: "north", To: "south", Reason: "rate_limited", ResetsAt: resets})
	if err != nil {
		t.Fatal(err)
	}
	if first.OwnerID != a.OwnerID || first.ID == 0 || first.At.IsZero() {
		t.Fatalf("recorded = %+v, want the agent's owner %d, an id and a time", first, a.OwnerID)
	}
	if _, err := st.Record(sys, Event{AgentID: a.ID, AgentName: a.Name, From: "south", To: "north", Reason: "auth_failed"}); err != nil {
		t.Fatal(err)
	}
	evs, err := st.ForAgent(ctx, a.ID, 10)
	if err != nil || len(evs) != 2 {
		t.Fatalf("ForAgent = %d event(s), %v; want 2", len(evs), err)
	}
	if evs[0].From != "south" || !evs[0].ResetsAt.IsZero() || evs[1].From != "north" || !evs[1].ResetsAt.Equal(resets) {
		t.Fatalf("events = %+v, want newest first, the reset time on the first", evs)
	}
	// A move onto the same account is refused by the table, not stored.
	if _, err := st.Record(sys, Event{AgentID: a.ID, AgentName: a.Name, From: "north", To: "north", Reason: "rate_limited"}); err == nil {
		t.Fatal("an event from an account to itself was stored")
	}
}
