package ui

import (
	"strconv"
	"strings"
	"time"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE PAGES SCREENS' PARTS (internal/api pages.go draws the screens): a row of
// the Pages list, a ⋯ menu, a time.
//
// The top layer is plain (the operator rule): a row is the page's title, its
// number ("#12", what the list's search takes), who made it and when (PlainWhen's words: "today at 9:14"), and a badge for each
// thing worth a look — only when there is one. No "0 …", no "(s)": a count that is zero is not drawn at all, and one
// that is drawn is said in words that agree with it ("1 reply", "3 replies").

// PageRowView is one row of the Pages list.
type PageRowView struct {
	ID int64
	// Title is the page's RAW title (plained here, once: pageCardTitle).
	Title string
	// Maker names who made the page; "" draws no "by".
	Maker   string
	Updated time.Time
	// Now is what Updated is told against (PlainWhen).
	Now time.Time
	// Versions, Pending (links waiting for the owner's approval) and Replies
	// (responses) are counts; Current is the version shown (what its links
	// show); Live is whether a live link exists.
	Versions, Current, Pending, Replies int
	Live                                bool
	// ShareURL is the page's reusable link when it has one (Share hands it out
	// at once, and the ⋯ menu then offers Revoke link and New link); Reusable
	// is whether the server keeps one (PageShareButton).
	ShareURL string
	Reusable bool
	// NoRevoke: the reader may not revoke the page's link (pages.MayRevoke: a
	// team's page, read by a member who may not), so the ⋯ draws no Revoke
	// link and no New link. Share stays: any member shares.
	NoRevoke bool
	// TeamID and Team mark a TEAM's page (0165), read by a member: the team's
	// user id and its mark in plain words ("Team: <name>"). 0 and "" for a
	// person's own page.
	TeamID int64
	Team   string
}

// pageBadges are a row's badges, in order: each only when it says something.
func pageBadges(v PageRowView) []string {
	var out []string
	// The version SHOWN (the current one, which a rollback moves), drawn only
	// when there is more than one.
	if v.Versions > 1 {
		out = append(out, "v"+strconv.Itoa(v.Current))
	}
	if v.Live {
		out = append(out, "Live")
	}
	if v.Pending > 0 {
		out = append(out, strconv.Itoa(v.Pending)+" waiting")
	}
	if v.Replies > 0 {
		out = append(out, plural(v.Replies, "reply", "replies"))
	}
	return out
}

// pageBadge is one badge.
func pageBadge(text string) g.Node {
	return Span(g.Attr("data-page-badge", ""), Class("inline-flex shrink-0 items-center rounded-full bg-s2 px-2 py-0.5 text-xs font-medium text-fg2 ring-1 ring-inset ring-line"),
		g.Text(text))
}

// PageRow is one row of the Pages list: the whole row opens the page's own
// view; its ⋯ (pageRowMenu) holds Share, Details and the link actions.
func PageRow(v PageRowView) g.Node {
	t := pageCardTitle(v.Title)
	var badges []g.Node
	for _, b := range pageBadges(v) {
		badges = append(badges, pageBadge(b))
	}
	var meta []g.Node
	// The page's number (operator decision: "see the page ID on the page
	// card … and search by ID"), "#12", the way the search box takes it. The
	// one id the plain top layer draws (TestThePagesScreensArePlain).
	if v.ID > 0 {
		meta = append(meta, Span(g.Attr("data-page-number", ""), Class(PagesNumberClass), g.Text("#"+strconv.FormatInt(v.ID, 10))))
	}
	if v.Maker != "" {
		if len(meta) > 0 {
			meta = append(meta, g.Text(" · "))
		}
		meta = append(meta, g.Text("by "+v.Maker))
	}
	if !v.Updated.IsZero() {
		if len(meta) > 0 {
			meta = append(meta, g.Text(" · "))
		}
		meta = append(meta, g.Raw(PlainWhenHTML("", v.Updated, v.Now)))
	}
	if v.TeamID > 0 {
		meta = append([]g.Node{Span(Class("font-semibold text-fg"), g.Attr("data-page-team", strconv.FormatInt(v.TeamID, 10)), g.Text(v.Team)),
			g.If(len(meta) > 0, g.Text(" · "))}, meta...)
	}
	return Li(g.Attr("data-page-id", strconv.FormatInt(v.ID, 10)), Class("flex min-w-0 items-center gap-1 py-1"),
		A(Href(PageViewPath(v.ID)), hx("hx-boost", "false"), g.Attr("data-page-row-open", ""),
			Class("press flex min-h-[44px] min-w-0 flex-1 flex-col gap-1 rounded-lg px-3 py-2 transition hover:bg-s2"),
			Span(g.Attr("data-page-title", ""), Class("truncate text-sm font-semibold text-fg"), g.Text(t)),
			g.If(len(meta) > 0 || len(badges) > 0, Span(Class("flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 text-xs text-fg2"),
				g.If(len(meta) > 0, Span(g.Attr("data-page-meta", ""), Class("min-w-0 max-w-full truncate"), g.Group(meta))),
				g.If(len(badges) > 0, Span(g.Attr("data-page-badges", ""), Class("flex flex-wrap items-center gap-1"), g.Group(badges)))))),
		pageRowMenu(v))
}

// pageRowMenu is a Pages row's ⋯: Share, Details (the page's own screen) and,
// when the page has a link to act on, Revoke link and New link.
func pageRowMenu(v PageRowView) g.Node {
	return MoreMenu("page-menu-"+strconv.FormatInt(v.ID, 10), "More for “"+pageCardTitle(v.Title)+"”",
		PageShareMenuItem(v.ID, v.Title, v.ShareURL, v.Reusable),
		A(Href(PageDetailPath(v.ID)), hx("hx-boost", "false"), g.Attr("data-page-details", ""), Class(pageMenuItem), g.Text("Details")),
		// Revoke link / New link act on the reusable link: none without a
		// link key (PageShareMenu's rule).
		g.If(v.Reusable && !v.NoRevoke, pageLinkActions(v.ID, v.ShareURL != "")))
}

// PageLinkMenu is the page screen's ⋯ beside Share: Revoke link and New link.
// nil when the page has no link to act on (live is false) — the screen then
// draws no ⋯ at all.
func PageLinkMenu(id int64, title string, live bool) g.Node {
	if !live {
		return nil
	}
	return MoreMenu("page-link-menu-"+strconv.FormatInt(id, 10), "Link options for “"+pageCardTitle(title)+"”", pageLinkActions(id, true))
}

// PageOpenButton is the page screen's primary button: the page's own view.
func PageOpenButton(id int64) g.Node {
	return A(Href(PageViewPath(id)), hx("hx-boost", "false"), g.Attr("data-page-view", ""), Class(pageButtonPrimary), g.Text("Open"))
}

// PageMenuItemClass is a ⋯ menu item's class, for an item a screen builds as a
// string (a version row's "Make current").
const PageMenuItemClass = pageMenuItem

// MoreMenu is a ⋯ button and the menu it opens: a native popover (Escape and
// a tap outside close it; Tab moves into it from the button), placed beside the
// button by floatingMenuScript. id is the menu's element id, unique in its
// document; label the button's accessible name.
func MoreMenu(id, label string, items ...g.Node) g.Node {
	return g.Group{
		Button(Type("button"), g.Attr("popovertarget", id), g.Attr("aria-haspopup", "menu"), g.Attr("aria-expanded", "false"),
			g.Attr("aria-label", label), g.Attr("title", label), g.Attr("data-more-menu-open", id),
			Class("press inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-lg text-fg2 transition hover:bg-s2"),
			g.Raw(`<svg class="h-5 w-5" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><circle cx="5" cy="12" r="1.75"/><circle cx="12" cy="12" r="1.75"/><circle cx="19" cy="12" r="1.75"/></svg>`)),
		Div(ID(id), g.Attr("popover", "auto"), g.Attr("role", "menu"), g.Attr("aria-label", label), g.Attr("data-floating-menu", ""),
			Class("invisible hidden [&:popover-open]:flex m-0 w-48 flex-col gap-1 rounded-xl border border-line bg-s1 p-1.5 text-fg shadow-2xl ring-1 ring-black/20"),
			g.Group(items)),
	}
}

// MoreMenuHTML is MoreMenu as HTML, for a screen built as a string; items are
// trusted markup (muster's own, every value in it escaped).
func MoreMenuHTML(id, label string, items ...string) string {
	return nodeHTML(MoreMenu(id, label, g.Raw(strings.Join(items, ""))))
}

// FloatingMenuScriptHTML is the script that places every MoreMenu, for a
// screen that draws one in a document without it (the Pages screens).
func FloatingMenuScriptHTML() string { return nodeHTML(floatingMenuScript()) }

// nodeHTML is n as HTML ("" for nil).
func nodeHTML(n g.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	_ = n.Render(&b)
	return b.String()
}
