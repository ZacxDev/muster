package ui

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

// Node and fixture helpers the retired Agents list's tests defined, kept for
// the tests that still use them (the list's own tests are gone with it).

func amp(s string) string { return strings.ReplaceAll(s, "&", "&amp;") }

// findAttr is the first element under n carrying attribute key.
func findAttr(n *html.Node, key string) *html.Node {
	var out *html.Node
	walkNodes(n, func(c *html.Node) bool {
		if out != nil {
			return false
		}
		if c.Type == html.ElementNode {
			if _, ok := nodeAttr(c, key); ok {
				out = c
				return false
			}
		}
		return true
	})
	return out
}

func focusNodeText(n *html.Node) string {
	var b strings.Builder
	walkNodes(n, func(c *html.Node) bool {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
		return true
	})
	return strings.Join(strings.Fields(b.String()), " ")
}

// insideTag reports whether n has an ancestor element named tag.
func insideTag(n *html.Node, tag string) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == tag {
			return true
		}
	}
	return false
}

func nodeAttr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func nodeHasClass(n *html.Node, cls string) bool {
	v, _ := nodeAttr(n, "class")
	for _, c := range strings.Fields(v) {
		if c == cls {
			return true
		}
	}
	return false
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// openTag is the first opening tag in html that carries attr.
func openTag(t *testing.T, html, attr string) string {
	t.Helper()
	i := strings.Index(html, attr)
	if i < 0 {
		t.Fatalf("no element carries %s", attr)
	}
	start := i
	if !strings.HasPrefix(attr, "<") {
		start = strings.LastIndex(html[:i], "<")
	}
	end := strings.Index(html[i:], ">")
	return html[start : i+end+1]
}

// parseArticle is markup's one <article>.
func parseArticle(t *testing.T, markup string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		t.Fatal(err)
	}
	var art *html.Node
	walkNodes(doc, func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == "article" {
			art = n
			return false
		}
		return true
	})
	if art == nil {
		t.Fatal("instrument check FAILED: no <article>")
	}
	return art
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

// pinParse parses a rendered list fragment.
func pinParse(t *testing.T, s string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader("<div id=root>" + s + "</div>"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return doc
}

// realisticAgent / realisticMirror are a card of each kind as the list really
// draws them: an agent with a repo, a kind and both footer times; a window
// with a kind, a label and a reported activity. Their times are distinct.
func realisticAgent() AgentCardView {
	return AgentCardView{ID: 7, Name: "worker-7", DisplayName: "Worker seven", Repo: "acme/widgets", Status: "running",
		CreatedAt: fixedNow.Add(-3 * time.Hour), LastActiveAt: fixedNow.Add(-4 * time.Minute)}
}

func realisticMirror() MirrorCardView {
	return MirrorCardView{Host: "tower", Session: "work", Window: "@3", Label: "mango:1", Kind: "opencode",
		Online: true, Interactive: true, ActiveAt: fixedNow.Add(-9 * time.Minute)}
}

// walkNodes visits n's subtree in document order; f returning false stops
// the descent below that node.
func walkNodes(n *html.Node, f func(*html.Node) bool) {
	if !f(n) {
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkNodes(c, f)
	}
}
