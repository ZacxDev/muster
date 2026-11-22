package ui

import (
	"html"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// A SESSION'S ASKS IN ITS OPEN PANEL (plugin_chip.go): the Open button counts
// the session's OPEN asks with its previews and pages, is drawn for a session
// whose asks are all answered (bare), and the panel's Asks tab lists them.

// TestTheOpenButtonCountsOpenAsks: previews, pages and open asks are one
// count (pairwise distinct inputs: a count missing any one cannot pass); an
// answered ask is named, not counted; a session with only answered asks
// draws the button with no count. RED before the Asks tab: the count was 7
// and the button with answered asks alone was nil.
func TestTheOpenButtonCountsOpenAsks(t *testing.T) {
	b := renderString(t, OpenButton(OpenCounts{Previews: mirror.WindowPreviews{Offered: 2, Open: 1}, Pages: 4, Asks: 5, AsksListed: 11}))
	for _, want := range []string{
		`data-preview-count="12"`, `data-open-previews="3"`, `data-open-pages="4"`, `data-open-asks="5"`, `data-open-asks-listed="11"`,
		`aria-label="Open: 1 preview open, 2 servers to preview, 4 pages, 5 open asks, 6 answered asks"`,
		`>12</span>`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("the button lacks %s:\n%s", want, b)
		}
	}
	one := renderString(t, OpenButton(OpenCounts{Asks: 1, AsksListed: 1}))
	if !strings.Contains(one, `data-preview-count="1"`) || !strings.Contains(one, `aria-label="Open: 1 open ask"`) {
		t.Errorf("one open ask: %s", one)
	}
	bare := renderString(t, OpenButton(OpenCounts{AsksListed: 3}))
	if !strings.Contains(bare, `id="preview-toggle"`) || !strings.Contains(bare, `aria-label="Open: 3 answered asks"`) || strings.Contains(bare, "data-open-badge") {
		t.Errorf("answered asks alone: a bare button expected: %s", bare)
	}
	if OpenButton(OpenCounts{}) != nil {
		t.Error("a button with nothing to open")
	}
}

