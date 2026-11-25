package pages

import (
	"log"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/theme"
)

// theme_notes.go — the theme check page_create and page_update answer with,
// beside the phone check: a note for each declaration in an html block's CSS
// that paints a LIGHT background, or DARK text with no background of its own,
// with a literal colour. A page is drawn in the server's dark theme
// (Content), so either one is a light patch, or unreadable text, on it. It
// refuses nothing; it names the declaration and the token to use instead.
//
// What it reads: the bodies of the block's <style> elements and its style="…"
// attributes, after HTML and CSS comments are removed and every <script> is
// dropped (a script's strings are not CSS). What it judges: a colour LITERAL —
// hex (3, 4, 6 or 8 digits), rgb()/rgba(), hsl()/hsla() and the CSS named
// colours — composited over the theme's background (so a faint white overlay
// is not "light") and measured by relative luminance (theme.Luminance). A
// var(…) (fallback included), currentColor, transparent, inherit and url(…)
// are not literals and are never judged.
//
// Dark text is judged per declaration block: `color:` with a dark literal is
// noted only when that block sets no background, or one whose literals are
// all dark too — a `.badge{background:var(--accent);color:#111}` is the
// accent's own label, and is left alone.

const (
	// ThemeLightBackgroundMin is the relative luminance, composited over the
	// theme's background, at or above which a background is light (#ddd is
	// about 0.72, #ccc about 0.60, the presets' accents 0.39 to 0.50).
	ThemeLightBackgroundMin = 0.7
	// ThemeDarkTextMax is the relative luminance, composited, below which a
	// text colour is dark (#333 is about 0.033, #555 about 0.09; on the
	// theme's background either reads under 3:1).
	ThemeDarkTextMax = 0.1
	// MaxThemeNotes is the most theme notes one version gets; past it, one
	// more says how many were left out.
	MaxThemeNotes = 6
)

var (
	tnHTMLComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	tnScript      = regexp.MustCompile(`(?is)<script\b.*?</script\s*>`)
	tnStyleElem   = regexp.MustCompile(`(?is)<style\b[^>]*>(.*?)</style\s*>`)
	tnStyleAttr   = regexp.MustCompile(`(?is)<([a-z][a-z0-9-]*)\b[^>]*?\sstyle\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	tnCSSComment  = regexp.MustCompile(`(?s)/\*.*?\*/`)
	tnRule        = regexp.MustCompile(`([^{}]*)\{([^{}]*)\}`)
	tnDecl        = regexp.MustCompile(`(?i)(?:^|[;{\s])(background-color|background|color)\s*:\s*([^;{}]*)`)
	tnHex         = regexp.MustCompile(`#([0-9a-fA-F]{3,8})\b`)
	tnFunc        = regexp.MustCompile(`(?i)\b(rgba?|hsla?)\(([^()]*)\)`)
	tnWord        = regexp.MustCompile(`[a-zA-Z]+`)
	tnNum         = regexp.MustCompile(`^([+-]?(?:\d+\.?\d*|\.\d+)(?:e[+-]?\d+)?)(%|deg|grad|rad|turn)?$`)
)

// themeBG is the background a colour is composited over: the theme's own
// (the presets change only the accent, so it is every instance's).
var themeBG = func() theme.RGB {
	t, _ := theme.Choice{}.Validate()
	return t["bg"]
}()

// ThemeNotes is the theme check of a version's blocks: one note per html
// block and offending declaration, in block order, each naming the block
// (blocks[i], 0-based, as the call's blocks array), the declaration and the
// token to use. Equal notes are said once; at most MaxThemeNotes, then one
// saying how many more. nil when there is nothing to say.
//
// The check is advisory and runs after the version is published, on paths
// (an in-process agent's first-turn tool call) with no recover of their own,
// so a parser fault here returns no notes rather than taking the server down.
func ThemeNotes(blocks []Block) (notes []string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("pages: theme check fault, its notes dropped: %v", r)
			notes = nil
		}
	}()
	seen := map[string]bool{}
	more := 0
	for i, b := range blocks {
		if b.Type != BlockHTML {
			continue
		}
		at := "blocks[" + strconv.Itoa(i) + "] (html): "
		for _, n := range htmlThemeNotes(b.HTML) {
			n = at + n
			if seen[n] {
				continue
			}
			seen[n] = true
			if len(notes) >= MaxThemeNotes {
				more++
				continue
			}
			notes = append(notes, n)
		}
	}
	if more > 0 {
		notes = append(notes, strconv.Itoa(more)+" more like these not listed: style the page with the theme's tokens throughout (var(--bg), var(--fg), var(--s1), var(--line), var(--accent)).")
	}
	return notes
}

