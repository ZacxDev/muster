package ui

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// slotPalette is a measured tmux slot palette (20 slots, 16 distinct
// colours): the gruvbox-family values a host's slot table assigns.
var slotPalette = []string{
	"#b8bb26", "#d79921", "#b16286", "#83a598", "#cc241d", "#689d6a", "#fe8019", "#d3869b", "#458588", "#928374",
	"#ebdbb2", "#af3a03", "#fabd2f", "#d65d0e", "#8f3f71", "#79740e",
}

// asRGB is the test palette's type for one of identity_color.go's values.
func asRGB(c idRGB) rgb { return rgb{c.r, c.g, c.b} }

// TestIdentitySurfacesAreTheThemeTokens: the surfaces the variants are moved
// against are input.css's own s1 and bg, per theme — parsed from the
// stylesheet by the parser TestThemeTokensMeetWCAG uses, never restated.
func TestIdentitySurfacesAreTheThemeTokens(t *testing.T) {
	dark, light := themes(t)
	for _, c := range []struct {
		name string
		got  []idRGB
		toks themeTokens
	}{{"dark", identitySurfacesDark, dark}, {"light", identitySurfacesLight, light}} {
		if len(c.got) != 2 || asRGB(c.got[0]) != c.toks["s1"] || asRGB(c.got[1]) != c.toks["bg"] {
			t.Fatalf("%s identity surfaces = %v, want input.css's s1 %v and bg %v", c.name, c.got, c.toks["s1"], c.toks["bg"])
		}
	}
}

// TestIdentityVariantsMeetContrastInBothThemes: for 1,000 random colours, the
// edge cases (white, black, pure yellow, pure blue, a pale cream) and the
// measured slot palette, the border variant meets 3:1 and the title 4.5:1
// against BOTH the s1 and the bg of its theme — checked with the theme test's
// own contrast function, not identity_color.go's — and each keeps its OKLCH
// hue. A colour that already meets its target is left exactly as it is.
func TestIdentityVariantsMeetContrastInBothThemes(t *testing.T) {
	dark, light := themes(t)
	colours := append([]string{"#ffffff", "#000000", "#ffff00", "#0000ff", "#fbf1c7", "#ff0000", "#00ff00", "#808080"}, slotPalette...)
	rnd := rand.New(rand.NewSource(20261021))
	for i := 0; i < 1000; i++ {
		colours = append(colours, fmt.Sprintf("#%06x", rnd.Intn(1<<24)))
	}
	check := func(c, which string, got idRGB, toks themeTokens, min float64) {
		t.Helper()
		for _, s := range []string{"s1", "bg"} {
			if r := contrast(asRGB(got), toks[s]); r < min {
				t.Errorf("%s %s = %v: %.2f:1 on %s, want ≥ %.1f", c, which, got, r, s, min)
			}
		}
	}
	checked := 0
	for _, c := range colours {
		tint, ok := identityTint(c)
		if !ok {
			t.Fatalf("identityTint(%q) refused a wire colour", c)
		}
		check(c, "border (dark)", tint.BorderDark, dark, 3)
		check(c, "border (light)", tint.BorderLight, light, 3)
		check(c, "title (dark)", tint.TitleDark, dark, 4.5)
		check(c, "title (light)", tint.TitleLight, light, 4.5)
		checked++
		// The hue is kept for any colour with real chroma (an 8-bit round trip
		// moves a near-grey's hue arbitrarily, so those are not compared).
		base := parseWire(c)
		if _, ca, cb := toOKLab(base); math.Hypot(ca, cb) > 0.05 {
			for _, v := range []idRGB{tint.BorderDark, tint.BorderLight, tint.TitleDark, tint.TitleLight} {
				_, va, vb := toOKLab(v)
				if math.Hypot(va, vb) < 0.02 {
					continue // moved to (near) white or black: no hue left to keep
				}
				if d := hueDelta(math.Atan2(cb, ca), math.Atan2(vb, va)); d > 4 {
					t.Errorf("%s → %v: hue moved %.1f°, want ≤ 4°", c, v, d)
				}
			}
		}
	}
	if checked != len(colours) || checked < 1000 {
		t.Fatalf("instrument check: %d colours checked", checked)
	}
	// What the palette costs, per theme (the design's table).
	moved := func(pick func(IdentityTint) idRGB) (n int, maxDL float64) {
		for _, c := range slotPalette {
			tint, _ := identityTint(c)
			v := pick(tint)
			if v != parseWire(c) {
				n++
				l0, _, _ := toOKLab(parseWire(c))
				l1, _, _ := toOKLab(v)
				maxDL = math.Max(maxDL, math.Abs(l1-l0))
			}
		}
		return
	}
	for _, m := range []struct {
		name string
		pick func(IdentityTint) idRGB
	}{
		{"border, dark", func(t IdentityTint) idRGB { return t.BorderDark }},
		{"border, light", func(t IdentityTint) idRGB { return t.BorderLight }},
		{"title, dark", func(t IdentityTint) idRGB { return t.TitleDark }},
		{"title, light", func(t IdentityTint) idRGB { return t.TitleLight }},
	} {
		n, dl := moved(m.pick)
		t.Logf("slot palette, %s: %d of %d colours moved, largest ΔL %.2f", m.name, n, len(slotPalette), dl)
	}
}

