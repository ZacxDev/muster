package ui

import (
	"html"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// SESSION CARDS IN THE CHIEF'S CHAT (operator decision, 2026-11-19). When the
// chief names one of its owner's sessions, the chat draws a compact card —
// the session's title, its computer and a status dot — that opens the session
// on a tap. Its tmux address is behind the card's ⓘ, never in the text.
//
// 🔴 THE CHIEF WRITES A MARKER; THE SERVER RESOLVES IT. The marker is
//
//	[[session:<id>]]
//
// where <id> is the session's id exactly as the chief's tools hand it out
// (sessions_overview, fleet_list, focus_search, route_to_session's subject,
// a hand-back's head): "mirror:<conversation key>" or "agent:<n>". The id is
// a KEY, never a name: the card's title, computer, status and link are what
// the server's own index holds for it (internal/api sessionCards), read under
// the VIEWER's principal from the viewer's OWN sessions. So:
//
//   - a marker naming a session the index holds draws its card;
//   - any other marker — another owner's session, a team's, an ended one, a
//     made-up id — draws the plain words "a session": never a card, and never
//     anything the marker's id could have looked up (no title leaks, because
//     nothing is looked up outside the viewer's own sessions).
//
// For old replies and for a model's slip, a raw `tmux <session>:<index> @
// <host>` or `mirror:<key>` in the text (or alone in a code span) is drawn as
// the same card ONLY when it is EXACTLY a key of the index; anything else —
// a near miss, another owner's — is left as written. No pattern alone ever
// makes a card: the index decides (the mention rule, mentions.go).

// SessionCard is one session a chief's reply can name, as the server read it.
type SessionCard struct {
	// Title is the session's name as the top layer shows it.
	Title string
	// Computer is the name of the computer it runs on ("" for an agent, which
	// runs in muster's own cluster).
	Computer string
	// Status is SessionCardBusy, SessionCardIdle or SessionCardWaiting.
	Status string
	// Href is the session's page. "" marks a key two sessions share: no card.
	Href string
	// Address is the session's technical address (its tmux target, an
	// agent's slug): shown only in the card's ⓘ.
	Address string
}

// The card's statuses.
const (
	SessionCardBusy    = "busy"
	SessionCardIdle    = "idle"
	SessionCardWaiting = "waiting"
)

// SessionCardMarker is the marker the chief writes for session id.
func SessionCardMarker(id string) string { return "[[session:" + id + "]]" }

// sessionCardUnknown is what a marker no card answers for draws.
const sessionCardUnknown = "a session"

// sessionCardStatusWords are each status's words: the dot's tooltip and part
// of the card's accessible name.
var sessionCardStatusWords = map[string]string{
	SessionCardBusy:    "working",
	SessionCardIdle:    "idle",
	SessionCardWaiting: "waiting on you",
}

// sessionCardDotClass is each status's dot colour.
var sessionCardDotClass = map[string]string{
	SessionCardBusy:    "bg-accent",
	SessionCardIdle:    "bg-idle-5",
	SessionCardWaiting: "bg-st-warning-fg",
}

var (
	// sessionCardMarkerRe is a marker, in ESCAPED inline text (no character of
	// it is one mdEscape changes).
	sessionCardMarkerRe = regexp.MustCompile(`\[\[session:([^\]\s\x00]{1,300})\]\]`)
	// sessionCardTmuxRe is a raw tmux target as mirror.WhereTarget spells it,
	// in escaped text; its host may carry the sentence's punctuation.
	sessionCardTmuxRe = regexp.MustCompile(`tmux ([^\s\x00]{1,200}) @ ([^\s\x00]{1,200})`)
	// sessionCardSubjectRe is a raw mirrored session id.
	sessionCardSubjectRe = regexp.MustCompile(`mirror:[A-Za-z0-9][A-Za-z0-9:_-]{0,200}`)
)

// sessionCardTrail is what a raw reference may end with that belongs to the
// sentence, not the reference.
const sessionCardTrail = `.,;:!?)]}"'’`

// sessionCardSeq numbers the cards' ⓘ sheets, process-wide, so two renders on
// one page (the log and a streamed reply) never share an element id.
var sessionCardSeq atomic.Uint64

// card is key's card, when the index holds a resolvable one.
func (mc *mentionCtx) card(key string) (SessionCard, bool) {
	if mc == nil || mc.mx == nil {
		return SessionCard{}, false
	}
	c, ok := mc.mx.sessionCard(key)
	return c, ok && c.Href != ""
}

// stashCard stashes card c's inline markup in v and keeps its sheet for the
// end of the message (markdownRender): the sheet is a <div>, which may not sit
// inside the paragraph the card is in.
func (mc *mentionCtx) stashCard(c SessionCard, v *mdVault) string {
	inline, sheet := sessionCardHTML(c, "sc-"+strconv.FormatUint(sessionCardSeq.Add(1), 10))
	mc.sheets = append(mc.sheets, sheet)
	return v.stash(inline)
}

// sessionCardCode is the card a code span's ESCAPED content stands for: a
// marker (a card, or the plain words for one that does not resolve), or a raw
// reference that resolves exactly. ok false: the span stays code.
func sessionCardCode(content string, v *mdVault, mc *mentionCtx) (string, bool) {
	if mc == nil || mc.mx == nil || !mc.mx.cardsOn() {
		return "", false
	}
	raw := strings.TrimSpace(html.UnescapeString(content))
	if m := sessionCardMarkerRe.FindStringSubmatch(raw); m != nil && m[0] == raw {
		if c, ok := mc.card(m[1]); ok {
			return mc.stashCard(c, v), true
		}
		return sessionCardUnknown, true
	}
	if c, ok := mc.card(raw); ok && (strings.HasPrefix(raw, "tmux ") || strings.HasPrefix(raw, "mirror:")) {
		return mc.stashCard(c, v), true
	}
	return "", false
}

// mdSessionCards replaces s's markers and the raw references that resolve
// exactly with cards. s is escaped inline text with code spans stashed.
func mdSessionCards(s string, v *mdVault, mc *mentionCtx) string {
	if mc == nil || mc.mx == nil || !mc.mx.cardsOn() {
		return s
	}
	s = sessionCardMarkerRe.ReplaceAllStringFunc(s, func(m string) string {
		id := sessionCardMarkerRe.FindStringSubmatch(m)[1]
		if c, ok := mc.card(html.UnescapeString(id)); ok {
			return mc.stashCard(c, v)
		}
		return sessionCardUnknown
	})
	s = sessionCardRaw(s, sessionCardTmuxRe, v, mc)
	return sessionCardRaw(s, sessionCardSubjectRe, v, mc)
}

// sessionCardRaw draws each match of re in s that starts at a word boundary
// and, less any trailing punctuation, is EXACTLY a key of the index, as its
// card; every other match is left as it was.
func sessionCardRaw(s string, re *regexp.Regexp, v *mdVault, mc *mentionCtx) string {
	var b strings.Builder
	done := 0
	for _, loc := range re.FindAllStringIndex(s, -1) {
		i, j := loc[0], loc[1]
		if i > 0 && sessionCardWordByte(s[i-1]) {
			continue
		}
		raw := html.UnescapeString(span(s, i, j))
		key := raw
		var c SessionCard
		ok := false
		for {
			if c, ok = mc.card(key); ok || key == "" {
				break
			}
			// Trailing punctuation one character at a time: a host may end
			// in a "." of its own.
			n := sessionCardLastRuneLen(key)
			if !strings.Contains(sessionCardTrail, span(key, len(key)-n, len(key))) {
				break
			}
			key = span(key, 0, len(key)-n)
		}
		if !ok {
			continue
		}
		b.WriteString(span(s, done, i))
		b.WriteString(mc.stashCard(c, v))
		b.WriteString(mdEscape(span(raw, len(key), len(raw))))
		done = j
	}
	if done == 0 {
		return s
	}
	b.WriteString(span(s, done, len(s)))
	return b.String()
}

func sessionCardWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == ':' || c == '/'
}

