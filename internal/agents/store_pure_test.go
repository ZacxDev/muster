package agents

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDeriveSessionTitle(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"single line trimmed", "  hello world  ", "hello world"},
		{"first non-empty line wins", "\n\n  first real line\nsecond", "first real line"},
		{"leading blank lines skipped", "\n   \n\t\nactual", "actual"},
		{"empty input", "", ""},
		{"all whitespace", "   \n\t\n  ", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveSessionTitle(tt.content); got != tt.want {
				t.Errorf("deriveSessionTitle(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}

func TestDeriveSessionTitleCaps(t *testing.T) {
	long := strings.Repeat("a", sessionTitleCap+20)
	got := deriveSessionTitle(long)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("over-cap title %q should end with an ellipsis", got)
	}
	// The result is sessionTitleCap runes plus the ellipsis.
	if n := utf8.RuneCountInString(got); n != sessionTitleCap+1 {
		t.Errorf("capped title rune count = %d, want %d (cap + ellipsis)", n, sessionTitleCap+1)
	}
}

func TestDeriveSessionTitleMultibyteCap(t *testing.T) {
	// Cap counts runes, not bytes: a title of multibyte runes must not split a rune.
	long := strings.Repeat("é", sessionTitleCap+5) // 2 bytes each
	got := deriveSessionTitle(long)
	if !utf8.ValidString(got) {
		t.Errorf("capped multibyte title is not valid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != sessionTitleCap+1 {
		t.Errorf("capped multibyte rune count = %d, want %d", n, sessionTitleCap+1)
	}
}

func TestDeriveSessionTitleExactlyAtCap(t *testing.T) {
	// Exactly at the cap must NOT get an ellipsis (boundary: len(r) > cap).
	exact := strings.Repeat("b", sessionTitleCap)
	got := deriveSessionTitle(exact)
	if strings.HasSuffix(got, "…") {
		t.Errorf("title exactly at cap should not be truncated: %q", got)
	}
	if got != exact {
		t.Errorf("deriveSessionTitle at cap = %q, want the input unchanged", got)
	}
}
