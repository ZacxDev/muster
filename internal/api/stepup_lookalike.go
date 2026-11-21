package api

import (
	"html"
	"regexp"
	"strings"
	"unicode"

	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// LOOK-ALIKE LETTERS ON A STEP-UP CARD'S PREVIEW (host_stepup_card.go).
//
// The preview draws a post's text as the post reads — an em dash is a dash, an
// emoji an emoji — which is what the operator asked for. The cost: a letter
// that only LOOKS like another ("pаypal.com" with a Cyrillic а) and a
// combining overlay mark (U+0336 striking a word through) render exactly as
// the real thing, on the one page whose purpose is to show what will be
// posted. So the renderer flags them, word by word, and never changes how an
// ordinary word reads:
//
//   - a word whose letters mix scripts that are not written together
//     (stepUpMixedScript: Latin with Cyrillic, Greek with Latin, …; the
//     Han/kana/Hangul combinations Unicode's "highly restrictive" profile
//     allows are not mixed);
//   - any non-ASCII code point inside a link, a domain, an e-mail address or
//     an @handle (stepUpLinkish): those are where a look-alike is a different
//     destination, and where the real thing is ASCII;
//   - any combining overlay mark (stepUpOverlay), wherever it is.
//
// A flagged word is drawn inside <mark data-stepup-lookalike>, and the card
// says once, under the preview and outside its mock, that it has some
// (stepUpLookalikeNote). A pure-Cyrillic word, "café", and every emoji are
// not flagged: those are ordinary text.
// ---------------------------------------------------------------------------

// stepUpLookalikeNote is the one line under a preview with a flagged word.
const stepUpLookalikeNote = "Contains look-alike letters — check the highlighted words."

// stepUpLinkRE is what stepUpLinkish takes for a link or a domain (after its
// surrounding punctuation is trimmed): an optional scheme, labels of letters,
// digits, marks or hyphens joined by dots, ending in a label of two or more
// letters, and an optional path, port, query or fragment.
var stepUpLinkRE = regexp.MustCompile(`^(?:[A-Za-z][A-Za-z0-9+.-]*://)?[\p{L}\p{N}\p{M}-]+(?:\.[\p{L}\p{N}\p{M}-]+)*\.[\p{L}\p{M}]{2,}(?:[/:?#].*)?$`)

// stepUpLinkish reports whether word w is a link, a domain, an e-mail address
// or an @handle.
func stepUpLinkish(w string) bool {
	if strings.Contains(w, "://") || strings.Contains(w, "@") {
		return true
	}
	t := strings.TrimFunc(w, func(r rune) bool { return unicode.IsPunct(r) && r != '/' && r != '#' })
	return t != "" && stepUpLinkRE.MatchString(t)
}

// stepUpOverlay reports whether r is a combining overlay mark: a stroke or a
// slash drawn THROUGH the letter before it (U+0334–U+0338, U+20D2–U+20D3,
// U+20D8–U+20DA, U+20E5–U+20E6, U+20EA–U+20EB).
func stepUpOverlay(r rune) bool {
	switch {
	case r >= 0x0334 && r <= 0x0338, r == 0x20D2, r == 0x20D3, r >= 0x20D8 && r <= 0x20DA,
		r == 0x20E5, r == 0x20E6, r == 0x20EA, r == 0x20EB:
		return true
	}
	return false
}

// stepUpScriptOf is letter r's script's name, "" for a letter of none of
// them (Common, Inherited).
func stepUpScriptOf(r rune) string {
	if r < 0x80 {
		return "Latin"
	}
	for name, t := range unicode.Scripts {
		if name == "Common" || name == "Inherited" {
			continue
		}
		if unicode.Is(t, r) {
			return name
		}
	}
	return ""
}

// stepUpScriptSets are the sets of scripts one word may mix and not be
// flagged (Unicode TS #39's "highly restrictive" level, beyond one script).
var stepUpScriptSets = []map[string]bool{
	{"Latin": true, "Han": true, "Hiragana": true, "Katakana": true},
	{"Latin": true, "Han": true, "Bopomofo": true},
	{"Latin": true, "Han": true, "Hangul": true},
}

// stepUpMixedScript reports whether one run of letters in word w (its marks
// with it) mixes scripts no single writing system mixes.
func stepUpMixedScript(w string) bool {
	run := map[string]bool{}
	mixed := func() bool {
		if len(run) < 2 {
			return false
		}
		for _, set := range stepUpScriptSets {
			ok := true
			for s := range run {
				ok = ok && set[s]
			}
			if ok {
				return false
			}
		}
		return true
	}
	for _, r := range w {
		switch {
		case unicode.IsLetter(r):
			if s := stepUpScriptOf(r); s != "" {
				run[s] = true
			}
		case unicode.IsMark(r):
		default:
			if mixed() {
				return true
			}
			clear(run)
		}
	}
	return mixed()
}

// stepUpLookalike reports whether word w (a run of non-space text) is drawn
// flagged: a mixed-script run of letters, a non-ASCII code point in a
// link-like word, or a combining overlay mark (the file header).
func stepUpLookalike(w string) bool {
	nonASCII := false
	for _, r := range w {
		if stepUpOverlay(r) {
			return true
		}
		nonASCII = nonASCII || r >= 0x80
	}
	if !nonASCII {
		return false
	}
	return stepUpMixedScript(w) || stepUpLinkish(w)
}

// stepUpLookalikeIn reports whether any word of t is flagged.
func stepUpLookalikeIn(t string) bool {
	for _, w := range strings.Fields(t) {
		if stepUpLookalike(w) {
			return true
		}
	}
	return false
}

// stepUpMarkedHTML is t escaped (stepUpHiddenHTML: every hidden code point a
// visible flag), every space kept exactly where it is, with each flagged word
// (stepUpLookalike) inside a <mark data-stepup-lookalike>.
func stepUpMarkedHTML(t string) string {
	var b strings.Builder
	for len(t) > 0 {
		i := strings.IndexFunc(t, stepUpSpace)
		if i == 0 {
			j := strings.IndexFunc(t, func(r rune) bool { return !stepUpSpace(r) })
			if j < 0 {
				j = len(t)
			}
			b.WriteString(html.EscapeString(t[:j]))
			t = t[j:]
			continue
		}
		if i < 0 {
			i = len(t)
		}
		w := t[:i]
		if stepUpLookalike(w) {
			b.WriteString(`<mark class="` + ui.StepUpLookalikeClass + `" data-stepup-lookalike>` + stepUpHiddenHTML(w) + `</mark>`)
		} else {
			b.WriteString(stepUpHiddenHTML(w))
		}
		t = t[i:]
	}
	return b.String()
}

// stepUpHiddenNote is the one line under a preview (or a draft's text) that
// holds a hidden character (ui.HiddenRune says which are).
const stepUpHiddenNote = "Contains hidden characters — each is marked where it sits, and is part of the text."

// stepUpHidden is ui.HiddenRune: whether r, between prev and next, is drawn as
// a hidden character's flag.
func stepUpHidden(prev, r, next rune) bool { return ui.HiddenRune(prev, r, next) }

// stepUpSpace reports whether r is a space a text is split into words on: a
// space that is not hidden (a U+2028 is a space to unicode.IsSpace, and is
// flagged like any other hidden character).
func stepUpSpace(r rune) bool { return unicode.IsSpace(r) && !stepUpHidden(0, r, 0) }

// stepUpHiddenHTML is ui.HiddenFlagHTML: t escaped, each hidden code point
// drawn as its flag.
func stepUpHiddenHTML(t string) string { return ui.HiddenFlagHTML(t) }

// stepUpHiddenIn reports whether t holds a hidden code point (stepUpHidden).
func stepUpHiddenIn(t string) bool {
	return strings.Contains(stepUpHiddenHTML(t), "data-stepup-hidden")
}

// plainTextHTML is text drawn AS WRITTEN, never markdown: an outside party's
// message quoted on a draft's card (draftQuoteHTML), a command a session
// waits on (a tool call's line, an opencode ask's quote). t escaped, its line breaks kept as breaks (its box is
// whitespace-pre-wrap; a carriage return is one), each hidden code point a
// visible flag. 🔴 NEVER MARKDOWN, unlike an ask's own words (ui.AskTextHTML):
// a quote is untrusted (a link's text there could name a different place than
// the one it opens), and a command's `*` and `_` are its own.
func plainTextHTML(t string) string {
	t = strings.ReplaceAll(strings.ReplaceAll(t, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(t, "\n")
	for i, l := range lines {
		lines[i] = stepUpHiddenHTML(strings.ReplaceAll(l, "\t", " "))
	}
	return strings.Join(lines, "\n")
}

// askTextHTML is an ask's words — a question, an answer, a log row's
// question, a nudge's message — as every Asks card draws them: ui.AskTextHTML
// (markdown, hidden code points flagged). Its box must be a <div>.
func askTextHTML(t string) string { return ui.AskTextHTML(t) }
