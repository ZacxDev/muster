package api

import (
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestAMarkdownAssetImageIsDrawnOnlyWhereItIsText: through the real markdown
// renderer (ui.MarkdownHTML, as pageRenderOptions passes it), ![a](asset:ID)
// becomes muster's <img> only where the rendered output is TEXT. Inside an
// attribute (a link whose address holds the image syntax) and inside a code
// span or block it stays the markdown as written, escaped — never a tag
// spliced into an attribute (a broken element) or into code. Each probe's
// markup is parsed, and no element of it may carry an attribute holding a "<"
// or an <img> the probe did not ask for. Control: the bare syntax is drawn.
func TestAMarkdownAssetImageIsDrawnOnlyWhereItIsText(t *testing.T) {
	const id = "AbCdEfGhIjKlMnOpQrStUv"
	ref := "![a](asset:" + id + ")"
	opts := pages.RenderOptions{Markdown: ui.MarkdownHTML, Assets: []pages.Asset{{PublicID: id, ContentType: "image/png"}}}
	draw := func(md string) string {
		v := pages.Version{Title: "t", Version: 1, Blocks: []pages.Block{{Type: pages.BlockMarkdown, Text: md}}}
		return pageContentBlocks(t, pages.Content(v, opts))
	}
	// imgs counts the <img> elements of markup, and fails on any attribute
	// that holds a tag (what a splice into an attribute leaves).
	imgs := func(name, markup string) int {
		n := 0
		z := html.NewTokenizer(strings.NewReader(markup))
		for {
			tt := z.Next()
			if tt == html.ErrorToken {
				return n
			}
			tok := z.Token()
			if tt == html.StartTagToken || tt == html.SelfClosingTagToken {
				if tok.Data == "img" {
					n++
				}
				for _, a := range tok.Attr {
					if strings.Contains(a.Val, "<") {
						t.Errorf("%s: <%s %s=%q>: a tag inside an attribute", name, tok.Data, a.Key, a.Val)
					}
				}
			}
		}
	}
	if got := draw(ref); !strings.Contains(got, `<img src="asset:`+id+`" alt="a">`) || imgs("control", got) != 1 {
		t.Fatalf("control: the bare syntax is not drawn as the asset: %s", got)
	}
	for _, c := range []struct{ name, md string }{
		{"in a link's address", "[x](https://x.example/" + ref + ")"},
		{"in a code span", "`" + ref + "`"},
		{"in a code block", "```\n" + ref + "\n```"},
	} {
		got := draw(c.md)
		if n := imgs(c.name, got); n != 0 {
			t.Errorf("%s: %d <img> drawn, want the markdown as text: %s", c.name, n, got)
		}
		if !strings.Contains(got, ref) {
			t.Errorf("%s: the markdown as written is gone: %s", c.name, got)
		}
		if strings.Contains(got, "MUSTERASSET") {
			t.Errorf("%s: a placeholder was left: %s", c.name, got)
		}
	}
}
