package ui

import (
	"html"
	"strconv"
)

// THE ASKS PAGE'S SHARED PIECES (internal/api renders the page; the classes
// live here because the stylesheet scans internal/ui, not internal/api — a
// class written only there is never emitted, with css-check green).
//
// Since the design review the Asks page has ONE card (asks_card.go: the feed
// card, AsksFeedCardClass; AsksCardClass is it too) and one each of the
// pieces cards are made of: the pill (AsksPillBaseClass — counts, kind
// badges, metadata), the primary button (AsksPrimaryButtonBase), the empty
// state (AsksEmptyHTML). A session page's asks block (internal/api
// attentionItemHTML) still draws a head that IS the link to the session
// (AsksHeadLinkClass, AsksChevron) and rows that are one link (AsksRowClass).
// Every target is 44px.

const (
	// AsksGroupClass is one group's <li> (its label, then its <ul>).
	AsksGroupClass = "flex flex-col gap-2"
	// AsksGroupListClass is a group's own <ul> of cards.
	AsksGroupListClass = "flex flex-col gap-2"
	// AsksCardClass is every Asks card that is not the Open/Answered feed's
	// expandable one — a draft, an outreach batch, a nudge, a log row, a
	// stalled session — and since the design review it IS the feed card
	// (AsksFeedCardClass): one card design on the page. Its <li> carries
	// AsksFixedCardAttrs (TestEveryAsksListItemIsACard).
	AsksCardClass = AsksFeedCardClass
	// AsksRowClass is a card that is ONE link as a whole: a waiting window, a
	// stalled session, a task to route. A real <a>, so it is keyboard- and
	// screen-reader-reachable as a link (never a div with a click handler).
	AsksRowClass = "press flex min-h-[44px] min-w-0 items-center gap-3 rounded-xl bg-s1 px-3 py-2.5 ring-1 ring-inset ring-line transition hover:bg-s2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"
	// AsksHeadLinkClass is a card's head when it links to the session: the
	// whole head row is the target.
	AsksHeadLinkClass = "press -m-1 flex min-h-[44px] min-w-0 items-center gap-3 rounded-lg p-1 transition hover:bg-s2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"
	// AsksHeadClass is a card's head with nothing to open.
	AsksHeadClass = "flex min-w-0 items-center gap-3"
	// AsksHeadTextClass wraps the head's lines, beside the chevron.
	AsksHeadTextClass = "flex min-w-0 flex-1 flex-col"
	// AsksTitleClass is the head's first line: the session's name.
	AsksTitleClass = "truncate text-sm font-semibold text-fg"
	// AsksSubClass is the head's muted second line: host, age.
	AsksSubClass = "truncate text-xs text-fg2"
	// AsksTargetClass is the place inside the sub line ("on atlas"): words,
	// not an address — the tmux target is behind the card's ⓘ
	// (ui.SessionPlaceSheet), so it is drawn as text, not as code.
	AsksTargetClass = "text-fg2"
	// AsksWaitLineClass is a waiting row's one line: what it waits on (its
	// pending call, a question's first line).
	AsksWaitLineClass = "truncate font-mono text-xs text-fg"
	// AsksTextClass is a card's question or message.
	AsksTextClass = "whitespace-pre-wrap break-words text-sm text-fg"
	// AsksMutedTextClass is a card's secondary line.
	AsksMutedTextClass = "truncate text-xs text-fg2"
	// AsksQuietCardClass dims a row that is held (a quiet episode).
	AsksQuietCardClass = "opacity-60"

	// THE ANSWER FORM's options, the card feed's (internal/api
	// askFeedAnswerFormHTML): full-width buttons; a tap answers.
	AsksOptionsClass = "flex flex-col gap-1.5"
	// AsksOptionsPairClass lays SHORT options (askOptionsShort) two to a row:
	// the same full-width buttons, half as many rows (the design review's
	// density target). A long option keeps the column.
	AsksOptionsPairClass = "grid grid-cols-2 gap-1.5"
	AsksOptionClass  = "press min-h-[44px] w-full rounded-lg bg-bg px-3 py-2 text-left text-sm leading-snug text-fg ring-1 ring-inset ring-edge transition hover:bg-s2"
	// AsksActionsClass is the row of a card's actions: the primary one fills
	// it, the secondary ones are small beside it.
	AsksActionsClass = "flex min-w-0 items-center gap-2"
	// AsksDecisionClass is a reply draft's decision (plugin_drafts.go):
	// Approve and send on its own full-width row, the Reject form (its reason
	// box, then Reject) on the row under it. 🔴 NOT AsksActionsClass: there the
	// primary form's flex-1 is a ZERO basis, and beside a reason box's
	// intrinsic width the Reject form took the row and left Approve and send a
	// sliver with its label unreadable — at a phone's width, and at a desktop's
	// before the Asks column widened (#689, e2e/tests/draft-card.spec.ts).
	AsksDecisionClass = "flex min-w-0 flex-col gap-2"
	// AsksIconButtonClass is a small secondary action drawn as an icon (a 44px
	// target): it MUST carry aria-label and title.
	AsksIconButtonClass = "press inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-lg text-fg2 transition hover:bg-s2 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"
	// AsksSecondaryButtonClass is a small secondary action with words.
	AsksSecondaryButtonClass = "press inline-flex min-h-[44px] shrink-0 items-center justify-center whitespace-nowrap rounded-lg px-3 text-sm font-medium text-fg2 ring-1 ring-inset ring-edge transition hover:bg-s2 hover:text-fg"
	// AsksPrimaryButtonBase is THE primary button (Answer, Approve and send):
	// one size, 44px, text-sm. AsksPrimaryButtonClass fills its row (a draft's
	// Approve and send, #689); AsksCardSendClass sits beside the answer box.
	AsksPrimaryButtonBase  = "press min-h-[44px] rounded-lg bg-accent px-4 text-sm font-semibold text-on-accent transition hover:bg-accent/90"
	AsksPrimaryButtonClass = AsksPrimaryButtonBase + " w-full py-2"
	// AsksQuoteClass is quoted text from outside (a draft's inbound message).
	AsksQuoteClass = "flex min-w-0 flex-col gap-1 border-l-2 border-edge pl-3"
	// AsksDetailsClass / AsksDetailsSummaryClass fold a card's secondary
	// facts (a nudge's quote, classifier, reason) behind one small toggle.
	AsksDetailsClass        = "group/more min-w-0"
	AsksDetailsSummaryClass = "press inline-flex min-h-[44px] cursor-pointer list-none items-center gap-1 rounded-lg px-1 text-xs font-medium text-fg2 transition hover:text-fg [&::-webkit-details-marker]:hidden"
	AsksDetailsBodyClass    = "flex flex-col gap-1 pb-1"
	// AsksPillBaseClass is THE pill (a count, a kind badge, a card's
	// metadata): its tone is added by the caller (AsksPillClass's muted one,
	// AsksKindBadge's per kind).
	AsksPillBaseClass = "inline-flex shrink-0 items-center rounded-full px-2 py-0.5 text-[11px] font-semibold"
	// AsksPillClass is a muted metadata pill (a nudge's mode, a draft's
	// plugin, an expiry, a delivery).
	AsksPillClass = AsksPillBaseClass + " bg-s2 text-fg2"
	// AsksPillsClass is a card's row of metadata pills.
	AsksPillsClass = "flex min-w-0 flex-wrap items-center gap-1.5"
)

// AsksGroupLabel is one group's label: its name and its count, as an <h2>
// with id — what the group's own list names as its aria-labelledby (the
// caller sets it). The count is its own element, so the label's words are the
// label's; a visually-hidden ", " between them makes a screen reader say
// "Questions, 2" rather than "Questions2".
func AsksGroupLabel(id, label string, n int) string {
	return `<h2 id="` + html.EscapeString(id) + `" class="flex items-center gap-2 px-1 pt-2 text-xs font-semibold uppercase tracking-wide text-fg2" data-asks-group-label>` +
		html.EscapeString(label) + `<span class="sr-only">, </span><span class="` + AsksPillClass + ` tabular-nums" data-asks-group-count>` +
		strconv.Itoa(n) + `</span></h2>`
}

// AsksChevron is the "opens" mark at a linked head's or row's end.
const AsksChevron = `<svg class="h-4 w-4 shrink-0 text-muted" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M9 18l6-6-6-6"/></svg>`

// AsksDismissIcon is Dismiss's glyph (the app's close mark).
const AsksDismissIcon = `<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M18 6 6 18M6 6l12 12"/></svg>`
