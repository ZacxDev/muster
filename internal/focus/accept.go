package focus

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// WHAT A SUMMARIZER'S ANSWER MUST BE BEFORE IT IS STORED (Accept). The prompt
// ASKS for a short, stable, plain title in the summarizer's own words; this is
// where the code ENFORCES it, for every Summarizer, because a model is free to
// ignore a prompt — and the one in production did: titles past 60 characters,
// sentences in transcript voice, and a title that was the session's own
// assistant text word for word, markdown included.

// TitleCapRunes is the longest title Accept lets through: the length the
// prompt asks for, enforced in code. A longer one is cut at a word boundary
// (CapTitle). It is a DISPLAY bound, distinct from MaxTitleRunes, the storage
// guard migration 0023 holds — a stored title from before this cap can still
// be longer.
const TitleCapRunes = 60

// The verbatim-text test (verbatim): an answer of at least verbatimMinWords
// words, of whose word verbatimN-grams at least verbatimShare also occur in
// the tail, is the transcript's own text. Shorter answers are not judged: a
// title of a few words can share a phrase with the prompt that asked for the
// work without being a copy of it.
const (
	verbatimN        = 4
	verbatimMinWords = 6
	verbatimShare    = 0.6
)

// markdownTitle matches what makes a title markdown rather than a name: a code
// span or fence, bold/underline emphasis, a heading, a list item or a quote at
// its start. "C#", "#123" and "a*b" are NOT matched — they are ordinary title
// text (TestCleanStripsNoMarkdown).
var markdownTitle = regexp.MustCompile("`|\\*\\*|__|^#+\\s|^[-*+>]\\s")

// Accept is sum as it may be stored for a session whose tail is tail and whose
// current title is prev ("" = none), or why it may not:
//   - ErrTranscriptText when the title is markdown (markdownTitle), when either
//     part carries a code fence, or when either part is mostly the tail word for
//     word (verbatim);
//   - otherwise the title cut to TitleCapRunes at a word boundary (CapTitle),
//     and — when it names prev in another case or spacing — prev itself, so a
//     kept title is stored byte-identical and writes no new history row.
//
// A refusal stores nothing: the refresh records the reason and the session
// keeps the focus it had.
func Accept(sum Summary, tail, prev string) (Summary, error) {
	if markdownTitle.MatchString(sum.Title) || strings.Contains(sum.Title+sum.Description, "```") {
		return Summary{}, ErrTranscriptText
	}
	if verbatim(sum.Title, tail) || verbatim(sum.Description, tail) {
		return Summary{}, ErrTranscriptText
	}
	sum.Title = CapTitle(sum.Title)
	if sum.Title == "" {
		return Summary{}, ErrEmpty
	}
	if prev != "" && sameTitle(sum.Title, prev) {
		sum.Title = prev
	}
	return sum, nil
}

// CapTitle is title at most TitleCapRunes runes: cut at the last word
// boundary inside the cap (a single word longer than the cap is cut at the
// cap), with trailing punctuation and any ellipsis — which the prompt forbids
// and a cut title must not claim — removed.
func CapTitle(title string) string {
	title = strings.TrimSpace(title)
	if utf8.RuneCountInString(title) > TitleCapRunes {
		r := []rune(title)
		cut := string(r[:TitleCapRunes])
		if !unicode.IsSpace(r[TitleCapRunes]) {
			if i := strings.LastIndexFunc(cut, unicode.IsSpace); i > 0 {
				cut = cut[:i]
			}
		}
		title = cut
	}
	return strings.TrimRightFunc(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(title), "…"), "..."),
		func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(",;:.-–—…", r) })
}

// FeedableTitle reports whether a stored title may be fed back to the
// summarizer as the one to keep (Request.PrevTitle): one Accept could have
// produced — within TitleCapRunes, not markdown. A title stored before the cap
// (up to the 300-rune guard, or transcript prose) is not fed back, so the
// summarizer is never told to keep a runaway.
func FeedableTitle(title string) bool {
	return title != "" && utf8.RuneCountInString(title) <= TitleCapRunes && !markdownTitle.MatchString(title)
}

// sameTitle reports whether a and b are one title written differently: the
// same words, ignoring case, spacing and punctuation.
func sameTitle(a, b string) bool {
	return strings.Join(words(a), " ") == strings.Join(words(b), " ")
}

// verbatim reports whether text is mostly the tail word for word: at least
// verbatimMinWords words, and at least verbatimShare of its word
// verbatimN-grams found in the tail.
func verbatim(text, tail string) bool {
	w := words(text)
	if len(w) < verbatimMinWords {
		return false
	}
	have := map[string]bool{}
	tw := words(tail)
	for i := 0; i+verbatimN <= len(tw); i++ {
		have[strings.Join(tw[i:i+verbatimN], " ")] = true
	}
	total, hit := 0, 0
	for i := 0; i+verbatimN <= len(w); i++ {
		total++
		if have[strings.Join(w[i:i+verbatimN], " ")] {
			hit++
		}
	}
	return total > 0 && float64(hit) >= verbatimShare*float64(total)
}

// words is s lower-cased and split into runs of letters and digits.
func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
}
