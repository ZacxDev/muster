package ui

import (
	"strconv"
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
		Status: agents.StatusRunning, ActiveSessionID: 3, AsksSrc: "/ui/attention/asker?agent=7",
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

// TestTheChiefsPageFoldsItsStatusRowIntoTheMenu: the claude-code chief's header
// is ONE row holding the status dot, the asks badge and the ⋯; the account, the
// failover with its whole line, the pool and the Terminal link are IN the ⋯
// menu; no Rendered | Terminal switch is drawn; and the asks block comes after
// the transcript. The same agent NOT the chief keeps its second row (the
// control): the fold is the chief's alone.
func TestTheChiefsPageFoldsItsStatusRowIntoTheMenu(t *testing.T) {
	doc := ccParse(t, renderString(t, AgentDetailPage(chiefPageView(true), Features{})))
	header := ccFirst(t, doc, "data-chief-header", "")
	menu := ccFirst(t, doc, "id", AgentMenuID)
	row := header.FirstChild
	for _, want := range []struct{ key, val string }{{"id", "agent-status"}, {"id", AskerAsksID + "-chip"}, {"id", AgentMenuID + "-open"}, {"data-header-tools", ""}} {
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
		t.Error("the chief's menu is aligned to its trigger's start")
	}
	// The tools are gap-1 and their box is their own width: three 2.75rem
	// controls, two 0.25rem gaps (agentHeaderToolsWidth's compact arithmetic).
	v := chiefPageView(true)
	v.FocusHistory = FocusHistoryPath("agent:7", "")
	for n, d := range map[int]AgentDetailView{2: chiefPageView(true), 3: v} {
		tools := ccFirst(t, ccParse(t, renderString(t, AgentDetailPage(d, Features{}))), "data-header-tools", "")
		cls, _ := ccAttr(tools, "class")
		want := "w-[" + strconv.FormatFloat(float64(n)*2.75+float64(n-1)*0.25, 'f', -1, 64) + "rem]"
		if !strings.Contains(" "+cls+" ", " gap-1 ") || !strings.Contains(" "+cls+" ", " "+want+" ") {
			t.Errorf("%d tools: class %q, want gap-1 and %s", n, cls, want)
		}
	}
	// The badge is the chip: there is one, in the header.
	if n := len(ccWithAttr(doc, "id", AskerAsksID+"-chip")); n != 1 {
		t.Errorf("%d asks chips", n)
	}
	page := renderString(t, AgentDetailPage(chiefPageView(true), Features{}))
	if log, asks, form := strings.Index(page, `id="chat-log"`), strings.Index(page, `id="`+AskerAsksID+`"`), strings.Index(page, `id="chat-form"`); !(log < asks && asks < form) {
		t.Errorf("the asks are not between the transcript (%d) and the composer (%d): %d", log, form, asks)
	}

	// The control: the same agent, not the chief.
	plain := ccParse(t, renderString(t, AgentDetailPage(chiefPageView(false), Features{})))
	if n := len(ccWithAttr(plain, "data-chief-header", "")); n != 0 {
		t.Fatal("a non-chief claude-code agent drew the chief's header")
	}
	pmenu := ccFirst(t, plain, "id", AgentMenuID)
	if ccInside(ccFirst(t, plain, "data-cc-account", ""), pmenu) || ccInside(ccFirst(t, plain, "id", "view-terminal"), pmenu) {
		t.Error("a non-chief agent's account or Terminal moved into its menu")
	}
	if len(ccWithAttr(plain, "id", "view-rendered")) != 1 {
		t.Error("a non-chief agent lost its Rendered | Terminal switch")
	}
	if a, _ := ccAttr(pmenu, "data-menu-align"); a != "start" {
		t.Errorf("a non-chief agent's menu align %q", a)
	}
}

// TestAGatewayChiefKeepsItsHeader: a chief that is not claude-code has no
// account, failover or terminal to fold, and its second row is the model
// picker: it keeps the page header.
func TestAGatewayChiefKeepsItsHeader(t *testing.T) {
	v := chiefPageView(true)
	v.Kind, v.Card.Kind = agents.KindGateway, agents.KindGateway
	doc := ccParse(t, renderString(t, AgentDetailPage(v, Features{})))
	if n := len(ccWithAttr(doc, "data-chief-header", "")); n != 0 {
		t.Error("a gateway chief drew the folded header")
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
