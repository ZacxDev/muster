package agents

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestRebindCCAccountMovesOnlyOffTheNamedAccount: the account failover's one
// write (migration 0066). It moves a claude-code agent off `from` exactly once
// — a second move off the same account matches no row — keeps the pin flags,
// and every OTHER UPDATE of cc_account is still refused by 0003's trigger.
func TestRebindCCAccountMovesOnlyOffTheNamedAccount(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	store := requirePGStore(t, ctx)
	name := "rebind-" + time.Now().Format("150405.000000")
	a, err := store.Create(ctx, Agent{Name: name, Namespace: "muster-agent-" + name, Status: StatusRunning, DisplayName: "rebind",
		Kind: KindClaudeCode, CCAccount: "north", CCAccountPinned: true, CCFailover: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupAgent(ctx, store.pool, a.ID) })
	if !a.CCAccountPinned || !a.CCFailover {
		t.Fatalf("the pin flags did not round-trip: %+v", a)
	}

	moved, err := store.RebindCCAccount(ctx, a.ID, "north", "south")
	if err != nil || !moved {
		t.Fatalf("first move = %v, %v; want moved", moved, err)
	}
	again, err := store.RebindCCAccount(ctx, a.ID, "north", "east")
	if err != nil || again {
		t.Fatalf("a second move off north = %v, %v; want no row (the agent is on south)", again, err)
	}
	got, err := store.Get(ctx, a.ID)
	if err != nil || got.CCAccount != "south" || !got.CCAccountPinned || !got.CCFailover {
		t.Fatalf("after the moves: %+v (%v); want south, flags kept", got, err)
	}
	// Every other write of the column is still refused (the trigger), and the
	// refusal is the trigger's own.
	_, err = store.pool.Exec(ctx, `UPDATE agents SET cc_account = 'east' WHERE id = $1`, a.ID)
	if err == nil || !strings.Contains(err.Error(), "fixed at creation") {
		t.Fatalf("a plain UPDATE of cc_account: %v, want the trigger's refusal", err)
	}
	// Nor can the move change the kind, or empty the account.
	if _, err := store.RebindCCAccount(ctx, a.ID, "south", ""); err == nil {
		t.Fatal("a move onto no account was accepted")
	}
}
