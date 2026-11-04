package agents

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestOwnerTypedChatMessagesReadsOnlyTheTypersOwnTypedTurns (migration 0079,
// step 3b-1): the read returns the agent's text USER rows typed by the user
// asked for, in the window, oldest first, the newest limit of them — never an
// untyped row, another typer's, an assistant row (whose typer the store refuses
// to write at all), a tool row, an older row or another agent's. Texts and ids
// are pairwise distinct.
func TestOwnerTypedChatMessagesReadsOnlyTheTypersOwnTypedTurns(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewPG(pool)
	tag := time.Now().Format("150405.000000")
	var created []int64
	defer func() {
		for _, id := range created {
			cleanupAgent(ctx, pool, id)
		}
	}()
	mk := func(suffix string) (Agent, ChatSession) {
		a, err := store.Create(ctx, Agent{Name: "oty-" + suffix + "-" + tag, Namespace: "devpod-oty-" + suffix + "-" + tag, Status: StatusRunning})
		if err != nil {
			t.Fatalf("create agent %s: %v", suffix, err)
		}
		created = append(created, a.ID)
		s, err := store.CreateSession(ctx, a.ID, a.Name)
		if err != nil {
			t.Fatal(err)
		}
		return a, s
	}
	chief, sess := mk("chief")
	other, otherSess := mk("other")
	const typer, someoneElse = 1, 4242
	add := func(a Agent, s ChatSession, m ChatMessage) ChatMessage {
		t.Helper()
		m.AgentID, m.SessionID = a.ID, s.ID
		got, err := store.AddChatMessage(ctx, m)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	old := add(chief, sess, ChatMessage{Role: "user", Content: "typed seven hours ago", TypedBy: typer})
	if _, err := pool.Exec(ctx, `UPDATE chat_messages SET created_at = now() - interval '7 hours' WHERE id = $1`, old.ID); err != nil {
		t.Fatal(err)
	}
	first := add(chief, sess, ChatMessage{Role: "user", Content: "typed first", TypedBy: typer})
	add(chief, sess, ChatMessage{Role: "user", Content: "not typed by anyone"})
	add(chief, sess, ChatMessage{Role: "user", Content: "typed by someone else", TypedBy: someoneElse})
	reply := add(chief, sess, ChatMessage{Role: "assistant", Content: "a reply its caller marked typed", TypedBy: typer})
	add(chief, sess, ChatMessage{Role: "user", Kind: "tool_call", Content: `{"typed":"as a tool"}`, TypedBy: typer})
	add(other, otherSess, ChatMessage{Role: "user", Content: "typed to another agent", TypedBy: typer})
	second := add(chief, sess, ChatMessage{Role: "user", Content: "typed second", TypedBy: typer})
	third := add(chief, sess, ChatMessage{Role: "user", Content: "typed third", TypedBy: typer})

	if first.TypedBy != typer || reply.TypedBy != 0 {
		t.Fatalf("stored typers: a typed user row %d, an assistant row %d (want %d and 0)", first.TypedBy, reply.TypedBy, typer)
	}
	since := time.Now().Add(-6 * time.Hour)
	got, err := store.OwnerTypedChatMessages(ctx, chief.ID, typer, since, 10)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(ms []ChatMessage) []int64 {
		var out []int64
		for _, m := range ms {
			out = append(out, m.ID)
		}
		return out
	}
	if want := []int64{first.ID, second.ID, third.ID}; !equalIDs(ids(got), want) {
		t.Fatalf("read %v (%+v), want %v", ids(got), got, want)
	}
	if got, _ := store.OwnerTypedChatMessages(ctx, chief.ID, typer, since, 2); !equalIDs(ids(got), []int64{second.ID, third.ID}) {
		t.Fatalf("limit 2 read %v, want the newest two oldest first", ids(got))
	}
	if got, _ := store.OwnerTypedChatMessages(ctx, chief.ID, 0, since, 10); len(got) != 0 {
		t.Fatalf("typer 0 read %v", ids(got))
	}
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
