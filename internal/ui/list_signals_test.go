package ui

import (
	"strconv"
	"strings"
	"testing"
)

// idleDotToken is the --mu-* token a dot's bg-idle-<i> utility draws with
// (tailwind.config.js maps idle.<i> to role('idle-<i>')), or "" for a class
// that is not on the recency scale.
func idleDotToken(class string) string {
	for _, c := range strings.Fields(class) {
		if strings.HasPrefix(c, "bg-idle-") {
			return strings.TrimPrefix(c, "bg-")
		}
	}
	return ""
}

// chroma is how far a colour is from grey: its largest channel minus its
// smallest. Gruvbox's red #CC241D is 175; the palette's neutral `muted` is 34.
func chroma(c rgb) int {
	hi, lo := c.r, c.r
	for _, v := range []int{c.g, c.b} {
		hi, lo = max(hi, v), min(lo, v)
	}
	return hi - lo
}

// neutralChromaMax is the most a dot may lean towards any hue. The palette's
// own neutrals (text, text2, muted, edge) carry a violet tint of 12-34; a red,
// orange, green or purple dot is far past it.
const neutralChromaMax = 34

// idleDotProblems is every way a theme's recency dots break the design: a dot
// that is the danger / error colour, one with a hue (not neutral), one under
// 3:1 on a surface a row sits on, or a ramp that does not fade monotonically.
func idleDotProblems(label string, tok themeTokens, classes []string) []string {
	var bad []string
	prev := -1.0
	for i, class := range classes {
		name := idleDotToken(class)
		c, ok := tok[name]
		if name == "" || !ok {
			bad = append(bad, label+": dot class "+class+" draws with no --mu-idle-* token")
			continue
		}
		for _, red := range []string{"danger", "st-error-fg"} {
			if c == tok[red] {
				bad = append(bad, label+": --mu-"+name+" is the "+red+" colour "+c.hex())
			}
		}
		if ch := chroma(c); ch > neutralChromaMax {
			bad = append(bad, label+": --mu-"+name+" "+c.hex()+" is not neutral (chroma "+strconv.Itoa(ch)+")")
		}
		for _, surface := range []string{"bg", "s1", "s2"} {
			if r := contrast(c, tok[surface]); r < 3 {
				bad = append(bad, label+": --mu-"+name+" "+c.hex()+" on "+surface+" is under 3:1")
			}
		}
		// The fade: each step strictly closer to the page than the one before
		// (contrast against bg strictly decreasing), so the steps stay ordered.
		if r := contrast(c, tok["bg"]); i > 0 && r >= prev {
			bad = append(bad, label+": --mu-"+name+" does not fade from the step before it")
		} else {
			prev = r
		}
	}
	return bad
}

// TestIdleDotsAreNeutral: an idle session's dot is grey, never red — red keeps
// meaning trouble (the operator's decision). Checked against the classes
// the renderers actually emit (every recencySteps bucket, and the session
// card's idle status dot) resolved to the token values tokens.css ships, in
// both themes.
func TestIdleDotsAreNeutral(t *testing.T) {
	if len(recencySteps) != 8 {
		t.Fatalf("recencySteps has %d buckets, want 8", len(recencySteps))
	}
	var classes []string
	for _, s := range recencySteps {
		classes = append(classes, s.Class)
	}
	dark, light := themes(t)
	for _, f := range append(idleDotProblems("dark", dark, classes), idleDotProblems("light", light, classes)...) {
		t.Error(f)
	}
	// The session card's idle dot is not on the ramp's order, so check it alone.
	card := []string{sessionCardDotClass[SessionCardIdle]}
	for _, f := range append(idleDotProblems("dark card", dark, card), idleDotProblems("light card", light, card)...) {
		t.Error(f)
	}

	// 🔴 NEGATIVE CONTROLS on the shipped tokens: each failure mode must be
	// seen, and only it.
	for _, tc := range []struct {
		what string
		set  func(t themeTokens)
		want string
	}{
		{"danger", func(t themeTokens) { t["idle-5"] = t["danger"] }, "is the danger colour"},
		{"gruvbox red", func(t themeTokens) { t["idle-5"] = rgb{204, 36, 29} }, "is not neutral"},
		{"near the page", func(t themeTokens) { t["idle-7"] = t["s2"] }, "on s2 is under 3:1"},
		{"out of order", func(t themeTokens) { t["idle-3"], t["idle-4"] = t["idle-4"], t["idle-3"] }, "does not fade"},
	} {
		broken := themeTokens{}
		for k, v := range dark {
			broken[k] = v
		}
		tc.set(broken)
		got := strings.Join(idleDotProblems("control", broken, classes), "\n")
		if !strings.Contains(got, tc.want) {
			t.Errorf("negative control %q: want a %q failure, got:\n%s", tc.what, tc.want, got)
		}
	}
}
