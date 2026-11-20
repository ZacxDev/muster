package ui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/plugins"
)

// handBackAnswer is the session's answer the fixtures' "From <session>" card
// carries: several words, none of them the collapsed line's.
const handBackAnswer = "Done: each retry is logged now, with its attempt number and the upstream status."

// handBackLog is a chief chat holding a routed request's hand-back (row 10,
// drawn as its card) and the chief's relay turn after it (row 11), whose text
// is relay.
func handBackLog(relay string) AgentDetailView {
	return AgentDetailView{
		Name: "chiefy", UnfoldNewest: true,
		Messages: []ChatLine{
			{ID: 9, Role: "user", Content: "ask heron to log each retry", Typed: true},
			{ID: 10, Role: "user", Content: "[muster] Route rt_0a1b (…): the session answered your owner's request."},
			{ID: 11, Role: "assistant", Content: relay},
		},
		RouteAnswers: map[int64][]RouteAnswerCard{10: {{From: "Billing migration", Body: handBackAnswer}}},
	}
}

// relayBubble is the chief's relay turn as the log draws it: its bubble.
func relayBubble(t *testing.T, v AgentDetailView) string {
	t.Helper()
	html := renderString(t, chatLogInner(v))
	b := regexp.MustCompile(`(?s)<div data-bubble="assistant"[^>]*data-msg-id="11".*?</div>`).FindString(html)
	if b == "" {
		t.Fatalf("no relay bubble:\n%s", html)
	}
	return b
}

// TestARelayThatRepeatsTheAnswerIsCollapsed: the chief's turn after a
// hand-back card that repeats the card's answer — verbatim, re-wrapped,
// blockquoted — draws "See the answer above" in its place; what it adds (a
// next step) is kept; an unrelated reply is untouched. The answer itself is
// still on the card.
func TestARelayThatRepeatsTheAnswerIsCollapsed(t *testing.T) {
	wrapped := strings.Replace(handBackAnswer, " with ", "\n  with ", 1)
	for _, c := range []struct {
		name, relay string
		collapsed   bool
		keep        string
	}{
		{"verbatim", handBackAnswer, true, ""},
		{"re-wrapped", wrapped, true, ""},
		{"quoted, plus a next step", "Heron says:\n\n> " + handBackAnswer + "\n\nNext I'll ask it to add a test.", true, "Next I&#39;ll ask it to add a test."},
		{"in quote marks", "Heron answered: “" + handBackAnswer + "” I'll watch the next deploy.", true, "I&#39;ll watch the next deploy."},
		{"unrelated", "Heron is still working on the retry logging; I'll tell you when it answers.", false, "Heron is still working on the retry logging"},
		{"a near repeat", strings.Replace(handBackAnswer, "attempt", "retry", 1), false, "with its retry number"},
	} {
		b := relayBubble(t, handBackLog(c.relay))
		if got := strings.Contains(b, "See the answer above."); got != c.collapsed {
			t.Errorf("%s: collapsed %v, want %v:\n%s", c.name, got, c.collapsed, b)
		}
		if c.collapsed && strings.Contains(b, "upstream status") {
			t.Errorf("%s: the repeated answer is still drawn:\n%s", c.name, b)
		}
		if c.keep != "" && !strings.Contains(b, c.keep) {
			t.Errorf("%s: %q is gone:\n%s", c.name, c.keep, b)
		}
		if c.collapsed && strings.Contains(b, "&gt;") {
			t.Errorf("%s: a stray quote mark is left:\n%s", c.name, b)
		}
	}
	// The card still carries the answer.
	html := renderString(t, chatLogInner(handBackLog(handBackAnswer)))
	if !regexp.MustCompile(`data-routed-body=""[^>]*>` + regexp.QuoteMeta(handBackAnswer) + `<`).MatchString(html) {
		t.Errorf("the card lost the answer:\n%s", html)
	}
}

// TestOnlyTheTurnAfterTheCardIsDeduped: once the owner writes again, a later
// reply that happens to repeat the answer is the chief's own words again.
func TestOnlyTheTurnAfterTheCardIsDeduped(t *testing.T) {
	v := handBackLog("Noted.")
	v.Messages = append(v.Messages,
		ChatLine{ID: 12, Role: "user", Content: "what did heron say exactly?", Typed: true},
		ChatLine{ID: 13, Role: "assistant", Content: handBackAnswer})
	html := renderString(t, chatLogInner(v))
	b := regexp.MustCompile(`(?s)<div data-bubble="assistant"[^>]*data-msg-id="13".*?</div>`).FindString(html)
	if !strings.Contains(b, "upstream status") || strings.Contains(b, "See the answer above") {
		t.Errorf("a reply after the owner's next message was collapsed:\n%s", b)
	}
}