// TestThePanelHasAnAsksTabOnlyWhileAsksAreListed: with the queue served
// (AsksOn) and pages off and no viewer plugin, the slot and the panel are
// drawn, the panel with an Asks tab reading the window's (or the agent's)
// list and counting the open ones; without AsksOn there is no Asks tab.
func TestThePanelHasAnAsksTabOnlyWhileAsksAreListed(t *testing.T) {
	c := &PluginChips{Host: "ws", Session: "main", Window: "@1", AsksOn: true, Asks: 2, AsksListed: 3, Viewer: 9}
	page := renderString(t, MirrorViewPage(MirrorViewView{Host: "ws", Session: "main", Window: "@1", Plugins: c}))
	for _, want := range []string{
		`id="preview-slot"`, `id="preview-toggle"`, `data-preview-count="2"`, `id="preview-panel"`,
		`data-asks-src="` + SessionAsksPath + `?host=ws&amp;session=main&amp;window=%401"`,
		`data-tab="asks"`, `data-open-count="asks"`, `>(2)</span>`, `data-session-asks-list=""`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
	agent := renderString(t, previewPanel(&PluginChips{AsksOn: true, Agent: "fixer one", Viewer: 9}))
	if !strings.Contains(agent, `data-asks-src="`+AgentSessionAsksPath("fixer one")+`"`) || AgentSessionAsksPath("fixer one") != "/ui/agents/fixer%20one/session-asks" {
		t.Errorf("the agent's Asks tab: %s", AgentSessionAsksPath("fixer one"))
	}
	off := renderString(t, previewPanel(&PluginChips{Host: "ws", Session: "main", Window: "@1", PagesOn: true, Pages: 1}))
	if strings.Contains(off, `data-tab="asks"`) || strings.Contains(off, `data-asks-src="`) {
		t.Error("an Asks tab without AsksOn")
	}
	if previewPanel(&PluginChips{}) != nil || previewButtonSlot(&PluginChips{}) != nil {
		t.Error("a panel or slot with nothing on")
	}
}

// TestTheAsksTabListsEachAskLinkedToItsPage: each ask links to its own page
// (a full navigation), the open one saying it waits, the answered one showing
// its answer as text; one with no page is listed unlinked; none is the empty
// line. The question is plained, the answer kept as typed, both escaped.
func TestTheAsksTabListsEachAskLinkedToItsPage(t *testing.T) {
	list := renderString(t, SessionAsksList([]SessionAsk{
		{ID: 41, Question: "Ship **it** now?", Open: true, When: "today", Page: "/attention/asks/41"},
		{ID: 17, Question: "Which branch?", Answer: `the <b>main</b> one & "its" tag`, When: "yesterday", Page: "/attention/asks/17"},
		{ID: 9, Question: "No page for this one", Answer: "ok"},
	}, ""))
	doc := parseFragment(t, list)
	rows := allAttr(doc, "data-session-ask")
	if len(rows) != 3 {
		t.Fatalf("%d rows, want 3:\n%s", len(rows), list)
	}
	open, answered, unlinked := rows[0], rows[1], rows[2]
	if open.Data != "a" || attrOf(open, "href") != "/attention/asks/41" || attrOf(open, "hx-boost") != "false" || attrOf(open, "data-session-ask") != "41" {
		t.Errorf("the open ask's row: <%s href=%q hx-boost=%q>", open.Data, attrOf(open, "href"), attrOf(open, "hx-boost"))
	}
	if got := focusNodeText(findAttr(open, "data-session-ask-question")); got != "Ship it now?" {
		t.Errorf("the question %q (plained expected)", got)
	}
	if got := focusNodeText(findAttr(open, "data-session-ask-state")); got != "Waiting on your answer" || findAttr(open, "data-session-ask-answer") != nil {
		t.Errorf("the open ask's state %q", got)
	}
	if answered.Data != "a" || attrOf(answered, "href") != "/attention/asks/17" {
		t.Errorf("the answered ask's row: <%s href=%q>", answered.Data, attrOf(answered, "href"))
	}
	if got := focusNodeText(findAttr(answered, "data-session-ask-answer")); got != `the <b>main</b> one & "its" tag` {
		t.Errorf("the answer reads %q (as typed expected)", got)
	}
	if strings.Contains(list, "<b>main") || !strings.Contains(list, "&lt;b&gt;main&lt;/b&gt;") {
		t.Error("the answer was written as markup")
	}
	if got := focusNodeText(findAttr(answered, "data-session-ask-state")); got != "Answered" {
		t.Errorf("the answered ask's state %q", got)
	}
	if unlinked.Data != "div" || attrOf(unlinked, "href") != "" {
		t.Errorf("an ask with no page is a <%s href=%q>, want an unlinked row", unlinked.Data, attrOf(unlinked, "href"))
	}
	if none := renderString(t, SessionAsksList(nil, "")); !strings.Contains(none, SessionAsksNone) || strings.Contains(none, "data-session-ask=") {
		t.Errorf("no asks: %s", none)
	}
	if strings.Contains(list, "data-session-asks-more") {
		t.Error("a More line with no more to see")
	}
	// More than the tab lists: a last line to the Asks page filtered to the
	// session, a full navigation.
	more := parseFragment(t, renderString(t, SessionAsksList([]SessionAsk{{ID: 1, Question: "q", Open: true, Page: "/attention/asks/1"}}, "/attention?from=agent%3Ax")))
	m := findAttr(more, "data-session-asks-more")
	if m == nil || m.Data != "a" || attrOf(m, "href") != "/attention?from=agent%3Ax" || attrOf(m, "hx-boost") != "false" || focusNodeText(m) != SessionAsksMore {
		t.Errorf("the More line: %v", m)
	}
}

// TestAnAgentsAsksBadgeIsNotDrawnOnAPhoneWhileTheOpenButtonListsItsAsks: the
// header's asks badge is max-sm:hidden while the Open control lists every ask
// the badge counts (agentAsksBadge: AsksOn and AsksCoverBadge, a personal
// agent of the viewer's), and drawn at every width otherwise — a team
// agent's (AsksCoverBadge false) included.
func TestAnAgentsAsksBadgeIsNotDrawnOnAPhoneWhileTheOpenButtonListsItsAsks(t *testing.T) {
	inner := AskerAsksBadgeHTML(2, "/attention?from=agent%3Ax", false)
	if got := renderString(t, agentAsksBadge(inner, &PluginChips{AsksOn: true, AsksCoverBadge: true, Agent: "x"})); !strings.Contains(got, `class="`+html.EscapeString(AskerAsksSlotClass)+` max-sm:hidden"`) {
		t.Errorf("with the Asks tab: %s", got)
	}
	for name, c := range map[string]*PluginChips{"no Open control": nil, "pages only": {PagesOn: true, Agent: "x"},
		"a team agent's": {AsksOn: true, Agent: "x"}} {
		if got := renderString(t, agentAsksBadge(inner, c)); !strings.Contains(got, `class="`+html.EscapeString(AskerAsksSlotClass)+`"`) {
			t.Errorf("%s: %s", name, got)
		}
	}
}

// TestTheMirroredViewsHeaderHasNoMenuAndNoHome: the Rendered view's header
// draws the message index as its own button and no ⋯ menu, no Home. RED
// before: the index was the ⋯ menu's item (role=menuitem) beside Home.
func TestTheMirroredViewsHeaderHasNoMenuAndNoHome(t *testing.T) {
	page := renderString(t, MirrorViewPage(MirrorViewView{Host: "ws", Session: "main", Window: "@1"}))
	doc := parseFragment(t, page)
	for _, id := range []string{"mv-menu", "mv-menu-open", "mv-back"} {
		if byID(doc, id) != nil {
			t.Errorf("the view draws #%s", id)
		}
	}
	idx := byID(doc, "chat-index-open")
	if idx == nil || !insideTag(idx, "header") || attrOf(idx, "role") == "menuitem" || attrOf(idx, "aria-label") != chatIndexLabel || attrOf(idx, "aria-controls") != "chat-index" {
		t.Fatalf("the message index is not the header's own button: %v", idx)
	}
	for p := idx.Parent; p != nil; p = p.Parent {
		if hasAttr(p, "popover") {
			t.Fatal("the message index is in a popover")
		}
	}
	if strings.Contains(page, `aria-label="More actions"`) {
		t.Error("the view still has a More actions control")
	}
}
