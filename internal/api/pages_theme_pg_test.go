package api

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/pages"
)

// TestTheOwnerAndALinkGetTheSameThemedContent: a markdown page's content
// document, as its owner's view frames it and as a link's viewer's does, is
// one document in the app's theme: byte-identical whoever asks and whatever
// the request says about a colour scheme, so no response varies by viewer and
// the content's no-store stays the only caching rule it needs.
func TestTheOwnerAndALinkGetTheSameThemedContent(t *testing.T) {
	f := newPageFixture(t)
	id := f.create(t, f.agent, "Themed", `[{"type":"markdown","text":"| a | b |\n| --- | --- |\n| c | [d](https://example.com/d) |"}]`)
	l := f.liveLink(t, f.owner, id)
	owner := "/pages/" + strconv.FormatInt(id, 10) + "/versions/1/content"
	link := pages.LinkPath(l.Token) + "/content"

	base := f.get(t, link, nil)
	assertContentHeaders(t, base, "the link's content")
	doc := base.Body.String()
	// The app's theme: its class on <html>, its palette, the renderer's
	// table and link utilities defined.
	for _, want := range []string{`<html lang="en" class="dark">`, "--mu-bg:17 15 28", "--mu-accent:228 181 86",
		".mp-markdown .border-line{", ".mp-markdown .text-accent{"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the content document lacks %q", want)
		}
	}
	if !strings.Contains(pageContentBlocks(t, doc), `<td class="border border-line px-2 py-1">`) {
		t.Fatalf("control: the page's table is not in its blocks: %s", pageContentBlocks(t, doc))
	}

	// A viewer whose device prefers light, or who still holds the old
	// light-theme choice, gets the same bytes; so does the owner.
	for _, c := range []struct {
		name, path string
		hdr        map[string]string
	}{
		{"the owner's view", owner, f.ownerHdr()},
		{"the owner's session on the link", link, f.ownerHdr()},
		{"a light-scheme device", link, map[string]string{"Sec-CH-Prefers-Color-Scheme": "light", "Cookie": "muster-theme=light"}},
		{"the owner on a light-scheme device", owner, merge(f.ownerHdr(), map[string]string{"Sec-CH-Prefers-Color-Scheme": "light"})},
	} {
		w := f.get(t, c.path, c.hdr)
		assertContentHeaders(t, w, c.name)
		if w.Body.String() != doc {
			t.Errorf("%s: the content differs from a link viewer's", c.name)
		}
		if v := w.Header().Values("Vary"); len(v) != 0 {
			t.Errorf("%s: Vary = %v; the content does not vary by viewer", c.name, v)
		}
	}
}
