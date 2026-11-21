// Package theme is the server's THEME: muster's palette as named tokens, the
// presets an admin picks from, an optional accent colour of the admin's own,
// and the contrast table every theme must pass before it is saved
// (docs/ui-kit.md#the-theme).
//
// 🔴 ONE CONTRAST TABLE. Pairs is the table the palette was designed against.
// internal/ui's TestThemeTokensMeetWCAG checks the shipped stylesheet with it,
// and Choice.Validate checks a theme an admin is about to save with it: the
// test and the save refuse the same things because they read the same rows.
//
// 🔴 THE DEFAULT PRESET IS THE SHIPPED PALETTE. web/css/tokens.css is what
// app.css is built from; the default preset here must equal its dark block,
// token for token (internal/ui TestTheDefaultPresetIsTheShippedPalette), so a
// server with no theme set and a server that chose the default draw the same
// colours. Likewise the shape and motion tokens (Fixed) equal web/css/kit.css's
// own defaults (internal/uikit TestFixedTokensAreKitCSSDefaults).
package theme

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// RGB is one colour.
type RGB struct{ R, G, B uint8 }

// Hex is "#RRGGBB", upper case.
func (c RGB) Hex() string { return fmt.Sprintf("#%02X%02X%02X", c.R, c.G, c.B) }

// Channels is the custom property value: "R G B" (so rgb(var(--x) / a) works).
func (c RGB) Channels() string { return fmt.Sprintf("%d %d %d", c.R, c.G, c.B) }

// ParseHex reads "#rrggbb" (either case). Nothing else: no short form, no
// alpha, no name.
func ParseHex(s string) (RGB, error) {
	if len(s) != 7 || s[0] != '#' {
		return RGB{}, ErrAccentFormat
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return RGB{}, ErrAccentFormat
	}
	return RGB{uint8(v >> 16), uint8(v >> 8), uint8(v)}, nil
}

