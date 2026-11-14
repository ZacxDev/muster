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
		{"Team\u200bExample", "", ErrNameControl},
		{"Team\u200eExample", "", ErrNameControl},
		{"Team\ufeffExample", "", ErrNameControl},
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

// TestCheckNameTextNamesTheRefusedCharacter: the refusal is ErrNameControl
// (and a *Error, for the branding route's code) and names the FIRST refused
// character, so the sentence a person reads says what was refused.
func TestCheckNameTextNamesTheRefusedCharacter(t *testing.T) {
	err := CheckNameText("Orla\u202eFinch\x07")
	var ce *NameCharError
	var be *Error
	if !errors.Is(err, ErrNameControl) || !errors.As(err, &ce) || !errors.As(err, &be) || be.Code != "name_control" {
		t.Fatalf("CheckNameText = %v, want a NameCharError that is ErrNameControl", err)
	}
	if ce.Rune != 0x202E || !strings.Contains(err.Error(), "U+202E") {
		t.Fatalf("names U+%04X (%q), want the first refused character, U+202E", ce.Rune, err.Error())
	}
	if err := CheckNameText("Orla Finch 🦊 李"); err != nil {
		t.Fatalf("an ordinary name: %v", err)
	}
}
