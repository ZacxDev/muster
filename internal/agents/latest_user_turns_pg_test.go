package agents

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestLatestUserTurnsReadsTheNewestAndTheTypersOwn: across BOTH of an agent's
// sessions, Latest is its newest user-role row from any source and Typed its
// newest row typed by exactly the asked user — never an untyped row, another
// typer's, an assistant row, or another agent's. AddChatMessage stores
// TypedBy 0 as NULL, and the read gives it back as 0. Typer ids are distinct
// from each other and from every row id.
func TestLatestUserTurnsReadsTheNewestAndTheTypersOwn(t *testing.T) {
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
	mk := func(suffix string) Agent {
		a, err := store.Create(ctx, Agent{Name: "lut-" + suffix + "-" + tag, Namespace: "devpod-lut-" + suffix + "-" + tag, Status: StatusRunning})
		if err != nil {
			t.Fatalf("create agent %s: %v", suffix, err)
		}
		created = append(created, a.ID)
		return a
	}
	chief, other := mk("chief"), mk("other")
	s1, err := store.CreateSession(ctx, chief.ID, chief.Name)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := store.CreateSession(ctx, chief.ID, chief.Name)
	if err != nil {
		t.Fatal(err)
	}
	so, err := store.CreateSession(ctx, other.ID, other.Name)
	if err != nil {
		t.Fatal(err)
	}
	const owner, stranger = int64(900_001), int64(900_002)
	add := func(a Agent, sess ChatSession, role, text string, by int64) ChatMessage {
		t.Helper()
		m, err := store.AddChatMessage(ctx, ChatMessage{AgentID: a.ID, SessionID: sess.ID, Role: role, Content: text, TypedBy: by})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	if got, err := store.LatestUserTurns(ctx, chief.ID, owner); err != nil || got.Latest.ID != 0 || got.Typed.ID != 0 {
		t.Fatalf("an empty chat: %+v %v; want neither", got, err)
	}
	ownersFirst := add(chief, s1, "user", "owner's first", owner)
	ownersLast := add(chief, s2, "user", "owner's last", owner) // the OTHER session
	add(chief, s2, "assistant", "a reply", 0)
	add(other, so, "user", "owner in another agent's chat", owner)
	strangers := add(chief, s1, "user", "a stranger's line", stranger)
	delivery := add(chief, s1, "user", "plugin delivery", 0)
	add(chief, s1, "assistant", "the chief's reply", 0)

	got, err := store.LatestUserTurns(ctx, chief.ID, owner)
	if err != nil {
		t.Fatal(err)
	}
	if got.Latest.ID != delivery.ID || got.Latest.TypedBy != 0 || got.Latest.Content != "plugin delivery" {
		t.Errorf("Latest = %+v; want the plugin delivery %d, untyped", got.Latest, delivery.ID)
	}
	if got.Typed.ID != ownersLast.ID || got.Typed.TypedBy != owner || got.Typed.AgentID != chief.ID ||
		got.Typed.SessionID != s2.ID || got.Typed.Content != "owner's last" {
		t.Errorf("Typed = %+v; want the owner's last line %d (session %d), not %d", got.Typed, ownersLast.ID, s2.ID, ownersFirst.ID)
	}
	if got, err := store.LatestUserTurns(ctx, chief.ID, stranger); err != nil || got.Typed.ID != strangers.ID || got.Typed.TypedBy != stranger {
		t.Errorf("the stranger's newest: %+v %v; want %d", got.Typed, err, strangers.ID)
	}
	if got, err := store.LatestUserTurns(ctx, chief.ID, 0); err != nil || got.Typed.ID != 0 || got.Latest.ID != delivery.ID {
		t.Errorf("typedBy 0 must read no typed row (an untyped row is nobody's): %+v %v", got, err)
	}
}
