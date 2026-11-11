package ui

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/opencodehost"
)

// bubbleTagOf is the opening tag of the [data-bubble] element whose body holds
// text — the nearest data-bubble before text's first occurrence — or "" when
// text is in no bubble (a relay row, say).
func bubbleTagOf(t *testing.T, html, text string) string {
	t.Helper()
	// As drawn TEXT (after a tag's ">"), not inside an attribute: a mirrored
	// prompt's bubble also carries its words in data-prompt.
	i := strings.Index(html, ">"+text)
	if i < 0 {
		t.Fatalf("%q is not drawn at all:\n%s", text, html)
	}
	j := strings.LastIndex(html[:i], `data-bubble="`)
	if j < 0 {
		return ""
	}
	start := strings.LastIndex(html[:j], "<")
	end := strings.Index(html[j:], ">")
	tag := html[start : j+end+1]
	// A relay row between that bubble and text holds text instead: a marker
	// in no bubble must not borrow an earlier bubble's tag.
	if strings.Contains(html[j+end+1:i], `data-relay-row`) {
		return ""
	}
	return tag
}

const tailAttr = `data-clamp-from="tail"`

// checkDirections asserts, for html, that every marker in tails sits in a
// bubble drawn tail-first, and every marker in heads in a bubble with NO
// direction (head-first, the default).
func checkDirections(t *testing.T, surface, html string, tails, heads []string) {
	t.Helper()
	for _, m := range tails {
		tag := bubbleTagOf(t, html, m)
		if !strings.Contains(tag, `data-bubble="assistant"`) || !strings.Contains(tag, tailAttr) {
			t.Errorf("%s: the agent reply %q is not a tail-first bubble: %s", surface, m, tag)
		}
	}
	for _, m := range heads {
		tag := bubbleTagOf(t, html, m)
		if tag == "" {
			t.Errorf("%s: %q is in no bubble", surface, m)
			continue
		}
		if strings.Contains(tag, "data-clamp-from") {
			t.Errorf("%s: %q is not head-first: %s", surface, m, tag)
		}
	}
}

