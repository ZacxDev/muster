package ui

import (
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/ZacxDev/muster/internal/agents"
)

// chiefPageView is a claude-code agent's page view; chief makes it the chief.
func chiefPageView(chief bool) AgentDetailView {
	return AgentDetailView{
		ID: 7, Name: "chief-cc", DisplayName: "chief", Kind: agents.KindClaudeCode, CCAccount: "beta-acct",
		LastFailover: "from alpha-acct · rate_limited · resets Nov 6 22:00 UTC", WorkerPool: "pool-gpu",
		Status: agents.StatusRunning, ActiveSessionID: 3, AsksSrc: "/attention/asker?from=agent%3Achief-cc",
		Asks: AskerAsksBadgeHTML(3, "/attention?from=agent%3Achief-cc", true),
		Card: AgentCardView{ID: 7, Name: "chief-cc", Status: agents.StatusRunning, Kind: agents.KindClaudeCode, IsChief: chief},
	}
}

func ccFirst(t *testing.T, n *html.Node, key, val string) *html.Node {
	t.Helper()
	got := ccWithAttr(n, key, val)
	if len(got) != 1 {
		t.Fatalf("%d elements with %s=%q, want 1", len(got), key, val)
	}
	return got[0]
}

// ccHasControl reports whether n is or holds a button, link or label.
func ccHasControl(n *html.Node) bool {
	if n.Type == html.ElementNode && (n.Data == "button" || n.Data == "a" || n.Data == "label") {
		return true
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if ccHasControl(c) {
			return true
		}
	}
	return false
}

func ccInside(n, ancestor *html.Node) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p == ancestor {
			return true
		}
	}
	return false
}

// TestEveryClaudeCodeAgentsPageFoldsItsStatusRowIntoTheMenu: a claude-code
// agent's header — the chief's AND a dispatched agent's (operator decision on
// PR #527) — is ONE row holding the status dot, the asks badge and the ⋯; the
// account, the failover with its whole line, the pool and the Terminal link are
// IN the ⋯ menu; no Rendered | Terminal switch is drawn; and there is no asks
// block, only the header's badge. A GATEWAY agent keeps its second row (the
// control, TestAGatewayAgentKeepsItsHeader).
func TestEveryClaudeCodeAgentsPageFoldsItsStatusRowIntoTheMenu(t *testing.T) {
	for _, chief := range []bool{true, false} {
		t.Run(map[bool]string{true: "the chief", false: "a dispatched agent"}[chief], func(t *testing.T) { foldedPageCase(t, chief) })
	}
}

