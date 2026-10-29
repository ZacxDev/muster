package ui

import (
	"strconv"
	"strings"
	"time"
	"unicode"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// WHAT A CARD SAYS ABOUT ITS RECENCY, WHAT IT IS WAITING ON, AND HOW LONG ITS
// TITLE MAY BE. Each is decided here, once, as a Go table or function; the
// cards render from it and the list's scripts are handed it as JSON, so the
// page, the script and the tests read one definition.

// ---------------------------------------------------------------------------
// RECENCY: a small dot beside "active … ago", tinted by how long ago that was.
//
// The palette and the thresholds are the operator's tmux idle-fade window
// colours — the 8-step Gruvbox scale their tmux status bar tints each window
// with (the "Idle-Fade Window Colors" block of their tmux config and the
// tmux-idle-update.sh script it runs: `idle_secs < 600` bright green, … , else
// grey). Mirroring it means a window reads the same colour in tmux and here.
// The colours are Gruvbox's published palette; nothing in the table is private.
//
// 🔴 THE DOT IS NOT THE CARD'S BORDER. The border is reserved for a per-window
// tmux identity colour (a separate design); recency must not claim it.
//
// The dot is decoration (aria-hidden): the words beside it ("active 3m ago")
// carry the same fact, so colour is never the only signal.

// recencyStep is one bucket: an entry idle for less than Under (0 = no upper
// bound, the last bucket) is drawn with Class.
type recencyStep struct {
	Under time.Duration
	// Class is the dot's colour utility: a role token, bg-idle-<i>, whose
	// value is tmux-idle-update.sh's colour for the bucket, defined once in
	// web/css/input.css (--mu-idle-<i>, both themes) — this package names no
	// hue (TestNoHardCodedColourInUI). TestRecencyTokensAreTheTmuxPalette holds
	// those values to the script's.
	Class string
	// Name is the script's own word for the bucket: the dot's tooltip.
	Name string
}

// recencySteps is the scale, freshest first. Its boundaries are the script's
// `(( idle_secs < N ))` tests, exclusive, in seconds: 600, 1800, 3600, 7200,
// 14400, 28800, 86400 — and its colours, in order: bright green, green, aqua,
// yellow, orange, red, purple, grey.
var recencySteps = []recencyStep{
	{10 * time.Minute, "bg-idle-0", "fresh (under 10 min)"},
	{30 * time.Minute, "bg-idle-1", "active (10–30 min)"},
	{time.Hour, "bg-idle-2", "warm (30–60 min)"},
	{2 * time.Hour, "bg-idle-3", "cooling (1–2 h)"},
	{4 * time.Hour, "bg-idle-4", "idle (2–4 h)"},
	{8 * time.Hour, "bg-idle-5", "stale (4–8 h)"},
	{24 * time.Hour, "bg-idle-6", "dormant (8–24 h)"},
	{0, "bg-idle-7", "ancient (over 24 h)"},
}

// recencyBucket is the index in recencySteps of an entry last active at t, as
// of now; -1 for a zero t (unknown: no dot). A t in the future (clock skew) is
// the freshest bucket.
func recencyBucket(now, t time.Time) int {
	if t.IsZero() {
		return -1
	}
	d := now.Sub(t)
	for i, s := range recencySteps {
		if s.Under == 0 || d < s.Under {
			return i
		}
	}
	return len(recencySteps) - 1
}

// recencyDotClass is the dot's class for a bucket: the fixed box (so a
// re-tint never moves anything) and the bucket's colour.
func recencyDotClass(bucket int) string {
	return "inline-block h-2 w-2 shrink-0 rounded-full " + recencySteps[bucket].Class
}

// recencyDot is the dot for an entry last active at t, or nothing for a zero
// t (cardActivity prints no "active" then either). data-recency-ts lets the
// list's script re-tint it as time passes without a re-render.
func recencyDot(now, t time.Time) g.Node {
	b := recencyBucket(now, t)
	if b < 0 {
		return nil
	}
	return Span(
		g.Attr("data-recency", strconv.Itoa(b)),
		g.Attr("data-recency-ts", strconv.FormatInt(t.UnixMilli(), 10)),
		g.Attr("aria-hidden", "true"),
		g.Attr("title", recencySteps[b].Name),
		Class(recencyDotClass(b)),
	)
}

// ---------------------------------------------------------------------------
// WAITING: whether an entry is blocked on the operator, from its attention
// (CardAttention, composed in internal/api from the attention queue's
// ONE read — never re-derived here). One word (waitingToken):
//
//	you   it is waiting on YOU, any of:
//	      - muster's attention_ask (the queue's "Question" row): a dispatched
//	        agent or a mirrored Claude session called attention_ask and is
//	        waiting for your answer;
//	      - a mirrored Claude Code session stopped at a prompt (status
//	        "waiting": a permission prompt, the session ASKING to go on; the
//	        queue's "Session waiting");
//	      - Claude Code's own AskUserQuestion picker, held open by a mirrored
//	        session (the queue's "Session question");
//	      - a dispatched agent blocked at a permission prompt (the "awaiting
//	        approval" badge, AgentCardView.AwaitingApproval) — not an attention
//	        item, but the same kind of block as a mirrored session's prompt.
//
// Whichever way an entry asks, it is waiting on you. A reply draft (kind
// "plugin") is not: nothing is blocked on it.

// waitKinds maps each attention kind a card can count to the wait word it
// files under ("" = none).
var waitKinds = map[string]string{
	"ask":      waitingToken,
	"session":  waitingToken,
	"question": waitingToken,
	"plugin":   "",
}

// waitTokens is an entry's wait word: waitingToken when any of its attention
// items' kinds files under it, or the agent is awaiting approval; "" for none.
func waitTokens(a *CardAttention, awaitingApproval bool) string {
	if awaitingApproval {
		return waitingToken
	}
	if a != nil && a.Count > 0 {
		for _, k := range a.Kinds {
			if w := waitKinds[k]; w != "" {
				return w
			}
		}
	}
	return ""
}

// waitingToken is the one wait word (waitTokens' "you"): an entry carrying it
// is waiting on the viewer (listEntry.waiting) — what the session switcher's
// Waiting view holds, its waiting dot, the sidebar's order and its rows'
// waiting mark all read. (It was the retired Agents list's "Waiting on you"
// chip's token.)
const waitingToken = "you"

// ---------------------------------------------------------------------------
// LONG TITLES: what heads a card — its NAME, or its FOCUS title when it has one
// (focus.go) — is clamped to cardTitleMax characters; past that it ends in a
// "…" BUTTON that opens the whole title in a popover. Both go through ONE
// mechanism, cardTitleRow: the row, the clamp, the "…" control and the popover.
//
// The row sits OUTSIDE the card's nav anchor (a button inside an <a> is
// invalid and its tap would open the card), so a card with a long name draws
// its name in a title row of its own above the anchor — the place its focus
// row already had. A short name is drawn inside the anchor as it always was.

// cardTitleMax is the most characters (runes) of a title a card draws.
const cardTitleMax = 60

// clampTitle is s as a card (and the agent page header) draws it: whole when
// it fits, else at most its first cardTitleMax runes (trailing space trimmed)
// and true.
//
// The cut is moved BACK to a grapheme-cluster boundary (graphemeJoins), so a
// clamp never ends on half of what a reader sees as one character: a family
// emoji (ZWJ sequence), a flag (regional-indicator pair), a letter with a
// combining accent, a skin-tone or variation-selector modifier.
func clampTitle(s string) (string, bool) {
	r := []rune(s)
	if len(r) <= cardTitleMax {
		return s, false
	}
	n := cardTitleMax
	for n > 0 && graphemeJoins(r, n) {
		n--
	}
	if n == 0 { // one cluster longer than the budget: cut it rather than draw nothing
		n = cardTitleMax
	}
	return strings.TrimRight(string(r[:n]), " \t"), true
}

// graphemeJoins reports whether r[n-1] and r[n] belong to ONE extended
// grapheme cluster, so a cut between them would split it. It covers the
// clusters a title realistically carries — Extend/SpacingMark (combining
// marks, variation selectors, emoji modifiers, tag characters), ZWJ
// sequences and regional-indicator pairs — not the whole of UAX #29 (no
// Hangul jamo or Prepend rules), which would need a dependency this module
// does not have.
func graphemeJoins(r []rune, n int) bool {
	a, b := r[n-1], r[n]
	if graphemeExtends(b) || a == '\u200d' {
		return true
	}
	if isRegionalIndicator(a) && isRegionalIndicator(b) {
		// Pairs are counted from the start of the run: the cut splits a flag
		// when an ODD number of indicators precede it.
		k := 0
		for i := n - 1; i >= 0 && isRegionalIndicator(r[i]); i-- {
			k++
		}
		return k%2 == 1
	}
	return false
}

// graphemeExtends is a rune that attaches to the one before it.
func graphemeExtends(c rune) bool {
	switch {
	case c == '\u200d', // ZWJ
		c >= 0xFE00 && c <= 0xFE0F, // variation selectors
		c >= 0xE0100 && c <= 0xE01EF,
		c >= 0x1F3FB && c <= 0x1F3FF, // emoji skin-tone modifiers
		c >= 0xE0020 && c <= 0xE007F: // tag characters (subdivision flags)
		return true
	}
	return unicode.In(c, unicode.Mn, unicode.Me, unicode.Mc)
}

func isRegionalIndicator(c rune) bool { return c >= 0x1F1E6 && c <= 0x1F1FF }

// titleMoreButton is THE "…" control that opens a clamped title's popover —
// the cards' (cardTitleRow) and the agent page header's (agentTitle). fit is
// only how it sits in its row (a card's top-aligned title row vs the header's
// centred one); the control, its name and its ARIA are the same everywhere.
//
// extra is appended to the button (the agent header passes `hidden` while its
// title is not clipped; see agentTitleFitScript).
func titleMoreButton(pop, fit string, extra ...g.Node) g.Node {
	return Button(
		Type("button"),
		g.Group(extra),
		g.Attr("data-title-more", ""),
		g.Attr("popovertarget", pop),
		g.Attr("aria-haspopup", "dialog"),
		g.Attr("aria-controls", pop),
		g.Attr("aria-expanded", "false"),
		g.Attr("aria-label", "Show the full title"),
		Class("press inline-flex h-11 min-w-[44px] shrink-0 justify-center rounded-lg font-semibold leading-tight text-muted transition hover:bg-s2 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus aria-expanded:bg-s2 "+fit),
		g.Text("…"),
	)
}

// fullTitlePopover is THE popover holding a clamped title whole: a native
// popover="auto" (top layer; an outside tap or Escape closes it) placed by
// floatingMenuScript ([data-floating-menu]). The text wraps anywhere — a
// 600-character token with no break opportunity included — and a title too
// long for the viewport scrolls inside the panel instead of running off it
// (floatingMenuScript's place() sets the height cap, from the viewport).
func fullTitlePopover(pop, full string) g.Node {
	return Div(
		ID(pop),
		g.Attr("popover", "auto"),
		g.Attr("role", "dialog"),
		g.Attr("aria-label", "Full title"),
		g.Attr("data-floating-menu", ""),
		g.Attr("data-title-full", ""),
		Class("invisible hidden [&:popover-open]:flex m-0 w-72 max-w-[calc(100vw-16px)] overflow-y-auto overscroll-contain flex-col gap-1.5 rounded-xl border border-line bg-s1 p-3 text-fg shadow-2xl ring-1 ring-black/20"),
		Span(Class("[overflow-wrap:anywhere] text-sm font-semibold"), g.Text(full)),
	)
}
