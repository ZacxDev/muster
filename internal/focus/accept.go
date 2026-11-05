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
// the tail's ASSISTANT lines, is the session's own reply echoed back. Shorter
// answers are not judged, and neither are the person's own lines: a title that
// names the job in the words the person asked for it with is what the prompt
// wants, not a copy.
//
// ⚠ A QUALITY GUARD AGAINST ECHOED TEXT, NOT A PRIVACY GUARD. It catches long
// prose copied from a reply — the runaway titles this was written for. A short
// title that carries a host name or a token copied from the transcript passes
// it; nothing here redacts, and nothing should be read as if it did.
const (
	verbatimN        = 4
	verbatimMinWords = 6
	verbatimShare    = 0.6
)

// markdownTitle matches what makes a title MARKDOWN rather than a name: a
// heading at its start, **bold**, or a [link](…). "C#", "#123", "a*b" and
// "__init__" are NOT matched — they are ordinary title text
// (TestCleanStripsNoMarkdown). Backticks and a leading list or quote marker are
// not refused but removed (plainTitle): they wrap or lead a name, they do not
// replace it.
var markdownTitle = regexp.MustCompile(`^#+\s|\*\*\S|\[[^\]]*\]\([^)]*\)`)

// listMarker is a leading list item or quote marker, removed by plainTitle.
var listMarker = regexp.MustCompile(`^(?:[-*+>]\s+)+`)

// plainTitle is title with its backticks and any leading list or quote marker
// removed.
func plainTitle(title string) string {
	return strings.TrimSpace(listMarker.ReplaceAllString(strings.ReplaceAll(title, "`", ""), ""))
}

// Accept is sum as it may be stored for a session whose tail is tail and whose
// current title is prev ("" = none), or why it may not. In order:
//   - a code fence in either part, or a title that is markdown (markdownTitle,
//     after plainTitle), is ErrMarkdown;
//   - a title that names prev — the same words, ignoring case, spacing and
//     punctuation — is prev itself, byte-identical, so the session's name does
//     not flicker; and a KEPT title is never judged a copy (it was asked for
//     word for word);
//   - any other title that is mostly the tail's assistant lines word for word
//     (verbatim) is ErrTranscriptText;
//   - a description that is mostly the assistant lines word for word is
//     DROPPED, and the title kept;
//   - the title is cut to TitleCapRunes at a word boundary (CapTitle).
//
// A refusal stores nothing: the refresh records the reason and the session
// keeps the focus it had.
func Accept(sum Summary, tail, prev string) (Summary, error) {
	if strings.Contains(sum.Title+sum.Description, "```") {
		return Summary{}, ErrMarkdown
	}
	sum.Title = plainTitle(sum.Title)
	if markdownTitle.MatchString(sum.Title) {
		return Summary{}, ErrMarkdown
	}
	replies := assistantText(tail)
	if prev != "" && sameTitle(sum.Title, prev) {
		sum.Title = prev
	} else if verbatim(sum.Title, replies) {
		return Summary{}, ErrTranscriptText
	}
	if verbatim(sum.Description, replies) {
		sum.Description = ""
	}
	sum.Title = CapTitle(sum.Title)
	if sum.Title == "" {
		return Summary{}, ErrEmpty
	}
	return sum, nil
}

// assistantText is the text of tail's assistant lines (RenderTail writes one
// "assistant: …" line per reply).
func assistantText(tail string) string {
	var b strings.Builder
	for _, l := range strings.Split(tail, "\n") {
		if t, ok := strings.CutPrefix(l, "assistant: "); ok {
			b.WriteString(t)
			b.WriteByte('\n')
		}
	}
	return b.String()
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
	return title != "" && utf8.RuneCountInString(title) <= TitleCapRunes && plainTitle(title) == title &&
		!markdownTitle.MatchString(title)
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
