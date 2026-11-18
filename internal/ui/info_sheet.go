package ui

import (
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE ⓘ DETAILS SHEET (the operator rule): muster is for people who are
// not engineers, so its own chrome shows no raw address, id, host:port, flag
// or command in its top layer. When a technical detail is genuinely needed —
// the tmux window a session runs in, so its owner can open it in a terminal —
// the surface says it in plain words ("on atlas") and puts an ⓘ beside them;
// the ⓘ opens this sheet, which holds the exact text and a Copy button.
//
// 🔴 ONE COMPONENT, EVERY SURFACE. Every ⓘ is InfoSheet (as a node) or
// InfoSheetHTML (as text, for a surface built as a string): a second copy is
// how one sheet ends up unreachable by keyboard or unlabelled for a screen
// reader while the others are fine.
//
// How it works, with no script of its own:
//   - The ⓘ is a <button popovertarget>: a native popover toggle. Keyboard
//     (Tab, Enter/Space) and a screen reader's activation open it, and the
//     browser exposes its expanded state; Escape and a tap outside close it
//     (popover="auto"), and so does its own Close button.
//   - The sheet is the popover: it renders in the top layer, so no card,
//     scroller or clipped header cuts it off. On a phone it is a bottom sheet
//     the width of the screen; from `sm` up it is centred — the focus history
//     dialog's own look.
//   - Copy calls window.cgCopy (the app script's), which falls back to
//     execCommand where the async clipboard is unavailable; the text is also
//     selectable in place.
//
// 🔴 IT IS A SIBLING, NEVER A CHILD, OF A LINK OR A TOGGLE. A button inside an
// <a> or a <button> is invalid HTML and its tap goes to the outer control.
// Callers put the sheet beside the control they annotate.
type InfoSheet struct {
	// ID is the sheet's element id, unique in the document; the trigger and
	// the heading take ids derived from it.
	ID string
	// Label is the trigger's accessible name ("Where this session runs"):
	// what a screen reader says for the ⓘ. Also its tooltip.
	Label string
	// Heading is the sheet's title ("Where it runs").
	Heading string
	// Lead is the plain sentence above the technical text.
	Lead string
	// Code is the exact technical text (a tmux address), drawn verbatim in
	// monospace and what Copy copies. "" draws no code row.
	Code string
	// Withheld: the code is a SETUP detail (an environment variable, a
	// command) and the viewer is not an admin — the sheet says to ask their
	// admin instead, and carries no code at all (not hidden: absent).
	// A session's own address is never withheld from its owner. Its callers
	// are the setup ⓘs (HostSettingSheet, HostDetailSheet), for a viewer who
	// is not an admin (Features.Setup).
	Withheld bool
	// WithheldText is what a withheld sheet says instead of its code; ""
	// is InfoSheetWithheldText.
	WithheldText string
	// Multiline keeps Code's line breaks (several .env lines): drawn as
	// written, and copied as written.
	Multiline bool
	// Extra is more of the technical detail, drawn under the code: absent,
	// like the code, when the sheet is withheld.
	Extra g.Node
	// Inline draws the ⓘ for a 16px text line (a header's status row): a
	// 20px-tall glyph whose 44px thumb target is its ::before, raised above a
	// title's hit area (z-20) — the Preview button's in-row shape
	// (plugin_chip.go), so the row does not grow and the title keeps its
	// width.
	Inline bool
}

// InfoSheetWithheldText is what a withheld sheet says instead of its code.
const InfoSheetWithheldText = "Ask your admin for these details."

// infoIcon is the ⓘ glyph, decorative: the button's name is its label.
const infoIcon = `<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M12 11v5"/><path d="M12 8h.01"/></svg>`

// Node is the ⓘ trigger and its sheet, in that order.
func (s InfoSheet) Node() g.Node {
	return g.Group{s.trigger(), s.sheet()}
}

// trigger is the ⓘ button.
func (s InfoSheet) trigger() g.Node {
	icon := infoIcon
	if s.Inline {
		icon = strings.Replace(infoIcon, `class="h-5 w-5"`, `class="h-4 w-4"`, 1)
	}
	return Button(
		Type("button"),
		ID(s.ID+"-open"),
		g.Attr("popovertarget", s.ID),
		g.Attr("aria-haspopup", "dialog"),
		g.Attr("aria-controls", s.ID),
		g.Attr("aria-label", s.Label),
		g.Attr("title", s.Label),
		g.Attr("data-info-open", s.ID),
		g.If(!s.Inline, Class("press inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-lg text-fg2 transition hover:bg-s2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus")),
		g.If(s.Inline, Class("press relative z-20 -my-0.5 inline-flex h-5 w-11 shrink-0 items-center justify-center rounded-full text-fg2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus "+
			"before:absolute before:-inset-x-1 before:top-1/2 before:h-11 before:-translate-y-1/2 before:content-['']")),
		g.Raw(icon),
	)
}

// sheet is the popover the trigger opens.
//
// data-asks-controls: inside an Asks card a tap on the sheet's text is the
// sheet's, never the card's expand toggle (ui/asks_card.go's CONTROLS);
// data-own-swipe likewise keeps a drag on it from swiping the card.
func (s InfoSheet) sheet() g.Node {
	heading := s.ID + "-title"
	var code g.Node
	switch {
	case s.Withheld:
		text := s.WithheldText
		if text == "" {
			text = InfoSheetWithheldText
		}
		code = P(Class("text-sm text-fg"), g.Attr("data-info-withheld", ""), g.Text(text))
	case s.Code != "":
		codeClass := "min-w-0 flex-1 break-all font-mono text-sm text-fg"
		if s.Multiline {
			codeClass += " whitespace-pre-wrap"
		}
		code = Div(
			Class("flex items-center gap-2 rounded-lg bg-s2 px-3 py-2"),
			Code(Class(codeClass), g.Attr("data-info-code", ""), g.Text(s.Code)),
			Button(
				Type("button"),
				g.Attr("data-copy-text", s.Code),
				g.Attr("data-info-copy", ""),
				g.Attr("onclick", "window.cgCopy && window.cgCopy(this)"),
				Class("press inline-flex min-h-[44px] shrink-0 items-center rounded-lg px-4 text-sm font-medium text-fg2 ring-1 ring-inset ring-line transition hover:bg-s2"),
				g.Text("Copy"),
			),
		)
	}
	return Div(
		ID(s.ID),
		g.Attr("popover", "auto"),
		g.Attr("role", "dialog"),
		g.Attr("aria-labelledby", heading),
		g.Attr("data-info-sheet", ""),
		g.Attr("data-asks-controls", ""),
		g.Attr("data-own-swipe", ""),
		Class("hidden [&:popover-open]:flex mx-auto mb-0 mt-auto max-h-[90dvh] w-full max-w-lg flex-col overflow-hidden rounded-t-2xl bg-s1 p-0 text-left text-fg shadow-2xl ring-1 ring-line backdrop:bg-black/60 sm:m-auto sm:rounded-2xl"),
		Div(
			Class("flex items-center gap-2 border-b border-line px-4 py-2"),
			// Not a heading element: the sheet sits inside cards and lists
			// whose outline is theirs; the dialog is named by it
			// (aria-labelledby) all the same.
			P(ID(heading), Class("flex-1 text-sm font-semibold text-fg"), g.Text(s.Heading)),
			Button(
				Type("button"),
				g.Attr("popovertarget", s.ID),
				g.Attr("popovertargetaction", "hide"),
				g.Attr("aria-label", "Close"),
				Class("press inline-flex h-11 w-11 items-center justify-center rounded-lg text-muted transition hover:bg-s2 hover:text-fg"),
				g.Raw(`<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>`),
			),
		),
		Div(
			Class("flex min-h-0 flex-col gap-3 overflow-y-auto px-4 py-3"),
			g.If(s.Lead != "", P(Class("text-sm text-fg2"), g.Text(s.Lead))),
			code,
			g.If(!s.Withheld && s.Extra != nil, s.Extra),
		),
	)
}

// InfoSheetHTML is s.Node() as HTML, for a surface built as a string.
func InfoSheetHTML(s InfoSheet) string {
	var b strings.Builder
	_ = s.Node().Render(&b)
	return b.String()
}

// SessionPlaceSheet is THE ⓘ of a mirrored session's place: id is the sheet's
// id (unique in its document), name the session's name as the surface shows
// it, ALREADY PLAINED by the caller — never plained again here (the title
// rule is applied once; internal/textplain's sink ledger) — (in the ⓘ's
// accessible name, so a screen reader moving between cards
// hears whose place each one is: "Where Billing migration runs"; "" for "this
// session"), host the computer it runs on and where its exact tmux target
// ("tmux fern:1 @ atlas"), copied whole.
// Callers draw it only when they hold an address.
//
// Never withheld: a session's own address is its owner's to see.
func SessionPlaceSheet(id, name, host, where string) InfoSheet {
	lead := "This session runs in a tmux window"
	if host != "" {
		lead += " on " + host
	}
	lead += ". To open it in a terminal on that computer, use this address:"
	label := "Where this session runs"
	if name != "" {
		label = "Where " + name + " runs"
	}
	return InfoSheet{ID: id, Label: label, Heading: "Where it runs", Lead: lead, Code: where}
}
