package ui

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// TestTheTerminalFootIsHiddenUntilItIsWhole: 🔴 on a Terminal view the key bar
// and what is below it (the composer, or the session switcher's room) are not
// shown until the last of them is parsed (termFootHead). A paint between the
// key bar and the composer drew the bar at the screen's foot and the composer
// then pushed it up by its height — measured 0.01861 at 1280×800 (cls.spec
// "the session views with a live preview", CPU throttled 4×; CI's flake).
//
// The RELATIONSHIP is pinned, both ways, for a page with a composer, with only
// the switcher, and with neither:
//   - the hiding attribute is set on <html> by an inline script in the head
//     (before anything of the body can paint), and the head's rule hides
//     exactly the foot's blocks;
//   - every block of the body's column from the key bar up to the reveal
//     script is one that rule hides — a new block added there unhidden fails;
//   - the reveal is after the end of the LAST foot block (a reveal moved up
//     past the composer fails) and before the first parser-blocking
//     <script src> (so no fetch holds the foot hidden).
func TestTheTerminalFootIsHiddenUntilItIsWhole(t *testing.T) {
	sw := NewSwitcher(navAgentFixture(), navMirrorFixture(), nil, SwitcherCurrent{Mirror: MirrorRef{Host: "laptop", Session: "s-fresh", Window: "@8"}})
	const rule = `html[data-term-foot-pending] :is(#term-keys, #mv-compose, [data-switcher-room]) { visibility: hidden; }`
	const set = `document.documentElement.setAttribute('data-term-foot-pending', '');`
	const reveal = `document.documentElement.removeAttribute('data-term-foot-pending');`
	hidden := func(n *html.Node) bool {
		id, _ := pinAttr(n, "id")
		_, room := pinAttr(n, "data-switcher-room")
		return id == "term-keys" || id == "mv-compose" || room
	}
	text := func(n *html.Node) string {
		if n.FirstChild == nil {
			return ""
		}
		return strings.TrimSpace(n.FirstChild.Data)
	}
	for name, c := range map[string]struct {
		v    AgentTerminalView
		foot []string // the foot's blocks, in order: an id, or "room" for the switcher's
	}{
		"a composer and the switcher": {AgentTerminalView{Name: "s1", WSPath: "/mirror/terminal/ws?host=laptop&session=s1",
			Composer: &MirrorComposerView{SendPath: MirrorTerminalSendPath, Host: "laptop", Session: "s1", Window: "@8"}, StepUp: &StepUpView{}, Switcher: sw},
			[]string{"term-keys", "mv-compose"}},
		"a composer, no switcher": {termKeysPages()["mirror"], []string{"term-keys", "mv-compose"}},
		"the switcher, no composer": {AgentTerminalView{Name: "s1", WSPath: "/mirror/terminal/ws?host=laptop&session=s1", StepUp: &StepUpView{}, Switcher: sw},
			[]string{"term-keys", "room"}},
		"neither": {termKeysPages()["agent"], []string{"term-keys"}},
	} {
		t.Run(name, func(t *testing.T) {
			doc, err := html.Parse(strings.NewReader(renderRaw(t, AgentTerminalPage(c.v))))
			if err != nil {
				t.Fatal(err)
			}
			heads := pinFind(doc, func(n *html.Node) bool { return n.Type == html.ElementNode && n.Data == "head" })
			bodies := pinFind(doc, func(n *html.Node) bool { return n.Type == html.ElementNode && n.Data == "body" })
			if len(heads) != 1 || len(bodies) != 1 {
				t.Fatalf("%d <head>, %d <body>", len(heads), len(bodies))
			}
			var rules, sets []string
			for _, n := range pinFind(heads[0], func(n *html.Node) bool { return n.Type == html.ElementNode }) {
				if n.Data == "style" && strings.Contains(text(n), "data-term-foot-pending") {
					rules = append(rules, text(n))
				}
				if _, src := pinAttr(n, "src"); n.Data == "script" && !src && text(n) == set {
					sets = append(sets, text(n))
				}
			}
			if len(rules) != 1 || rules[0] != rule {
				t.Errorf("the head's foot rule: %q, want exactly [%q]", rules, rule)
			}
			if len(sets) != 1 {
				t.Errorf("%d inline head scripts set the attribute, want 1", len(sets))
			}
			// The body's column, child by child.
			var kids []*html.Node
			for k := bodies[0].FirstChild; k != nil; k = k.NextSibling {
				if k.Type == html.ElementNode {
					kids = append(kids, k)
				}
			}
			keys, rev := -1, -1
			for i, k := range kids {
				if id, _ := pinAttr(k, "id"); id == "term-keys" && keys < 0 {
					keys = i
				}
				if k.Data == "script" && text(k) == reveal {
					if rev >= 0 {
						t.Fatalf("two reveal scripts in the body")
					}
					rev = i
				}
			}
			if keys < 0 || rev < 0 || rev < keys {
				t.Fatalf("key bar at child %d, reveal at child %d: want the reveal after the bar", keys, rev)
			}
			var got []string
			for _, k := range kids[keys:rev] {
				if !hidden(k) {
					id, _ := pinAttr(k, "id")
					t.Errorf("<%s id=%q> is in the foot (between the key bar and the reveal) but the rule does not hide it", k.Data, id)
					continue
				}
				if id, _ := pinAttr(k, "id"); id != "" {
					got = append(got, id)
				} else {
					got = append(got, "room")
				}
			}
			if strings.Join(got, ",") != strings.Join(c.foot, ",") {
				t.Errorf("the foot's blocks: %v, want %v", got, c.foot)
			}
			// The first parser-blocking script of the body comes after the reveal.
			for _, n := range pinFind(bodies[0], func(n *html.Node) bool { return n.Type == html.ElementNode && n.Data == "script" }) {
				src, ok := pinAttr(n, "src")
				if !ok {
					continue
				}
				if _, d := pinAttr(n, "defer"); d {
					continue
				}
				for p := n; p != nil; p = p.Parent {
					if p.Parent == bodies[0] {
						for i, k := range kids {
							if k == p && i < rev {
								t.Errorf("<script src=%q> (body child %d) is before the reveal (child %d): its fetch would hold the foot hidden", src, i, rev)
							}
						}
					}
				}
			}
		})
	}
}
