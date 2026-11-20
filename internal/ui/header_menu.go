package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// A MIRRORED WINDOW'S HEADER ⋯ MENU (operator ask: "when looking at an agent
// session that has previews, it's quite crowded and the layout is a little
// bit broken because things are competing for space").
//
// The Rendered view's header row held, at 390px: ☰, Home (←), the title,
// the message index, the Rendered | Terminal switch — and, below it, the
// status line, the ⓘ, the context chip AND the Preview pill, all on one 16px
// line. The Open button (previews and pages, plugin_chip.go) is now in the
// header row at every width, so the row's SECONDARY actions moved here:
//
//   - Home, the way out (the ☰'s navigation reaches it too);
//   - the message index ("Your messages and focus history", chat_index.go),
//     a sheet read now and then, not a control used every turn.
//
// The switch stays in the row: it is how the view is used. The menu is the
// agent page's ⋯ pattern (agentActionsMenu): popover="auto" in the top layer
// (Escape and a tap outside close it, the browser's own), placed by
// floatingMenuScript under its trigger, right edges aligned; its items are
// Tab-reachable from the trigger, and an item that opens a sheet closes the
// menu as it is picked (data-menu-close).

// MirrorMenuID is the Rendered view's header ⋯ menu.
const MirrorMenuID = "mv-menu"

// headerMenuItemClass is one item's look in a header ⋯ menu (the agent
// menu's items').
const headerMenuItemClass = "press flex min-h-[44px] w-full items-center gap-2 rounded-lg px-3 text-left text-sm font-medium text-fg transition hover:bg-s2 focus:bg-s2 focus:outline-none"

// mirrorHeaderMenu is the ⋯ trigger and its menu: Home (home) and the
// message index of the log logID.
func mirrorHeaderMenu(home, logID string) g.Node {
	return g.Group{
		Button(
			Type("button"),
			ID(MirrorMenuID+"-open"),
			g.Attr("aria-label", "More actions"),
			g.Attr("title", "More actions"),
			g.Attr("aria-haspopup", "menu"),
			g.Attr("aria-expanded", "false"),
			g.Attr("popovertarget", MirrorMenuID),
			Class("press inline-flex h-11 w-11 shrink-0 cursor-pointer items-center justify-center rounded-lg text-fg2 transition hover:bg-s2 aria-expanded:bg-s2"),
			g.Raw(`<svg class="h-5 w-5" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><circle cx="4" cy="10" r="1.6"/><circle cx="10" cy="10" r="1.6"/><circle cx="16" cy="10" r="1.6"/></svg>`),
		),
		Div(
			ID(MirrorMenuID),
			g.Attr("popover", "auto"),
			g.Attr("role", "menu"),
			g.Attr("aria-label", "More actions"),
			g.Attr("data-floating-menu", ""),
			Class("invisible hidden [&:popover-open]:flex m-0 w-64 max-w-[calc(100vw-16px)] flex-col gap-1 rounded-xl border border-line bg-s1 p-1.5 text-fg shadow-2xl ring-1 ring-black/20"),
			A(
				ID("mv-back"),
				// Home (the chief, or the session opened last): the Agents list
				// page this arrow went back to is retired.
				Href(home),
				g.Attr("role", "menuitem"),
				Class(headerMenuItemClass),
				g.Raw(`<svg class="h-4 w-4 shrink-0 text-fg2" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M15 18l-6-6 6-6"/></svg>`),
				Span(g.Text("Home")),
			),
			chatIndexMenuItem(logID),
		),
	}
}
