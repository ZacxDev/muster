package api

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
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
	// Rows 1–6 were written BEFORE migration 0096 (WrittenBeforeRelayLink):
	// only for those is the preface read at all. Row 7 is the same untyped text
	// written AFTER it — a `muster chief ask` or a machine send spelling the
	// preface — and is a bubble: since 0096 only the stored link makes a relay.
	lines := chatLinesOf([]agents.ChatMessage{
		{ID: 1, Role: "user", Content: turn, WrittenBeforeRelayLink: true},
		{ID: 2, Role: "user", Content: turn, Kind: "text", WrittenBeforeRelayLink: true},
		{ID: 3, Role: "user", Content: turn, TypedBy: 1, WrittenBeforeRelayLink: true},
		{ID: 4, Role: "assistant", Content: turn, WrittenBeforeRelayLink: true},
		{ID: 5, Role: "user", Content: turn, Kind: "tool_result", WrittenBeforeRelayLink: true},
		{ID: 6, Role: "user", Content: "What changed overnight?", WrittenBeforeRelayLink: true},
		{ID: 7, Role: "user", Content: turn},
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

// TestTheStoredLinkChoosesTheRelayRowAndTheTextIsOnlyTheFallback: the drawn
// transcript (chatLinesOf → ui.ChatLogBody) draws a row CARRYING a stored
// plugin-message link (chat_messages.plugin_message_id, 0096) as the collapsed
// relay row even though its text does NOT begin with muster's relay preface;
// an owner-typed row whose text DOES read as a relay stays the owner's bubble,
// link or no link; a pre-0096 row (no link) is still read by its preface, and a
// post-0096 row with no link is a bubble whatever its text. The
// fixture texts, ids and plugin labels are pairwise distinct, so a relay drawn
// from the wrong row, or labelled from the wrong field, is seen.
func TestTheStoredLinkChoosesTheRelayRowAndTheTextIsOnlyTheFallback(t *testing.T) {
	at := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	relayText := plugins.RenderMessage(plugins.Plugin{Label: "Ledger inbox", DataScope: "ledger"},
		plugins.Message{ID: 517, Sender: "fin@example.test", Subject: "Invoice batch", Body: "b", ReceivedAt: &at},
		false, plugins.Hints{}, plugins.Retry{}, nil)
	const linkedText = "Weekly digest for the platform team (forwarded by muster)."
	lines := chatLinesOf([]agents.ChatMessage{
		// Linked, text not a relay preface: the link alone makes it a relay row.
		{ID: 101, Role: "user", Content: linkedText, PluginMessageID: 4401},
		// Typed by the owner, relay-looking text: their bubble.
		{ID: 102, Role: "user", Content: relayText, TypedBy: 7},
		// Typed AND (impossibly) linked: still the owner's bubble — typing wins.
		{ID: 103, Role: "user", Content: "Pasted: " + relayText, TypedBy: 7, PluginMessageID: 4402},
		// Pre-0096 (no link), untyped, the preface: the fallback reads it.
		{ID: 104, Role: "user", Content: relayText, WrittenBeforeRelayLink: true},
		// Post-0096, untyped, NO link, the preface spelled (a `muster chief ask`):
		// a bubble — the text no longer decides for a row written since 0096.
		{ID: 106, Role: "user", Content: relayText},
		// Untyped, no link, plain text: a bubble.
		{ID: 105, Role: "user", Content: "Status of the gpu pool?"},
	})
	body, _, err := ui.ChatLogBody(ui.AgentDetailView{Name: "chief", Messages: lines})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	// What each row was drawn as, by its data-msg-id.
	drawn := map[string]string{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			var id, kind string
			for _, a := range n.Attr {
				switch a.Key {
				case "data-msg-id":
					id = a.Val
				case "data-relay-row":
					kind = "relay"
				case "data-bubble":
					kind = "bubble:" + a.Val
				}
			}
			if id != "" && kind != "" {
				if kind == "relay" {
					if _, open := relayAttr(n, "open"); open {
						kind = "relay(open)"
					}
				}
				drawn[id] = kind
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	want := map[string]string{"101": "relay", "102": "bubble:user", "103": "bubble:user", "104": "relay", "105": "bubble:user", "106": "bubble:user"}
	for id, w := range want {
		if drawn[id] != w {
			t.Errorf("row %s drawn as %q, want %q", id, drawn[id], w)
		}
	}
	// The labels: the linked row's text names no plugin, so it says so; the
	// fallback row's preface names its plugin and subject.
	if r := lines[0].Relay; r == nil || r.MessageID != 4401 || r.Source != "" {
		t.Errorf("row 101's relay: %+v; want message 4401, no source read from its text", r)
	}
	if r := lines[3].Relay; r == nil || r.Source != "Ledger inbox" || r.Subject != "Invoice batch" || r.MessageID != 517 {
		t.Errorf("row 104's relay: %+v", r)
	}
	if s := string(body); !strings.Contains(s, "Message from a plugin · #4401") || !strings.Contains(s, "Message from Ledger inbox · Invoice batch") {
		t.Errorf("the relay rows' labels are not drawn:\n%s", s)
	}
}

func relayAttr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}
