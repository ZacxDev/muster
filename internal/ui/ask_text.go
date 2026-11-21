package ui

import (
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/fence"
)

// ---------------------------------------------------------------------------
// ASK TEXT: what an Asks card, the ask page and the chief's question card in
// the chat draw an ask's words with — its question, its answer, a log row's
// question, a nudge's message, a why line (AskTextHTML).
//
// 🔴 IT IS markdown.go's RENDERER, NOT A SECOND ONE. AskTextHTML is that
// renderer with mdOpts.ask set, which changes four things and nothing else:
//
//   - every HIDDEN code point (HiddenRune: a bidi control, a zero-width or
//     other invisible character, a control character) is drawn as its visible
//     U+XXXX flag (HiddenFlagHTML's mark), never written raw — in a link's
//     text too. The escape (askEscape) turns each into a token no markdown
//     pattern can read, and the last pass draws the token as its flag;
//   - a URL holding one is NOT a link: its text stays text, the code point
//     flagged where it sits (mdAutolink, mdLinkify);
//   - a paragraph is not wrapped in a <p>: its box is whitespace-pre-wrap, so
//     plain ask text keeps its line breaks and blank lines exactly as before
//     it was markdown (markdownRenderOpts);
//   - a YouTube link carrying a start or an end (t=, start=, end=) has its clip
//     range after its anchor, small and muted ("2:00–2:45": askClip). No
//     network call, no thumbnail: the range is read from the URL alone.
//
// 🔴 THE MACHINE TAG `[watch:<video id>#<n>] ` that starts a watch
// officer's question is drawn AS WRITTEN (askWatchTag): the officer reads it
// back to attribute the answer.
//
// NOT a step-up card's post preview, nor a draft's: those show a post exactly
// as its venue will (internal/api stepUpPostHTML), and markdown there would
// show what is not sent.
// ---------------------------------------------------------------------------

// AskTextHTML is ask text t as HTML (the block above): markdown, safe to embed
// in a whitespace-pre-wrap box that is a <div> (a list or a quote is a block).
func AskTextHTML(t string) string {
	t = strings.ReplaceAll(t, "\t", " ")
	out := markdownRenderOpts(t, mdOpts{ask: true})
	return askHiddenTokenRe.ReplaceAllStringFunc(out, func(tok string) string {
		r, err := strconv.ParseUint(tok[len(askHiddenSep)+2:len(tok)-len(askHiddenSep)], 16, 32)
		if err != nil {
			return ""
		}
		return hiddenFlag(rune(r))
	})
}

// askHiddenSep brackets a hidden code point's token in escaped ask text
// ("\x01U+202E\x01"). It is a control character, so author text can never
// spell it: askEscape turns an author's own U+0001 into a token too.
const askHiddenSep = "\x01"

// askHiddenTokenRe is one hidden code point's token.
var askHiddenTokenRe = regexp.MustCompile("\x01U\\+[0-9A-F]{4,6}\x01")

// askWatchTag is the watch officer's machine tag at the start of a question,
// as it reads escaped: `[watch:<video id>#<n>]`.
var askWatchTag = regexp.MustCompile(`^\[` + `watch:[^\]\s#` + "\x00" + `]+#[0-9]+\]`)

// askEscape is mdEscape for ask text: each hidden code point (HiddenRune)
// becomes its token (askHiddenSep), every other character is escaped as
// mdEscape escapes it.
func askEscape(s string) string {
	rs := []rune(s)
	var b strings.Builder
	from := 0
	for i, r := range rs {
		prev, next := rune(0), rune(0)
		if i > 0 {
			prev = rs[i-1]
		}
		if i+1 < len(rs) {
			next = rs[i+1]
		}
		if !HiddenRune(prev, r, next) {
			continue
		}
		b.WriteString(mdEscape(string(rs[:i][from:])))
		b.WriteString(askHiddenSep + fmt.Sprintf("U+%04X", r) + askHiddenSep)
		from = i + 1
	}
	b.WriteString(mdEscape(string(rs[from:])))
	return b.String()
}

// ---------------------------------------------------------------------------
// HIDDEN CHARACTERS IN TEXT AN ASKS CARD DRAWS (a post's preview, a draft, a
// quoted message, a question or an answer).
//
// A bidi control (U+202E turns the rest of a line around), a zero-width or
// other invisible character, a control character: drawn as itself it is
// either invisible or reorders what is around it, so what the owner reads is
// not what is sent. Drawn spelled out (`\u{202e}`, flows.VisibleEscape) it
// was safe but unreadable — every em dash and emoji spelled out too. So each
// one is drawn IN PLACE as a small flag naming its code point
// (<mark data-stepup-hidden>U+202E</mark>, StepUpHiddenClass), and the
// character itself is never written into the page. A U+FFFD — what muster's
// own sanitising (fence.Sanitize) leaves where it took such a character out
// of a stored card — is flagged the same way.
//
// Not flagged, since they are how ordinary text is written: a variation
// selector after a symbol (❤️, the emoji form) or a digit/#/* (a keycap), and a
// zero-width joiner between two symbols (a family emoji).
// ---------------------------------------------------------------------------

