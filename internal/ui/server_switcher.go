package ui

import (
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/web"
)

// THE SERVER SWITCHER (docs/design/web-server-switcher.md): the sidebar's top
// row, where the server in view's LOGO is the button that opens the list of
// the other servers this browser knows. The web counterpart of the Android
// app's switcher bar.
//
// ONE ROW, THE LOGO ONLY (the operator's decision, "One row: the logo is the
// switcher"): the brand row and the server row were two rows, the logo, the
// name and, in the list, every server's address. The row now draws the
// current server's picture (serverPicture: its logo, else a monogram of its
// name, else muster's compact square mark), a chevron, and the sidebar's search and
// close buttons; the name is the button's accessible name and its tooltip.
// The list keeps every server's name (pictures alone cannot tell servers
// apart) and shows an address only where two names collide (servers.js).
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
// drawn at its final size with the server's picture, so the script changes
// only its colours, letters and name, never its box.

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

// serverDotBadge is a server's colour as a small badge on its picture's
// corner, in the open list only: the same swatch as serverDot (the Android
// app's palette, a function of the SET of servers on the list, servers.js
// colours), which tells two entries with the same picture apart. The row has
// none: it draws the picture alone (operator decision on round 0, D2).
func serverDotBadge() g.Node {
	return Span(g.Attr("data-server-dot", ""), g.Attr("aria-hidden", "true"),
		Class("absolute -bottom-0.5 -right-0.5 h-2.5 w-2.5 rounded-full bg-muted ring-2 ring-s1"))
}

// serverMonogramClasses is the servers' colours as role tokens, one per
// colour of the palette and in its order: --mu-server-<i> in
// web/css/input.css, the Android app's palette (core/auth Tenants.PALETTE).
// servers.js spells the same colours as hex (PALETTE), and
// TestTheMonogramPaletteIsTheScripts pins the stylesheet's values to the
// script's.
var serverMonogramClasses = [...]string{"bg-server-0", "bg-server-1", "bg-server-2", "bg-server-3", "bg-server-4", "bg-server-5", "bg-server-6", "bg-server-7"}

// serverMonogram is a server's picture when it has no logo: the first code
// point of each of the first two words of its name, a word being a run of
// code points that are not separators (monogramSeparator). A name with no
// word is "?". The first code point is upper-cased only inside the Latin
// (U+0000–U+024F), Greek (U+0370–U+03FF) and Cyrillic (U+0400–U+04FF)
// blocks, and only where its uppercase is one code point in those blocks too
// (monogramUpper); anything else is kept as it is.
//
// 🔴 THE RULE IS CHOSEN SO GO AND THE SCRIPT CANNOT DISAGREE. servers.js
// monogram redraws what this drew, and the two languages' Unicode tables
// differ: Go's are 15.0, a browser's are newer, so a letter added since is a
// letter to the script and a separator to unicode.IsLetter; and Go's ToUpper
// is the SIMPLE mapping (ᾳ → titlecase ᾼ) where JavaScript's is the FULL one
// (ᾳ → "ΑΙ"). So nothing here asks a Unicode table what a letter is: the
// separators are fixed ranges, and case changes only in blocks whose
// mappings are decades old and agree. Both are held to the same literal
// table (TestTheMonogram, e2e/servers.test.mjs) and to a code point by code
// point table of every first letter in those blocks (TestTheMonogramLetters).
func serverMonogram(name string) string {
	words := strings.FieldsFunc(name, monogramSeparator)
	if len(words) == 0 {
		return "?"
	}
	var sb strings.Builder
	for _, w := range words[:min(2, len(words))] {
		r, _ := utf8.DecodeRuneInString(w)
		sb.WriteRune(monogramUpper(r))
	}
	return sb.String()
}

// monogramSeparator reports whether r splits a name into words: C0 and C1
// controls, ASCII space, punctuation and symbols, Latin-1's no-break space,
// punctuation and symbols (U+00A0–U+00BF, ×, ÷), the Ogham space mark, the
// General Punctuation block (U+2000–U+206F: its spaces, dashes, separators),
// the ideographic space and the BOM. Fixed ranges, the same in servers.js
// (MONOGRAM_SEP).
func monogramSeparator(r rune) bool {
	return r <= 0x2f || (r >= 0x3a && r <= 0x40) || (r >= 0x5b && r <= 0x60) || (r >= 0x7b && r <= 0xbf) ||
		r == 0xd7 || r == 0xf7 || r == 0x1680 || (r >= 0x2000 && r <= 0x206f) || r == 0x3000 || r == 0xfeff
}