func channelLum(v uint8) float64 {
	c := float64(v) / 255
	if c <= 0.04045 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

// Luminance is the WCAG 2.x relative luminance.
func Luminance(c RGB) float64 {
	return 0.2126*channelLum(c.R) + 0.7152*channelLum(c.G) + 0.0722*channelLum(c.B)
}

// Contrast is the WCAG 2.x contrast ratio of a and b.
func Contrast(a, b RGB) float64 {
	la, lb := Luminance(a), Luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// Statuses are the status roles, in display order.
var Statuses = []string{"open", "progress", "review", "complete", "error", "warning", "running"}

// Roles are the theme's colour roles that are not a status.
var Roles = []string{"bg", "s1", "s2", "s3", "line", "edge", "text", "text2", "muted", "accent", "on-accent", "focus", "danger", "on-danger"}

// ColourNames is every colour token of a theme, without the --mu- prefix: the
// 14 roles, then each status's -fg and -bg (28).
func ColourNames() []string {
	out := append([]string(nil), Roles...)
	for _, s := range Statuses {
		out = append(out, "st-"+s+"-fg", "st-"+s+"-bg")
	}
	return out
}

// Fixed are the shape, type and motion tokens: the same in every theme, in
// v1. Their values equal web/css/kit.css's :root defaults.
var Fixed = []struct{ Name, Value string }{
	{"radius-sm", ".375rem"},
	{"radius-md", ".5rem"},
	{"radius-lg", "1rem"},
	{"radius-full", "9999px"},
	{"font-sans", `ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif`},
	{"font-mono", `ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace`},
	{"dur-fast", "120ms"},
	{"dur-base", "200ms"},
	{"dur-slow", "320ms"},
	{"ease-standard", "cubic-bezier(.2, 0, 0, 1)"},
	{"ease-enter", "cubic-bezier(0, 0, 0, 1)"},
	{"ease-exit", "cubic-bezier(.3, 0, 1, 1)"},
}

// Durations are the Fixed tokens prefers-reduced-motion sets to 0ms.
var Durations = []string{"dur-fast", "dur-base", "dur-slow"}

// Pair is one row of the contrast table: fg on bg needs at least Min.
type Pair struct {
	FG, BG string
	Min    float64
}

// Pairs is the table the palette was designed against (WCAG 2.2 AA): text
// roles on every surface at 4.5, the accent as link text, the two filled-
// button labels, the focus ring and the control edge at 3 (1.4.11 / 2.4.13),
// and every status's chip label at 4.5 and glyph on the card at 3.
//
// ⚠ `line` IS ABSENT ON PURPOSE: it is a decorative divider between regions,
// which 1.4.11 does not cover. A control's boundary is `edge`, which is here.
func Pairs() []Pair {
	var out []Pair
	for _, fg := range []string{"text", "text2", "muted"} {
		for _, bg := range []string{"bg", "s1", "s2", "s3"} {
			out = append(out, Pair{fg, bg, 4.5})
		}
	}
	for _, bg := range []string{"bg", "s1", "s2", "s3"} {
		out = append(out, Pair{"accent", bg, 4.5})
	}
	out = append(out, Pair{"on-accent", "accent", 4.5}, Pair{"on-danger", "danger", 4.5})
	for _, bg := range []string{"bg", "s1", "s2"} {
		out = append(out, Pair{"focus", bg, 3})
	}
	for _, bg := range []string{"bg", "s1"} {
		out = append(out, Pair{"edge", bg, 3})
	}
	out = append(out, Pair{"danger", "s1", 3})
	for _, st := range Statuses {
		out = append(out,
			Pair{"st-" + st + "-fg", "st-" + st + "-bg", 4.5},
			Pair{"st-" + st + "-fg", "s1", 3})
	}
	return out
}

// Tokens is one theme's colours, keyed by name without the --mu- prefix.
type Tokens map[string]RGB

// Failures checks every Pair against t and returns one line per failure (or
// per missing token), each starting with label.
func Failures(label string, t Tokens) []string {
	var bad []string
	for _, p := range Pairs() {
		fg, okf := t[p.FG]
		bg, okb := t[p.BG]
		if !okf || !okb {
			bad = append(bad, fmt.Sprintf("%s: %s on %s — token missing", label, p.FG, p.BG))
			continue
		}
		if r := Contrast(fg, bg); r < p.Min {
			bad = append(bad, fmt.Sprintf("%s: %s %s on %s %s = %.2f:1, needs %.1f:1",
				label, p.FG, fg.Hex(), p.BG, bg.Hex(), r, p.Min))
		}
	}
	return bad
}

// base is muster's own dark palette ("Brass Roll"), every token: the default
// preset. web/css/tokens.css's dark block, value for value.
var base = Tokens{
	"bg": {17, 15, 28}, "s1": {25, 22, 42}, "s2": {34, 30, 55}, "s3": {45, 40, 70},
	"line": {47, 42, 72}, "edge": {110, 103, 144},
	"text": {238, 235, 247}, "text2": {195, 189, 217}, "muted": {154, 147, 181},
	"accent": {228, 181, 86}, "on-accent": {30, 22, 6}, "focus": {242, 203, 120},
	"danger": {209, 59, 69}, "on-danger": {255, 255, 255},
	"st-open-fg": {185, 179, 211}, "st-open-bg": {36, 32, 58},
	"st-progress-fg": {143, 184, 255}, "st-progress-bg": {27, 37, 71},
	"st-review-fg": {215, 166, 255}, "st-review-bg": {46, 31, 69},
	"st-complete-fg": {127, 214, 164}, "st-complete-bg": {22, 52, 40},
	"st-error-fg": {255, 143, 143}, "st-error-bg": {61, 26, 34},
	"st-warning-fg": {255, 174, 94}, "st-warning-bg": {58, 38, 21},
	"st-running-fg": {95, 224, 192}, "st-running-bg": {18, 51, 47},
}

// Preset is one of the themes an admin picks from. A preset changes the
// accent and the two colours drawn from it; the surfaces, the text and the
// statuses stay muster's (docs/ui-kit.md#the-theme says why).
type Preset struct {
	Name, Label string
	Accent      RGB
	// OnAccent and Focus are set for the default preset, whose values are the
	// shipped palette's; a zero value is derived from Accent (Derive).
	OnAccent, Focus *RGB
}

// Default is the default preset's name.
const Default = "brass"

// Presets is the closed list, the default first.
var Presets = []Preset{
	{Name: Default, Label: "Brass (muster)", Accent: base["accent"], OnAccent: ptr(base["on-accent"]), Focus: ptr(base["focus"])},
	{Name: "harbor", Label: "Harbor (blue)", Accent: RGB{110, 176, 245}},
	{Name: "verdigris", Label: "Verdigris (green)", Accent: RGB{92, 204, 158}},
	{Name: "ember", Label: "Ember (coral)", Accent: RGB{246, 140, 112}},
}

func ptr(c RGB) *RGB { return &c }

// PresetNamed is the preset called name, and whether there is one.
func PresetNamed(name string) (Preset, bool) {
	for _, p := range Presets {
		if p.Name == name {
			return p, true
		}
	}
	return Preset{}, false
}

// mix is a moved toward b by f (0 = a, 1 = b), per channel, rounded.
func mix(a, b RGB, f float64) RGB {
	m := func(x, y uint8) uint8 { return uint8(math.Round(float64(x) + (float64(y)-float64(x))*f)) }
	return RGB{m(a.R, b.R), m(a.G, b.G), m(a.B, b.B)}
}

var (
	black = RGB{0, 0, 0}
	white = RGB{255, 255, 255}
)

// Derive is the two colours an accent brings with it: on-accent, a label on
// the accent fill (the accent darkened to near black, or white, whichever
// reads better), and focus, the ring (the accent lightened toward white).
func Derive(accent RGB) (onAccent, focus RGB) {
	dark := mix(accent, black, 0.87)
	onAccent = dark
	if Contrast(white, accent) > Contrast(dark, accent) {
		onAccent = white
	}
	return onAccent, mix(accent, white, 0.3)
}

// Choice is what an admin saves: a preset, and optionally an accent of their
// own ("#rrggbb", lower case; "" for the preset's own). The zero value is the
// default theme.
type Choice struct {
	Preset string
	Accent string
}

// The refusals. Each is a distinct value, so a caller (and a test) can tell
// which guard refused.
var (
	ErrUnknownPreset = errors.New("theme: there is no such preset")
	ErrAccentFormat  = errors.New("theme: an accent colour is #RRGGBB")
)

// ContrastError refuses a theme that fails the contrast table. Failures names
// each failing pair.
type ContrastError struct{ Failures []string }

func (e *ContrastError) Error() string {
	return "theme: the colours do not meet the contrast table: " + strings.Join(e.Failures, "; ")
}

// Normalize is c with the default preset spelled out and the accent in lower
// case, or an error for an unknown preset or a malformed accent.
func (c Choice) Normalize() (Choice, error) {
	if c.Preset == "" {
		c.Preset = Default
	}
	if _, ok := PresetNamed(c.Preset); !ok {
		return Choice{}, ErrUnknownPreset
	}
	if c.Accent != "" {
		if _, err := ParseHex(c.Accent); err != nil {
			return Choice{}, err
		}
		c.Accent = strings.ToLower(c.Accent)
	}
	return c, nil
}

// IsDefault reports whether c draws exactly the shipped palette: the default
// preset and no accent of its own.
func (c Choice) IsDefault() bool {
	n, err := c.Normalize()
	return err == nil && n.Preset == Default && n.Accent == ""
}

// Resolve is every colour token c draws. It does not check contrast
// (Validate does).
func (c Choice) Resolve() (Tokens, error) {
	n, err := c.Normalize()
	if err != nil {
		return nil, err
	}
	p, _ := PresetNamed(n.Preset)
	t := Tokens{}
	for k, v := range base {
		t[k] = v
	}
	if n.Accent != "" {
		a, _ := ParseHex(n.Accent)
		on, focus := Derive(a)
		t["accent"], t["on-accent"], t["focus"] = a, on, focus
		return t, nil
	}
	on, focus := Derive(p.Accent)
	if p.OnAccent != nil {
		on = *p.OnAccent
	}
	if p.Focus != nil {
		focus = *p.Focus
	}
	t["accent"], t["on-accent"], t["focus"] = p.Accent, on, focus
	return t, nil
}

// Validate is Resolve, refused with a *ContrastError when any Pair fails.
func (c Choice) Validate() (Tokens, error) {
	t, err := c.Resolve()
	if err != nil {
		return nil, err
	}
	if bad := Failures("theme", t); len(bad) > 0 {
		return nil, &ContrastError{Failures: bad}
	}
	return t, nil
}

// Selector is the theme block's selector. `:root:root` out-ranks app.css's
// own `:root, .dark` palette whatever the order the two stylesheets load in,
// and on a plugin's page it is simply the root.
const Selector = ":root:root"

// CSS is the theme block for t: every colour token, then every Fixed token,
// on Selector; then the reduced-motion rule. Names are sorted in
// ColourNames' order, so equal themes are equal bytes.
func CSS(t Tokens) string {
	var b strings.Builder
	b.WriteString(Selector + " {\n  color-scheme: dark;\n")
	for _, n := range ColourNames() {
		b.WriteString("  --mu-" + n + ": " + t[n].Channels() + ";\n")
	}
	for _, f := range Fixed {
		b.WriteString("  --mu-" + f.Name + ": " + f.Value + ";\n")
	}
	b.WriteString("}\n@media (prefers-reduced-motion: reduce) {\n  " + Selector + " {")
	for _, d := range Durations {
		b.WriteString(" --mu-" + d + ": 0ms;")
	}
	b.WriteString(" }\n}\n")
	return b.String()
}

// ETag is the first 16 hex digits of a sha256 over b: the one spelling of an
// ETag for a kit file (and of the theme's own, over its CSS).
func ETag(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// Names lists the preset names, sorted (for an error message).
func Names() []string {
	var out []string
	for _, p := range Presets {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}
