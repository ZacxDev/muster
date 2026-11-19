package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// TestTheSignInPageKeepsTheHandoffUnderTheSwitchersKey: the sign-in page's
// keep script (ServerHandoffKeepHTML) writes the sessionStorage key that
// web/static/servers.js reads (HANDOFF_KEY), in the "<ms>|<hash>" form its
// kept() parses — the two halves of one hand-off, in two languages.
func TestTheSignInPageKeepsTheHandoffUnderTheSwitchersKey(t *testing.T) {
	src, err := os.ReadFile("../../web/static/servers.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "var HANDOFF_KEY = '"+ServerHandoffKey+"';") {
		t.Fatalf("servers.js does not read the key the sign-in page writes (%q)", ServerHandoffKey)
	}
	keep := ServerHandoffKeepHTML()
	for _, want := range []string{"sessionStorage.setItem('" + ServerHandoffKey + "',", "Date.now()+'|'+location.hash", "/^#muster-from=/"} {
		if !strings.Contains(keep, want) {
			t.Errorf("the keep script lacks %q: %s", want, keep)
		}
	}
}

// TestTheServerRowIsAnnouncedOnce: the row's button is named by its
// aria-label ("Switch server, current: <name>") and carries no
// aria-describedby pointing back into itself, which read the name twice.
func TestTheServerRowIsAnnouncedOnce(t *testing.T) {
	page := renderString(t, serverSwitcher(Brand{Name: "Team Example"}))
	start := strings.Index(page, `<button id="server-switcher-toggle"`)
	if start < 0 {
		t.Fatal("no row button")
	}
	tag := page[start : start+strings.Index(page[start:], ">")]
	if strings.Contains(tag, "aria-describedby") {
		t.Errorf("the row button has a description: %s", tag)
	}
	if !strings.Contains(tag, `aria-label="Switch server, current: Team Example"`) {
		t.Errorf("the row button is not named for the server in view: %s", tag)
	}
}

// TestTheRowIsTheLogoOnly pins the row button for each branding state, by
// literal: its accessible name and tooltip, the picture it draws (the logo,
// the monogram, or muster's square mark: never the lockup), no colour dot,
// the monogram's letters and colour, and
// NO text at all on screen (no name, no address: the operator's ask, "just
// show the logo"). The monogram is drawn in every state, hidden behind a
// picture, for servers.js to show when the logo fails.
//
// 🔴 REGRESSION TEST: on origin/main the button held a dot, "Server in view:"
// and the name as text, no aria-label, and no picture.
func TestTheRowIsTheLogoOnly(t *testing.T) {
	cases := []struct {
		name      string
		b         Brand
		label     string // the accessible name's server
		picture   string // the visible picture's src prefix; "" = the monogram
		letters   string
		colour    string // the monogram's background class
		monoShown bool
	}{
		{"a logo and a name", Brand{Name: "Example Studio", ETag: "e1"}, "Example Studio", "/brand/logo.png?v=e1", "ES", "bg-server-1", false},
		{"a name only", Brand{Name: "Example Studio"}, "Example Studio", "", "ES", "bg-server-1", true},
		{"a logo only", Brand{ETag: "e1"}, "This server", "/brand/logo.png?v=e1", "TS", "bg-server-0", false},
		{"no branding", Brand{}, "This server", "/static/icons/favicon.svg?v=", "TS", "bg-server-0", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := parseFragment(t, renderString(t, serverSwitcher(c.b)))
			btn := byID(root, "server-switcher-toggle")
			if btn == nil {
				t.Fatal("no row button")
			}
			if v, _ := nodeAttr(btn, "aria-label"); v != "Switch server, current: "+c.label {
				t.Errorf("aria-label %q", v)
			}
			if v, _ := nodeAttr(btn, "title"); v != c.label {
				t.Errorf("title %q", v)
			}
			// No text on screen: the only text node in the button is the
			// monogram's (aria-hidden, and hidden behind a picture).
			mono := findAttr(btn, "data-server-monogram")
			if mono == nil {
				t.Fatal("no monogram")
			}
			if got := focusNodeText(btn); got != c.letters {
				t.Errorf("the button's text is %q, want the monogram's %q alone", got, c.letters)
			}
			var colours []string
			for _, cls := range strings.Fields(attrOf(mono, "class")) {
				if strings.HasPrefix(cls, "bg-") {
					colours = append(colours, cls)
				}
			}
			if strings.Join(colours, " ") != c.colour {
				t.Errorf("monogram background %v, want %s alone", colours, c.colour)
			}
			if _, hidden := nodeAttr(mono, "hidden"); hidden == c.monoShown {
				t.Errorf("monogram hidden=%v, want shown=%v", hidden, c.monoShown)
			}
			var imgs []string
			walkNodes(btn, func(n *html.Node) bool {
				if n.Type == html.ElementNode && n.DataAtom == atom.Img {
					src, _ := nodeAttr(n, "src")
					imgs = append(imgs, src)
				}
				return true
			})
			switch {
			case c.picture == "" && len(imgs) != 0:
				t.Errorf("a monogram row draws images %v", imgs)
			case c.picture != "" && (len(imgs) != 1 || !strings.HasPrefix(imgs[0], c.picture)):
				t.Errorf("the row draws %v, want one image at %s…", imgs, c.picture)
			}
			for _, banned := range []string{"data-server-label", "data-server-host", "data-brand-name", "data-brand-mark", "data-server-dot"} {
				if findAttr(btn, banned) != nil {
					t.Errorf("the row button carries [%s]", banned)
				}
			}
		})
	}
}

