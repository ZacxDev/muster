package api

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/theme"
)

// TestTheOwnerAndALinkGetTheSameThemedContent: a markdown page's content
// document, as its owner's view frames it and as a link's viewer's does, is
// one document in the app's theme: byte-identical whoever asks and whatever
// the request says about a colour scheme, so no response varies by viewer and
// the content's no-store stays the only caching rule it needs.
//
// The bytes DO depend on the server's own theme (the instance's branding,
// TestEveryPageDocumentIsDrawnInTheInstancesTheme): this fixture has no
// branding store, so it is the default theme's document.
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
	// The default theme inlines no theme block (the palette above is it).
	if strings.Contains(doc, theme.Selector) {
		t.Errorf("the default theme's content inlines a theme block")
	}
	if !strings.Contains(pageContentBlocks(t, doc), `<td class="border border-line px-2 py-1">`) {
		t.Fatalf("control: the page's table is not in its blocks: %s", pageContentBlocks(t, doc))
	}
	assertSameForEveryViewer(t, f, owner, link, doc)
}

// assertSameForEveryViewer: a viewer whose device prefers light, or who
// still holds the old light-theme choice, gets the same bytes as a link's
// viewer; so does the owner.
func assertSameForEveryViewer(t *testing.T, f *pageFixture, owner, link, doc string) {
	t.Helper()
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

// tnAccentRe is the inlined theme block's accent: the instance's theme is
// written `--mu-accent: R G B;` (theme.CSS) — with the space the embedded
// palette's minified `--mu-accent:228 181 86` does not have.
var tnAccentRe = regexp.MustCompile(`--mu-accent: (\d+ \d+ \d+);`)

// TestEveryPageDocumentIsDrawnInTheInstancesTheme: with an admin's theme set
// (a preset and an accent of their own, instance_branding), every document a
// page is served as inlines that theme — the link's content and shell, the
// owner's version content (what the owner's view and a session's Pages tab
// frame) and the owner's shell, embedded or not — so a page shows the
// instance's palette, not muster's default. Two different accents, so a
// hard-coded value cannot pass both; and the default theme's documents carry
// none (the negative control). The content stays the same for every viewer.
//
// The accents' channels are written by hand: #5AA9F0 is 90 169 240, #7FD0FF
// is 127 208 255.
func TestEveryPageDocumentIsDrawnInTheInstancesTheme(t *testing.T) {
	f := newPageFixture(t)
	mem := &memBranding{}
	f.srv.ext.Branding = mem
	id := f.create(t, f.agent, "Themed", `[{"type":"markdown","text":"# Hi"},{"type":"html","html":"<div class=\"card\">x</div>"}]`)
	l := f.liveLink(t, f.owner, id)
	sid := strconv.FormatInt(id, 10)
	ownerContent := "/pages/" + sid + "/versions/1/content"
	linkContent := pages.LinkPath(l.Token) + "/content"
	paths := []struct {
		name, path string
		owner      bool
	}{
		{"the link's content", linkContent, false},
		{"the owner's version content", ownerContent, true},
		{"the link's shell", pages.LinkPath(l.Token), false},
		{"the owner's shell", pageViewPath(id), true},
		{"the owner's shell in a session's panel", pageViewPath(id) + "?embed=1", true},
	}
	serve := func(p string, owner bool) string {
		t.Helper()
		var hdr map[string]string
		if owner {
			hdr = f.ownerHdr()
		}
		w := f.get(t, p, hdr)
		if w.Code != 200 {
			t.Fatalf("GET %s: %d %s", p, w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	setTheme := func(preset, accent string) {
		mem.mu.Lock()
		mem.b.Theme = theme.Choice{Preset: preset, Accent: accent}
		mem.mu.Unlock()
		// Expire the caches under their own locks: the page create above left
		// a push goroutine reading them (goNotify → pushIcon → uiBrand), and
		// overwriting the structs whole races it.
		f.srv.brand.mu.Lock()
		f.srv.brand.ok = false
		f.srv.brand.mu.Unlock()
		f.srv.themeFiles.mu.Lock()
		f.srv.themeFiles.block = ""
		f.srv.themeFiles.mu.Unlock()
	}
	byTheme := map[string]string{}
	for _, c := range []struct {
		name, preset, accent, want string
	}{
		{"harbor #5aa9f0", "harbor", "#5aa9f0", "90 169 240"},
		{"ember #7fd0ff", "ember", "#7fd0ff", "127 208 255"},
	} {
		setTheme(c.preset, c.accent)
		for _, p := range paths {
			doc := serve(p.path, p.owner)
			m := tnAccentRe.FindAllStringSubmatch(doc, -1)
			if len(m) != 1 || m[0][1] != c.want {
				t.Errorf("%s, %s: the inlined theme's accent = %q, want exactly one, %q", c.name, p.name, m, c.want)
			}
			if !strings.Contains(doc, theme.Selector+" {") {
				t.Errorf("%s, %s: no %s theme block", c.name, p.name, theme.Selector)
			}
		}
		doc := serve(linkContent, false)
		byTheme[c.name] = doc
		assertSameForEveryViewer(t, f, ownerContent, linkContent, doc)
	}
	if byTheme["harbor #5aa9f0"] == byTheme["ember #7fd0ff"] {
		t.Error("two themes serve byte-identical content")
	}

	setTheme("", "")
	for _, p := range paths {
		if doc := serve(p.path, p.owner); strings.Contains(doc, theme.Selector) || tnAccentRe.MatchString(doc) {
			t.Errorf("the default theme, %s: a theme block is inlined", p.name)
		}
	}
	if serve(linkContent, false) == byTheme["harbor #5aa9f0"] {
		t.Error("the default theme's content is byte-identical to a themed one's")
	}
}
