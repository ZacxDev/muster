package ui

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// TestTheTerminalPaintsWholeBeforeAnyScript: 🔴 in the terminal page, no
// <script src> stands inside the column it lays out — from #term down to the
// end of the key bar and, on a mirrored window, of the composer under it.
//
// A <script src> is parser-blocking: while its file is fetched (a first visit,
// a fresh browser) Chromium may paint the document parsed so far. The key bar
// sits above the composer, and the terminal takes the rest of the column, so a
// paint before the composer exists draws the bar at the bottom, and the
// composer's arrival then moves it up by the composer's height. Measured: a
// 0.01259 layout shift of nav#term-keys opening the terminal in its frame at
// 390px (cls.spec "repos, connectors, the mirrored views and an agent
// terminal"), after the floating menu's script became a static file (#278)
// while it stood between the bar and the composer.
//
// Script BEFORE the column (in the head, at the top of the body) is not this:
// nothing of the column is painted yet, so nothing of it can move.
func TestTheTerminalPaintsWholeBeforeAnyScript(t *testing.T) {
	for name, v := range termKeysPages() {
		t.Run(name, func(t *testing.T) {
			// renderRaw, NOT renderString: renderString inlines the static scripts
			// (expandStaticScripts), and an inline script is exactly what this is not about.
			doc, err := html.Parse(strings.NewReader(renderRaw(t, AgentTerminalPage(v))))
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
			at := func(id string) (int, *html.Node) {
				for i, n := range order {
					if v, _ := pinAttr(n, "id"); v == id {
						return i, n
					}
				}
				return -1, nil
			}
			from, _ := at("term")
			last := -1
			for _, id := range []string{"term-keys", "mv-compose"} {
				if i, n := at(id); i >= 0 && end[n] > last {
					last = end[n]
				}
			}
			if from < 0 || last < 0 {
				t.Fatalf("the column is not there: #term at %d, its end at %d", from, last)
			}
			if _, c := at("mv-compose"); (v.Composer != nil) != (c != nil) {
				t.Fatalf("composer drawn %t; the view has one: %t", c != nil, v.Composer != nil)
			}
			for i := from; i <= last; i++ {
				if n := order[i]; n.Data == "script" {
					if src, ok := pinAttr(n, "src"); ok {
						t.Errorf("a parser-blocking <script src=%q> inside the terminal's column (element %d of %d..%d): a paint while it loads moves the key bar when the rest arrives", src, i, from, last)
					}
				}
			}
		})
	}
}
