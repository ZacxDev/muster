// Package fence renders UNTRUSTED TEXT as a fenced block an agent is handed as a
// turn: a preface line written by muster, then an opening marker at column 0
// carrying a fresh nonce, the content lines, and a closing marker — with EVERY
// line between the markers behind [ContentPrefix].
//
// 🔴 THE PREFIX IS WHAT MAKES THE FENCE STRUCTURAL. No content text can reach
// column 0, where the markers live, so no text can close the block early or open
// a forged one; the marker word and the block's own nonce are additionally
// replaced inside the fence ([MarkerPlaceholder], [NoncePlaceholder]), so a
// line cannot even QUOTE the real closing marker.
//
// 🔴 [Sanitize] IS THE CALLER'S JOB, PER FIELD, BEFORE A LINE IS BUILT. A field
// holding a line break would start a line with no content prefix; [Sanitize]
// folds every line break into " ⏎ " and replaces every control, format,
// separator and unassigned code point, so no output of it can. The block itself
// does not sanitise lines a second time: a caller-composed line (a standing
// sentence, a stanza header) is muster's own text.
//
// This package was extracted from internal/memorelay. A byte golden of the memo
// relay's rendered turn checked the move changed no byte; it was deleted once the
// extraction landed, so internal/memorelay's own tests are what pin its shape now.
// internal/plugins renders inbound plugin messages through it as well.
package fence

import (
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// ContentPrefix begins every line between the markers.
	ContentPrefix = "| "
	// MarkerPlaceholder replaces the block's marker word wherever a content line
	// carries it (any case).
	MarkerPlaceholder = "[fence-marker]"
	// NoncePlaceholder replaces the block's nonce wherever a content line carries
	// it (any case).
	NoncePlaceholder = "[nonce]"
)

// Block is one fenced turn. Marker is the word the opening and closing markers
// carry (e.g. "cairn-memo"); it should be a plain lower-case word, since it is
// matched case-insensitively inside the content.
type Block struct {
	// Preface is the one line OUTSIDE the fence, muster's own words: what the
	// turn is and that it asks for nothing. It is written verbatim.
	Preface string
	Marker  string
	// Count is the number of items the block carries, stated in the opening
	// marker.
	Count int
	// Lines are the content lines, each written behind ContentPrefix. An empty
	// string is a blank content line (the prefix alone).
	Lines []string
}

// NewNonce is a fresh 16-hex-character nonce, drawn per render, so text that
// guesses an earlier block's nonce still sits behind the content prefix of this
// one.
func NewNonce() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error (Go 1.24+).
	return hex.EncodeToString(b[:])
}

// Render is the whole turn: the preface, a blank line, then the fenced block.
func (b Block) Render(nonce string) string {
	markerRE := regexp.MustCompile("(?i)" + regexp.QuoteMeta(b.Marker))
	nonceRE := regexp.MustCompile("(?i)" + regexp.QuoteMeta(nonce))
	var s strings.Builder
	s.WriteString(b.Preface + "\n\n")
	s.WriteString("<<<" + b.Marker + " untrusted nonce=" + nonce + " count=" + strconv.Itoa(b.Count) + ">>>\n")
	for _, line := range b.Lines {
		line = markerRE.ReplaceAllLiteralString(line, MarkerPlaceholder)
		line = nonceRE.ReplaceAllLiteralString(line, NoncePlaceholder)
		s.WriteString(ContentPrefix + line + "\n")
	}
	s.WriteString("<<<end " + b.Marker + " nonce=" + nonce + ">>>\n")
	return s.String()
}

// openRE and closeRE are Render's two marker lines.
var (
	openRE  = regexp.MustCompile(`^<<<([a-z][a-z0-9-]*) untrusted nonce=([0-9a-f]{16}) count=([0-9]+)>>>$`)
	closeRE = regexp.MustCompile(`^<<<end ([a-z][a-z0-9-]*) nonce=([0-9a-f]{16})>>>$`)
)

