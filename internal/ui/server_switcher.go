package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/web"
)

// THE SERVER SWITCHER (docs/design/web-server-switcher.md): the sidebar row
// that shows which muster server is in view and opens the list of the others
// this browser knows. The web counterpart of the Android app's switcher bar.
//
// The list lives in the browser (web/static/servers.js), so the server draws
// only the frame: the row (fixed height, so filling it in moves nothing), the
// menu, the Add server and Manage servers dialogs, the hand-off offer, and the
// templates the script clones for each server. Every class is written here,
// where Tailwind scans for them.
//
// The row is filled in by servers.js, DEFERRED: a parser-blocking <script
// src> in the sidebar would stand in front of the view pages' composers, and a
// paint while it loads moves them (terminal_paint_order_test.go). The row is
// drawn at its final size with the server's name (or "This server"), so the
// script changes only its text, never its box.

const serverPrimaryBtn = "press min-h-[44px] rounded-md px-3 py-2 text-sm font-semibold transition bg-accent text-on-accent hover:bg-accent/90"
const serverSecondaryBtn = "press min-h-[44px] rounded-md px-3 py-2 text-sm font-semibold transition bg-bg text-fg ring-1 ring-inset ring-edge"

// serverDot is a server's colour: a small round swatch the script paints.
func serverDot() g.Node {
	return Span(g.Attr("data-server-dot", ""), g.Attr("aria-hidden", "true"),
		Class("inline-block h-3 w-3 shrink-0 rounded-full bg-muted ring-1 ring-inset ring-black/20"))
}

// serverLogo is a server's logo, hidden until it has loaded: this server's
// own, or another's /brand/icon-192.png drawn straight from that server
// (servers.js logoSrc). No crossorigin attribute, so it is never a CORS
// request; no Referer.
func serverLogo() g.Node {
	return Img(g.Attr("data-server-logo", ""), Alt(""), g.Attr("hidden", ""), g.Attr("referrerpolicy", "no-referrer"), Width("20"), Height("20"),
		Class("h-5 w-5 shrink-0 rounded object-contain"))
}

// serverSwitcher is the sidebar's server row and everything it opens. b is
// this server's branding: its name and logo label its own entry.
func serverSwitcher(b Brand) g.Node {
	selfLabel := "This server"
	if b.Name != "" {
		selfLabel = b.Name
	}
	selfLogo := ""
	if b.HasLogo() {
		selfLogo = b.Icon192()
	}
	return Div(
		ID("server-switcher"),
		g.Attr("data-self-name", b.Name),
		g.If(selfLogo != "", g.Attr("data-self-logo", selfLogo)),
		Class("border-b border-line px-3 py-2"),
		Button(
			ID("server-switcher-toggle"),
			Type("button"),
			g.Attr("aria-expanded", "false"),
			g.Attr("aria-controls", "server-switcher-menu"),
			g.Attr("aria-describedby", "server-switcher-hint"),
			Class("press flex h-11 w-full min-w-0 items-center gap-2 rounded-lg px-2 text-left text-sm text-fg transition hover:bg-s2"),
			serverDot(),
			Span(ID("server-switcher-hint"), Class("sr-only"), g.Text("Server in view:")),
			Span(g.Attr("data-server-label", ""), Class("min-w-0 flex-1 truncate font-medium"), g.Text(selfLabel)),
			g.Raw(`<svg class="h-4 w-4 shrink-0 text-muted" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 8l4 4 4-4"/></svg>`),
		),
		Div(
			ID("server-switcher-menu"),
			g.Attr("hidden", ""),
			Class("mt-1 flex flex-col gap-1"),
			H2(ID("server-switcher-heading"), Class("px-2 pt-1 text-xs font-semibold uppercase tracking-wide text-muted"), g.Text("Servers")),
			Ul(ID("server-switcher-list"), g.Attr("aria-labelledby", "server-switcher-heading"), Class("flex flex-col")),
			Div(
				Class("flex gap-2 px-1 pb-1 pt-1"),
				Button(ID("server-add-open"), Type("button"), Class("flex-1 "+serverSecondaryBtn), g.Text("Add server")),
				Button(ID("server-manage-open"), Type("button"), Class("flex-1 "+serverSecondaryBtn), g.Text("Manage servers")),
			),
		),
		// One server in the menu: a link to its origin (the script sets href).
		Template(ID("server-item-tpl"),
			Li(A(
				Class("press flex min-h-[44px] min-w-0 items-center gap-2 rounded-lg px-2 text-sm text-fg transition hover:bg-s2 aria-[current=true]:bg-s2"),
				serverDot(), serverLogo(),
				Span(Class("flex min-w-0 flex-1 flex-col"),
					Span(g.Attr("data-server-label", ""), Class("truncate font-medium")),
					Span(g.Attr("data-server-host", ""), Class("truncate text-xs text-muted")),
				),
				Span(g.Attr("data-server-current", ""), g.Attr("hidden", ""), Class("shrink-0 text-xs font-semibold text-accent"), g.Text("In view")),
			)),
		),
	)
}

