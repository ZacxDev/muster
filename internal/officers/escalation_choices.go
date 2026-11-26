package officers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// officer_complete's `choices`: the options an escalation offers the person
// it reaches, which the HQ API turns into the decision buttons of the
// escalation's DM (a tap comes back to the officer as a directive item). The
// HQ API's rules, checked here first so the instance gets a clear refusal
// before anything is sent (the HQ API stays the authority and checks them
// again): at most MaxEscalationChoices {value, label} objects, only on
// outcome escalated; each value matches choiceValueRe (a stable id), is not
// one of the HQ API's own buttons (ReservedChoiceValues) and is not repeated;
// each label is 1 to MaxChoiceLabel characters once cleaned as the HQ API
// cleans it (cleanChoiceLabel); no other keys. muster forwards the argument AS SENT.
// ---------------------------------------------------------------------------

// ChoicesInstruction is THE sentence an HQ officer's instance is given about
// an escalation's options: in its kickoff's step 3 (KickoffText) and in its
// system prompt's escalation paragraph (internal/api). A builtin officer is
// given neither: its backend has no decision buttons.
const ChoicesInstruction = "When your escalation offers the person options, send them in the same call as choices " +
	"(at most 4 {value, label}: a short label, and a value that stays the same whenever you offer that option), never as prose. " +
	"Each becomes a button; a tap comes back to you as a directive item: carry it out within your charter."

// MaxEscalationChoices is the most choices one escalation may carry (the DM
// adds its own "Other…" button, and one row holds five).
const MaxEscalationChoices = 4

// MaxChoiceLabel is the most characters a choice's label may have.
const MaxChoiceLabel = 80

// ReservedChoiceValues are the HQ API's own button values, which an
// officer's choice may not take.
var ReservedChoiceValues = []string{"resolve", "other"}

var choiceValueRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// CheckChoices checks an instance's `choices` argument (raw as sent) for
// outcome. Absent, JSON null and [] are no choices, and pass. It returns why
// the argument is refused, or "".
func CheckChoices(raw json.RawMessage, outcome string) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var in []struct {
		Value *string `json:"value"`
		Label *string `json:"label"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || dec.More() {
		return "choices must be an array of {value, label} objects (no other keys)"
	}
	if len(in) == 0 {
		return ""
	}
	if outcome != "escalated" {
		return fmt.Sprintf("choices are taken only with outcome escalated, not %s: they are the buttons of the escalation's DM", outcome)
	}
	if len(in) > MaxEscalationChoices {
		return fmt.Sprintf("at most %d choices (the DM adds its own Other… button): you sent %d", MaxEscalationChoices, len(in))
	}
	seen := map[string]bool{}
	for i, c := range in {
		if c.Value == nil || c.Label == nil {
			return fmt.Sprintf("choices[%d] needs both value and label", i)
		}
		v := strings.TrimSpace(*c.Value)
		l := cleanChoiceLabel(*c.Label)
		switch {
		case !choiceValueRe.MatchString(v):
			return fmt.Sprintf("choices[%d].value %q must match %s (a short stable id, e.g. \"rollback\")", i, v, choiceValueRe)
		case isReservedChoice(v):
			return fmt.Sprintf("choices[%d].value %q is one of the HQ API's own buttons (%s): pick another id", i, v, strings.Join(ReservedChoiceValues, ", "))
		case seen[v]:
			return fmt.Sprintf("choices[%d].value %q is repeated: each choice needs its own value", i, v)
		case l == "" || utf8.RuneCountInString(l) > MaxChoiceLabel:
			return fmt.Sprintf("choices[%d].label must be 1 to %d characters", i, MaxChoiceLabel)
		}
		seen[v] = true
	}
	return ""
}

// cleanChoiceLabel is a label as the HQ API counts it (its release-text
// cleaning): invalid UTF-8, control, format (zero-width, bidi) and line or
// paragraph separator characters become spaces, whitespace runs collapse to
// one space, the ends are trimmed. So a label of only invisible characters is
// empty here, as it is there.
func cleanChoiceLabel(s string) string {
	s = strings.ToValidUTF8(s, " ")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

func isReservedChoice(v string) bool {
	for _, r := range ReservedChoiceValues {
		if v == r {
			return true
		}
	}
	return false
}
