package ui

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/pages"
)

// phoneTable is a markdown table of n columns (header, separator, one row).
func phoneTable(n int) string {
	h, s, r := make([]string, n), make([]string, n), make([]string, n)
	for i := range h {
		h[i], s[i], r[i] = "h", "---", "v"
	}
	return "| " + strings.Join(h, " | ") + " |\n| " + strings.Join(s, " | ") + " |\n| " + strings.Join(r, " | ") + " |"
}

// phoneSpan is an inline code span holding n characters.
func phoneSpan(n int) string { return "`" + strings.Repeat("x", n) + "`" }

// TestPhoneNotesFireAtTheirBoundaries: a table of 5 columns and a code span
// of 60 characters are fine; 6 and 61 are each one note — and the counts
// overshoot too (9 columns, 140 characters) — naming the block by its index
// in the call's blocks (non-markdown blocks count). Each note is pinned
// whole.
func TestPhoneNotesFireAtTheirBoundaries(t *testing.T) {
	md := func(s string) pages.Block { return pages.Block{Type: pages.BlockMarkdown, Text: s} }
	q := pages.Block{Type: pages.BlockQuestion, ID: "q", Label: "Q"}

	if got := PagePhoneNotes([]pages.Block{md(phoneTable(5)), md("see " + phoneSpan(60) + " ok"), md("![a](data:image/png;base64,AAAA)"),
		md("![a](asset:AbCdEfGhIjKlMnOpQrStUv)")}); got != nil {
		t.Errorf("at the boundary: %q, want none", got)
	}

	table := func(i, n string) string {
		return "blocks[" + i + "] (markdown): a table with " + n + " columns is cramped on a phone (it scrolls sideways). " +
			"Keep a table to 5 columns or fewer: split it, or make the extra columns a list under a heading per row."
	}
	span := func(i, n string) string {
		return "blocks[" + i + "] (markdown): an inline code span of " + n + " characters is hard to read on a phone. " +
			"Keep inline code to 60 characters or fewer: put a long command or URL in a fenced code block, or link it once."
	}
	got := PagePhoneNotes([]pages.Block{q, md(phoneTable(6)), md("see " + phoneSpan(61)), md(phoneTable(9) + "\n\nand " + phoneSpan(140))})
	want := []string{table("1", "6"), span("2", "61"), table("3", "9"), span("3", "140")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("past the boundary:\n got %q\nwant %q", got, want)
	}
}

// TestPhoneNotesNameAnImageThePageCannotDraw: an http(s) image, an SVG data:
// image and malformed base64 are each named (the long one shortened); one in
// a code span or a fenced block is code, shown as written on purpose.
func TestPhoneNotesNameAnImageThePageCannotDraw(t *testing.T) {
	img := func(i, q string) string {
		return "blocks[" + i + "] (markdown): the image `" + q + "` is shown as text, not drawn. " +
			"In markdown write ![what it shows](asset:<id>) (page_asset_upload) or a small data:image/png, jpeg, webp or gif base64 URI; " +
			"an http(s) address or an SVG is never drawn."
	}
	long := "![chart](data:image/svg+xml;base64," + strings.Repeat("PHN2Zz4g", 20) + ")"
	got := PagePhoneNotes([]pages.Block{
		{Type: pages.BlockMarkdown, Text: "# Shots\n\n![shot](https://example.com/a.png)"},
		{Type: pages.BlockHTML, HTML: `<img src="https://example.com/a.png">`},
		{Type: pages.BlockMarkdown, Text: "| a | b |\n|---|---|\n| " + long + " | x |"},
		{Type: pages.BlockMarkdown, Text: "`![x](https://example.com/a.png)`\n\n```\n![x](https://example.com/a.png)\n```\n\n![m](data:image/png;base64,AAAAA)"},
	})
	want := []string{
		img("0", "![shot](https://example.com/a.png)"),
		img("2", "![chart](data:image/svg+xml;base64,PHN2Zz4gPHN2Zz4gPHN2Zz4gP…"),
		img("3", "![m](data:image/png;base64,AAAAA)"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// TestPhoneNotesSeeOnlyWhatTheRendererDrawsAsATable: pipes in a fenced block,
// a bullet or a heading are no table to the renderer, so none to the check.
func TestPhoneNotesSeeOnlyWhatTheRendererDrawsAsATable(t *testing.T) {
	wide := phoneTable(7)
	for _, md := range []string{"```\n" + wide + "\n```", "- " + wide, "# " + wide} {
		if got := PagePhoneNotes([]pages.Block{{Type: pages.BlockMarkdown, Text: md}}); got != nil {
			t.Errorf("%q: %q, want none", md, got)
		}
	}
	// Control: the same table on its own is one note.
	if got := PagePhoneNotes([]pages.Block{{Type: pages.BlockMarkdown, Text: wide}}); len(got) != 1 {
		t.Errorf("control: %q, want one note", got)
	}
}
