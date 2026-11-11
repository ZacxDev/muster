package agents

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestNoPrimaryIsEverElectedOntoAnArchivedChat (PR #609 round 1 F4, the
// auditor's repro): an agent with NO default yet, whose legacy-key session —
// the one the election prefers — is archived. Before the fix the next
// DefaultSession elected it, the 0114 CHECK refused the UPDATE, and every
// delivery to the agent failed from then on.
//
//   - the election skips the archived session and picks a live one;
//   - SetArchived elects a primary FIRST when there is none, so archiving the
//     session the election would pick is refused as archiving the primary;
//   - with EVERY session archived the election opens a new one.
func TestNoPrimaryIsEverElectedOntoAnArchivedChat(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatal(err)
	}
	store := NewPG(pool)
	newAgent := func(tag string) Agent {
		name := "arch-" + tag + "-" + time.Now().Format("150405.000000")
		a, err := store.Create(ctx, Agent{Name: name, Namespace: "devpod-" + name, Status: StatusRunning})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cleanupAgent(context.Background(), pool, a.ID) })
		return a
	}

	t.Run("the repro: the preferred session archived with no default", func(t *testing.T) {
		a := newAgent("repro")
		legacy, err := store.LatestOrCreateSession(ctx, a.ID, a.Name) // the legacy key: what the election prefers
		if err != nil {
			t.Fatal(err)
		}
		other, err := store.CreateSession(ctx, a.ID, a.Name)
		if err != nil {
			t.Fatal(err)
		}
		// Archived while the agent has no default (what an image without the
		// election-before-archive left, or a direct write).
		if _, err := pool.Exec(ctx, `UPDATE chat_sessions SET archived_at = now() WHERE id = $1`, legacy.ID); err != nil {
			t.Fatal(err)
		}
		got, err := store.DefaultSession(ctx, a.ID, a.Name)
		if err != nil {
			t.Fatalf("THE ELECTION FAILED (every delivery would): %v", err)
		}
		if got.ID != other.ID || got.ArchivedAt != nil {
			t.Errorf("elected %d (archived %v), want the live session %d", got.ID, got.ArchivedAt, other.ID)
		}
	})

	t.Run("archiving the would-be primary elects first and is refused", func(t *testing.T) {
		a := newAgent("first")
		legacy, err := store.LatestOrCreateSession(ctx, a.ID, a.Name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateSession(ctx, a.ID, a.Name); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SetArchived(ctx, a.ID, legacy.ID, true); !errors.Is(err, ErrArchivePrimary) {
			t.Fatalf("archiving the session the election picks: %v, want ErrArchivePrimary", err)
		}
		if got, err := store.DefaultSession(ctx, a.ID, a.Name); err != nil || got.ID != legacy.ID || got.ArchivedAt != nil {
			t.Errorf("the primary is %+v (%v), want the live legacy session", got, err)
		}
	})

	t.Run("every session archived: a new primary", func(t *testing.T) {
		a := newAgent("all")
		only, err := store.LatestOrCreateSession(ctx, a.ID, a.Name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE chat_sessions SET archived_at = now() WHERE id = $1`, only.ID); err != nil {
			t.Fatal(err)
		}
		got, err := store.DefaultSession(ctx, a.ID, a.Name)
		if err != nil || got.ID == only.ID || !got.IsDefault || got.ArchivedAt != nil {
			t.Errorf("with every session archived the primary is %+v (%v), want a new live one", got, err)
		}
	})
}