// TestTheSidebarTopIsOneRow: the sidebar's first child is the switcher, and
// its first row holds, in order, the logo button, the search button and the
// close button — the brand row and the server row are one. Nothing else in
// the sidebar draws the brand (no second logo, no name).
//
// 🔴 REGRESSION TEST: on origin/main the brand row (logo, name, search,
// close) came first and the server row second.
func TestTheSidebarTopIsOneRow(t *testing.T) {
	feat := featuresAllOn
	feat.Brand = Brand{Name: "Example Studio", ETag: "e1"}
	doc, err := html.Parse(strings.NewReader(renderString(t, Page(defaultTab, feat))))
	if err != nil {
		t.Fatal(err)
	}
	side := byID(doc, "sidebar")
	if side == nil {
		t.Fatal("no #sidebar")
	}
	first := side.FirstChild
	for first != nil && first.Type != html.ElementNode {
		first = first.NextSibling
	}
	if v, _ := nodeAttr(first, "id"); v != "server-switcher" {
		t.Fatalf("the sidebar's first child is #%s, want #server-switcher", v)
	}
	row := first.FirstChild
	var ids []string
	for c := row.FirstChild; c != nil; c = c.NextSibling {
		if v, ok := nodeAttr(c, "id"); ok {
			ids = append(ids, v)
		}
	}
	if strings.Join(ids, " ") != "server-switcher-toggle sidebar-search sidebar-close" {
		t.Errorf("the top row holds %v, want the logo button, search, close", ids)
	}
	var logos, names int
	walkNodes(side, func(n *html.Node) bool {
		if _, ok := nodeAttr(n, "data-brand-logo"); ok {
			logos++
		}
		if _, ok := nodeAttr(n, "data-brand-name"); ok {
			names++
		}
		return true
	})
	if logos != 1 || names != 0 {
		t.Errorf("the sidebar draws %d logos and %d names, want 1 and 0", logos, names)
	}
}