// monogramCaseBlock reports whether r is in the blocks where a monogram
// changes case: Latin (U+0000–U+024F), Greek (U+0370–U+03FF), Cyrillic
// (U+0400–U+04FF).
func monogramCaseBlock(r rune) bool { return r <= 0x24f || (r >= 0x370 && r <= 0x4ff) }

// monogramUpper is r upper-cased when both r and its uppercase are in those
// blocks, else r. Both ends, because Unicode adds a case pair by encoding a
// NEW capital elsewhere: ƛ (U+019B) gained Ꟍ (U+A7DC) in Unicode 16, which a
// browser maps and Go's 15.0 tables do not; the blocks themselves are full,
// so no new pair can land inside them. Go's simple mapping has no entry for
// the letters there whose full uppercase is several code points (ß, ŉ, ǰ, ΐ,
// ΰ), so they stay as they are — as servers.js keeps them.
func monogramUpper(r rune) rune {
	if !monogramCaseBlock(r) {
		return r
	}
	if u := unicode.ToUpper(r); monogramCaseBlock(u) {
		return u
	}
	return r
}

// serverMonogramColour is the monogram's background, as the index of its
// palette colour: the one the 32-bit FNV-1a hash of the name's UTF-16 code
// units picks (servers.js fnv1a, the hash its colours already use), so a name
// is always the same colour, on every server and in both languages. Every
// palette colour is light enough for the monogram's black letters (at least
// 4.8:1).
func serverMonogramColour(name string) int {
	h := uint32(0x811c9dc5)
	for _, u := range utf16.Encode([]rune(name)) {
		h ^= uint32(u)
		h *= 0x01000193
	}
	return int(h % uint32(len(serverMonogramClasses)))
}

// serverMonogramClass is a monogram's box, the size of a menu entry's logo.
const serverMonogramClass = "inline-flex h-7 w-7 shrink-0 select-none items-center justify-center rounded-md text-xs font-bold leading-none text-black"

// serverMark is muster's own compact square mark, drawn where a logo slot
// shows a server with no branding at all (the row, and this server's entry in
// the list): the brand pack's favicon.svg (docs/brand.md), the mark on its
// ground, already served for the tab. Not the horizontal lockup: the operator's
// decision on round 0 (Q1), the row is the same size whatever is in it.
func serverMark() string { return web.Asset("/static/icons/favicon.svg") }

// serverPicture is the row button's picture of the server in view, and
// nothing else (no colour badge): its logo; with none, a monogram of its
// name; with no branding at all, muster's compact square mark
// (serverMark). A monogram is always drawn (hidden behind a logo or the
// mark) so servers.js can show it when the picture fails to load, and so it
// can redraw it when the person has given this server a label of their own.
func serverPicture(b Brand, name string) g.Node {
	mono := Span(g.Attr("data-server-monogram", ""), g.Attr("aria-hidden", "true"),
		g.If(b.HasLogo() || b.drawsLockup(), g.Attr("hidden", "")),
		Class(serverMonogramClass+" "+serverMonogramClasses[serverMonogramColour(name)]), g.Text(serverMonogram(name)))
	var pic g.Node
	switch {
	case b.HasLogo():
		// It SHRINKS (min-w-0 shrink, here and on the avatar): a wide logo
		// on a phone gives up width, letterboxed to its left, so the chevron
		// beside it stays inside the button and its hover and focus ring.
		// At 8rem and shrink-0, a 6:1 logo at 375 px pushed the chevron out
		// (e2e/tests/branding.spec.ts, "a wide logo at 375 px").
		pic = Img(g.Attr("data-brand-logo", ""), g.Attr("data-server-picture", ""), Src(b.Versioned(BrandLogoPath)), Alt(""),
			Class("h-7 w-auto min-w-0 max-w-[8rem] shrink object-contain object-left"))
	case b.drawsLockup():
		// width/height are the file's own 16 × 16 viewBox (the brand
		// ledger's declared size, internal/api brand_assets_test.go); the
		// class draws it at the logo slot's 28 px.
		pic = Img(g.Attr("data-server-mark", ""), g.Attr("data-server-picture", ""), Src(serverMark()), Alt(""),
			Width("16"), Height("16"), Class("h-7 w-7 shrink-0 rounded-md"))
	}
	return Span(g.Attr("data-server-avatar", ""), Class("inline-flex min-w-0 shrink items-center"), pic, mono)
}

// serverSwitcherName is the name the row is drawn with before servers.js
// runs: the branding's name, else "This server" (servers.js SELF_FALLBACK).
// Never the host: no address in the top layer.
func serverSwitcherName(b Brand) string {
	if b.Name != "" {
		return b.Name
	}
	return "This server"
}

// ServerSwitcherLabel is the row button's accessible name for a server named
// name. servers.js spells the same words (TOGGLE_LABEL).
func ServerSwitcherLabel(name string) string { return "Switch server, current: " + name }

