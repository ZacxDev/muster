package branding

import (
	"errors"
	"strings"
	"testing"
)

// TestNormalizeNameEdges: every edge of the name rule. A blank name clears
// (no error); surrounding white space is trimmed; 64 characters is the
// longest, counted in characters, not bytes (64 four-byte runes pass); a
// control character or a text-direction override anywhere is refused, as is
// invalid UTF-8.
func TestNormalizeNameEdges(t *testing.T) {
	cases := []struct {
		in, want string
		err      *Error
	}{
		{"", "", nil},
		{"   \t\n ", "", nil},
		{"  Team Example  ", "Team Example", nil},
		{"Ünïcödé · 名前", "Ünïcödé · 名前", nil},
		{strings.Repeat("a", 64), strings.Repeat("a", 64), nil},
		{strings.Repeat("😀", 64), strings.Repeat("😀", 64), nil},
		{" " + strings.Repeat("b", 64) + " ", strings.Repeat("b", 64), nil},
		{strings.Repeat("a", 65), "", ErrNameTooLong},
		{strings.Repeat("😀", 65), "", ErrNameTooLong},
		{"Team\tExample", "", ErrNameControl},
		{"Team\x00Example", "", ErrNameControl},
		{"Team\u0085Example", "", ErrNameControl},
		{"Team‮elpmaxE", "", ErrNameControl},
		{"Team⁦x⁩", "", ErrNameControl},
		{"Team؜Example", "", ErrNameControl},
		{"Team\xffExample", "", ErrNameInvalid},
	}
	for _, c := range cases {
		got, err := NormalizeName(c.in)
		if c.err == nil {
			if err != nil || got != c.want {
				t.Errorf("NormalizeName(%q) = %q, %v; want %q", c.in, got, err, c.want)
			}
			continue
		}
		if !errors.Is(err, c.err) {
			t.Errorf("NormalizeName(%q) err = %v, want %s", c.in, err, c.err.Code)
		}
	}
}