// TestTheListDrawsNoAddressUnlessNamesCollide: a menu entry's address slot
// starts hidden (servers.js shows it only where two names collide), and an
// entry draws a monogram and a logo slot.
func TestTheListDrawsNoAddressUnlessNamesCollide(t *testing.T) {
	page := renderString(t, serverSwitcher(Brand{}))
	i := strings.Index(page, `<template id="server-item-tpl">`)
	if i < 0 {
		t.Fatal("no item template")
	}
	item := page[i : i+strings.Index(page[i:], "</template>")]
	for _, want := range []string{`<span data-server-host="" hidden`, `<span data-server-monogram="" aria-hidden="true"`, `<img data-server-logo="" alt="" hidden`, `<span data-server-dot="" aria-hidden="true"`} {
		if !strings.Contains(item, want) {
			t.Errorf("the item template lacks %s:\n%s", want, item)
		}
	}
}

// serverTokens is web/css/input.css's --mu-server-<i> values, as lower-case
// hex, once per theme block that defines them.
func serverTokens(t *testing.T) [][]string {
	t.Helper()
	css, err := os.ReadFile("../../web/css/input.css")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`--mu-server-(\d): (\d+) (\d+) (\d+);`)
	var out [][]string
	for _, m := range re.FindAllStringSubmatch(string(css), -1) {
		i, _ := strconv.Atoi(m[1])
		if i == 0 {
			out = append(out, nil)
		}
		if len(out) == 0 || len(out[len(out)-1]) != i {
			t.Fatalf("--mu-server-%s out of order", m[1])
		}
		var px [3]int
		for k := range px {
			px[k], _ = strconv.Atoi(m[2+k])
		}
		out[len(out)-1] = append(out[len(out)-1], fmt.Sprintf("#%02x%02x%02x", px[0], px[1], px[2]))
	}
	return out
}

// TestTheMonogram holds serverMonogram and serverMonogramColour to the
// literal table servers.js is held to as well (e2e/servers.test.mjs): the
// letters, and the colour the class names (its --mu-server-<i> value).
func TestTheMonogram(t *testing.T) {
	raw, err := os.ReadFile("testdata/server-monograms.json")
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Cases []struct{ Name, Letters, Colour string }
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}
	if len(table.Cases) < 10 {
		t.Fatalf("%d cases: the table did not load", len(table.Cases))
	}
	tokens := serverTokens(t)
	if len(tokens) == 0 {
		t.Fatal("no --mu-server-<i> tokens")
	}
	colours := map[string]bool{}
	for _, c := range table.Cases {
		if got := serverMonogram(c.Name); got != c.Letters {
			t.Errorf("serverMonogram(%q) = %q, want %q", c.Name, got, c.Letters)
		}
		i := serverMonogramColour(c.Name)
		if i < 0 || i >= len(tokens[0]) || tokens[0][i] != c.Colour {
			t.Errorf("serverMonogramColour(%q) = %d (%s), want %s", c.Name, i, serverMonogramClasses[i], c.Colour)
		}
		colours[c.Colour] = true
	}
	// A table whose cases all land on one colour cannot see a hard-coded one.
	if len(colours) < 5 {
		t.Errorf("the table covers %d colours", len(colours))
	}
}