// cssBlock is one declaration block and where it is.
type cssBlock struct{ where, decls string }

// htmlThemeNotes is one html block's notes, unprefixed.
func htmlThemeNotes(src string) []string {
	src = tnHTMLComment.ReplaceAllString(src, "")
	src = tnScript.ReplaceAllString(src, "")
	var blocks []cssBlock
	for _, m := range tnStyleElem.FindAllStringSubmatch(src, -1) {
		css := tnCSSComment.ReplaceAllString(m[1], "")
		for _, r := range tnRule.FindAllStringSubmatch(css, -1) {
			blocks = append(blocks, cssBlock{where: strings.Join(strings.Fields(r[1]), " "), decls: r[2]})
		}
	}
	for _, m := range tnStyleAttr.FindAllStringSubmatch(tnStyleElem.ReplaceAllString(src, ""), -1) {
		blocks = append(blocks, cssBlock{where: "a <" + strings.ToLower(m[1]) + "> style attribute", decls: tnCSSComment.ReplaceAllString(m[2]+m[3], "")})
	}
	var notes []string
	for _, b := range blocks {
		notes = append(notes, blockThemeNotes(b)...)
	}
	return notes
}

func blockThemeNotes(b cssBlock) []string {
	type decl struct{ prop, value string }
	var bgs, colors []decl
	for _, m := range tnDecl.FindAllStringSubmatch(b.decls, -1) {
		d := decl{strings.ToLower(m[1]), strings.TrimSpace(m[2])}
		if d.prop == "color" {
			colors = append(colors, d)
		} else {
			bgs = append(bgs, d)
		}
	}
	where := b.where
	if where == "" {
		where = "a rule"
	}
	spell := func(d decl) string { return strconv.Quote(d.prop + ":" + d.value) }
	var notes []string
	// A block whose background is undecided (a var(), a light or mid
	// literal) owns its text's contrast; only one with no background, or a
	// dark one, puts its text on the page's dark background.
	textOnDark := true
	for _, d := range bgs {
		lits, other := colourLiterals(d.value)
		light := false
		for _, c := range lits {
			if theme.Luminance(c) >= ThemeLightBackgroundMin {
				light = true
			}
			if theme.Luminance(c) >= ThemeDarkTextMax {
				textOnDark = false
			}
		}
		if other {
			textOnDark = false
		}
		if light {
			notes = append(notes, spell(d)+" in "+where+" paints a light background, but the page is drawn in the server's dark theme. "+
				"Use var(--bg) for the page, var(--s1) for a card or panel, var(--line) for a stripe or divider, var(--accent) for emphasis.")
		}
	}
	if !textOnDark {
		return notes
	}
	for _, d := range colors {
		lits, _ := colourLiterals(d.value)
		for _, c := range lits {
			if theme.Luminance(c) < ThemeDarkTextMax {
				notes = append(notes, spell(d)+" in "+where+" is dark text on the page's dark background. "+
					"Use var(--fg) for text, var(--fg2) for secondary text.")
				break
			}
		}
	}
	return notes
}

// colourLiterals is every colour literal in a declaration's value,
// composited over themeBG, and whether the value also has something that is
// not a literal but may be a colour (a var(), currentColor), whose luminance
// is unknown. var(…) and url(…) are removed first, nested parentheses
// included, so a var()'s fallback is never judged.
func colourLiterals(v string) (out []theme.RGB, other bool) {
	v, other = stripFuncs(v, "var", "url", "env")
	for _, m := range tnFunc.FindAllStringSubmatch(v, -1) {
		if c, a, ok := parseColourFunc(strings.ToLower(m[1]), m[2]); ok {
			out = append(out, composite(c, a))
		}
	}
	v = tnFunc.ReplaceAllString(v, " ")
	for _, m := range tnHex.FindAllStringSubmatch(v, -1) {
		if c, a, ok := parseHexAlpha(m[1]); ok {
			out = append(out, composite(c, a))
		}
	}
	v = tnHex.ReplaceAllString(v, " ")
	for _, w := range tnWord.FindAllString(v, -1) {
		lw := strings.ToLower(w)
		if c, ok := namedColours[lw]; ok {
			out = append(out, c)
		} else if lw == "currentcolor" {
			other = true
		}
	}
	return out, other
}