// TestAPaleColourIsDarkenedOnWhite is the adjustment's own red/green case:
// pale yellow on the light theme's white card fails 3:1 as it is, so its
// light variants must have MOVED — darker, same hue — while its dark-theme
// title (already readable there) is left exactly as it was.
func TestAPaleColourIsDarkenedOnWhite(t *testing.T) {
	const pale = "#fbf1c7"
	_, light := themes(t)
	if r := contrast(asRGB(parseWire(pale)), light["s1"]); r >= 3 {
		t.Fatalf("instrument check: %s is already %.2f:1 on white, so this case proves nothing", pale, r)
	}
	tint, _ := identityTint(pale)
	l0, _, _ := toOKLab(parseWire(pale))
	for _, v := range []idRGB{tint.BorderLight, tint.TitleLight} {
		if l, _, _ := toOKLab(v); l >= l0 {
			t.Fatalf("%s's light variant %v is not darker (L %.3f vs %.3f)", pale, v, l, l0)
		}
	}
	if tint.TitleDark != parseWire(pale) {
		t.Fatalf("a colour already readable on the dark card was moved: %v", tint.TitleDark)
	}
}

// TestTheStylesheetTintsTheBorderAndTheTitle pins the CSS half: the variables
// switch with the theme, and the border, ring and title read them — with
// selectors that out-rank the card's own utilities.
func TestTheStylesheetTintsTheBorderAndTheTitle(t *testing.T) {
	css := readInputCSS(t)
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	for sel, want := range map[string]string{
		"[data-identity]":                   "--id-bd: var(--id-bd-d); --id-tt: var(--id-tt-d);",
		":root:not(.dark) [data-identity]":  "--id-bd: var(--id-bd-l); --id-tt: var(--id-tt-l);",
		"[data-identity][data-mirror-card]": "border-color: rgb(var(--id-bd)); --tw-ring-color: rgb(var(--id-bd));",
		"[data-identity] [data-mirror-name], [data-identity] [data-card-title]": "color: rgb(var(--id-tt));",
	} {
		if got := norm(cssBlock(t, css, sel)); got != want {
			t.Errorf("%s { %s }, want { %s }", sel, got, want)
		}
	}
}

// parseWire is a wire colour as idRGB.
func parseWire(c string) idRGB {
	var r, g, b int
	fmt.Sscanf(c, "#%02x%02x%02x", &r, &g, &b)
	return idRGB{r, g, b}
}

// hueDelta is the angle between two hues (radians in), in degrees.
func hueDelta(a, b float64) float64 {
	d := math.Abs(a-b) * 180 / math.Pi
	if d > 180 {
		d = 360 - d
	}
	return d
}

// TestTheTintIsOnTheCardsOnly: the operator asked for the CARDS — and later
// for the sidebar's agent rows to carry the same signal (nav_agents.go, which
// tints only the row's left edge). The callers of identityAttrs are those two
// — not the rendered view's header, not the terminal page's — and the
// stylesheet's title rule reaches only a card's title elements. The ledger
// fails when the set of callers grows or shrinks.
func TestTheTintIsOnTheCardsOnly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	callers := map[string]int{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "identity_color.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(string(b), "identityAttrs("); n > 0 {
			callers[f] = n
		}
	}
	// The sidebar row only: the Agents list's mirrored card, its other
	// caller, is retired with the list.
	if len(callers) != 1 || callers["nav_agents.go"] != 1 {
		t.Fatalf("identityAttrs is called from %v, want exactly once, from nav_agents.go (the sidebar row)", callers)
	}
	if css := readInputCSS(t); strings.Contains(css, "data-identity-title") {
		t.Fatal("input.css still tints a [data-identity-title] (a page header's title)")
	}
}

// TestTheIdentityCacheStaysBounded: colours come from hosts, so a stream of
// distinct ones must not grow the cache past identityCacheMax. The control is
// the slot palette: once computed, each of its 16 colours is a HIT, and the
// palette is resident again right after a flood.
func TestTheIdentityCacheStaysBounded(t *testing.T) {
	size := func() (n, hits int) {
		identityCache.Lock()
		defer identityCache.Unlock()
		return len(identityCache.m), identityCache.hits
	}
	for _, c := range slotPalette {
		identityTint(c)
	}
	_, h0 := size()
	for _, c := range slotPalette {
		identityTint(c)
	}
	if _, h1 := size(); h1-h0 != len(slotPalette) {
		t.Fatalf("control: the warm slot palette scored %d hits of %d", h1-h0, len(slotPalette))
	}
	const n = 3*identityCacheMax + 7
	peak := 0
	for i := 0; i < n; i++ {
		identityTint(fmt.Sprintf("#%06x", 0x100000+i*37))
		if l, _ := size(); l > peak {
			peak = l
		}
	}
	if peak > identityCacheMax {
		t.Fatalf("after %d distinct colours the cache peaked at %d entries, over %d", n, peak, identityCacheMax)
	}
	if peak < identityCacheMax {
		t.Fatalf("instrument check: the flood only reached %d entries, so the bound was never tested", peak)
	}
	for _, c := range slotPalette {
		identityTint(c)
	}
	_, h2 := size()
	for _, c := range slotPalette {
		identityTint(c)
	}
	if _, h3 := size(); h3-h2 != len(slotPalette) {
		t.Fatalf("after the flood the slot palette scored %d hits of %d", h3-h2, len(slotPalette))
	}
	t.Logf("%d distinct colours: the cache peaked at %d (max %d)", n, peak, identityCacheMax)
}
