package api

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
)

// TestAFreshPrimaryWaitsForAChatWriteInFlight (PR #609 round 2 N2),
// deterministically: a chat message being written into the primary — its
// insert holds FOR KEY SHARE on the chat_sessions row through the foreign key,
// for as long as its transaction is open — when a fresh primary is asked for.
// The fresh primary WAITS for it (a bounded lock_timeout) and succeeds; with
// NOWAIT it answered "a fresh primary is already being made" to every delivery
// that happened to be mid-write.
func TestAFreshPrimaryWaitsForAChatWriteInFlight(t *testing.T) {
	f := newChatsFixture(t, "")
	ctx := asUser(1)
	primary, err := f.srv.primarySession(ctx, f.chief)
	if err != nil {
		t.Fatal(err)
	}
	sys := authz.System(context.Background())
	tx, err := f.pool.Begin(sys)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(sys, `INSERT INTO chat_messages (agent_id, owner_id, session_id, role, content, kind)
		VALUES ($1, 1, $2, 'user', 'a delivery mid-write', 'text')`, f.chief.ID, primary.ID); err != nil {
		t.Fatal(err)
	}
	committed := make(chan error, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		committed <- tx.Commit(sys)
	}()
	start := time.Now()
	old, fresh, err := f.srv.ext.Agents.(agents.ChatStore).FreshPrimary(ctx, f.chief.ID, f.chief.Name, "", "")
	if cerr := <-committed; cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil {
		t.Fatalf("a fresh primary while a chat write held the primary's row: %v (after %s); want it to wait and succeed", err, time.Since(start).Round(time.Millisecond))
	}
	if old.ID != primary.ID || fresh.ID == primary.ID {
		t.Errorf("FreshPrimary: old %d new %d, want old %d and a new one", old.ID, fresh.ID, primary.ID)
	}
	if rows := f.rowsIn(t, primary.ID); len(rows) != 1 {
		t.Errorf("the write in flight was lost: %v", rows)
	}
}
