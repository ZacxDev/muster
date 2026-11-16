package plugins

import (
	"regexp"
	"strings"
)

// THE VENUE RULES OF AN OUTBOUND DRAFT (docs/plugins.md "Venue rules"): the
// shape a body and its media must have for the venue to take them, checked
// when the officer FILES the draft (FileOutbound), so a draft the venue cannot
// take is refused in the officer's own turn — with the part, its length and
// the limit — instead of reaching the plugin, being refused there, and coming
// back a round trip later as a draft.refused item.
//
// 🔴 THIS IS THE ONE PLACE THESE RULES LIVE. A venue this table does not name
// gets no check here: muster does not invent limits for a venue it does not
// know (the plugin's own check still applies to every draft).
//
// The rules are the venue's, not a plugin's: a plugin may refuse more (its
// check is the final word), never less.

// Venue names with rules (as a plugin names them on its messages and drafts).
const (
	VenueX      = "x"
	VenueReddit = "reddit"
)

const (
	// XMaxWeight is the most a post on X may weigh (XWeightedLength).
	XMaxWeight = 280
	// XMaxThreadParts bounds an X thread: X's composer takes at most 25
	// posts in one thread.
	XMaxThreadParts = 25
	// XURLWeight is what X counts for every link, whatever its length (X
	// wraps each one in a t.co link of this length).
	XURLWeight = 23
	// XThreadSeparator splits an X post's body into a thread: a line that is
	// exactly this (trailing whitespace ignored). In a reply it is not a
	// split: a reply is one part.
	XThreadSeparator = "---8<---"
)

// xURLRE is what XWeightedLength counts as a link: an optional http(s)
// scheme, a host of dot-separated labels ending in a letters-only label of
// two or more, and an optional path up to the next space. Wider than X's own
// detection (which also wants a known top-level domain), so a word it takes
// for a link that X does not is counted 23 — longer than its own length only
// when it is shorter than 23, which errs long.
var xURLRE = regexp.MustCompile(`(?i)(?:https?://)?(?:[a-z0-9-]+\.)+[a-z]{2,}(?:/[^\s\p{Z}]*)?`)

// xLight reports whether code point r weighs 1 on X: twitter-text v3's
// weight-100 ranges (U+0000-U+10FF, U+2000-U+200D, U+2010-U+201F,
// U+2032-U+2037). Every other code point (CJK, emoji, …) weighs 2.
func xLight(r rune) bool {
	return r <= 0x10FF || (r >= 0x2000 && r <= 0x200D) || (r >= 0x2010 && r <= 0x201F) || (r >= 0x2032 && r <= 0x2037)
}

// XWeightedLength is text's length as X counts it against XMaxWeight
// (twitter-text v3's weighting): every link XURLWeight; every other code
// point 1 in xLight's ranges and 2 outside them.
//
// ⚠ It errs LONG, never short, against X's own count: twitter-text counts a
// whole emoji sequence (a ZWJ family, a flag, a skin tone) as 2, and this
// counts each code point of it. So a part this passes, X takes.
func XWeightedLength(text string) int {
	n := 0
	rest := xURLRE.ReplaceAllStringFunc(text, func(string) string {
		n += XURLWeight
		return "" // the spaces around it stay, and are counted below
	})
	for _, r := range rest {
		if xLight(r) {
			n++
		} else {
			n += 2
		}
	}
	return n
}

// normPart is one part's text as it is posted: line endings LF, trailing
// whitespace off every line, leading and trailing blank lines dropped.
func normPart(s string) string {
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n"), "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight(ln, " \t\f\v ")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

// XParts is body split into the posts it becomes on X: for a post, the
// thread parts between XThreadSeparator lines (one part when it has none);
// for a reply, the whole body as one part. Each part is normPart'd.
func XParts(body, targetKind string) []string {
	if targetKind != TargetPost {
		return []string{normPart(body)}
	}
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n"), "\n")
	var parts []string
	var cur []string
	for _, ln := range lines {
		if strings.TrimRight(ln, " \t\f\v ") == XThreadSeparator {
			parts = append(parts, normPart(strings.Join(cur, "\n")))
			cur = nil
			continue
		}
		cur = append(cur, ln)
	}
	return append(parts, normPart(strings.Join(cur, "\n")))
}

// CheckVenueRules refuses (ErrInvalid) an outbound draft of targetKind on
// venue whose body (as screened) or media count the venue cannot take:
//
//   - x, a post or a reply: every part (XParts) weighs at most XMaxWeight
//     (XWeightedLength) and none is empty; a thread has at most
//     XMaxThreadParts parts, and media[i] goes on part i+1, so a thread has
//     no more media than parts. A single post or a reply carries up to
//     MaxMediaRefs media (validate's bound).
//   - reddit, a reply: no media.
//
// Any other venue or target kind: nil (no rule is known).
func CheckVenueRules(venue, targetKind, body string, media int) error {
	switch {
	case venue == VenueReddit && targetKind == TargetReply:
		if media > 0 {
			return invalid("a reddit reply carries no media: leave media out")
		}
	case venue == VenueX && (targetKind == TargetPost || targetKind == TargetReply):
		parts := XParts(body, targetKind)
		if len(parts) > XMaxThreadParts {
			return invalid("an x thread has at most %d parts; this one has %d", XMaxThreadParts, len(parts))
		}
		for i, p := range parts {
			if p == "" {
				return invalid("x part %d of %d is empty: a thread part needs text (parts are separated by a line that is exactly %s)",
					i+1, len(parts), XThreadSeparator)
			}
			if w := XWeightedLength(p); w > XMaxWeight {
				return invalid("x part %d of %d is %d characters as X counts them; the limit is %d (a link counts %d, CJK and emoji 2): shorten it%s",
					i+1, len(parts), w, XMaxWeight, XURLWeight, xSplitHint(targetKind))
			}
		}
		if len(parts) > 1 && media > len(parts) {
			return invalid("an x thread puts media[i] on part i+1: %d media for %d parts", media, len(parts))
		}
	}
	return nil
}

// xSplitHint is how an over-long X body could be split, by target kind.
func xSplitHint(targetKind string) string {
	if targetKind == TargetPost {
		return ", or split the post into a thread with a line that is exactly " + XThreadSeparator
	}
	return " (a reply is one part: " + XThreadSeparator + " in a reply is not a split)"
}
