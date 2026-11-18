package api

import (
	"fmt"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// TestARehomedChatGetsANewLaneAndKeepsItsHistory is re-homing on the real
// store (officerChatLanes over agents.PGStore): chat c-9's lane lives on
// instance 2 (a chat-only instance: max_instances 1), with marks, a secret and
// a recorded turn. Rehome from 2 sets that session aside — its key no longer
// the chat's, its secret cleared, its turn still readable on instance 2's
// agent — so the chat has no home, and its next Open on instance 1 makes a
// new session with no marks. A home past the chat ceiling is still read as
// its number (5), never an error that would hold the chat for ever; a Rehome
// with nothing to move is no error.
func TestARehomedChatGetsANewLaneAndKeepsItsHistory(t *testing.T) {
	c := newChatPG(t)
	lanes := officerChatLanes{s: c.s}
	row := func(n int) agents.Agent {
		t.Helper()
		name := c.o.InstanceName(n)
		a, err := c.pg.Create(c.ctx, agents.Agent{Name: name, Namespace: "rehome-" + name, Status: agents.StatusRunning, Kind: agents.KindClaudeCode})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		t.Cleanup(func() { _ = c.pg.Delete(c.ctx, a.ID) })
		return a
	}
	two := row(2)
	row(5)
	key := officers.ChatSessionKey("c-9")
	if _, err := lanes.Open(c.ctx, c.o, 2, "c-9"); err != nil {
		t.Fatalf("open on 2: %v", err)
	}
	if err := lanes.Mark(c.ctx, c.o, 2, "c-9", officers.ChatMarks{Seen: 7, Answered: 7, Charter: "v3"}); err != nil {
		t.Fatal(err)
	}
	old, err := c.pg.SessionByKeyAnyAgent(c.ctx, key)
	if err != nil || old.AgentID != two.ID {
		t.Fatalf("c-9's session: %+v %v; want instance 2's", old, err)
	}
	if _, err := c.pg.EnsureChatLaneSecret(c.ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.pg.AddChatMessage(c.ctx, agents.ChatMessage{AgentID: two.ID, SessionID: old.ID, Role: "user", Content: "the old turn"}); err != nil {
		t.Fatal(err)
	}
	if n, err := lanes.Home(c.ctx, c.o, "c-9"); err != nil || n != 2 {
		t.Fatalf("home %d %v, want 2", n, err)
	}
	if err := lanes.Rehome(c.ctx, c.o, "c-9", 2); err != nil {
		t.Fatalf("rehome from 2: %v", err)
	}
	if n, err := lanes.Home(c.ctx, c.o, "c-9"); err != nil || n != 0 {
		t.Fatalf("home after the rehome: %d %v, want none", n, err)
	}
	if s, err := c.pg.ChatLaneSecret(c.ctx, two.ID, key); err != nil || s != "" {
		t.Fatalf("instance 2's lane secret for c-9 after the rehome: %q %v; want none", s, err)
	}
	moved, err := c.pg.SessionByKeyAnyAgent(c.ctx, fmt.Sprintf("officer-chat-moved:%d:%s", old.ID, key))
	if err != nil || moved.ID != old.ID || moved.AgentID != two.ID {
		t.Fatalf("the set-aside session: %+v %v; want session %d on instance 2", moved, err, old.ID)
	}
	if msgs, err := c.pg.ListChatMessages(c.ctx, old.ID); err != nil || len(msgs) != 1 || msgs[0].Content != "the old turn" {
		t.Fatalf("the set-aside session's history: %+v %v; want the old turn kept", msgs, err)
	}
	m, err := lanes.Open(c.ctx, c.o, 1, "c-9")
	if err != nil || m != (officers.ChatMarks{}) {
		t.Fatalf("open on 1 after the rehome: %+v %v; want a new session with no marks", m, err)
	}
	if n, err := lanes.Home(c.ctx, c.o, "c-9"); err != nil || n != 1 {
		t.Fatalf("home after the open on 1: %d %v, want 1", n, err)
	}
	// A lane past the chat ceiling (3): its home reads as 5.
	if _, err := lanes.Open(c.ctx, c.o, 5, "c-7"); err != nil {
		t.Fatalf("open on 5: %v", err)
	}
	if n, err := lanes.Home(c.ctx, c.o, "c-7"); err != nil || n != 5 {
		t.Fatalf("a home past the ceiling: %d %v, want 5 (so it is re-homed, not held)", n, err)
	}
	if err := lanes.Rehome(c.ctx, c.o, "c-none", 2); err != nil {
		t.Fatalf("rehome with nothing to move: %v, want none", err)
	}
}
