package ui

import (
	"io"

	g "maragu.dev/gomponents"
)

// RenderPage writes muster's full document shell to w. activeTab is the tab to
// show on first paint, derived from the request path for SPA-style deep-linking
// and clamped by normalizeTab, so an unknown path lands on the default tab
// rather than rendering a shell with nothing selected.
//
// 🔴 THE SIGNATURE IS NARROWER THAN UPSTREAM'S BY FOUR ARGUMENTS, AND EVERY ONE
// OF THEM WAS ROUTER-OWNED STATE. Upstream also took the pending permission
// requests (so the queue could be server-rendered into the first byte) and two
// auto-approve snapshots (so the banner would not grow mid-load and shove the
// page down). muster has neither surface: there is no request store here and no
// auto-approve machinery, so there is no state to render eagerly and nothing to
// reserve height for. Every panel in muster's shell fetches its own partial on
// `load`.
//
// This file used to carry four more writers — the request list partial, the
// header popover's quick list, the post-decision card-removal fragment and a
// single-card render helper. All four take a store.Request, which is the
// permission router's type, and all four stay with it.
func RenderPage(w io.Writer, activeTab string) error {
	return Page(activeTab).Render(w)
}

// compile-time assertion that the shell is a renderable node, so a refactor
// that changes Page's return type fails here rather than at every call site.
var _ func(string) g.Node = Page
