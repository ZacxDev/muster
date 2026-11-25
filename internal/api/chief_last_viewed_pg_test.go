package api

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
)

// TestLastViewedSessionIsTheAgentsOwnLatestView: the store half
// (agents.SessionViewStore, migration 0187) — none viewed is "none" (ok
// false), the latest viewed_at wins whatever the ids' order, an archived chat
// is still answered (the caller, chiefOpenSession, decides), and another
// agent's view is not this one's.
func TestLastViewedSessionIsTheAgentsOwnLatestView(t *testing.T) {
	f := newChatsFixture(t, "")
	ctx := asUser(1)
	st, ok := f.srv.sessionViewStore()
	if !ok {
		t.Fatal("the PG store has no 0187 half")
	}
	mk := func() agents.ChatSession {
		s, err := f.srv.ext.Agents.CreateSession(ctx, f.chief.ID, f.chief.Name)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if _, found, err := st.LastViewedSession(ctx, f.chief.ID); err != nil || found {
		t.Fatalf("none viewed: found %v, err %v", found, err)
	}
	// The primary first (elected as the oldest), so the two below are side chats.
	if _, err := f.srv.primarySession(ctx, f.chief); err != nil {
		t.Fatal(err)
	}
	older, newer := mk(), mk()
	for _, id := range []int64{newer.ID, older.ID} { // the OLDER id viewed last
		if err := st.MarkSessionViewed(ctx, id); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	got, found, err := st.LastViewedSession(ctx, f.chief.ID)
	if err != nil || !found || got.ID != older.ID {
		t.Errorf("last viewed: %d (found %v, err %v), want %d", got.ID, found, err, older.ID)
	}
	cs, _ := f.srv.chiefChatStore()
	if _, err := cs.SetArchived(ctx, f.chief.ID, older.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.LastViewedSession(ctx, f.chief.ID); got.ID != older.ID || got.ArchivedAt == nil {
		t.Errorf("an archived chat viewed last: got %d (archived %v), want %d archived", got.ID, got.ArchivedAt != nil, older.ID)
	}
	// Another agent's view.
	other, err := agents.NewPG(f.pool).Create(ctx, agents.Agent{Name: f.chief.Name + "-other", Namespace: f.chief.Namespace + "-other",
		Status: agents.StatusRunning, HooksToken: "other-token-" + f.chief.Name})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, other.ID)
	})
	os, err := f.srv.ext.Agents.CreateSession(ctx, other.ID, other.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkSessionViewed(ctx, os.ID); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.LastViewedSession(ctx, f.chief.ID); got.ID != older.ID {
		t.Errorf("another agent's view moved the chief's: %d, want %d", got.ID, older.ID)
	}
}