// serverSwitcherDialogs is the switcher's three dialogs and its script, for
// the sidebar's ROOT, not its panel: a shut panel is visibility: hidden
// (web/css/input.css), which a modal dialog inside it inherits — and the
// hand-off offer opens on load, with the panel shut on a phone. Placed after
// the panel, so the script (deferred anyway) finds the row.
func serverSwitcherDialogs() g.Node {
	return g.Group{
		serverAddDialog(),
		serverManageDialog(),
		serverOfferDialog(),
		Script(Src(web.Asset("/static/servers.js")), Defer()),
	}
}

func serverDialogClass() g.Node {
	return Class("w-[calc(100%-2rem)] max-w-sm rounded-2xl bg-s1 p-0 text-fg ring-1 ring-line backdrop:bg-black/60")
}

// serverAddDialog asks for another server's address.
func serverAddDialog() g.Node {
	return Dialog(
		ID("server-add"),
		g.Attr("aria-labelledby", "server-add-title"),
		serverDialogClass(),
		// A <div>, not a <form>: the sidebar is in every page, inside forms'
		// documents, and a form here would be a form nobody submits to the
		// server. Enter in the field and the Add button both run the add.
		Div(
			ID("server-add-form"),
			Class("flex flex-col gap-3 px-5 py-6"),
			H2(ID("server-add-title"), Class("text-base font-semibold text-fg"), g.Text("Add server")),
			P(ID("server-add-help"), Class("text-sm text-fg2"),
				g.Text("Another muster server's address. You sign in there as usual; this list is kept in this browser only.")),
			Label(For("server-add-input"), Class("text-sm font-medium text-fg"), g.Text("Server address")),
			Input(ID("server-add-input"), Name("server"), Type("text"), g.Attr("inputmode", "url"), g.Attr("autocomplete", "url"),
				g.Attr("autocapitalize", "none"), g.Attr("spellcheck", "false"), Placeholder("https://muster.example.com"),
				g.Attr("aria-describedby", "server-add-help server-add-error"),
				Class("min-h-[44px] rounded-md bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus")),
			P(ID("server-add-error"), g.Attr("role", "alert"), g.Attr("hidden", ""),
				Class("rounded-lg bg-st-error-bg px-3 py-2 text-sm text-st-error-fg ring-1 ring-inset ring-st-error-fg/40")),
			Div(Class("flex justify-end gap-2"),
				Button(Type("button"), g.Attr("data-dialog-close", ""), Class(serverSecondaryBtn), g.Text("Cancel")),
				Button(ID("server-add-submit"), Type("button"), Class(serverPrimaryBtn), g.Text("Add")),
			),
		),
	)
}

// serverManageDialog lists every server with its label (editable) and a
// Remove button (not for this server).
func serverManageDialog() g.Node {
	return Dialog(
		ID("server-manage"),
		g.Attr("aria-labelledby", "server-manage-title"),
		serverDialogClass(),
		Div(
			Class("flex flex-col gap-3 px-5 py-6"),
			H2(ID("server-manage-title"), Class("text-base font-semibold text-fg"), g.Text("Manage servers")),
			P(Class("text-sm text-fg2"), g.Text("Labels and the list are kept in this browser. Removing a server does not sign you out of it or turn off its notifications.")),
			Ul(ID("server-manage-list"), Class("flex flex-col gap-2")),
			Div(Class("flex justify-end"),
				Button(Type("button"), g.Attr("data-dialog-close", ""), Class(serverPrimaryBtn), g.Text("Done")),
			),
		),
		Template(ID("server-manage-row-tpl"),
			Li(Class("flex min-w-0 items-center gap-2"),
				serverDot(), serverLogo(),
				Span(Class("flex min-w-0 flex-1 flex-col gap-1"),
					Input(Type("text"), g.Attr("maxlength", "64"), g.Attr("data-server-label-input", ""),
						Class("min-h-[44px] w-full rounded-md bg-bg px-3 py-2 text-sm text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus")),
					Span(g.Attr("data-server-host", ""), Class("truncate text-xs text-muted")),
				),
				Span(g.Attr("data-server-current", ""), g.Attr("hidden", ""), Class("shrink-0 text-xs font-semibold text-accent"), g.Text("In view")),
				Button(Type("button"), g.Attr("data-server-remove", ""), Class("shrink-0 "+serverSecondaryBtn), g.Text("Remove")),
			),
		),
	)
}

// serverOfferDialog is the hand-off: a switch from another server named it
// in the fragment, and this page offers to add it (never adds it unasked).
func serverOfferDialog() g.Node {
	return Dialog(
		ID("server-offer"),
		g.Attr("aria-labelledby", "server-offer-title"),
		serverDialogClass(),
		Div(
			Class("flex flex-col gap-3 px-5 py-6"),
			H2(ID("server-offer-title"), Class("text-base font-semibold text-fg"), g.Text("Add the server you came from?")),
			P(Class("text-sm text-fg2"),
				g.Text("You switched here from "), Strong(g.Attr("data-server-host", ""), Class("break-all text-fg")),
				g.Text(". Add it to this server's list so you can switch back?")),
			Div(Class("flex justify-end gap-2"),
				Button(Type("button"), g.Attr("data-dialog-close", ""), Class(serverSecondaryBtn), g.Text("Not now")),
				Button(ID("server-offer-add"), Type("button"), Class(serverPrimaryBtn), g.Text("Add")),
			),
		),
	)
}