func foldedPageCase(t *testing.T, chief bool) {
	doc := ccParse(t, renderString(t, AgentDetailPage(chiefPageView(chief), Features{})))
	header := ccFirst(t, doc, "data-folded-header", "")
	menu := ccFirst(t, doc, "id", AgentMenuID)
	row := header.FirstChild
	for _, want := range []struct{ key, val string }{{"id", "agent-status"}, {"id", AskerAsksBadgeID}, {"id", AgentMenuID + "-open"}, {"data-header-tools", ""}} {
		if n := ccFirst(t, doc, want.key, want.val); n.Parent == nil || !ccInside(n, row) {
			t.Errorf("%s=%q is not in the header's one row", want.key, want.val)
		}
	}
	// ONE child of the header holds controls: the row. (The others are the view
	// frame's script and the Updating… chip.)
	rows := 0
	for c := header.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data != "script" && ccHasControl(c) {
			rows++
		}
	}
	if rows != 1 {
		t.Errorf("%d header rows hold controls, want 1", rows)
	}
	for _, key := range []string{"data-cc-account", "data-failover", "data-failover-detail", "data-worker-pool"} {
		if n := ccFirst(t, doc, key, ""); !ccInside(n, menu) {
			t.Errorf("%s is not in the ⋯ menu", key)
		}
	}
	if got := ccText(ccFirst(t, doc, "data-failover-detail", "")); got != "from alpha-acct · rate_limited · resets Nov 6 22:00 UTC" {
		t.Errorf("the failover line in the menu: %q", got)
	}
	if got := ccText(ccFirst(t, doc, "data-cc-account", "")); got != "beta-acct" {
		t.Errorf("the account in the menu: %q", got)
	}
	term := ccFirst(t, doc, "id", "view-terminal")
	if !ccInside(term, menu) {
		t.Error("Terminal is not in the ⋯ menu")
	}
	if href, _ := ccAttr(term, "href"); href != AgentTerminalPath("chief-cc") {
		t.Errorf("Terminal's href %q", href)
	}
	if v, _ := ccAttr(term, "data-view-swap"); v != ViewTerminal {
		t.Errorf("Terminal is not a view-swap link: %q", v)
	}
	if n := len(ccWithAttr(doc, "id", "view-rendered")); n != 0 {
		t.Errorf("%d Rendered segments: the switch is drawn", n)
	}
	// The menu is right-aligned to a trigger at its row's end.
	if _, ok := ccAttr(menu, "data-menu-align"); ok {
		t.Error("the folded menu is aligned to its trigger's start")
	}
	// The tools are gap-1 (agentHeaderToolsWith's compact row): a worker's
	// three — New chat, Chat history, What it worked on — in the row, none of
	// them in the ⋯ menu (operator ask: the history buttons back on the page),
	// so three controls wide; the chief's list icon alone, one control wide.
	v := chiefPageView(chief)
	v.FocusHistory = FocusHistoryPath("agent:7", "")
	vdoc := ccParse(t, renderString(t, AgentDetailPage(v, Features{})))
	tools := ccFirst(t, vdoc, "data-header-tools", "")
	wide := " w-[6.75rem] "
	if v.ChiefChat != nil {
		wide = " w-11 "
	}
	if cls, _ := ccAttr(tools, "class"); !strings.Contains(" "+cls+" ", " gap-1 ") || !strings.Contains(" "+cls+" ", wide) {
		t.Errorf("tools: class %q, want gap-1 and%s", cls, wide)
	}
	vmenu := ccFirst(t, vdoc, "id", AgentMenuID)
	hist, focusOpen := ccWithAttr(tools, "aria-label", "Chat history"), ccWithAttr(vdoc, "id", "focus-history-open")
	if n := len(ccWithAttr(vmenu, "aria-label", "Chat history")) + len(ccWithAttr(vmenu, "for", sessionDrawerID)); n != 0 {
		t.Errorf("the ⋯ menu holds %d Chat history controls", n)
	}
	if v.ChiefChat != nil {
		if len(hist)+len(focusOpen) != 0 {
			t.Errorf("the chief's page carries Chat history (%d) or What it worked on (%d)", len(hist), len(focusOpen))
		}
	} else {
		if len(hist) != 1 || !ccInside(hist[0], tools) || len(focusOpen) != 1 || !ccInside(focusOpen[0], tools) {
			t.Errorf("a worker's Chat history (%d) and What it worked on (%d) are not the header's tools", len(hist), len(focusOpen))
		}
		if len(hist) == 1 && ccInside(hist[0], vmenu) || len(focusOpen) == 1 && ccInside(focusOpen[0], vmenu) {
			t.Error("a history control is still in the ⋯ menu")
		}
	}
	// The ⋯ keeps its other actions: Rename, Start and Stop, Terminal.
	for _, k := range []string{"rename", "Start", "Stop"} {
		if n := len(ccWithAttr(vmenu, "data-agent-action", k)); n != 1 {
			t.Errorf("the ⋯ has %d %q items, want 1", n, k)
		}
	}
	if term := ccWithAttr(vdoc, "id", "view-terminal"); len(term) != 1 || !ccInside(term[0], vmenu) {
		t.Error("Terminal is not the ⋯ menu's")
	}
	// The asks are the header's badge alone: one link, in the header, to the
	// Asks page filtered to this agent's asks — and no asks block anywhere.
	badgeInHeader(t, doc, header, "/attention?from=agent%3Achief-cc", "3")
	// Delete follows the chief guard, not the fold (the positive control that
	// the two cases really are the chief and not).
	if got := len(ccWithAttr(menu, "data-agent-action", "delete")); got != map[bool]int{true: 0, false: 1}[chief] {
		t.Errorf("chief %v: %d Delete items", chief, got)
	}
}

