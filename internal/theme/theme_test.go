package theme

import (
	"errors"
	"math"
	"math/rand"
	"strings"
	"testing"
)

// independentContrast is the WCAG ratio computed here from the formula, not by
// calling Contrast, so a broken Contrast cannot agree with itself.
func independentContrast(a, b RGB) float64 {
	lin := func(v uint8) float64 {
		c := float64(v) / 255
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	l := func(c RGB) float64 { return 0.2126*lin(c.R) + 0.7152*lin(c.G) + 0.0722*lin(c.B) }
	x, y := l(a), l(b)
	if x < y {
		x, y = y, x
	}
	return (x + 0.05) / (y + 0.05)
}

func TestContrastAnchors(t *testing.T) {
	if r := Contrast(RGB{0, 0, 0}, RGB{255, 255, 255}); math.Abs(r-21) > 1e-9 {
		t.Fatalf("contrast(black, white) = %v, want 21", r)
	}
	if r := Contrast(RGB{0x77, 0x77, 0x77}, RGB{255, 255, 255}); math.Abs(r-4.48) > 0.01 {
		t.Fatalf("contrast(#777, white) = %.3f, want 4.48", r)
	}
}

// TestThePairTableIsTheDesignedTable pins its size: 38 rows (a table that
// shrank would check less while reporting the same green), every status
// present, and `line` absent.
func TestThePairTableIsTheDesignedTable(t *testing.T) {
	p := Pairs()
	if len(p) != 38 {
		t.Fatalf("Pairs() has %d rows, want 38", len(p))
	}
	for _, r := range p {
		if r.FG == "line" || r.BG == "line" {
			t.Errorf("line is a decorative divider and is not in the table: %+v", r)
		}
	}
	if n := len(ColourNames()); n != 28 {
		t.Errorf("ColourNames() has %d names, want 28 (14 roles + 7 status pairs)", n)
	}
}

// TestEveryPresetPassesTheTable: each preset resolves, validates, and every
// pair is re-checked here with an independently computed ratio.
func TestEveryPresetPassesTheTable(t *testing.T) {
	if len(Presets) != 4 || Presets[0].Name != Default {
		t.Fatalf("Presets = %d, first %q; want 4 with %q first", len(Presets), Presets[0].Name, Default)
	}
	accents := map[RGB]string{}
	for _, p := range Presets {
		tok, err := Choice{Preset: p.Name}.Validate()
		if err != nil {
			t.Errorf("preset %s: %v", p.Name, err)
			continue
		}
		for _, pr := range Pairs() {
			if r := independentContrast(tok[pr.FG], tok[pr.BG]); r < pr.Min {
				t.Errorf("preset %s: %s on %s = %.2f, needs %.1f", p.Name, pr.FG, pr.BG, r, pr.Min)
			}
		}
		if prev, dup := accents[tok["accent"]]; dup {
			t.Errorf("presets %s and %s share an accent", p.Name, prev)
		}
		accents[tok["accent"]] = p.Name
	}
}

// accentSurfaceMin is the lowest ratio of accent against the four surfaces:
// the only accent-dependent pairs an accent can fail (on-accent and focus are
// derived to pass whenever these do).
func accentSurfaceMin(a RGB) float64 {
	m := math.Inf(1)
	for _, s := range []string{"bg", "s1", "s2", "s3"} {
		m = math.Min(m, independentContrast(a, base[s]))
	}
	return m
}

// TestARandomAccentIsAcceptedExactlyWhenItReads is the property: for 4000
// random accents (seeded, so a failure reproduces), Validate accepts exactly
// those whose ratio against every surface is at least 4.5:1 — and when it
// accepts, every pair of the resolved theme passes (re-checked
// independently); when it refuses, it says why with a *ContrastError naming
// the accent.
func TestARandomAccentIsAcceptedExactlyWhenItReads(t *testing.T) {
	rng := rand.New(rand.NewSource(20240611))
	var accepted, refused int
	for i := 0; i < 4000; i++ {
		a := RGB{uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256))}
		c := Choice{Preset: "harbor", Accent: strings.ToLower(a.Hex())}
		tok, err := c.Validate()
		want := accentSurfaceMin(a) >= 4.5
		switch {
		case want && err != nil:
			t.Fatalf("accent %s reads at %.2f on every surface but was refused: %v", a.Hex(), accentSurfaceMin(a), err)
		case !want && err == nil:
			t.Fatalf("accent %s reads at %.2f on some surface but was accepted", a.Hex(), accentSurfaceMin(a))
		case err == nil:
			accepted++
			if tok["accent"] != a {
				t.Fatalf("accent %s resolved to %s", a.Hex(), tok["accent"].Hex())
			}
			for _, pr := range Pairs() {
				if r := independentContrast(tok[pr.FG], tok[pr.BG]); r < pr.Min {
					t.Fatalf("accent %s accepted but %s on %s = %.2f", a.Hex(), pr.FG, pr.BG, r)
				}
			}
		default:
			refused++
			var ce *ContrastError
			if !errors.As(err, &ce) || len(ce.Failures) == 0 || !strings.Contains(ce.Failures[0], "accent") {
				t.Fatalf("accent %s refused with %v; want a *ContrastError naming an accent pair", a.Hex(), err)
			}
		}
	}
	// Both outcomes must have happened, or the property was never tested in
	// one direction.
	if accepted < 100 || refused < 100 {
		t.Fatalf("accepted %d, refused %d of 4000: the sample does not reach both sides", accepted, refused)
	}
}

