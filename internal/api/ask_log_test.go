package api

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/asklog"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// TestAReplysProvenanceIsWhatMusterCanProve: the provenance switch, one case
// per source, with literal values. A reply the send queue carried is named by
// the ROW that carried it — the owner's own row is muster_send, a routed row
// route, a nudge row nudge — even when its words look like something else;
// one no row carried is route or nudge by the prefix muster writes, else pane.
// A dispatched agent's message typed into muster's chat is muster_send.
func TestAReplysProvenanceIsWhatMusterCanProve(t *testing.T) {
	sent := []sendqueue.SentBody{
		{Body: "go ahead"},
		{Body: "  ship it\n", RouteID: "rt_0123456789abcdef01234567"},
		{Body: chiefnudge.NudgeLabel + " carry on", ChiefNudgeID: 7},
		{Body: RoutedPrefacePrefix + "a teammate: looks right"}, // the owner's own row, worded like a route
	}
	for _, c := range []struct{ reply, want string }{
		{"go ahead", asklog.ProvMusterSend},
		{"ship it", asklog.ProvRoute},
		{chiefnudge.NudgeLabel + " carry on", asklog.ProvNudge},
		{RoutedPrefacePrefix + "a teammate: looks right", asklog.ProvMusterSend},
		{"typed in the terminal", ""},
	} {
		if got := askLogProvenanceOf(c.reply, sent); got != c.want {
			t.Errorf("askLogProvenanceOf(%q) = %q, want %q", c.reply, got, c.want)
		}
	}
	for _, c := range []struct{ reply, want string }{
		{"yes", asklog.ProvPane},
		{RoutedPrefacePrefix + "the review bot: two findings", asklog.ProvRoute},
		{"<<<routed untrusted nonce=0123456789abcdef count=1>>>", asklog.ProvRoute},
		{chiefnudge.NudgeLabel + " you asked whether to continue: yes", asklog.ProvNudge},
		{"routed to you by nobody (lower case)", asklog.ProvPane},
	} {
		if got := askLogTextProvenance(c.reply); got != c.want {
			t.Errorf("askLogTextProvenance(%q) = %q, want %q", c.reply, got, c.want)
		}
	}
	for _, c := range []struct {
		m    agents.ChatMessage
		want string
	}{
		{agents.ChatMessage{Content: "yes", TypedBy: 1}, asklog.ProvMusterSend},
		{agents.ChatMessage{Content: "yes"}, asklog.ProvPane},
		{agents.ChatMessage{Content: chiefnudge.NudgeLabel + " go on"}, asklog.ProvNudge},
		{agents.ChatMessage{Content: RoutedPrefacePrefix + "a plugin"}, asklog.ProvRoute},
	} {
		if got := askLogAgentProvenance(c.m); got != c.want {
			t.Errorf("askLogAgentProvenance(%+v) = %q, want %q", c.m, got, c.want)
		}
	}
}

// TestTheTurnEndMessageLooksPastRepliesThatArrivedFirst: the turn's message is
// the last assistant message before any trailing user messages; its key names
// the prompt that started the turn, so the same question in two turns is two
// keys and one turn read twice is one.
func TestTheTurnEndMessageLooksPastRepliesThatArrivedFirst(t *testing.T) {
	msgs := []chiefnudge.Msg{{Role: "user", Text: "go", ID: "line:10"}, {Role: "assistant", Text: "Shall I continue?"}}
	m, k1, ok := askLogTurnQuestion(msgs)
	if !ok || m.Text != "Shall I continue?" || k1 != "line:10\x00"+askLogTextHash("Shall I continue?") {
		t.Fatalf("the turn's message: %+v %q %v", m, k1, ok)
	}
	if m, k, ok := askLogTurnQuestion(append(msgs, chiefnudge.Msg{Role: "user", Text: "yes", ID: "line:20"})); !ok || m.Text != "Shall I continue?" || k != k1 {
		t.Fatalf("past a reply: %+v %q %v", m, k, ok)
	}
	again := append(append([]chiefnudge.Msg(nil), msgs...), chiefnudge.Msg{Role: "user", Text: "yes", ID: "line:20"}, chiefnudge.Msg{Role: "assistant", Text: "Shall I continue?"})
	if _, k, ok := askLogTurnQuestion(again); !ok || k == k1 || k != "line:20\x00"+askLogTextHash("Shall I continue?") {
		t.Fatalf("the same words in the next turn: %q %v", k, ok)
	}
	if _, _, ok := askLogTurnQuestion([]chiefnudge.Msg{{Role: "user", Text: "hello"}}); ok {
		t.Fatal("a transcript with no assistant message has a turn-end message")
	}
	if _, _, ok := askLogTurnQuestion(nil); ok {
		t.Fatal("an empty transcript has a turn-end message")
	}
}

// askLogTextHash is the turn key's text part, spelled here from its
// definition (sha256, first 8 bytes, hex) rather than called.
func askLogTextHash(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:8])
}