// TestTheChiefsNewestReplyNeverFolds: in the chief's chat the newest reply's
// bubble carries data-clamp-never (bubble_clamp.go never clamps it) and an
// older long reply does not, so it still folds. Off the chief's chat no bubble
// carries it.
func TestTheChiefsNewestReplyNeverFolds(t *testing.T) {
	long := strings.Repeat("A long line of the chief's answer.\n", 40)
	v := AgentDetailView{Name: "chiefy", UnfoldNewest: true, Messages: []ChatLine{
		{ID: 1, Role: "user", Content: "first", Typed: true},
		{ID: 2, Role: "assistant", Content: "OLDER " + long},
		{ID: 3, Role: "user", Content: "second", Typed: true},
		{ID: 4, Role: "assistant", Content: "NEWEST " + long},
		// A tool row after it, and a sentinel: neither is "the newest reply".
		{ID: 5, Kind: "tool_call", Role: "assistant", ToolID: "t1", ToolName: "fleet_list"},
		{ID: 6, Role: "assistant", Content: "NO_REPLY"},
	}}
	html := renderString(t, chatLogInner(v))
	open := regexp.MustCompile(`<div data-bubble="assistant"[^>]*>`).FindAllString(html, -1)
	if len(open) != 2 {
		t.Fatalf("%d assistant bubbles, want 2:\n%s", len(open), html)
	}
	if strings.Contains(open[0], "data-clamp-never") || !strings.Contains(open[0], `data-msg-id="2"`) {
		t.Errorf("the older reply would not fold: %s", open[0])
	}
	if !strings.Contains(open[1], "data-clamp-never") || !strings.Contains(open[1], `data-msg-id="4"`) {
		t.Errorf("the newest reply would fold: %s", open[1])
	}
	v.UnfoldNewest = false
	if strings.Contains(renderString(t, chatLogInner(v)), "data-clamp-never") {
		t.Error("a chat that is not the chief's unfolds a reply")
	}
	// The clamp script honours the mark before measuring.
	js := renderString(t, bubbleClampScript())
	if !strings.Contains(js, "if (el.hasAttribute('data-clamp-never')) return;") {
		t.Error("the clamp script does not skip a data-clamp-never bubble")
	}
}

// TestTheRoutedCardsCarryTheAppsIconsNotAnEmoji: the "From your chief" card
// carries the chief's hexagon, a "From <session>" card the session icon, and
// neither carries ✉ — nor does a relayed plugin row.
func TestTheRoutedCardsCarryTheAppsIconsNotAnEmoji(t *testing.T) {
	chief := renderString(t, routedPromptCard(plugins.RoutedBlock{Kind: plugins.RoutedKindRequest, Via: plugins.RoutedViaChief,
		RouteID: "rt_1", Body: "Please also log each retry.", OwnerTyped: "log each retry"}, "raw", "p-1", "p-1-info"))
	answer := renderString(t, routeAnswerRow(ChatLine{ID: 7, Content: "raw"}, []RouteAnswerCard{{From: "Billing migration", Body: "Done."}}))
	relay := renderString(t, relayRow(ChatLine{ID: 8, Relay: &ChatRelay{Source: "Agent mail"}, Content: "x"}))
	for name, c := range map[string]struct{ html, icon, body string }{
		"From your chief":    {chief, "chief", clusterIcon.Body},
		"From <session>":     {answer, "session", routedIconBodies[routedIconSession]},
		"relayed plugin row": {relay, "message", routedIconBodies[routedIconMessage]},
	} {
		if strings.Contains(c.html, "✉") {
			t.Errorf("%s: carries ✉:\n%s", name, c.html)
		}
		if !strings.Contains(c.html, `data-routed-icon="`+c.icon+`"`) || !strings.Contains(c.html, c.body) {
			t.Errorf("%s: no %s icon:\n%s", name, c.icon, c.html)
		}
		if !regexp.MustCompile(`data-routed-icon="` + c.icon + `"[^>]*class="[^"]*text-fg2`).MatchString(c.html) {
			t.Errorf("%s: the icon is not in the card icons' colour (text-fg2):\n%s", name, c.html)
		}
	}
	if !strings.Contains(chief, ">From your chief<") {
		t.Errorf("the chief card's heading changed:\n%s", chief)
	}
}
