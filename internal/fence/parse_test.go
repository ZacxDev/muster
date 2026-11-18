package fence

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseReadsBackWhatRenderWrote: Parse is Render's inverse — preface,
// marker, count, nonce and every content line (a blank one included, and one
// whose trailing space a transcript stripped).
func TestParseReadsBackWhatRenderWrote(t *testing.T) {
	b := Block{Preface: "muster test: two items.\nsecond preface line", Marker: "routed-message", Count: 2,
		Lines: []string{"standing line", "", "  body: " + Sanitize("a\nb")}}
	text := b.Render(nonce)
	for name, in := range map[string]string{
		"as rendered":             text,
		"trailing space stripped": strings.ReplaceAll(text, "| \n", "|\n"),
		"CRLF":                    strings.ReplaceAll(text, "\n", "\r\n"),
	} {
		got, gotNonce, ok := Parse(in)
		if !ok || gotNonce != nonce || !reflect.DeepEqual(got, b) {
			t.Errorf("%s: Parse = %+v, %q, %v; want %+v", name, got, gotNonce, ok, b)
		}
	}
	if Unfold(got3(t, text)) != "  body: a\nb" {
		t.Errorf("Unfold did not restore the line break")
	}
}

func got3(t *testing.T, text string) string {
	t.Helper()
	b, _, ok := Parse(text)
	if !ok {
		t.Fatal("parse")
	}
	return b.Lines[2]
}

// TestParseRefusesWhatRenderCannotWrite: a line outside the content prefix, a
// closing marker of another block or nonce, no closing marker, and anything
// after the block are each not a rendered block.
func TestParseRefusesWhatRenderCannotWrite(t *testing.T) {
	text := Block{Preface: "p", Marker: "m", Count: 1, Lines: []string{"x"}}.Render(nonce)
	for name, in := range map[string]string{
		"unprefixed line":  strings.Replace(text, "| x\n", "| x\ny\n", 1),
		"other nonce":      strings.Replace(text, "<<<end m nonce="+nonce, "<<<end m nonce=fedcba9876543210", 1),
		"other marker":     strings.Replace(text, "<<<end m ", "<<<end n ", 1),
		"no close":         strings.Replace(text, "<<<end m nonce="+nonce+">>>\n", "", 1),
		"text after":       text + "and then this",
		"no fence at all":  "p\n\n| x\n",
		"marker in a line": "p\n\n| <<<m untrusted nonce=" + nonce + " count=1>>>\n",
	} {
		if _, _, ok := Parse(in); ok {
			t.Errorf("%s: Parse accepted %q", name, in)
		}
	}
	if _, _, ok := Parse(text); !ok {
		t.Fatal("CONTROL: the unmodified block does not parse")
	}
}
