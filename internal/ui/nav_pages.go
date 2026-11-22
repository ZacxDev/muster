package ui

import (
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE APP'S PAGES, ONE LIST (operator ask: the command palette "will also
// match on routes. So I could just type pages and it would show the pages page
// as an option"). navPages is every page of the app a viewer may open from the
// sidebar — the tabs, Asks, Pages, Officers, the owner's app tabs, Admin,
// Account, Features — in the sidebar's order, each gated as the sidebar gates
// it. TWO READERS, NO THIRD LIST:
//
//   - the sidebar draws its More section from it (sidebarIn, navPageLink),
//     (byte for byte the hand-written links it replaced, proven once by
//     goldens in the change that made it);
//   - the command palette searches it (paletteBox, palettePagesTemplate): its
//     label, its route's words and its own words. The two read the same set
//     in the same order (TestThePaletteSearchesExactlyTheSidebarsPages).
//
// So a page added here is in the sidebar and found by the palette, and a page
// a viewer's sidebar does not draw (Admin for a member, a feature that is
// off) is not found either: the gate is applied once, here.

// navPageKind is how the sidebar draws a nav page.
type navPageKind int

const (
	// navTab is a musterTabs tab (sidebarTab: the shell's router owns it).
	navTab navPageKind = iota
	// navLink is a standalone page's link (Asks, Pages, Officers).
	navLink
	// navApp is one of the owner's app tabs (app_page.go).
	navApp
	// navPref is an account link, in the prefs block after the install button.
	navPref
)

// navPage is one page of the app in the nav.
type navPage struct {
	// Entry is the nav entry it is: what activeTab / currentTab name (a tab
	// key, a Nav* constant, NavAppEntry).
	Entry string
	// ID is its sidebar link's element id ("" for a tab: sidebarTab has none).
	ID    string
	Href  string
	Label string
	Icon  string
	// Words are what else the palette finds it by, beside its label and its
	// route ("questions" finds Asks); never drawn.
	Words string
	Kind  navPageKind
	// app is an app tab's plugin id (navApp).
	app int64
}

// navPages is the pages viewer feat's sidebar draws, in its order.
func navPages(feat Features) []navPage {
	var out []navPage
	for _, t := range visibleTabs(feat) {
		out = append(out, navPage{Entry: t.Key, Href: pathForTab(t.Key), Label: t.Label, Icon: t.Icon, Words: navTabWords[t.Key], Kind: navTab})
	}
	// Asks (the attention queue, attention.go), drawn only when the queue is
	// served. 🔴 ITS COUNT IS NOT ON ITS LINK: it is on the header of the
	// sidebar's collapsed "More" (navMore), which holds the link — on the link
	// it would be hidden whenever More is shut.
	if feat.Attention {
		out = append(out, navPage{Entry: NavAttention, ID: "sidebar-attention", Href: "/attention", Label: "Asks", Icon: "🔔", Words: "attention questions answers drafts", Kind: navLink})
	}
	if feat.Pages {
		out = append(out, navPage{Entry: NavPages, ID: "sidebar-pages", Href: PagesPath, Label: "Pages", Icon: "📄", Words: "docs documents shared links", Kind: navLink})
	}
	if feat.Officers {
		out = append(out, navPage{Entry: NavOfficers, ID: "sidebar-officers", Href: OfficersPath, Label: "Officers", Icon: "🎖️", Words: "schedules", Kind: navLink})
	}
	for _, a := range feat.Apps {
		id := strconv.FormatInt(a.ID, 10)
		out = append(out, navPage{Entry: NavAppEntry(a.ID), ID: "sidebar-app-" + id, Href: AppPagePath(a.ID), Label: a.Label, Icon: a.Icon, Words: "app", Kind: navApp, app: a.ID})
	}
	if feat.Admin {
		out = append(out, navPage{Entry: NavAdmin, ID: "sidebar-admin", Href: "/admin/users", Label: "Admin", Icon: "🛡️", Words: "users settings", Kind: navPref})
	}
	out = append(out,
		navPage{Entry: NavAccount, ID: "sidebar-account", Href: "/account", Label: "Account", Icon: "👤", Words: "settings profile sign out", Kind: navPref},
		navPage{Entry: NavFeatures, ID: "sidebar-features", Href: FeaturesPagePath, Label: "Features", Icon: "🧰", Words: "settings", Kind: navPref})
	return out
}

// navTabWords are a tab's palette words, beside its label and route.
var navTabWords = map[string]string{
	"tasks":      "board todo",
	"connectors": "integrations credentials",
}

// navPagesOf is the pages of kind k, in order.
func navPagesOf(ps []navPage, k ...navPageKind) []navPage {
	var out []navPage
	for _, p := range ps {
		for _, want := range k {
			if p.Kind == want {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// navPageLink is page p's sidebar link: marked active (the accent) and current
// (aria-current="page") by the entries the page names; hasPanels is the
// shell's (sidebarTab).
func navPageLink(p navPage, activeTab, currentTab string, hasPanels bool) g.Node {
	active, current := activeTab == p.Entry, currentTab == p.Entry
	switch p.Kind {
	case navTab:
		return sidebarTab(p.Entry, p.Label, p.Icon, active, current, hasPanels)
	case navApp:
		return A(ID(p.ID), Href(p.Href), hx("hx-boost", "false"),
			g.Attr("data-app-tab", strconv.FormatInt(p.app, 10)),
			g.If(current, g.Attr("aria-current", "page")),
			Class("press flex min-h-[44px] w-full items-center gap-3 rounded-lg px-3 text-left text-sm font-medium transition "+navEntryTone(active)),
			Span(Class("text-base"), g.Attr("aria-hidden", "true"), g.Text(p.Icon)), Span(Class("min-w-0 truncate"), g.Text(p.Label)))
	}
	// A standalone document outside the shell: a full navigation, never a
	// body swap of a document that has no panels.
	return A(ID(p.ID), Href(p.Href), hx("hx-boost", "false"),
		g.If(current, g.Attr("aria-current", "page")),
		Class("press flex min-h-[44px] w-full items-center gap-3 rounded-lg px-3 text-left text-sm font-medium transition "+navEntryTone(active)),
		Span(Class("text-base"), g.Text(p.Icon)), Span(g.Text(p.Label)))
}

// navPageLinks is pages ps's sidebar links.
func navPageLinks(ps []navPage, activeTab, currentTab string, hasPanels bool) g.Node {
	return g.Map(ps, func(p navPage) g.Node { return navPageLink(p, activeTab, currentTab, hasPanels) })
}
