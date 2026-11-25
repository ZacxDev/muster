package pages_test

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/pages"
)

const (
	lightBgAdvice = " paints a light background, but the page is drawn in the server's dark theme. " +
		"Use var(--bg) for the page, var(--s1) for a card or panel, var(--line) for a stripe or divider, var(--accent) for emphasis."
	darkTextAdvice = " is dark text on the page's dark background. Use var(--fg) for text, var(--fg2) for secondary text."
)

func htmlBlocks(src ...string) []pages.Block {
	var out []pages.Block
	for _, s := range src {
		out = append(out, pages.Block{Type: pages.BlockHTML, HTML: s})
	}
	return out
}

// TestThemeNotesNameEachLiteralLightBackgroundAndDarkText: the theme check's
// positives, each with its whole note pinned — every colour syntax it claims
// to read — and its negatives, which must say nothing.
func TestThemeNotesNameEachLiteralLightBackgroundAndDarkText(t *testing.T) {
	bg := func(decl, where string) []string {
		return []string{"blocks[0] (html): " + strconv.Quote(decl) + " in " + where + lightBgAdvice}
	}
	fg := func(decl, where string) []string {
		return []string{"blocks[0] (html): " + strconv.Quote(decl) + " in " + where + darkTextAdvice}
	}
	for _, c := range []struct {
		name, html string
		want       []string
	}{
		// Positives.
		{"short hex", `<style>body{background:#fff}</style>`, bg("background:#fff", "body")},
		{"long hex, background-color", `<style>main { background-color: #FFFFFF; }</style>`, bg("background-color:#FFFFFF", "main")},
		{"near-white hex", `<style>.wrap{background:#fafafa}</style>`, bg("background:#fafafa", ".wrap")},
		{"named white", `<style>html, body{background:white}</style>`, bg("background:white", "html, body")},
		{"named whitesmoke", `<div style="background: whitesmoke">x</div>`, bg("background:whitesmoke", "a <div> style attribute")},
		{"rgb comma", `<style>.card{background:rgb(255,255,255)}</style>`, bg("background:rgb(255,255,255)", ".card")},
		{"rgb space and percent", `<style>.card{background:rgb(100% 100% 100%)}</style>`, bg("background:rgb(100% 100% 100%)", ".card")},
		{"hsl space", `<style>section{background:hsl(0 0% 98%)}</style>`, bg("background:hsl(0 0% 98%)", "section")},
		{"hsla comma, opaque", `<style>section{background:hsla(210,20%,96%,1)}</style>`, bg("background:hsla(210,20%,96%,1)", "section")},
		{"8-digit hex, opaque", `<style>.p{background:#ffffffff}</style>`, bg("background:#ffffffff", ".p")},
		{"shorthand with an image", `<style>.hero{background:#fff url(asset:abc) no-repeat center}</style>`,
			bg("background:#fff url(asset:abc) no-repeat center", ".hero")},
		{"gradient with a light stop", `<style>.g{background:linear-gradient(#111, #f5f5f5)}</style>`,
			bg("background:linear-gradient(#111, #f5f5f5)", ".g")},
		{"inside @media", `<style>@media (min-width:600px){ body{background:#fff} }</style>`, bg("background:#fff", "body")},
		{"black text, no background", `<style>body{color:#000}</style>`, fg("color:#000", "body")},
		{"#111 text", `<style>p, li{color:#111}</style>`, fg("color:#111", "p, li")},
		{"named black in a style attribute", `<p style="color:black">x</p>`, fg("color:black", "a <p> style attribute")},
		{"#333 text on a dark literal background", `<style>.x{background:#1a1a1a;color:#333}</style>`, fg("color:#333", ".x")},
		{"both in one block: the background only", `<style>body{background:#fff;color:#111}</style>`, bg("background:#fff", "body")},

		// Negatives.
		{"tokens", `<style>body{background:var(--bg);color:var(--fg)}.c{background:var(--s1)}</style>`, nil},
		{"a var fallback is not judged", `<style>body{background:var(--bg, #fff);color:var(--fg,#000)}</style>`, nil},
		{"a dark background", `<style>body{background:#111}</style>`, nil},
		{"a mid-luminance accent background", `<style>.b{background:#e4b556}.c{background:#6eb0f5}.d{background:#f68c70}</style>`, nil},
		{"light text", `<style>body{color:#eee}</style>`, nil},
		{"dark text on the accent (its own label)", `<style>.badge{background:var(--accent);color:#111}</style>`, nil},
		{"dark text on a mid literal", `<style>.badge{background:#e4b556;color:#1e1606}</style>`, nil},
		{"a faint white overlay is not light", `<style>.o{background:rgba(255,255,255,.06)}.p{background:#ffffff10}.q{background:rgb(255 255 255 / 5%)}</style>`, nil},
		{"a CSS comment", `<style>/* body{background:#fff} */ body{background:var(--bg)}</style>`, nil},
		{"an HTML comment", `<!-- <div style="background:#fff"></div> -->`, nil},
		{"a script's strings", `<script>el.innerHTML='<div style="background:#fff;color:#000"></div>'</script>`, nil},
		{"border-color and custom properties are not background or color", `<style>.c{border-color:#fff;--my-color:#000;outline-color:white}</style>`, nil},
		{"transparent, inherit, currentColor", `<style>.c{background:transparent;color:inherit}.d{color:currentColor}</style>`, nil},
		{"prose naming a colour", `<p>Paint the background: white, and the color: black.</p>`, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := pages.ThemeNotes(htmlBlocks(c.html)); !reflect.DeepEqual(got, c.want) {
				t.Errorf("ThemeNotes(%s)\n got %q\nwant %q", c.html, got, c.want)
			}
		})
	}
}

