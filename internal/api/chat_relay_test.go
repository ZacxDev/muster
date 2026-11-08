package api

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/plugins"
)

// TestOnlyAnUntypedUserRowIsARelay: chatLinesOf marks a row as a plugin relay
// (ui.ChatLine.Relay, drawn as a collapsed system row) only when it is a USER
// text row nobody typed AND its text is muster's relay preface. The same text
// typed by the owner (pasted into the chat), as an assistant's text, or as a
// tool result is not one; an untyped row of other text is not one either.
func TestOnlyAnUntypedUserRowIsARelay(t *testing.T) {
	at := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	turn := plugins.RenderMessage(plugins.Plugin{Label: "Agent mail", DataScope: "mail"},
		plugins.Message{ID: 928, Sender: "ops@example.test", Subject: "Quarterly capacity review", Body: "b", ReceivedAt: &at},
		false, plugins.Hints{}, plugins.Retry{}, nil)
	lines := chatLinesOf([]agents.ChatMessage{
		{ID: 1, Role: "user", Content: turn},
		{ID: 2, Role: "user", Content: turn, Kind: "text"},
		{ID: 3, Role: "user", Content: turn, TypedBy: 1},
		{ID: 4, Role: "assistant", Content: turn},
		{ID: 5, Role: "user", Content: turn, Kind: "tool_result"},
		{ID: 6, Role: "user", Content: "What changed overnight?"},
	})
	want := map[int64]bool{1: true, 2: true}
	for _, l := range lines {
		if got := l.Relay != nil; got != want[l.ID] {
			t.Errorf("row %d: relay %v, want %v", l.ID, got, want[l.ID])
		}
	}
	if r := lines[0].Relay; r == nil || r.Source != "Agent mail" || r.Subject != "Quarterly capacity review" || r.Task {
		t.Errorf("row 1's relay: %+v", r)
	}
	if !lines[2].Typed {
		t.Error("the typed row lost its Typed mark")
	}
}
