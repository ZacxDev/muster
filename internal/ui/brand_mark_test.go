package ui

import (
	"bytes"
	"strings"
	"testing"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// markParts is brandMark(b) parsed: its <img data-brand-logo> elements, its
// [data-brand-name] elements, and every non-blank text node anywhere in it.
type markParts struct {
	logos, names []*html.Node
	texts        []string
}

func parseMark(t *testing.T, b Brand) markParts {
	t.Helper()
	var buf bytes.Buffer
	if err := brandMark(b).Render(&buf); err != nil {
		t.Fatal(err)
	}
	nodes, err := html.ParseFragment(strings.NewReader(buf.String()), &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div})
	if err != nil {
		t.Fatal(err)
	}
	var p markParts
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			if s := strings.TrimSpace(n.Data); s != "" {
				p.texts = append(p.texts, s)
			}
		case html.ElementNode:
			if _, ok := nodeAttr(n, "data-brand-logo"); ok && n.DataAtom == atom.Img {
				p.logos = append(p.logos, n)
			}
			if _, ok := nodeAttr(n, "data-brand-name"); ok {
				p.names = append(p.names, n)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	for _, n := range nodes {
		walk(n)
	}
	return p
}

// TestBrandMarkHidesTheNameBesideAWordmark: the header's and the sidebar's
// brand, by structure. A logo that shows the name (LogoShowsName) is drawn
// ALONE: one <img>, its alt the name — so the name is still announced — and
// no name element and no text at all. A logo without the flag is the logo
// with alt="" (the text beside it says the name) and then the name element.
// The flag with no logo changes nothing: the name element, and no <img>. A
// wordmark logo with no name has nothing to announce: alt="".
func TestBrandMarkHidesTheNameBesideAWordmark(t *testing.T) {
	const name, etag = "Team Example", "0123456789abcdef"
	cases := []struct {
		label    string
		brand    Brand
		logos    int
		alt      string
		nameText string // the one [data-brand-name] element's text; "" for none
	}{
		{"a wordmark logo", Brand{Name: name, ETag: etag, LogoShowsName: true}, 1, name, ""},
		{"a logo beside the name", Brand{Name: name, ETag: etag}, 1, "", name},
		{"the flag with no logo", Brand{Name: name, LogoShowsName: true}, 0, "", name},
		{"a wordmark logo with no name", Brand{ETag: etag, LogoShowsName: true}, 1, "", ""},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			p := parseMark(t, c.brand)
			if len(p.logos) != c.logos {
				t.Fatalf("%d <img data-brand-logo>, want %d", len(p.logos), c.logos)
			}
			if c.logos == 1 {
				alt, ok := nodeAttr(p.logos[0], "alt")
				if !ok || alt != c.alt {
					t.Errorf("the logo's alt = %q (present %v), want %q", alt, ok, c.alt)
				}
			}
			if c.nameText == "" {
				if len(p.names) != 0 || len(p.texts) != 0 {
					t.Errorf("%d name element(s) and text %q, want neither", len(p.names), p.texts)
				}
				return
			}
			if len(p.names) != 1 || len(p.texts) != 1 || p.texts[0] != c.nameText {
				t.Fatalf("%d name element(s), text %q; want one, %q", len(p.names), p.texts, c.nameText)
			}
		})
	}
}
