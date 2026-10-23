package ui

import (
	"io"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/web"
)

// THE SIDE NAV ON EVERY SIGNED-IN PAGE.
//
// The slide-out navigation (sidebar) is drawn by ONE function, and every
// document a signed-in person reads carries it: the shell, the detail pages,
// the mirrored views, the terminal — and the server-authored pages internal/api
// writes as strings (Attention, Account, API tokens, Credentials, Passkeys,
// Plugins, Browser flows, the admin pages), which reach it through NavDocument
// below rather than through a second copy of the sidebar.
//
// What stays BARE is enumerated, each with its reason, by the two ledgers that
// pin this: TestEveryDocumentIsNavOrNamedBare (this package) and
// TestEveryAPIDocumentIsNavOrNamedBare (internal/api). A document added to
// either side without a decision fails there.

// The nav entries that are not tabs: each names a sidebar link a document can
// mark as the one it belongs to (sidebar's activeTab / currentTab).
const (
	NavAttention = "attention"
	NavAccount   = "account"
	NavAdmin     = "admin"
)

// NavChrome is what a page outside the shell needs to draw the navigation:
// which optional entries exist (Feat) and whether the document is EMBEDDED.
//
// 🔴 Embedded IS THE VIEW-SWAP FRAME (view_swap.go). The Rendered | Terminal
// switch opens the OTHER view in a same-origin <iframe> laid over the whole
// page; that page already carries the nav, so a nav inside the frame would be
// a second sidebar, a second hamburger and a second /events stream drawn over
// the first. A framed view draws none of them — and none of the sidebar's
// offset either, or its column would start 18rem in on a desktop with no rail
// beside it. internal/api sets it from the frame's own request (the browser's
// Sec-Fetch-Dest: iframe), so every load of the frame — the first, a reload, a
// New chat's ?session= — is embedded without anything carried in its URL.
type NavChrome struct {
	Feat     Features
	Embedded bool
}

// navOffset is the content column's left offset beside the desktop rail:
// lg:pl-72, or nothing in an embedded document, which has no rail.
func navOffset(embedded bool) string {
	if embedded {
		return ""
	}
	return "lg:pl-72"
}

// navRoot is the sidebar plus the wiring that keeps its Attention badge live:
// an /events stream on a WRAPPER (the shell's shape — never on <body>, which a
// boosted swap would strand). The badge also refreshes on load and once a
// minute without it.
//
// overlay keeps the panel a slide-out at EVERY width: see sidebarIn.
func navRoot(active, current string, feat Features, overlay bool) g.Node {
	return Div(
		hx("hx-ext", "sse"),
		hx("sse-connect", "/events"),
		sidebarIn(active, current, false, feat, overlay),
	)
}

// The four pieces a full-height view (the mirrored Rendered view, the
// terminal) places itself — its layout is its own, so it cannot be wrapped —
// each nothing in an embedded document. Split by WHERE each goes: the head's
// scripts (htmx and its SSE extension, for the badge), the sidebar (first in
// <body>), the hamburger (in the page's own header), and the open/close script
// (last in <body>).

func overlayNavHead(n NavChrome) g.Node {
	if n.Embedded {
		return nil
	}
	return g.Group{
		Script(Src(web.Asset("/static/vendor/htmx.min.js")), Defer()),
		Script(Src(web.Asset("/static/vendor/sse.js")), Defer()),
	}
}

func overlayNav(n NavChrome, entry string) g.Node {
	if n.Embedded {
		return nil
	}
	return navRoot(entry, entry, n.Feat, true)
}

func overlayNavButton(n NavChrome) g.Node {
	if n.Embedded {
		return nil
	}
	return overlayOpenButton()
}

func overlayNavScript(n NavChrome) g.Node {
	if n.Embedded {
		return nil
	}
	return appScript(n.Feat)
}

// NavDocumentView is a server-authored page drawn inside the navigation.
type NavDocumentView struct {
	// Title is the page's name; the document's <title> is "<Title> · muster".
	Title string
	// Entry is the nav entry the page belongs to (a tab key, NavAttention,
	// NavAccount, NavAdmin), or "" for a page with none (Plugins, Browser
	// flows): no link is then highlighted or marked current.
	Entry string
	Feat  Features
	// Main is the page's content: TRUSTED markup, built by its caller with
	// every value escaped there. It is placed as it is.
	Main string
}

// NavDocument is the document every server-authored signed-in page is: the
// navigation, the shell's header, and the page's own content in the column the
// bare pages use (max-w-sm), so the content reads as it did before.
//
// 🔴 <body> CARRIES NO hx-boost, unlike the shell's. These pages are made of
// plain forms — sign out, mint a token, revoke a device — whose POST answers
// with a redirect or a whole document; boosted, each would become an AJAX body
// swap of a page that was never written for one. The sidebar's links are
// therefore ordinary navigations here, as their own hx-boost="false" links
// already were.
func NavDocument(v NavDocumentView) g.Node {
	return Doctype(
		HTML(Class("dark"), Lang("en"),
			Head(
				Meta(Charset("utf-8")),
				Meta(Name("viewport"), Content("width=device-width, initial-scale=1, viewport-fit=cover")),
				Meta(Name("color-scheme"), Content("dark light")),
				ThemeHead(),
				Meta(Name("robots"), Content("noindex")),
				manifestLink(),
				TitleEl(g.Text(v.Title+" · muster")),
				Link(Rel("stylesheet"), Href(web.Asset("/static/app.css"))),
				Script(Src(web.Asset("/static/vendor/htmx.min.js")), Defer()),
				Script(Src(web.Asset("/static/vendor/sse.js")), Defer()),
				faroHead(),
			),
			Body(
				Class("min-h-dvh bg-bg text-fg antialiased selection:bg-accent/30"),
				navRoot(v.Entry, v.Entry, v.Feat, false),
				Div(
					Class(navOffset(false)),
					// The shell's own header (hamburger + wordmark), without the push
					// affordances: their script is the shell's, not this page's.
					header(false),
					Main(
						g.Attr("data-nav-document", ""),
						Class("mx-auto w-full max-w-sm px-4 pb-[calc(4rem+env(safe-area-inset-bottom))] pt-6"),
						g.Raw(v.Main),
					),
				),
				// Sidebar open/close (and the router, which off the shell only
				// navigates).
				appScript(v.Feat),
				// The worker, update toast and app badge — on every document.
				pwaChrome(),
			),
		),
	)
}

// RenderNavDocument writes NavDocument.
func RenderNavDocument(w io.Writer, v NavDocumentView) error {
	return NavDocument(v).Render(w)
}