// 🔴 AN AGENT'S REPLY IS CLAMPED FROM ITS TAIL ON EVERY SURFACE, AND NOTHING
// ELSE IS. The direction is the server's, from the ROLE (clampFrom): the agent
// chat page and the chief panel (chatLogInner's chatBubble), the mirrored
// Claude Code view and the mirrored opencode view (MirrorTranscript.Ops) all
// draw a reply data-clamp-from="tail"; the reader's own message, a kickoff (the
// first user turn) and a relayed plugin message (a relay row, no bubble at all)
// are not.
func TestAnAgentReplyIsClampedFromItsTailOnEverySurface(t *testing.T) {
	msgs := []ChatLine{
		{Role: "user", Content: "KICKOFF-MARK start the work", ID: 1},
		{Role: "assistant", Content: "REPLY-ONE-MARK done", ID: 2},
		{Role: "user", Content: "RELAY-MARK relay body", ID: 3, Relay: &ChatRelay{Source: "mail", Subject: "s", MessageID: 9}},
		{Role: "user", Content: "OWNER-MARK what next?", ID: 4},
		{Role: "assistant", Content: "REPLY-TWO-MARK this", ID: 5},
	}
	tails := []string{"REPLY-ONE-MARK", "REPLY-TWO-MARK"}
	heads := []string{"KICKOFF-MARK", "OWNER-MARK"}

	// The agent chat page's transcript.
	page := render(chatLogInner(AgentDetailView{Name: "a", Status: "running", Messages: msgs}))
	checkDirections(t, "agent chat", page, tails, heads)
	if tag := bubbleTagOf(t, page, "RELAY-MARK"); tag != "" {
		t.Errorf("agent chat: the relayed plugin message is drawn as a bubble: %s", tag)
	}
	if n := strings.Count(page, tailAttr); n != len(tails) {
		t.Errorf("agent chat: %d tail-first bubbles, want %d (the replies only):\n%s", n, len(tails), page)
	}

	// The chief panel's body: the same transcript, on the panel surface.
	cv := sampleAgentDetailView()
	cv.Surface = ChatSurfaceChiefPanel
	cv.Messages = msgs
	var b strings.Builder
	if err := RenderChiefPanelBody(&b, ChiefPanelView{Configured: true, Chat: cv}); err != nil {
		t.Fatal(err)
	}
	panel := b.String()
	checkDirections(t, "chief panel", panel, tails, heads)
	if tag := bubbleTagOf(t, panel, "RELAY-MARK"); tag != "" {
		t.Errorf("chief panel: the relayed plugin message is drawn as a bubble: %s", tag)
	}

	// The mirrored Claude Code view.
	var cc MirrorTranscript
	ccHTML := ""
	for _, op := range cc.Ops([]cctranscript.Event{
		{Kind: cctranscript.KindPrompt, Text: "CC-PROMPT-MARK go", Line: 1},
		{Kind: cctranscript.KindText, Text: "CC-REPLY-MARK went"},
	}) {
		ccHTML += op.HTML
	}
	checkDirections(t, "mirrored Claude Code", ccHTML, []string{"CC-REPLY-MARK"}, []string{"CC-PROMPT-MARK"})

	// The mirrored opencode view: opencode's messages become the same events.
	var evs []cctranscript.Event
	for _, m := range []opencodehost.Message{
		{Type: "user", Text: "OC-PROMPT-MARK go"},
		{Type: "assistant", Finish: "stop", Content: []opencodehost.Content{{Type: "text", Text: "OC-REPLY-MARK went"}}},
	} {
		for _, k := range opencodehost.Render(m) {
			evs = append(evs, k.Event)
		}
	}
	var oc MirrorTranscript
	ocHTML := ""
	for _, op := range oc.Ops(evs) {
		ocHTML += op.HTML
	}
	checkDirections(t, "mirrored opencode", ocHTML, []string{"OC-REPLY-MARK"}, []string{"OC-PROMPT-MARK"})

	// The agent chat's live bubble() (agentChatScript) spells clampFrom's rule.
	var full strings.Builder
	if err := RenderAgentDetail(&full, sampleAgentDetailView(), featuresAllOn); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full.String(), "if (role === 'assistant') d.setAttribute('data-clamp-from', 'tail');") {
		t.Error("the agent chat's live bubble() does not draw an assistant bubble tail-first")
	}
}

// THE BOTTOM-ALIGNED LOGS' ONE COUPLING (input.css, "Chat logs are
// bottom-aligned"): the ::before item's negative margin-top cancels the log's
// gap, so it must be the gap both logs are drawn with — gap-3, 0.75rem. A log
// whose gap changed alone would gain (or lose) the difference above its first
// message in a full transcript. The behaviour itself is tail-clamp.spec.ts's.
func TestTheChatLogsGapMatchesTheBottomAlignRule(t *testing.T) {
	css, err := os.ReadFile("../../web/css/input.css")
	if err != nil {
		t.Fatal(err)
	}
	rule := regexp.MustCompile(`#mv-log::before,\s*#chat-log::before\s*\{([^}]*)\}`).FindSubmatch(css)
	if rule == nil {
		t.Fatal("input.css has no #mv-log::before, #chat-log::before rule")
	}
	for _, want := range []string{"margin-top: -0.75rem;", "margin-bottom: auto;"} {
		if !strings.Contains(string(rule[1]), want) {
			t.Errorf("the bottom-align rule lacks %q: %s", want, rule[1])
		}
	}
	var page strings.Builder
	if err := RenderAgentDetail(&page, sampleAgentDetailView(), featuresAllOn); err != nil {
		t.Fatal(err)
	}
	logs := map[string]string{
		"#mv-log":   openTag(t, renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main"})), `id="mv-log"`),
		"#chat-log": openTag(t, page.String(), `id="chat-log"`),
	}
	for name, tag := range logs {
		classes := regexp.MustCompile(`class="([^"]*)"`).FindStringSubmatch(tag)
		if classes == nil || !strings.Contains(" "+classes[1]+" ", " gap-3 ") || !strings.Contains(" "+classes[1]+" ", " flex-col ") {
			t.Errorf("%s is not a gap-3 flex column, which the bottom-align rule assumes: %s", name, tag)
		}
	}
}
