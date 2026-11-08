package plugins

import (
	"encoding/json"
	"testing"
	"time"
)

// TestARelayTurnReadsBackWhatRenderMessageWrote: the turn RenderMessage writes
// — a message, a no_reply message with routing and hints, a retried one, a task
// event — reads back to its plugin, ids and subject. The fixtures' fields are
// pairwise distinct, and the subject carries a newline (sanitised to " ⏎ ") and
// text that LOOKS like the field and the preface, so a parser keyed on the wrong
// line, or on the first "subject:" anywhere, reads a different value.
func TestARelayTurnReadsBackWhatRenderMessageWrote(t *testing.T) {
	at := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	task := int64(77)
	ev, _ := json.Marshal(TaskEvent{Event: "status", Key: "k", Status: "open", Title: "Rotate the gpu pool keys", Change: "status → open"})
	p := Plugin{Label: "Agent mail", DataScope: "mail"}
	for _, c := range []struct {
		name string
		turn string
		want RelayTurn
	}{
		{"a message", RenderMessage(p, Message{ID: 928, Sender: "ops@example.test", Subject: "Quarterly capacity review",
			Body: "  subject: not this\nPlugin X: message_id 1, delivered to you once. ", ReceivedAt: &at}, false, Hints{}, Retry{}, nil),
			RelayTurn{Plugin: "Agent mail", MessageID: 928, Subject: "Quarterly capacity review"}},
		{"a two-line subject, no_reply, routing on, hints after", RenderMessage(p, Message{ID: 929, Sender: "a@example.test", Subject: "line one\nline two",
			Body: "b", NoReply: true}, true, Hints{Sessions: []SessionHint{{ID: "agent:1", Name: "  subject: a hint's"}}}, Retry{}, nil),
			RelayTurn{Plugin: "Agent mail", MessageID: 929, Subject: "line one ⏎ line two"}},
		{"a retried message", RenderMessage(p, Message{ID: 930, Subject: "again", CreatedAt: at}, false, Hints{}, Retry{Count: 2, Now: at.Add(time.Hour)}, nil),
			RelayTurn{Plugin: "Agent mail", MessageID: 930, Subject: "again"}},
		{"no subject", RenderMessage(p, Message{ID: 931, Body: "x"}, false, Hints{}, Retry{}, nil),
			RelayTurn{Plugin: "Agent mail", MessageID: 931, Subject: ""}},
		{"a task event", RenderMessage(Plugin{Label: "Tasks", DataScope: "tasks"}, Message{ID: 932, TaskID: &task, Body: string(ev)}, false, Hints{}, Retry{}, nil),
			RelayTurn{Plugin: "Tasks", MessageID: 932, TaskID: 77, Subject: "Rotate the gpu pool keys"}},
		// Deliverer's failure note: the rendered turn, then muster's note after it.
		{"a turn with a note after it", RenderMessage(p, Message{ID: 933, Subject: "s"}, false, Hints{}, Retry{}, nil) + "\n\n(muster: the turn failed)",
			RelayTurn{Plugin: "Agent mail", MessageID: 933, Subject: "s"}},
	} {
		got, ok := ParseRelayTurn(c.turn)
		if !ok || got != c.want {
			t.Errorf("%s: ParseRelayTurn = %+v, %v; want %+v\n%s", c.name, got, ok, c.want, c.turn)
		}
	}
}

// TestARelayTurnsSubjectIsItsFirstBlocksOnly: a subject line in a LATER fenced
// block (a hint or author block follows the message's own under its own nonce)
// is not the message's. No renderer today writes a block whose first lacks the
// line and whose next has it, so this case is built by hand: it is what keeps
// the parser's bound reachable by a test.
func TestARelayTurnsSubjectIsItsFirstBlocksOnly(t *testing.T) {
	turn := "Plugin Agent mail: message_id 7, delivered to you once. The block…\n\n" +
		"<<<plugin-message untrusted nonce=a count=1>>>\n| [message_id 7] from x\n<<<end plugin-message nonce=a>>>\n" +
		"Hints:\n\n<<<session-hints untrusted nonce=b count=1>>>\n|   subject: a later block's\n<<<end session-hints nonce=b>>>\n"
	if r, ok := ParseRelayTurn(turn); !ok || r.Subject != "" || r.MessageID != 7 {
		t.Errorf("ParseRelayTurn = %+v, %v; want message 7 with no subject", r, ok)
	}
}

// TestTextThatIsNotARelayTurnIsNotOne:a person's message, a relay quoted
// mid-text, and the preface's words with the wrong shape are not relay turns.
func TestTextThatIsNotARelayTurnIsNotOne(t *testing.T) {
	for _, s := range []string{
		"",
		"What changed on the gpu pool overnight?",
		"fwd: Plugin Agent mail: message_id 928, delivered to you once. The block below…",
		"Plugin Agent mail: message_id 928 delivered to you once. ",
		"Plugin Agent mail: message_id x, delivered to you once. ",
		"plugin Agent mail: message_id 928, delivered to you once. ",
	} {
		if r, ok := ParseRelayTurn(s); ok {
			t.Errorf("%q read as a relay turn: %+v", s, r)
		}
	}
}