// sessionCardLastRuneLen is the byte length of s's last rune.
func sessionCardLastRuneLen(s string) int {
	_, n := utf8.DecodeLastRuneInString(s)
	return n
}

// sessionCardHTML is card c's inline markup and its ⓘ sheet, the sheet's
// element id id. The card is phrasing content (a span holding a link and the
// ⓘ button), so it sits inside a sentence.
func sessionCardHTML(c SessionCard, id string) (inline, sheet string) {
	status := c.Status
	if sessionCardStatusWords[status] == "" {
		status = SessionCardIdle
	}
	// The name as every list draws a session's: through the title rule
	// (plainOr), so a markdown name draws as words.
	title := plainOr(c.Title, sessionCardUnknown)
	name := title
	if c.Computer != "" {
		name += " on " + c.Computer
	}
	info := InfoSheet{ID: id, Label: "Where this session runs", Heading: "Where it runs",
		Lead: "Its address, for a terminal on its computer:", Code: c.Address, Inline: true}
	card := Span(
		g.Attr("data-session-card", status),
		Class("mx-0.5 inline-flex max-w-full items-center gap-0.5 rounded-xl bg-bg/60 align-middle ring-1 ring-inset ring-line"),
		A(
			Href(c.Href),
			hx("hx-boost", "false"),
			g.Attr("data-session-card-open", ""),
			g.Attr("aria-label", "Open "+name+", "+sessionCardStatusWords[status]),
			Class("press inline-flex min-h-[32px] min-w-0 items-center gap-1.5 rounded-xl py-0.5 pl-2 pr-1 text-fg no-underline hover:bg-s2"),
			Span(
				g.Attr("data-session-status", status),
				g.Attr("aria-hidden", "true"),
				g.Attr("title", sessionCardStatusWords[status]),
				Class("inline-block h-2 w-2 shrink-0 rounded-full "+sessionCardDotClass[status]),
			),
			Span(g.Attr("data-session-card-title", ""), Class("min-w-0 truncate font-medium"), g.Text(title)),
			g.If(c.Computer != "", Span(g.Attr("data-session-card-computer", ""), Class("shrink-0 text-xs text-muted"), g.Text(c.Computer))),
		),
		g.If(c.Address != "", info.trigger()),
	)
	var a, b strings.Builder
	_ = card.Render(&a)
	if c.Address != "" {
		_ = info.sheet().Render(&b)
	}
	return a.String(), b.String()
}