// Parse reads back a turn [Block.Render] wrote: ok only when s is EXACTLY one
// rendered block — a preface, then an opening marker at column 0, every line
// up to the closing marker behind [ContentPrefix], a closing marker carrying
// the opening one's marker word and nonce, and nothing but white space after
// it. Lines are the content lines without their prefix. A line a transcript
// stripped of its trailing space (a blank content line, "|") reads as blank.
//
// It is for DRAWING a turn muster sent (the human's view of it), never for
// deciding anything: what the agent was handed is s itself.
func Parse(s string) (b Block, nonce string, ok bool) {
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), " \t\n"), "\n")
	open := -1
	for i, l := range lines {
		if openRE.MatchString(l) {
			open = i
			break
		}
	}
	if open < 0 || len(lines) < open+2 {
		return Block{}, "", false
	}
	m := openRE.FindStringSubmatch(lines[open])
	c := closeRE.FindStringSubmatch(lines[len(lines)-1])
	if c == nil || c[1] != m[1] || c[2] != m[2] {
		return Block{}, "", false
	}
	count, err := strconv.Atoi(m[3])
	if err != nil {
		return Block{}, "", false
	}
	b = Block{Preface: strings.TrimRight(strings.Join(lines[:open], "\n"), "\n"), Marker: m[1], Count: count}
	for _, l := range lines[open+1 : len(lines)-1] {
		switch {
		case strings.HasPrefix(l, ContentPrefix):
			b.Lines = append(b.Lines, l[len(ContentPrefix):])
		case l == strings.TrimRight(ContentPrefix, " "):
			b.Lines = append(b.Lines, "")
		default:
			return Block{}, "", false // a line outside the prefix: not a block Render wrote
		}
	}
	return b, m[2], true
}

// Unfold is [Sanitize]'s line-break folding undone, for showing a field as
// the text it was: each " ⏎ " back to a line break.
func Unfold(s string) string { return strings.ReplaceAll(s, " ⏎ ", "\n") }

// Cut is the first n RUNES of s, with "…" when anything was dropped.
func Cut(s string, n int) string {
	seen := 0
	for i := range s {
		if seen == n {
			return s[:i] + "…"
		}
		seen++
	}
	return s
}

// Unsafe reports whether [Sanitize] must replace r: every code point in General
// Category C* (Cc, Cf, Co, Cs, Cn), plus Zl, Zp, the variation selectors and the
// other default-ignorable code points (invisible letters and marks). "In none of
// L, M, N, P, S, Z" is exactly C*, unassigned code points included.
func Unsafe(r rune) bool {
	switch {
	case !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z):
		return true
	case unicode.In(r, unicode.Zl, unicode.Zp):
		return true
	case unicode.Is(unicode.Variation_Selector, r):
		return true
	case unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r):
		return true
	}
	return false
}

// Sanitize is the replacement every untrusted field passes through before it is
// written into a turn: each [Unsafe] code point becomes U+FFFD, except a line
// break (" ⏎ ", CRLF folded first) and a tab (one space). Invalid UTF-8 decodes
// to U+FFFD per invalid byte. Sanitising already-sanitised text is a no-op.
//
// 🔴 IT IS ALSO WHAT KEEPS A FIELD ON ONE LINE AND OUT OF A RUNTIME'S REFUSAL.
// A line break inside a field would start a line with no content prefix, and the
// claude-code runtime refuses a prompt holding any control character other than
// newline and tab (a 400, nothing pasted). No output of this function holds either.
func Sanitize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(" ⏎ ")
		case r == '\t':
			b.WriteByte(' ')
		case Unsafe(r):
			b.WriteRune(utf8.RuneError)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// MaxURLBytes bounds a URL [WebURL] keeps.
const MaxURLBytes = 2048

// WebURL is u when it is a URL an outside source may hand muster to SHOW AS
// TEXT: an absolute http or https URL with a host and no userinfo, at most
// [MaxURLBytes] bytes of UTF-8 with no space and no [Unsafe] code point; ""
// otherwise. It says nothing about where the URL leads: nothing muster draws
// from it is a link.
func WebURL(u string) string {
	if u == "" || len(u) > MaxURLBytes || !utf8.ValidString(u) {
		return ""
	}
	for _, r := range u {
		if r == ' ' || Unsafe(r) {
			return ""
		}
	}
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" || p.User != nil {
		return ""
	}
	return u
}
