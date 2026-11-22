package api

import (
	"strings"
	"testing"
)

// TestThePaletteQueryIsAnIdOrText: an id — any length, below the text floor
// included — names an ask and a page, or one of them by its word; text runs
// from three runes to two hundred; anything else is nothing to search.
func TestThePaletteQueryIsAnIdOrText(t *testing.T) {
	for raw, want := range map[string]paletteFindQuery{
		"7":                      {AskID: 7, PageID: 7},
		"#7":                     {AskID: 7, PageID: 7},
		" # 42 ":                 {AskID: 42, PageID: 42},
		"ask 9":                  {AskID: 9},
		"Ask #9":                 {AskID: 9},
		"page 3":                 {PageID: 3},
		"doc#3":                  {PageID: 3},
		"0":                      {},
		"ab":                     {},
		"abc":                    {Text: "abc"},
		"draft 4":                {Text: "draft 4"},
		"  kestrel  ":            {Text: "kestrel"},
		strings.Repeat("x", 200): {Text: strings.Repeat("x", 200)},
		strings.Repeat("x", 201): {},
		"":                       {},
	} {
		if got := parsePaletteFind(raw); got != want {
			t.Errorf("parsePaletteFind(%q) = %+v, want %+v", raw, got, want)
		}
	}
}
