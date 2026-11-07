package asklog

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// LastParagraph is text's last paragraph: the text after its last blank line,
// trimmed ("" for none). What a session ASKED, not the report before it.
func LastParagraph(text string) string {
	paras := reBlankLine.Split(strings.ReplaceAll(text, "\r\n", "\n"), -1)
	for i := len(paras) - 1; i >= 0; i-- {
		if p := strings.TrimSpace(paras[i]); p != "" {
			return p
		}
	}
	return ""
}

var (
	reBlankLine = regexp.MustCompile(`\n[ \t]*\n`)
	reSpace     = regexp.MustCompile(`\s+`)
	reCode      = regexp.MustCompile("`[^`]*`")
	rePRURL     = regexp.MustCompile(`https?://github\.com/[^\s/]+/[^\s/]+/pull/[0-9]+[^\s]*`)
	rePRRef     = regexp.MustCompile(`#[0-9]+\b`)
	rePath      = regexp.MustCompile(`[^\s]*/[^\s]*`)
	reHex       = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	reDigits    = regexp.MustCompile(`[0-9]+`)
	reHexLetter = regexp.MustCompile(`[a-f]`)
	reHexDigit  = regexp.MustCompile(`[0-9]`)
)

// pathTrail is what a path token may end with that belongs to the sentence,
// not the path (kept after {path}).
const pathTrail = ".,;:!?)]'\""

// Normalise is q as a rule's pattern is compared with it (asks.question_norm),
// in this order:
//
//  1. NFC, lower case, the last paragraph only (LastParagraph);
//  2. a backticked span to {code};
//  3. a GitHub pull-request URL, or #<digits>, to {pr};
//  4. a token holding a '/' and no space to {path} (sentence punctuation it
//     ends with is kept after it);
//  5. a 7–40 character run of hex holding at least one digit AND one letter
//     a–f, to {sha} — so neither a number nor a word like "defaced" is a sha;
//  6. any other run of digits to {n};
//  7. runs of white space to one space, and leading and trailing punctuation
//     stripped (the braces of a placeholder excepted).
//
// It is tested with literal input/output pairs, never with its own output.
func Normalise(q string) string {
	s := strings.ToLower(norm.NFC.String(q))
	s = LastParagraph(s)
	s = reCode.ReplaceAllString(s, "{code}")
	s = rePRURL.ReplaceAllString(s, "{pr}")
	s = rePRRef.ReplaceAllString(s, "{pr}")
	s = rePath.ReplaceAllStringFunc(s, func(tok string) string {
		trimmed := strings.TrimRight(tok, pathTrail)
		if !strings.Contains(trimmed, "/") {
			return tok
		}
		return "{path}" + tok[len(trimmed):]
	})
	s = reHex.ReplaceAllStringFunc(s, func(h string) string {
		if reHexLetter.MatchString(h) && reHexDigit.MatchString(h) {
			return "{sha}"
		}
		return h
	})
	s = reDigits.ReplaceAllString(s, "{n}")
	s = strings.TrimSpace(reSpace.ReplaceAllString(s, " "))
	s = strings.TrimFunc(s, func(r rune) bool { return (unicode.IsPunct(r) || unicode.IsSymbol(r)) && r != '{' && r != '}' })
	return strings.TrimSpace(s)
}

// prefilterStarts are the openings that make a last paragraph a question even
// without a '?' (the design's list).
var prefilterStarts = []string{"shall i", "should i", "want me to", "do you want", "ready to", "let me know"}

// Prefilter reports whether text — a turn's last assistant message — looks
// like it asks the owner something: its last paragraph holds a '?', or begins
// (after leading markdown and punctuation) with one of prefilterStarts. In
// code, no model: a turn end that fails it writes no row.
func Prefilter(text string) bool {
	p := LastParagraph(text)
	if p == "" {
		return false
	}
	if strings.ContainsRune(p, '?') {
		return true
	}
	lead := strings.ToLower(strings.TrimLeftFunc(p, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) || unicode.IsSpace(r) }))
	lead = reSpace.ReplaceAllString(lead, " ")
	for _, st := range prefilterStarts {
		if strings.HasPrefix(lead, st) {
			return true
		}
	}
	return false
}