// TestANonCompliantAccentIsRefused: a dark accent fails text-on-surface pairs,
// and the refusal names the pairs. Fixture colour #3A2F6B is no preset's.
func TestANonCompliantAccentIsRefused(t *testing.T) {
	_, err := Choice{Preset: Default, Accent: "#3a2f6b"}.Validate()
	var ce *ContrastError
	if !errors.As(err, &ce) {
		t.Fatalf("Validate(#3a2f6b) = %v, want a *ContrastError", err)
	}
	if !strings.Contains(ce.Error(), "accent #3A2F6B on bg") {
		t.Errorf("the refusal does not name the failing pair: %v", ce)
	}
}

func TestBadChoicesAreRefusedByName(t *testing.T) {
	for in, want := range map[Choice]error{
		{Preset: "neon"}:                 ErrUnknownPreset,
		{Preset: Default, Accent: "red"}: ErrAccentFormat,
		{Accent: "#12345"}:               ErrAccentFormat,
		{Accent: "#12345g"}:              ErrAccentFormat,
		{Accent: "123456#"}:              ErrAccentFormat,
		{Accent: "#1234567"}:             ErrAccentFormat,
	} {
		if _, err := in.Validate(); !errors.Is(err, want) {
			t.Errorf("%+v: %v, want %v", in, err, want)
		}
	}
}

func TestDefaultness(t *testing.T) {
	for c, want := range map[Choice]bool{
		{}:                                   true,
		{Preset: Default}:                    true,
		{Preset: "harbor"}:                   false,
		{Preset: Default, Accent: "#e4b556"}: false,
	} {
		if got := c.IsDefault(); got != want {
			t.Errorf("%+v.IsDefault() = %v, want %v", c, got, want)
		}
	}
}

// TestTheDefaultResolvesToTheBasePalette: the zero Choice is the shipped
// palette, every token. (internal/ui pins base to tokens.css.)
func TestTheDefaultResolvesToTheBasePalette(t *testing.T) {
	tok, err := Choice{}.Validate()
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != len(base) {
		t.Fatalf("%d tokens, want %d", len(tok), len(base))
	}
	for k, v := range base {
		if tok[k] != v {
			t.Errorf("%s = %s, want %s", k, tok[k].Hex(), v.Hex())
		}
	}
}

// TestCSSCarriesEveryTokenAndMovesWithTheAccent: the block declares each
// colour and Fixed token once, sets the durations to 0ms under reduced
// motion, and two themes differing only in accent differ in bytes and ETag.
func TestCSSCarriesEveryTokenAndMovesWithTheAccent(t *testing.T) {
	a, _ := Choice{}.Resolve()
	b, _ := Choice{Accent: "#5aa9f0"}.Resolve()
	ca, cb := CSS(a), CSS(b)
	for _, n := range ColourNames() {
		if strings.Count(ca, "--mu-"+n+":") != 1 {
			t.Errorf("--mu-%s is not declared exactly once", n)
		}
	}
	for _, f := range Fixed {
		if !strings.Contains(ca, "--mu-"+f.Name+": "+f.Value+";") {
			t.Errorf("--mu-%s: %s missing", f.Name, f.Value)
		}
	}
	if !strings.Contains(ca, "--mu-accent: 228 181 86;") || !strings.Contains(cb, "--mu-accent: 90 169 240;") {
		t.Errorf("the accent channels are not the theme's:\n%s\n%s", ca, cb)
	}
	if !strings.Contains(ca, "@media (prefers-reduced-motion: reduce) {\n  :root:root { --mu-dur-fast: 0ms; --mu-dur-base: 0ms; --mu-dur-slow: 0ms; }\n}") {
		t.Errorf("the reduced-motion rule is missing or changed:\n%s", ca)
	}
	if ETag([]byte(ca)) == ETag([]byte(cb)) {
		t.Error("two different themes have one ETag")
	}
	if len(ETag([]byte(ca))) != 16 {
		t.Error("an ETag is 16 hex digits")
	}
}
