package ui

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

// PINNED ENTRIES ARE CARDS (pins.go). The Agents list used to draw a pinned
// entry as a label-only chip in a row above the cards, unpinned from the chip's
// × behind an "Edit pins" toggle. The operator asked for that row to go — the
// session switcher's Pins view supersedes it — so a pinned entry is its card
// again, still drawn first, and the card's own pin toggle unpins it. Every
// test here renders the LIST (AgentsCardsWithMirrors) or RenderAgentCard, so
// each compiles against the chip code and fails there on what it asserts. The
// codenames are synthetic.

// pinParse parses a rendered list fragment.
func pinParse(t *testing.T, s string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader("<div id=root>" + s + "</div>"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return doc
}

func pinAttr(n *html.Node, k string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == k {
			return a.Val, true
		}
	}
	return "", false
}

// pinFind is every element under n (n included) for which ok holds, in
// document order.
func pinFind(n *html.Node, ok func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.ElementNode && ok(x) {
			out = append(out, x)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

func pinHas(k string) func(*html.Node) bool {
	return func(n *html.Node) bool { _, ok := pinAttr(n, k); return ok }
}

func pinText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

// pinArticles is the list's top-level <article> children by id.
func pinArticles(t *testing.T, s string) map[string]*html.Node {
	t.Helper()
	out := map[string]*html.Node{}
	for _, a := range pinFind(pinParse(t, s), func(n *html.Node) bool { return n.Data == "article" }) {
		id, _ := pinAttr(a, "id")
		out[id] = a
	}
	return out
}

// pinWin is a pinned window with a codename label, most recently active first
// by i (so the list's order is the fixtures' order).
func pinWin(i int, host, session, window, label string) MirrorCardView {
	return MirrorCardView{Host: host, Session: session, Window: window, Title: session + ":" + window[1:],
		Label: label, Online: true, Pinned: true, ActiveAt: time.Now().Add(-time.Duration(i+1) * time.Minute)}
}

// pinRender renders an html node back to markup.
func pinRender(t *testing.T, n *html.Node) string {
	t.Helper()
	var b strings.Builder
	if err := html.Render(&b, n); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestAPinnedEntryIsItsCardLedFirstWithAnUnpinToggle: a pinned window and a
// pinned agent are drawn as their CARDS (a nav anchor in the body slot), marked
// data-pinned, ahead of every unpinned card; the window's pin button is
// pressed and posts to the Unpin route for ITS entry, and the agent's ⋯ menu
// offers "Unpin" (posting the same route) instead of "Pin to top". No chip and
// no "Edit pins" toggle is drawn. The unpinned entries are the control: they
// still offer Pin to top, unpressed, and carry no data-pinned. Red at
// origin/main: the pinned entries were chips (no card slot, no pin toggle).
func TestAPinnedEntryIsItsCardLedFirstWithAnUnpinToggle(t *testing.T) {
	w := pinWin(5, "hosta", "s1", "@1", "mango:1")
	free := pinWin(0, "hosta", "s2", "@2", "vapor:2")
	free.Pinned = false
	ag := AgentCardView{ID: 9, Name: "nine", Status: "running", Pinned: true, LastActiveAt: time.Now().Add(-time.Hour)}
	other := AgentCardView{ID: 3, Name: "three", Status: "running", LastActiveAt: time.Now()}
	s := renderString(t, AgentsCardsWithMirrors([]AgentCardView{ag, other}, []MirrorCardView{w, free}))
	for _, bad := range []string{"data-pinned-chip", "agents-pins-edit", "data-pins-head"} {
		if strings.Contains(s, bad) {
			t.Errorf("the list draws %s, the removed pinned row", bad)
		}
	}
	arts := pinFind(pinParse(t, s), func(n *html.Node) bool { return n.Data == "article" })
	var order []string
	for _, a := range arts {
		id, _ := pinAttr(a, "id")
		order = append(order, id)
	}
	// Pinned first (the older of them is still pinned, so it leads the newer
	// unpinned ones: the order is orderList's, not activity alone).
	if len(order) != 4 || !(order[0] == "agent-9" || order[0] == mirrorCardID(w)) || !(order[1] == "agent-9" || order[1] == mirrorCardID(w)) {
		t.Fatalf("list order = %v, want the two pinned entries first", order)
	}
	byID := pinArticles(t, s)
	for _, id := range []string{"agent-9", mirrorCardID(w)} {
		a := byID[id]
		if _, ok := pinAttr(a, "data-pinned"); !ok {
			t.Errorf("%s: the pinned card carries no data-pinned", id)
		}
		body := false
		for _, n := range pinFind(a, pinHas("data-card-slot")) {
			if v, _ := pinAttr(n, "data-card-slot"); v == "body" {
				body = true
			}
		}
		if !body {
			t.Errorf("%s: the pinned entry is not drawn as its card (no body slot)", id)
		}
	}
	mcard := pinRender(t, byID[mirrorCardID(w)])
	for _, want := range []string{`action="/agents/pins/delete"`, `name="window" value="@1"`, `aria-pressed="true"`, `aria-label="Unpin"`} {
		if !strings.Contains(mcard, want) {
			t.Errorf("the pinned window's card lacks %s:\n%s", want, mcard)
		}
	}
	acard := pinRender(t, byID["agent-9"])
	for _, want := range []string{`action="/agents/pins/delete"`, `name="agent_id" value="9"`, `>Unpin<`} {
		if !strings.Contains(acard, want) {
			t.Errorf("the pinned agent's card lacks %s:\n%s", want, acard)
		}
	}
	if strings.Contains(acard, "Pin to top") || strings.Contains(mcard, "Pin to top") {
		t.Errorf("a pinned card offers Pin to top")
	}
	// The control: unpinned cards pin, unpressed, and are not marked.
	for _, id := range []string{"agent-3", mirrorCardID(free)} {
		c := pinRender(t, byID[id])
		if !strings.Contains(c, "Pin to top") || !strings.Contains(c, `action="/agents/pins"`) || strings.Contains(c, "data-pinned") || strings.Contains(c, `aria-pressed="true"`) {
			t.Errorf("%s: an unpinned card should offer Pin to top, unpressed:\n%s", id, c)
		}
	}
}

// TestRenderAgentCardDrawsAPinnedAgentAsItsCard: the rename save/cancel swap
// of #agent-<id> draws a pinned agent as its card, the same markup the list
// draws for it. Red at origin/main: it drew the chip.
func TestRenderAgentCardDrawsAPinnedAgentAsItsCard(t *testing.T) {
	ag := AgentCardView{ID: 5, Name: "five", DisplayName: "Five", Status: "running", Pinned: true, LastActiveAt: time.Now()}
	var b strings.Builder
	if err := RenderAgentCard(&b, ag); err != nil {
		t.Fatal(err)
	}
	if want := renderString(t, agentCard(ag)); b.String() != want {
		t.Errorf("RenderAgentCard(pinned) is not the agent's card:\n%s\nwant:\n%s", b.String(), want)
	}
	if strings.Contains(b.String(), "data-pinned-chip") || !strings.Contains(b.String(), "data-pinned") {
		t.Errorf("RenderAgentCard(pinned) should be the card, marked pinned:\n%s", b.String())
	}
}

// TestSwitcherLabelsAreTheWholeName: a window's pill reads its WHOLE codename,
// ":<index>" and all — "mango:1", "vapor:2", "wheat:13" — whether or not
// another entry shares the rest; the same codename and index on two hosts adds
// the list's qualifier; a window with no codename shows its
// "<session>:<index>" and a label that only repeats the session is no
// codename; an agent shows its display name, else its name, WHOLE.
//
// REGRESSION TEST (operator report: the pills were "missing the number after
// the colon"): red at the commit before, where a codename alone in its view
// dropped its ":<index>" ("mango", "vapor", "wheat").
func TestSwitcherLabelsAreTheWholeName(t *testing.T) {
	long := strings.Repeat("x", 70)
	cases := []struct {
		name   string
		agents []AgentCardView
		wins   []MirrorCardView
		want   []string
	}{
		{"distinct codenames", nil, []MirrorCardView{
			pinWin(0, "hosta", "s1", "@1", "mango:1"),
			pinWin(1, "hosta", "s2", "@2", "vapor:2"),
			pinWin(2, "hosta", "s3", "@3", "wheat:13"),
		}, []string{"mango:1", "vapor:2", "wheat:13"}},
		{"one codename twice", nil, []MirrorCardView{
			pinWin(0, "hosta", "s1", "@1", "mango:1"),
			pinWin(1, "hosta", "s1", "@2", "mango:2"),
			pinWin(2, "hosta", "s2", "@3", "vapor:3"),
		}, []string{"mango:1", "mango:2", "vapor:3"}},
		{"one label on two hosts", nil, func() []MirrorCardView {
			a, b := pinWin(0, "hosta", "s1", "@1", "mango:1"), pinWin(1, "hostb", "s1", "@1", "mango:1")
			a.Qualifier, b.Qualifier = "hosta", "hostb"
			return []MirrorCardView{a, b}
		}(), []string{"mango:1 · hosta", "mango:1 · hostb"}},
		{"fallbacks", []AgentCardView{
			{ID: 1, Name: "named-agent", DisplayName: "Pretty agent", Status: "running", Pinned: true, LastActiveAt: time.Now().Add(-10 * time.Minute)},
			{ID: 2, Name: "bare-agent", Status: "running", Pinned: true, LastActiveAt: time.Now().Add(-11 * time.Minute)},
			{ID: 3, Name: "long-agent", DisplayName: long, Status: "running", Pinned: true, LastActiveAt: time.Now().Add(-12 * time.Minute)},
		}, []MirrorCardView{pinWin(0, "hosta", "build", "@7", ""), pinWin(1, "hosta", "deploy", "@8", "deploy")},
			[]string{"build:7", "deploy:8", "Pretty agent", "bare-agent", long}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, it := range NewSwitcher(c.agents, c.wins, nil, SwitcherCurrent{}).All {
				got = append(got, it.Label)
			}
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Errorf("switcher labels = %q, want %q", got, c.want)
			}
		})
	}
}
