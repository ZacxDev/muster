package ui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTyperLabelEscapesAndCapsTheName pins the typer label (chat_typer.go)
// without a database: the whole label as it is drawn, for a name carrying
// HTML, for names just over and exactly at the cap, made of multi-byte runes
// (so a cap counted in BYTES cuts them in a different place, or splits a rune),
// and for the api's fallback, which passes through as is.
func TestTyperLabelEscapesAndCapsTheName(t *testing.T) {
	const open = `<div data-typed-by="" class="mb-1 text-xs font-medium text-fg2">`
	const shut = `</div>`
	cases := []struct {
		name, in, want string
	}{
		{"html in a name renders escaped", `<img src=x onerror=alert(1)>&"Mallory'`,
			`&lt;img src=x onerror=alert(1)&gt;&amp;&#34;Mallory&#39;`},
		// 41 three-byte runes: 39 of them and the ellipsis, 40 runes in all.
		{"41 multi-byte runes are capped at 40 with the ellipsis", strings.Repeat("日", 41),
			"日日日日日日日日日日日日日日日日日日日日日日日日日日日日日日日日日日日日日日日…"},
		{"60 two-byte runes are capped at 40 with the ellipsis", strings.Repeat("é", 60),
			"éééééééééééééééééééééééééééééééééééééé" + "é…"},
		{"a 40-rune name is unchanged", strings.Repeat("日", 40),
			"日日日日日日日日日日日日日日日日日日日日日日日日日日日日日日日日日日日日日日日日"},
		{"the fallback passes through as is", "user #1099511627776", "user #1099511627776"},
	}
	for _, c := range cases {
		got := TyperLabelHTML(c.in)
		if want := open + c.want + shut; got != want {
			t.Errorf("%s: TyperLabelHTML(%q)\n got: %s\nwant: %s", c.name, c.in, got, want)
		}
		body := strings.TrimSuffix(strings.TrimPrefix(got, open), shut)
		if !utf8.ValidString(body) {
			t.Errorf("%s: the label splits a rune: %q", c.name, body)
		}
	}
	// The literals above are counted by hand; this checks the counting.
	for _, s := range []string{cases[1].want, cases[2].want, cases[3].want} {
		if n := utf8.RuneCountInString(s); n != TyperNameMax {
			t.Fatalf("fixture: %q is %d runes; want %d", s, n, TyperNameMax)
		}
	}
	if got := TyperLabelHTML(""); got != "" {
		t.Errorf(`TyperLabelHTML("") = %q; want no label`, got)
	}
}