// HiddenRune reports whether r, between prev and next (0 for none), is drawn
// as a flag (the block above).
func HiddenRune(prev, r, next rune) bool {
	if r == utf8.RuneError {
		return true
	}
	if !fence.Unsafe(r) || r == '\n' || r == '\t' {
		return false
	}
	switch {
	case r == 0xFE0E || r == 0xFE0F:
		return !(unicode.Is(unicode.So, prev) || unicode.IsDigit(prev) || prev == '#' || prev == '*')
	case r == 0x200D:
		return !(unicode.Is(unicode.So, prev) || prev == 0xFE0F) || !unicode.Is(unicode.So, next)
	}
	return true
}

// hiddenFlag is hidden code point r's flag.
func hiddenFlag(r rune) string {
	cp := fmt.Sprintf("U+%04X", r)
	return `<mark class="` + StepUpHiddenClass + `" data-stepup-hidden="` + cp + `" title="Hidden character ` + cp + `">` + cp + `</mark>`
}

// HiddenFlagHTML is t escaped, each hidden code point (HiddenRune) drawn as
// its flag, every other character as itself.
func HiddenFlagHTML(t string) string {
	rs := []rune(t)
	var b strings.Builder
	from := 0
	for i, r := range rs {
		prev, next := rune(0), rune(0)
		if i > 0 {
			prev = rs[i-1]
		}
		if i+1 < len(rs) {
			next = rs[i+1]
		}
		if !HiddenRune(prev, r, next) {
			continue
		}
		b.WriteString(html.EscapeString(string(rs[:i][from:])))
		b.WriteString(hiddenFlag(r))
		from = i + 1
	}
	b.WriteString(html.EscapeString(string(rs[from:])))
	return b.String()
}

// ---------------------------------------------------------------------------
// A YOUTUBE LINK'S CLIP RANGE.
// ---------------------------------------------------------------------------

// AskClipClass is a YouTube link's clip range, after its anchor.
const AskClipClass = "ml-1 whitespace-nowrap text-xs tabular-nums text-muted"

// clip is the clip range drawn after an anchor to href (already escaped):
// ask text's (askClip), "" for any other render.
func (o mdOpts) clip(href string) string {
	if !o.ask {
		return ""
	}
	return askClip(html.UnescapeString(href))
}

// askClip is the clip range of link u, "" for none: a youtube.com/watch,
// youtube.com/shorts/… or youtu.be/… link with a start (t= or start=) and/or
// an end (end=), each a number of seconds or YouTube's 1h2m3s form. A value
// it cannot read, or an end not after the start, draws no range at all.
func askClip(u string) string {
	p, err := url.Parse(u)
	if err != nil || !askYouTube(p) {
		return ""
	}
	q := p.Query()
	startRaw := q.Get("start")
	if startRaw == "" {
		startRaw = q.Get("t")
	}
	endRaw := q.Get("end")
	if startRaw == "" && endRaw == "" {
		return ""
	}
	start, end := 0, -1
	if startRaw != "" {
		s, ok := askClipSeconds(startRaw)
		if !ok {
			return ""
		}
		start = s
	}
	if endRaw != "" {
		e, ok := askClipSeconds(endRaw)
		if !ok || e <= start {
			return ""
		}
		end = e
	}
	label := askClipTime(start)
	if end >= 0 {
		label += "–" + askClipTime(end)
	}
	return `<span class="` + AskClipClass + `" data-ask-clip>` + label + `</span>`
}

// askYouTube reports whether p is a YouTube video link: youtu.be/<id>, or
// youtube.com (www., m.) /watch?v=<id> or /shorts/<id>, over http(s).
func askYouTube(p *url.URL) bool {
	if p.Scheme != "https" && p.Scheme != "http" {
		return false
	}
	host := strings.ToLower(p.Hostname())
	switch host {
	case "youtu.be":
		return len(strings.Trim(p.Path, "/")) > 0
	case "youtube.com", "www.youtube.com", "m.youtube.com":
		switch {
		case p.Path == "/watch":
			return p.Query().Get("v") != ""
		case strings.HasPrefix(p.Path, "/shorts/"):
			return len(strings.Trim(strings.TrimPrefix(p.Path, "/shorts/"), "/")) > 0
		}
	}
	return false
}

// askClipSecondsRe is YouTube's time form: 90, 90s, 1m30s, 1h2m3s.
var askClipSecondsRe = regexp.MustCompile(`^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s?)?$`)

// askClipSeconds reads a t=/start=/end= value as seconds; ok false for one it
// cannot read.
func askClipSeconds(v string) (int, bool) {
	m := askClipSecondsRe.FindStringSubmatch(v)
	if m == nil || v == "" {
		return 0, false
	}
	n := 0
	for i, mul := range []int{3600, 60, 1} {
		if m[i+1] == "" {
			continue
		}
		x, err := strconv.Atoi(m[i+1])
		if err != nil || x > 1_000_000 {
			return 0, false
		}
		n += x * mul
	}
	return n, true
}

// askClipTime is s seconds as a clip time: m:ss, or h:mm:ss from an hour.
func askClipTime(s int) string {
	h, m, sec := s/3600, s%3600/60, s%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	}
	return fmt.Sprintf("%d:%02d", m, sec)
}