// TestTheMonogramPaletteIsTheScripts: the stylesheet's server colours (the
// classes the server draws a monogram with) are servers.js's PALETTE (the
// colours the script redraws it with), colour for colour and in order, in
// every theme; and the script names the row as ServerSwitcherLabel does.
func TestTheMonogramPaletteIsTheScripts(t *testing.T) {
	src, err := os.ReadFile("../../web/static/servers.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`var PALETTE = \[([^\]]*)\];`).FindStringSubmatch(string(src))
	if m == nil {
		t.Fatal("servers.js has no PALETTE")
	}
	var palette []string
	for _, q := range strings.Split(m[1], ",") {
		palette = append(palette, strings.ToLower(strings.Trim(strings.TrimSpace(q), "'")))
	}
	if len(palette) != len(serverMonogramClasses) {
		t.Fatalf("servers.js has %d colours, the server %d classes", len(palette), len(serverMonogramClasses))
	}
	tokens := serverTokens(t)
	if len(tokens) != 2 {
		t.Fatalf("--mu-server-<i> is defined in %d theme blocks, want 2 (dark and light)", len(tokens))
	}
	for k, theme := range tokens {
		if strings.Join(theme, " ") != strings.Join(palette, " ") {
			t.Errorf("theme %d's server colours %v are not servers.js's %v", k, theme, palette)
		}
	}
	// The monogram's letters are black (text-black): each colour must carry
	// them at the AA minimum, in both themes (the same values).
	dark, light := themes(t)
	for i := range serverMonogramClasses {
		for name, th := range map[string]themeTokens{"dark": dark, "light": light} {
			bg, ok := th[fmt.Sprintf("server-%d", i)]
			if !ok {
				t.Fatalf("no %s --mu-server-%d", name, i)
			}
			if r := contrast(rgb{0, 0, 0}, bg); r < 4.5 {
				t.Errorf("black on %s server-%d is %.2f:1, under 4.5", name, i, r)
			}
		}
	}
	for i, cls := range serverMonogramClasses {
		if cls != fmt.Sprintf("bg-server-%d", i) {
			t.Errorf("class %d is %q", i, cls)
		}
	}
	if !strings.Contains(string(src), "var TOGGLE_LABEL = '"+ServerSwitcherLabel("")+"';") {
		t.Errorf("servers.js does not name the row as ServerSwitcherLabel does (%q)", ServerSwitcherLabel(""))
	}
}

// TestTheMonogramLetters holds serverMonogram, code point by code point, to
// the table of every one-code-point name from U+0000 to U+04FF that was
// generated apart from it (Python's full case mapping) and that servers.js is
// held to as well: the blocks where case changes, and every separator there.
// Plus the two shapes the rule exists for, past those blocks: a letter newer
// than Go's tables, and an iota-subscript letter, each kept as it is.
func TestTheMonogramLetters(t *testing.T) {
	raw, err := os.ReadFile("testdata/server-monogram-letters.json")
	if err != nil {
		t.Fatal(err)
	}
	var table struct{ Letters []string }
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}
	if len(table.Letters) != 0x500 {
		t.Fatalf("%d entries, want 0x500", len(table.Letters))
	}
	changed, bad := 0, 0
	for cp, want := range table.Letters {
		if got := serverMonogram(string(rune(cp))); got != want {
			if bad++; bad <= 20 {
				t.Errorf("serverMonogram(U+%04X) = %q, want %q", cp, got, want)
			}
		}
		if want != "?" && want != string(rune(cp)) {
			changed++
		}
	}
	// Positive control: the table does upper-case (a-z alone is 26).
	if changed < 200 {
		t.Errorf("only %d code points change case in the table", changed)
	}
	for _, c := range []struct{ name, want string }{{"ᲊ", "ᲊ"}, {"ᾳ", "ᾳ"}, {"\U00010d70", "\U00010d70"}} {
		if got := serverMonogram(c.name); got != c.want {
			t.Errorf("serverMonogram(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestTheOfferClaimsNoOrigin: the hand-off offer says a LINK asked, never "you
// came from", and starts its focus on Not now.
func TestTheOfferClaimsNoOrigin(t *testing.T) {
	page := renderString(t, serverSwitcherDialogs())
	i := strings.Index(page, `id="server-offer"`)
	if i < 0 {
		t.Fatal("no offer dialog")
	}
	offer := page[i:]
	offer = offer[:strings.Index(offer, "</dialog>")]
	for _, banned := range []string{"came from", "switched here from", "switch back"} {
		if strings.Contains(strings.ToLower(offer), banned) {
			t.Errorf("the offer claims where you came from (%q)", banned)
		}
	}
	if !strings.Contains(offer, "A link asked") || !strings.Contains(offer, "data-server-origin-text") {
		t.Errorf("the offer lacks the neutral copy or the origin slot: %s", offer)
	}
	if !strings.Contains(offer, `id="server-offer-dismiss"`) || !strings.Contains(offer[strings.Index(offer, `id="server-offer-dismiss"`):], "autofocus") {
		t.Errorf("Not now does not take the focus: %s", offer)
	}
}