// TestAGatewayAgentKeepsItsHeader: an agent that is not claude-code — the chief
// or a dispatched one — has no account, failover or terminal to fold, and its
// second row is the model picker: it keeps the page header, its ⋯ at that
// row's start and — as every agent page — its asks badge in its header.
func TestAGatewayAgentKeepsItsHeader(t *testing.T) {
	for _, chief := range []bool{true, false} {
		v := chiefPageView(chief)
		v.Kind, v.Card.Kind = agents.KindGateway, agents.KindGateway
		doc := ccParse(t, renderString(t, AgentDetailPage(v, Features{})))
		if n := len(ccWithAttr(doc, "data-folded-header", "")); n != 0 {
			t.Errorf("chief %v: a gateway agent drew the folded header", chief)
		}
		menu := ccFirst(t, doc, "id", AgentMenuID)
		if a, _ := ccAttr(menu, "data-menu-align"); a != "start" {
			t.Errorf("chief %v: a gateway agent's menu align %q", chief, a)
		}
		if n := len(ccWithAttr(menu, "data-cc-account", "")) + len(ccWithAttr(menu, "id", "view-terminal")); n != 0 {
			t.Errorf("chief %v: a gateway agent's menu holds claude-code items", chief)
		}
		badgeInHeader(t, doc, ccFirst(t, doc, "data-agent-title", "").Parent.Parent, "/attention?from=agent%3Achief-cc", "3")
	}
}

// badgeInHeader: doc draws exactly one asks badge, inside header, a link to
// href saying n — and no asks block (the element the removed block drew, or
// its chip).
func badgeInHeader(t *testing.T, doc, header *html.Node, href, n string) {
	t.Helper()
	links := ccWithAttr(doc, "data-asker-asks-link", "")
	if len(links) != 1 {
		t.Fatalf("%d asks badges, want 1", len(links))
	}
	if !ccInside(links[0], header) {
		t.Error("the asks badge is not in the header")
	}
	if got, _ := ccAttr(links[0], "href"); got != href {
		t.Errorf("the badge links to %q, want %q", got, href)
	}
	if got := ccText(links[0]); got != n {
		t.Errorf("the badge says %q, want %q", got, n)
	}
	for _, id := range []string{"asker-asks", "asker-asks-chip"} {
		if got := len(ccWithAttr(doc, "id", id)); got != 0 {
			t.Errorf("the page draws #%s (the removed asks block)", id)
		}
	}
}

// TestARelayLineIsACollapsedSystemRow: a line carrying a Relay is drawn as a
// CLOSED <details> naming its plugin and subject (a task event says so), with
// the whole stored turn inside — never as a bubble; the person's line beside it
// stays their bubble.
func TestARelayLineIsACollapsedSystemRow(t *testing.T) {
	const turn = "Plugin Mail-X: message_id 5, delivered to you once. The block…\n\n<<<plugin-message untrusted nonce=n count=1>>>\n|   subject: Budget Q4\n<<<end plugin-message nonce=n>>>\n"
	doc := ccRenderLog(t, AgentDetailView{Name: "chief", Messages: []ChatLine{
		{Role: "user", Content: turn, ID: 11, Relay: &ChatRelay{Source: "Mail-X", Subject: "Budget Q4"}},
		{Role: "user", Content: "Tasks: task event", ID: 12, Relay: &ChatRelay{Source: "Tasks-Y", Subject: "Rotate keys", Task: true}},
		{Role: "user", Content: "What changed overnight?", ID: 13, Typed: true},
		{Role: "user", Content: "no subject here", ID: 14, Relay: &ChatRelay{Source: "Mail-Z"}},
	}})
	rows := ccWithAttr(doc, "data-relay-row", "")
	if len(rows) != 3 {
		t.Fatalf("%d relay rows, want 3", len(rows))
	}
	for i, want := range []string{"Message from Mail-X · Budget Q4", "Task event from Tasks-Y · Rotate keys", "Message from Mail-Z"} {
		r := rows[i]
		if r.Data != "details" {
			t.Errorf("row %d is a <%s>", i, r.Data)
		}
		if _, open := ccAttr(r, "open"); open {
			t.Errorf("row %d is drawn open", i)
		}
		if got := ccText(ccFirst(t, r, "data-relay-label", "")); got != want {
			t.Errorf("row %d says %q, want %q", i, got, want)
		}
		if len(ccWithAttr(r, "data-bubble", "")) != 0 {
			t.Errorf("row %d holds a bubble", i)
		}
	}
	if !strings.Contains(ccText(rows[0]), "delivered to you once") {
		t.Error("the row does not hold the whole stored turn")
	}
	if id, _ := ccAttr(rows[0], "data-msg-id"); id != "11" {
		t.Errorf("row's data-msg-id %q", id)
	}
	bubbles := ccWithAttr(doc, "data-bubble", "user")
	if len(bubbles) != 1 || ccText(bubbles[0]) != "What changed overnight?" {
		t.Errorf("the person's line is not the one user bubble: %d bubbles", len(bubbles))
	}
}