// TestThemeNotesThreshold: the boundary is luminance, not a word list — on
// either side of ThemeLightBackgroundMin (#ddd ~0.72 is light, #ccc ~0.60 is
// not) and of ThemeDarkTextMax (#555 ~0.09 is dark, #666 ~0.13 is not).
func TestThemeNotesThreshold(t *testing.T) {
	for _, c := range []struct {
		html string
		n    int
	}{
		{`<style>a{background:#ddd}</style>`, 1}, {`<style>a{background:#ccc}</style>`, 0},
		{`<style>a{color:#555}</style>`, 1}, {`<style>a{color:#666}</style>`, 0},
	} {
		if got := pages.ThemeNotes(htmlBlocks(c.html)); len(got) != c.n {
			t.Errorf("%s: %d notes, want %d: %q", c.html, len(got), c.n, got)
		}
	}
}

// TestThemeNotesNameTheBlockDedupeAndCap: notes name their block by its index
// in the call's blocks (markdown blocks counted, never checked), equal notes
// are said once, and past MaxThemeNotes one note says how many more.
func TestThemeNotesNameTheBlockDedupeAndCap(t *testing.T) {
	blocks := []pages.Block{{Type: pages.BlockMarkdown, Text: "<style>body{background:#fff}</style>"}}
	blocks = append(blocks, htmlBlocks(`<style>body{background:#fff}</style><style>body{background:#fff}</style>`)...)
	if got, want := pages.ThemeNotes(blocks), []string{`blocks[1] (html): "background:#fff" in body` + lightBgAdvice}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dedupe:\n got %q\nwant %q", got, want)
	}

	var css strings.Builder
	for i := 0; i < pages.MaxThemeNotes+3; i++ {
		css.WriteString(".c" + strconv.Itoa(i) + "{background:#fff}")
	}
	got := pages.ThemeNotes(htmlBlocks("<style>" + css.String() + "</style>"))
	if len(got) != pages.MaxThemeNotes+1 {
		t.Fatalf("cap: %d notes, want %d: %q", len(got), pages.MaxThemeNotes+1, got)
	}
	if want := "3 more like these not listed: style the page with the theme's tokens throughout " +
		"(var(--bg), var(--fg), var(--s1), var(--line), var(--accent))."; got[pages.MaxThemeNotes] != want {
		t.Errorf("the cap's note: %q", got[pages.MaxThemeNotes])
	}
	if pages.ThemeNotes(nil) != nil {
		t.Error("no blocks: want nil")
	}
}
