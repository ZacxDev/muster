package branding

import (
	"fmt"
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
	ErrNameControl = &Error{"name_control", "the name contains a control, text-direction or invisible formatting character"}
)

// NameCharError is CheckNameText's refusal: the first refused character. It
// is ErrNameControl (errors.Is, and errors.As to *Error), so a caller that
// answers with the code and message keeps working; Rune names the character,
// for a sentence that says what was refused.
type NameCharError struct{ Rune rune }

func (e *NameCharError) Error() string {
	return fmt.Sprintf("%s (U+%04X)", ErrNameControl.Msg, e.Rune)
}

func (e *NameCharError) Unwrap() error { return ErrNameControl }

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
	if err := CheckNameText(s); err != nil {
		return "", err
	}
	return s, nil
}

// CheckNameText is THE character rule for a name a person reads as a name:
// the instance's name (NormalizeName) and every name stored in
// users.display_name — a person's (users.RedeemInvite) and a team's
// (users.CreateTeam). It refuses, as a *NameCharError naming the first one,
//
//   - a control character: C0 (U+0000–U+001F, tab and newline included), DEL
//     and C1 (U+007F–U+009F) — unicode.IsControl;
//   - an invisible or text-direction formatting character (nameFormatChar).
//
// It checks characters only: trimming, emptiness and length are each caller's.
// One rule, one place: a writer of a name calls this, never a copy of it
// (internal/users' TestEveryDisplayNameWriterIsChecked holds the writers to it).
func CheckNameText(s string) error {
	for _, r := range s {
		if unicode.IsControl(r) || nameFormatChar(r) {
			return &NameCharError{Rune: r}
		}
	}
	return nil
}

// nameFormatChar reports the invisible and text-direction formatting
// characters a name may not hold: the zero-width space (U+200B), the two
// direction marks LRM and RLM (U+200E, U+200F), the Arabic letter mark
// (U+061C), the bidi embeddings and overrides (U+202A–U+202E), the bidi
// isolates (U+2066–U+2069) and the zero-width no-break space / BOM (U+FEFF).
// Each either reorders the text around it or draws nothing, so two names that
// look identical can differ.
//
// ⚠ THE JOINERS ARE ALLOWED: ZWNJ (U+200C) and ZWJ (U+200D). They are part
// of how real names are written: ZWJ joins an emoji sequence (a family, a
// person at a laptop) and ZWNJ is required in Persian and in Indic scripts.
// Allowing them is a decision (#666), so don't "complete" the 200B–200F
// range.
func nameFormatChar(r rune) bool {
	return r == 0x061C || r == 0x200B || r == 0x200E || r == 0x200F || (r >= 0x202A && r <= 0x202E) ||
		(r >= 0x2066 && r <= 0x2069) || r == 0xFEFF
}