// asciiLower lowers A-Z only, so the result is byte-for-byte as long as s.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// stripFuncs removes every call of the named functions (case-insensitive,
// nested parentheses included) and reports whether one was a var() or env().
func stripFuncs(v string, names ...string) (string, bool) {
	// ASCII-only lowering keeps every byte at its index; strings.ToLower
	// shrinks some runes (the Kelvin sign to "k") and the indexes into v
	// below would then run past the end of lower.
	lower := asciiLower(v)
	var b strings.Builder
	dynamic := false
	for i := 0; i < len(v); {
		hit := ""
		for _, n := range names {
			if strings.HasPrefix(lower[i:], n+"(") && (i == 0 || !isIdent(lower[i-1])) {
				hit = n
				break
			}
		}
		if hit == "" {
			b.WriteByte(v[i])
			i++
			continue
		}
		if hit != "url" {
			dynamic = true
		}
		depth := 0
		j := i + len(hit)
		for ; j < len(v); j++ {
			if v[j] == '(' {
				depth++
			} else if v[j] == ')' {
				depth--
				if depth == 0 {
					j++
					break
				}
			}
		}
		b.WriteByte(' ')
		i = j
	}
	return b.String(), dynamic
}

func isIdent(c byte) bool {
	return c == '-' || c == '_' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// composite is c at alpha a over themeBG.
func composite(c theme.RGB, a float64) theme.RGB {
	if a >= 1 {
		return c
	}
	if a < 0 {
		a = 0
	}
	m := func(x, y uint8) uint8 { return uint8(math.Round(float64(x)*a + float64(y)*(1-a))) }
	return theme.RGB{R: m(c.R, themeBG.R), G: m(c.G, themeBG.G), B: m(c.B, themeBG.B)}
}

// parseHexAlpha reads 3, 4, 6 or 8 hex digits.
func parseHexAlpha(h string) (theme.RGB, float64, bool) {
	switch len(h) {
	case 3, 4:
		var x strings.Builder
		for i := 0; i < len(h); i++ {
			x.WriteByte(h[i])
			x.WriteByte(h[i])
		}
		h = x.String()
	case 6, 8:
	default:
		return theme.RGB{}, 0, false
	}
	n, err := strconv.ParseUint(h, 16, 64)
	if err != nil {
		return theme.RGB{}, 0, false
	}
	a := 1.0
	if len(h) == 8 {
		a = float64(n&0xff) / 255
		n >>= 8
	}
	return theme.RGB{R: uint8(n >> 16), G: uint8(n >> 8), B: uint8(n)}, a, true
}

// parseColourFunc reads the arguments of rgb()/rgba()/hsl()/hsla(), in the
// comma or the space syntax, with an optional alpha ("/ a" or a fourth
// argument), each a number or a percentage.
func parseColourFunc(fn, args string) (theme.RGB, float64, bool) {
	args = strings.ReplaceAll(args, ",", " ")
	args = strings.ReplaceAll(args, "/", " / ")
	f := strings.Fields(args)
	alpha := 1.0
	if len(f) == 5 && f[3] == "/" {
		f = append(f[:3], f[4])
	}
	if len(f) == 4 {
		a, unit, ok := cssNum(f[3])
		if !ok || (unit != "" && unit != "%") {
			return theme.RGB{}, 0, false
		}
		if unit == "%" {
			a /= 100
		}
		alpha = clamp(a, 0, 1)
		f = f[:3]
	}
	if len(f) != 3 {
		return theme.RGB{}, 0, false
	}
	var x [3]float64
	var u [3]string
	for i, s := range f {
		n, unit, ok := cssNum(s)
		if !ok {
			return theme.RGB{}, 0, false
		}
		x[i], u[i] = n, unit
	}
	if strings.HasPrefix(fn, "rgb") {
		var c [3]uint8
		for i := range x {
			v := x[i]
			if u[i] == "%" {
				v = v * 255 / 100
			} else if u[i] != "" {
				return theme.RGB{}, 0, false
			}
			c[i] = uint8(math.Round(clamp(v, 0, 255)))
		}
		return theme.RGB{R: c[0], G: c[1], B: c[2]}, alpha, true
	}
	h := x[0]
	switch u[0] {
	case "", "deg":
	case "turn":
		h *= 360
	case "rad":
		h = h * 180 / math.Pi
	case "grad":
		h = h * 0.9
	default:
		return theme.RGB{}, 0, false
	}
	// hsl() takes its saturation and lightness as percentages (a bare number
	// in the space syntax means the same).
	s, l := clamp(x[1], 0, 100)/100, clamp(x[2], 0, 100)/100
	return hslRGB(h, s, l), alpha, true
}

func cssNum(s string) (float64, string, bool) {
	m := tnNum.FindStringSubmatch(strings.ToLower(s))
	if m == nil {
		return 0, "", false
	}
	n, err := strconv.ParseFloat(m[1], 64)
	return n, m[2], err == nil
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// hslRGB is CSS Color 4's hsl-to-rgb.
func hslRGB(h, s, l float64) theme.RGB {
	h = math.Mod(math.Mod(h, 360)+360, 360)
	f := func(n float64) uint8 {
		k := math.Mod(n+h/30, 12)
		a := s * math.Min(l, 1-l)
		return uint8(math.Round(255 * (l - a*math.Max(-1, math.Min(k-3, math.Min(9-k, 1))))))
	}
	return theme.RGB{R: f(0), G: f(8), B: f(4)}
}

// namedColours is CSS's named colours (CSS Color 4), each judged by its
// value like any other literal.
var namedColours = func() map[string]theme.RGB {
	const list = "aliceblue f0f8ff antiquewhite faebd7 aqua 00ffff aquamarine 7fffd4 azure f0ffff beige f5f5dc bisque ffe4c4 black 000000 " +
		"blanchedalmond ffebcd blue 0000ff blueviolet 8a2be2 brown a52a2a burlywood deb887 cadetblue 5f9ea0 chartreuse 7fff00 " +
		"chocolate d2691e coral ff7f50 cornflowerblue 6495ed cornsilk fff8dc crimson dc143c cyan 00ffff darkblue 00008b " +
		"darkcyan 008b8b darkgoldenrod b8860b darkgray a9a9a9 darkgreen 006400 darkgrey a9a9a9 darkkhaki bdb76b darkmagenta 8b008b " +
		"darkolivegreen 556b2f darkorange ff8c00 darkorchid 9932cc darkred 8b0000 darksalmon e9967a darkseagreen 8fbc8f " +
		"darkslateblue 483d8b darkslategray 2f4f4f darkslategrey 2f4f4f darkturquoise 00ced1 darkviolet 9400d3 deeppink ff1493 " +
		"deepskyblue 00bfff dimgray 696969 dimgrey 696969 dodgerblue 1e90ff firebrick b22222 floralwhite fffaf0 forestgreen 228b22 " +
		"fuchsia ff00ff gainsboro dcdcdc ghostwhite f8f8ff gold ffd700 goldenrod daa520 gray 808080 green 008000 greenyellow adff2f " +
		"grey 808080 honeydew f0fff0 hotpink ff69b4 indianred cd5c5c indigo 4b0082 ivory fffff0 khaki f0e68c lavender e6e6fa " +
		"lavenderblush fff0f5 lawngreen 7cfc00 lemonchiffon fffacd lightblue add8e6 lightcoral f08080 lightcyan e0ffff " +
		"lightgoldenrodyellow fafad2 lightgray d3d3d3 lightgreen 90ee90 lightgrey d3d3d3 lightpink ffb6c1 lightsalmon ffa07a " +
		"lightseagreen 20b2aa lightskyblue 87cefa lightslategray 778899 lightslategrey 778899 lightsteelblue b0c4de lightyellow ffffe0 " +
		"lime 00ff00 limegreen 32cd32 linen faf0e6 magenta ff00ff maroon 800000 mediumaquamarine 66cdaa mediumblue 0000cd " +
		"mediumorchid ba55d3 mediumpurple 9370db mediumseagreen 3cb371 mediumslateblue 7b68ee mediumspringgreen 00fa9a " +
		"mediumturquoise 48d1cc mediumvioletred c71585 midnightblue 191970 mintcream f5fffa mistyrose ffe4e1 moccasin ffe4b5 " +
		"navajowhite ffdead navy 000080 oldlace fdf5e6 olive 808000 olivedrab 6b8e23 orange ffa500 orangered ff4500 orchid da70d6 " +
		"palegoldenrod eee8aa palegreen 98fb98 paleturquoise afeeee palevioletred db7093 papayawhip ffefd5 peachpuff ffdab9 " +
		"peru cd853f pink ffc0cb plum dda0dd powderblue b0e0e6 purple 800080 rebeccapurple 663399 red ff0000 rosybrown bc8f8f " +
		"royalblue 4169e1 saddlebrown 8b4513 salmon fa8072 sandybrown f4a460 seagreen 2e8b57 seashell fff5ee sienna a0522d " +
		"silver c0c0c0 skyblue 87ceeb slateblue 6a5acd slategray 708090 slategrey 708090 snow fffafa springgreen 00ff7f " +
		"steelblue 4682b4 tan d2b48c teal 008080 thistle d8bfd8 tomato ff6347 turquoise 40e0d0 violet ee82ee wheat f5deb3 " +
		"white ffffff whitesmoke f5f5f5 yellow ffff00 yellowgreen 9acd32"
	f := strings.Fields(list)
	m := make(map[string]theme.RGB, len(f)/2)
	for i := 0; i+1 < len(f); i += 2 {
		c, _, _ := parseHexAlpha(f[i+1])
		m[f[i]] = c
	}
	return m
}()
