package branding

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxNameRunes is the longest display name, in characters (Unicode code
// points — what Postgres's char_length counts, so the column's CHECK and this
// agree).
const MaxNameRunes = 64

// The name's refusals.
var (
	ErrNameInvalid = &Error{"name_invalid", "the name is not valid UTF-8"}
	ErrNameTooLong = &Error{"name_too_long", "the name is longer than 64 characters"}
	ErrNameControl = &Error{"name_control", "the name contains a control or text-direction character"}
)

// NormalizeName is raw as stored: trimmed of surrounding white space, or
// refused. An empty result (raw was empty or only white space) means "clear
// the name", and is not an error.
//
// 🔴 TEXT-DIRECTION CONTROLS ARE REFUSED WITH THE C0/C1 CONTROLS. The name is
// drawn beside the logo on every page and on the sign-in form, where a
// right-to-left override (U+202E) could make it read as something it is not.
func NormalizeName(raw string) (string, error) {
	if !utf8.ValidString(raw) {
		return "", ErrNameInvalid
	}
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if utf8.RuneCountInString(s) > MaxNameRunes {
		return "", ErrNameTooLong
	}
	for _, r := range s {
		if unicode.IsControl(r) || bidiControl(r) {
			return "", ErrNameControl
		}
	}
	return s, nil
}

// bidiControl reports the explicit directional formatting characters
// (Unicode's Bidi_Control set, less the two marks LRM/RLM, which only nudge
// neutral characters and cannot reorder a run).
func bidiControl(r rune) bool {
	return r == '؜' || (r >= '‪' && r <= '‮') || (r >= '⁦' && r <= '⁩')
}
