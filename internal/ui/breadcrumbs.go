package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// Breadcrumb is one step of a page's breadcrumbs (Breadcrumbs): its words, and
// where a tap goes — every step but the last links back, so each needs an
// Href; the last is the current page, and its Href is not drawn.
type Breadcrumb struct {
	Label string
	Href  string
}

// The breadcrumbs' classes: the trail, a step that links back, and the
// current page.
const (
	BreadcrumbsClass       = "flex min-w-0 flex-wrap items-center gap-1 text-sm text-fg2"
	BreadcrumbLinkClass    = "inline-flex min-h-[44px] items-center rounded px-1 font-semibold text-fg2 underline underline-offset-2 transition hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"
	BreadcrumbCurrentClass = "min-w-0 truncate px-1 text-fg"
)

// Breadcrumbs is the trail back from a page to where it sits: a
// <nav aria-label="Breadcrumb"> holding an ordered list, each step a link but
// the LAST, which is the page itself (aria-current="page", plain words), with
// a "›" between steps that a screen reader skips (aria-hidden). nil for no
// steps.
//
// The pages shell draws its own crumbs in its own stylesheet
// (internal/pages, render.go); this is the one for the app's pages.
func Breadcrumbs(steps []Breadcrumb) g.Node {
	if len(steps) == 0 {
		return nil
	}
	items := make([]g.Node, 0, len(steps))
	for i, s := range steps {
		var sep g.Node
		if i > 0 {
			sep = Span(g.Attr("aria-hidden", "true"), g.Text("›"))
		}
		var step g.Node
		if i == len(steps)-1 {
			step = Span(g.Attr("aria-current", "page"), Class(BreadcrumbCurrentClass), g.Text(s.Label))
		} else {
			step = A(Href(s.Href), Class(BreadcrumbLinkClass), g.Attr("data-breadcrumb", ""), g.Text(s.Label))
		}
		items = append(items, Li(Class("flex min-w-0 items-center gap-1"), sep, step))
	}
	return Nav(g.Attr("aria-label", "Breadcrumb"), g.Attr("data-breadcrumbs", ""), Ol(Class(BreadcrumbsClass), g.Group(items)))
}