// serverSwitcher is the sidebar's top row and everything it opens: the
// server in view's picture as the button, then trailing (the search and close
// buttons) at the row's end. b is this server's branding: its name and logo
// draw the row and its own entry in the list.
func serverSwitcher(b Brand, trailing ...g.Node) g.Node {
	name := serverSwitcherName(b)
	selfLogo := ""
	if b.HasLogo() {
		selfLogo = b.Icon192()
	}
	return Div(
		ID("server-switcher"),
		g.Attr("data-self-name", b.Name),
		g.If(selfLogo != "", g.Attr("data-self-logo", selfLogo)),
		// No branding at all: this server's entry in the list is muster's
		// own square mark, as the row is.
		g.If(b.drawsLockup(), g.Attr("data-self-mark", serverMark())),
		Class("border-b border-line"),
		// The row: min-h keeps it the height the 44px close button gives it
		// (py-3 + 44px = 68px) where that button is hidden — the desktop rail
		// — so its border lines up with the content header's beside it.
		Div(
			Class("flex min-h-[4.25rem] items-center gap-3 px-4 py-3"),
			Button(
				ID("server-switcher-toggle"),
				Type("button"),
				g.Attr("aria-expanded", "false"),
				g.Attr("aria-controls", "server-switcher-menu"),
				g.Attr("aria-label", ServerSwitcherLabel(name)),
				g.Attr("title", name),
				Class("press -ml-1.5 inline-flex h-11 min-h-[44px] min-w-[44px] max-w-full items-center gap-1.5 rounded-lg px-1.5 text-fg transition hover:bg-s2"),
				serverPicture(b, name),
				g.Raw(`<svg class="h-4 w-4 shrink-0 text-muted" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 8l4 4 4-4"/></svg>`),
			),
			Span(Class("flex-1")),
			g.Group(trailing),
		),
		Div(
			ID("server-switcher-menu"),
			g.Attr("hidden", ""),
			Class("flex flex-col gap-1 px-3 pb-2"),
			H2(ID("server-switcher-heading"), Class("px-2 pt-1 text-xs font-semibold uppercase tracking-wide text-muted"), g.Text("Servers")),
			Ul(ID("server-switcher-list"), g.Attr("aria-labelledby", "server-switcher-heading"), Class("flex flex-col")),
			Div(
				Class("flex gap-2 px-1 pb-1 pt-1"),
				Button(ID("server-add-open"), Type("button"), Class("flex-1 "+serverSecondaryBtn), g.Text("Add server")),
				Button(ID("server-manage-open"), Type("button"), Class("flex-1 "+serverSecondaryBtn), g.Text("Manage servers")),
			),
		),
		// One server in the menu: a link to its origin (the script sets href).
		// Its picture (the logo, or the monogram until the logo loads or when
		// it fails), its name, and its address only where another entry has
		// the same name (servers.js render).
		Template(ID("server-item-tpl"),
			Li(A(
				Class("press flex min-h-[44px] min-w-0 items-center gap-2.5 rounded-lg px-2 text-sm text-fg transition hover:bg-s2 aria-[current=true]:bg-s2"),
				Span(g.Attr("data-server-avatar", ""), Class("relative inline-flex h-7 w-7 shrink-0"),
					Span(g.Attr("data-server-monogram", ""), g.Attr("aria-hidden", "true"), Class(serverMonogramClass)),
					Img(g.Attr("data-server-logo", ""), Alt(""), g.Attr("hidden", ""), g.Attr("referrerpolicy", "no-referrer"), Width("28"), Height("28"),
						Class("h-7 w-7 shrink-0 rounded-md object-contain")),
					serverDotBadge(),
				),
				// The name (jargon audit F028: no address line; the exact
				// address is behind the ⓘ in Manage servers). The one
				// exception (#877's operator decision): where two entries'
				// names collide, servers.js shows each one's host under it,
				// or the two would read the same; hidden otherwise.
				Span(Class("flex min-w-0 flex-1 flex-col"),
					Span(g.Attr("data-server-label", ""), Class("truncate font-medium")),
					Span(g.Attr("data-server-host", ""), g.Attr("hidden", ""), Class("truncate text-xs text-muted")),
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
				Input(Type("text"), g.Attr("maxlength", "64"), g.Attr("data-server-label-input", ""),
					Class("min-h-[44px] w-full min-w-0 flex-1 rounded-md bg-bg px-3 py-2 text-sm text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus")),
				Span(g.Attr("data-server-current", ""), g.Attr("hidden", ""), Class("shrink-0 text-xs font-semibold text-accent"), g.Text("In view")),
				// The server's address, behind the ⓘ with Copy (jargon audit
				// F028), never under its name. servers.js gives each row's
				// copy its own ids and fills in the address.
				serverAddressSheet().Node(),
				Button(Type("button"), g.Attr("data-server-remove", ""), Class("shrink-0 "+serverSecondaryBtn), g.Text("Remove")),
			),
		),
	)
}

// ServerAddressSheetTpl is the id the Manage row template's address ⓘ is
// drawn with; servers.js spells it once too (ADDRESS_TPL) and renames it per
// row. TestTheManageRowsAddressSheetIsTheOneServersJSRenames pins the two.
const ServerAddressSheetTpl = "server-address-tpl"

// serverAddressSheet is a Manage row's ⓘ: the server's exact address, to
// copy. Live: the script fills the code in.
func serverAddressSheet() InfoSheet {
	return InfoSheet{
		ID:      ServerAddressSheetTpl,
		Label:   "Server address",
		Heading: "Server address",
		Lead:    "To add this server on another device, paste this address into Add server there.",
		Live:    true,
	}
}

// ServerAddressSheetHTML is the Manage row template's address ⓘ exactly as
// every page with the sidebar carries it: empty (servers.js fills each
// clone's code from the browser's own list), and not setup — the addresses
// are the ones this person keeps in this browser. Page-wide "no ⓘ code row
// for a member" checks remove exactly this string first
// (WithoutServerAddressSheet), so any OTHER sheet still fails them.
func ServerAddressSheetHTML() string { return InfoSheetHTML(serverAddressSheet()) }

// WithoutServerAddressSheet is page with its one copy of
// ServerAddressSheetHTML removed. ok is false when the page has no copy, so a
// caller can tell "removed" from "never there".
func WithoutServerAddressSheet(page string) (out string, ok bool) {
	sheet := ServerAddressSheetHTML()
	if !strings.Contains(page, sheet) {
		return page, false
	}
	return strings.Replace(page, sheet, "", 1), true
}

// serverOfferDialog is the hand-off: a link's fragment (#muster-from=…) named
// another server, and this page offers to add it (never adds it unasked).
//
// 🔴 THE COPY CLAIMS NOTHING ABOUT WHERE THE PERSON CAME FROM. Anyone can craft
// that link, so the page cannot know it was a switch from the person's own
// server: it says a LINK asked, shows the FULL origin (scheme, host and port)
// prominently, and its focus starts on "Not now" (servers.js), so a stray
// Enter does not add it.
func serverOfferDialog() g.Node {
	return Dialog(
		ID("server-offer"),
		g.Attr("aria-labelledby", "server-offer-title"),
		g.Attr("aria-describedby", "server-offer-why"),
		serverDialogClass(),
		Div(
			Class("flex flex-col gap-3 px-5 py-6"),
			H2(ID("server-offer-title"), Class("text-base font-semibold text-fg"), g.Text("Add a server to your list?")),
			P(ID("server-offer-why"), Class("text-sm text-fg2"),
				g.Text("A link asked to add this server to the list of servers you switch between:")),
			P(g.Attr("data-server-origin-text", ""), Class("break-all rounded-lg bg-bg px-3 py-2 font-mono text-base font-semibold text-fg ring-1 ring-inset ring-edge")),
			P(Class("text-sm text-fg2"), g.Text("Add it only if it is a muster server you use.")),
			Div(Class("flex justify-end gap-2"),
				Button(ID("server-offer-dismiss"), Type("button"), g.Attr("data-dialog-close", ""), g.Attr("autofocus", ""), Class(serverSecondaryBtn), g.Text("Not now")),
				Button(ID("server-offer-add"), Type("button"), Class(serverPrimaryBtn), g.Text("Add")),
			),
		),
	)
}

// ServerHandoffKey is the sessionStorage key the sign-in page keeps a
// switcher hand-off under; servers.js spells it once too (HANDOFF_KEY), and
// TestTheSignInPageKeepsTheHandoffUnderTheSwitchersKey pins the two together.
const ServerHandoffKey = "muster-from"

// ServerHandoffKeepHTML is the inline script every server-written auth page
// carries (internal/api writeAuthDocument): a switch to a server you are not
// signed in to lands on /login with its #muster-from fragment, and the
// post-sign-in redirect drops the fragment. This keeps it, with the time, in
// sessionStorage (this tab only), where the first signed-in page's servers.js
// reads it once and offers the add. It stores the raw fragment and decides
// nothing: servers.js validates it exactly as it validates a fragment, and
// ignores it after ten minutes.
func ServerHandoffKeepHTML() string {
	return `<script>try{if(/^#muster-from=/.test(location.hash))sessionStorage.setItem('` + ServerHandoffKey + `',Date.now()+'|'+location.hash)}catch(e){}</script>`
}
