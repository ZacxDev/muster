package ui

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// TestTheViewPagesPaintWholeBeforeAnyScript: 🔴 on each of the three view
// pages — the terminal, the mirrored Rendered view and an agent's chat — no
// <script src> stands in the <body> before the end of what the page lays out:
// its header, its log or terminal, its key bar and its composer.
//
// A <script src> is parser-blocking: while its file is fetched (a first visit,
// a fresh browser) Chromium may paint the document parsed so far, and what is
// parsed after it then arrives in a painted page. MEASURED on the terminal: the
// floating menu's script, made a static file by #278, stood between the key
// bar and the composer; a paint while it loaded drew the bar at the bottom and
// the composer then pushed it up — a 0.01259 layout shift of nav#term-keys
// opening the terminal in its frame at 390px (cls.spec "repos, connectors, the
// mirrored views and an agent terminal").
//
// ⚠ ON THE RENDERED VIEW AND THE AGENT PAGE THIS IS AN INVARIANT GUARD: neither
// had a script there (checked when the terminal's was found), so no shift of
// theirs is explained by it. It keeps the next static script from landing in
// front of their composers the way the floating menu's did on the terminal.
//
// Script in the head is not this: nothing of the body is painted yet. Inline
// script is not either: it needs no fetch to run.
func TestTheViewPagesPaintWholeBeforeAnyScript(t *testing.T) {
	agent := sampleAgentDetailView()
	agent.Name, agent.Kind = "keen-wren", "claude-code"
	pages := map[string]struct {
		doc  string
		last []string // the page's laid-out blocks; the latest end is where scripts may start
	}{
		"terminal, agent":    {renderRaw(t, AgentTerminalPage(termKeysPages()["agent"])), []string{"term", "term-keys"}},
		"terminal, mirrored": {renderRaw(t, AgentTerminalPage(termKeysPages()["mirror"])), []string{"term", "term-keys", "mv-compose"}},
		"rendered view": {renderRaw(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Interactive: true,
			StepUp: stepUpFixture(), Nonce: "0123456789abcdef"})), []string{"mv-log", "mv-compose"}},
		"agent page": {renderRaw(t, AgentDetailPage(agent, featuresAllOn)), []string{"chat-log", "chat-form"}},
	}
	for name, p := range pages {
		t.Run(name, func(t *testing.T) {
			// renderRaw, NOT renderString: renderString inlines the static
			// scripts (expandStaticScripts), and an inline script is exactly
			// what this is not about.
			doc, err := html.Parse(strings.NewReader(p.doc))
			if err != nil {
				t.Fatal(err)
			}
			// Every element in document order, and where each one's subtree ends.
			var order []*html.Node
			end := map[*html.Node]int{}
			var walk func(n *html.Node)
			walk = func(n *html.Node) {
				if n.Type == html.ElementNode {
					order = append(order, n)
				}
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c)
				}
				if n.Type == html.ElementNode {
					end[n] = len(order) - 1
				}
			}
			walk(doc)
			body, last := -1, -1
			for i, n := range order {
				if n.Data == "body" && body < 0 {
					body = i
				}
				id, _ := pinAttr(n, "id")
				for _, want := range p.last {
					if id == want && end[n] > last {
						last = end[n]
					}
				}
			}
			if body < 0 || last < 0 {
				t.Fatalf("the page is not there: <body> at %d, its last block ends at %d", body, last)
			}
			for _, id := range p.last {
				if !strings.Contains(p.doc, `id="`+id+`"`) {
					t.Fatalf("#%s is not drawn: the guard would measure a page it does not describe", id)
				}
			}
			for i := body; i <= last; i++ {
				if n := order[i]; n.Data == "script" {
					// A deferred script does not block the parser (it runs after the
					// document is parsed), so it moves nothing in mid-paint.
					if _, deferred := pinAttr(n, "defer"); deferred {
						continue
					}
					if src, ok := pinAttr(n, "src"); ok {
						t.Errorf("a parser-blocking <script src=%q> in the body before the page's last laid-out block ends (element %d of %d..%d): a paint while it loads moves what is parsed after it", src, i, body, last)
					}
				}
			}
		})
	}
}
