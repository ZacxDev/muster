package ui

import (
	"html"
	"strconv"
)

// THE ASKS PAGE'S LIST COMPONENTS (internal/api renders the page; the classes
// live here because the stylesheet scans internal/ui, not internal/api — a
// class written only there is never emitted, with css-check green).
//
// The page is a FLAT LIST (operator decision 2026-11-12, "design pass on the
// asks page"): no page card, no intro, no "Back to muster". Each group of
// items has one label with its count (AsksGroupLabel); each item is a card
// whose head names the session — its name as the title, host and age muted
// under it, the tmux target small — and, when the item is about a session
// muster can open, the head IS a link to it, with a chevron (AsksChevron). A
// row with nothing to answer (a waiting window, a stalled session, a task) is
// one link as a whole (AsksRowClass). Every target is 44px.

const (
	// AsksGroupClass is one group's <li> (its label, then its <ul>).
	AsksGroupClass = "flex flex-col gap-2"
	// AsksGroupListClass is a group's own <ul> of cards.
	AsksGroupListClass = "flex flex-col gap-2"
	// AsksCardClass is a card that holds controls (an ask's answer form, a
	// draft's decision): a plain block, its head the link.
	AsksCardClass = "flex min-w-0 flex-col gap-2 rounded-xl bg-s1 p-3 ring-1 ring-inset ring-line"
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
	// AsksTargetClass is the tmux target inside the sub line, small.
	AsksTargetClass = "font-mono text-[11px] text-muted"
	// AsksWaitLineClass is a waiting row's one line: what it waits on (its
	// pending call, a question's first line).
	AsksWaitLineClass = "truncate font-mono text-xs text-fg"
	// AsksTextClass is a card's question or message.
	AsksTextClass = "whitespace-pre-wrap break-words text-sm text-fg"
	// AsksMutedTextClass is a card's secondary line.
	AsksMutedTextClass = "truncate text-xs text-fg2"
	// AsksQuietCardClass dims a row that is held (a quiet episode).
	AsksQuietCardClass = "opacity-60"

	// THE ANSWER FORM, Asks-page variant: the options as full-width chips
	// first; the free-text box behind "Type an answer" (a <details>, so it
	// opens with no script and is a real disclosure for a screen reader);
	// Dismiss a small icon button.
	AsksOptionsClass = "flex flex-col gap-1.5"
	AsksOptionClass  = "press min-h-[44px] w-full rounded-lg bg-bg px-3 py-2 text-left text-sm leading-snug text-fg ring-1 ring-inset ring-edge transition hover:bg-s2"
	// AsksTypeClass is the <details> holding the box; it reserves its shut
	// height itself, so a parse that yields between it and its <summary>
	// cannot draw the browser's default summary first (nav_agents.go navMore).
	AsksTypeClass        = "group/type min-h-[44px] min-w-0"
	AsksTypeSummaryClass = "press flex min-h-[44px] cursor-pointer list-none items-center gap-2 rounded-lg px-2 text-sm font-medium text-fg2 transition hover:bg-s2 hover:text-fg group-open/type:hidden [&::-webkit-details-marker]:hidden"
	AsksTypeBodyClass    = "flex flex-col gap-1.5"
	// AsksActionsClass is the row of a card's actions: the primary one fills
	// it, the secondary ones are small beside it.
	AsksActionsClass = "flex min-w-0 items-center gap-2"
	// AsksIconButtonClass is a small secondary action drawn as an icon (a 44px
	// target): it MUST carry aria-label and title.
	AsksIconButtonClass = "press inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-lg text-fg2 transition hover:bg-s2 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"
	// AsksSecondaryButtonClass is a small secondary action with words.
	AsksSecondaryButtonClass = "press inline-flex min-h-[44px] shrink-0 items-center justify-center whitespace-nowrap rounded-lg px-3 text-sm font-medium text-fg2 ring-1 ring-inset ring-edge transition hover:bg-s2 hover:text-fg"
	// AsksPrimaryButtonClass is a card's one primary action.
	AsksPrimaryButtonClass = "press min-h-[44px] w-full rounded-lg bg-accent px-3 py-2 text-base font-semibold text-on-accent transition hover:bg-accent/90"
	// AsksQuoteClass is quoted text from outside (a draft's inbound message).
	AsksQuoteClass = "flex min-w-0 flex-col gap-1 border-l-2 border-edge pl-3"
	// AsksDetailsClass / AsksDetailsSummaryClass fold a card's secondary
	// facts (a nudge's quote, classifier, reason) behind one small toggle.
	AsksDetailsClass        = "group/more min-w-0"
	AsksDetailsSummaryClass = "press inline-flex min-h-[44px] cursor-pointer list-none items-center gap-1 rounded-lg px-1 text-xs font-medium text-fg2 transition hover:text-fg [&::-webkit-details-marker]:hidden"
	AsksDetailsBodyClass    = "flex flex-col gap-1 pb-1"
	// AsksPillClass is a small status pill (a nudge's mode).
	AsksPillClass = "inline-flex shrink-0 items-center rounded-full bg-s2 px-2 py-0.5 text-[11px] font-semibold text-fg2"
)

// AsksGroupLabel is one group's label: its name and its count, as an <h2>
// with id (the group's aria-labelledby). The count is its own element, so the
// label's words are the label's.
func AsksGroupLabel(id, label string, n int) string {
	return `<h2 id="` + html.EscapeString(id) + `" class="flex items-center gap-2 px-1 pt-2 text-xs font-semibold uppercase tracking-wide text-fg2" data-asks-group-label>` +
		html.EscapeString(label) + `<span class="inline-flex min-w-[1.25rem] items-center justify-center rounded-full bg-s2 px-1.5 text-[11px] font-semibold tabular-nums text-fg2" data-asks-group-count>` +
		strconv.Itoa(n) + `</span></h2>`
}

// AsksChevron is the "opens" mark at a linked head's or row's end.
const AsksChevron = `<svg class="h-4 w-4 shrink-0 text-muted" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M9 18l6-6-6-6"/></svg>`

// AsksDismissIcon is Dismiss's glyph (the app's close mark).
const AsksDismissIcon = `<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M18 6 6 18M6 6l12 12"/></svg>`

// AsksTypeIcon is "Type an answer"'s glyph (a keyboard).
const AsksTypeIcon = `<svg class="h-4 w-4 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><rect x="2" y="6" width="20" height="12" rx="2"/><path d="M6 10h.01M10 10h.01M14 10h.01M18 10h.01M7 14h10"/></svg>`
