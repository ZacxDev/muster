package ui

import (
	"strconv"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE SESSION SWITCHER: a bar BELOW the message input of a session's page — a
// mirrored window's Rendered view and a dispatched agent's chat page — that
// hot-swaps to another session in one tap (operator ask: "add another bar
// component below the message input bar where I can hot swap between active
// sessions").
//
// THREE VIEWS, picked from a menu the button on the bar's left opens (it
// animates open UPWARD, the bar being at the screen's bottom — switcherViewMenu):
//
//   - Recent: the sessions the viewer last OPENED — the SAME server-side,
//     cross-device list the Agents tab's "Recently viewed" strip reads
//     (internal/recents; recents.go), newest first. Unlike the strip it keeps
//     pinned entries: here the pins are a view of their own, not the head of a
//     list beside it. The page's own session is left out (it is always the
//     newest: the page recorded the visit before it was drawn).
//   - Pins: the viewer's pins (internal/pins), in the list's order.
//   - All: every entry the Agents list draws, in its order (orderList).
//
// In Pins and All the page's own session is drawn as "current" (aria-current,
// not a link), so the bar says where the viewer is.
//
// Every entry is labelled as its pinned chip is (pinnedChipLabels: the
// window's codename, an agent's name — disambiguated only against the other
// entries of the same view; plained through textplain.Title there, once) —
// except that an agent's name is never clamped here (sessionSwitcherLabels) —
// and opens what its card opens (MirrorCardHref, "/agents/<name>") with a real
// load.
//
// 🔴 A PILL IS ALWAYS AS WIDE AS ITS WHOLE LABEL (operator ask: the pills were
// "often cut off … ensure that it's always wide enough to show the full
// title"). There is no width cap and no ellipsis: a long title makes a long
// pill, and the row scrolls sideways (below). This replaced #276's 26rem cap.
//
// A SEARCH (the magnifier on the bar's right, or "/" and Ctrl/Cmd+K) opens a
// command-palette-style box over the page that finds any session the All view
// holds by its tmux codename ("wheat", "mango:2") or its title, as it is typed
// (switcherSearchBox). It reads the All row's own entries — no request.
//
// 🔴 THE MENU AND THE PALETTE ARE POPOVERS (the top layer), so opening either
// moves nothing on the page: no layout shift, and no clipping by the
// composer's box. A browser with no Popover API keeps the old behaviour — the
// left button cycles the views — and draws no search button.
//
// 🔴 SERVER-RENDERED, ALL THREE VIEWS, AT A FIXED HEIGHT — THAT IS THE
// NO-LAYOUT-SHIFT CONTRACT. The bar paints with the page (never empty, then
// filled), its row is h-11 whatever it holds (an entry, an empty-state line),
// and a view switch only toggles which row is shown. The viewer's choice of
// view is a per-device convenience kept in localStorage; the inline script
// right after the bar applies it before the bar is painted, so a page that
// loads in "All" never shows "Recent" first.
//
// 🔴 IT SCROLLS SIDEWAYS INSIDE ITSELF (each row is overflow-x-auto, min-w-0),
// so thirty long names never widen the page.

// SwitcherID is the bar's element id.
const SwitcherID = "session-switcher"

// switcherViewKey is where the viewer's chosen view is kept (localStorage).
const switcherViewKey = "muster.switcher.view"

// The switcher's views, as data-switcher-view and the storage value spell
// them, in the order the toggle cycles them.
const (
	SwitcherRecent = "recent"
	SwitcherPins   = "pins"
	SwitcherAll    = "all"
)

// switcherViews is the cycle: each view's key, its button label, and what its
// row says when it holds nothing.
var switcherViews = []struct{ key, label, empty string }{
	{SwitcherRecent, "Recent", "No other recent sessions"},
	{SwitcherPins, "Pins", "Nothing pinned"},
	{SwitcherAll, "All", "No other sessions"},
}

// switcherRecentMax is how many entries Recent shows.
const switcherRecentMax = 8

// SwitcherCurrent names the page's own session: an agent by id, or a mirrored
// window by its address (AgentID 0). A window addressed by its session alone
// (Window "") is the current one whichever of that session's windows a card
// names.
type SwitcherCurrent struct {
	AgentID int64
	Mirror  MirrorRef
}

// is reports whether e is the page's own session.
func (c SwitcherCurrent) is(e listEntry) bool {
	if e.agent != nil {
		return c.AgentID != 0 && e.agent.ID == c.AgentID
	}
	m := e.mirror
	return c.AgentID == 0 && c.Mirror.Host != "" && m.Host == c.Mirror.Host && m.Session == c.Mirror.Session &&
		(c.Mirror.Window == "" || m.Window == c.Mirror.Window)
}

// Switcher is the bar's content, resolved on the server: one list per view.
type Switcher struct {
	Recent, Pins, All []SwitcherItem
}

// SwitcherItem is one entry of a view.
type SwitcherItem struct {
	Href, Label string
	// Kind is "agent" or "mirror".
	Kind string
	// Current: the page's own session (drawn, not linked).
	Current bool
	// Waiting: the entry waits on the viewer (its card's data-wait).
	Waiting bool
	// Host and HostIcon are the entry's host and its icon key — the pinned
	// chip's (pinnedChip): the cluster for an agent, the window's host's
	// otherwise. HostNamed: the label already spells the host
	// (pinnedChipLabelNamesHost), so the icon is decorative.
	Host, HostIcon string
	HostNamed      bool
	// Code and Title are what the search finds the entry by (switcherSearchBox):
	// Code its tmux codename ("mango:2") for a window — its "<session>:<index>"
	// title when the host named none — or its name for an agent; Title what it
	// is called beyond that (its display name, its focus), "" for nothing more.
	Code, Title string
}

// NewSwitcher resolves the bar against the entries the Agents list draws
// (list, mirrors — internal/api builds them the way it builds the list) and
// the viewer's recently opened refs (newest first, as the strip's).
func NewSwitcher(list []AgentCardView, mirrors []MirrorCardView, refs []RecentRef, cur SwitcherCurrent) *Switcher {
	entries := orderList(list, mirrors)
	byAgent := map[int64]listEntry{}
	byMirror := map[MirrorRef]listEntry{}
	var pins []listEntry
	for _, e := range entries {
		if e.agent != nil {
			byAgent[e.agent.ID] = e
		} else {
			byMirror[MirrorRef{Host: e.mirror.Host, Session: e.mirror.Session, Window: e.mirror.Window}] = e
		}
		if e.pinned() {
			pins = append(pins, e)
		}
	}
	var recent []listEntry
	seen := map[listEntry]bool{}
	for _, r := range refs {
		if len(recent) == switcherRecentMax {
			break
		}
		var e listEntry
		var ok bool
		if r.AgentID != 0 {
			e, ok = byAgent[r.AgentID]
		} else {
			e, ok = byMirror[r.Mirror]
		}
		// Gone (deleted, no longer listed), the page itself, or a repeat.
		if !ok || cur.is(e) || seen[e] {
			continue
		}
		seen[e] = true
		recent = append(recent, e)
	}
	return &Switcher{Recent: switcherItems(recent, cur), Pins: switcherItems(pins, cur), All: switcherItems(entries, cur)}
}

// switcherItems is es as one view's items, labelled against each other.
func switcherItems(es []listEntry, cur SwitcherCurrent) []SwitcherItem {
	labels := sessionSwitcherLabels(es)
	out := make([]SwitcherItem, len(es))
	for i, e := range es {
		it := SwitcherItem{Label: labels[i], Current: cur.is(e)}
		if e.agent != nil {
			a := e.agent
			it.Href, it.Kind = a.PageHref(), "agent"
			it.Waiting = waitTokens(a.Attention, a.AwaitingApproval) != ""
			it.Host, it.HostIcon = podHost, clusterIcon.Key
			it.Code = a.Name
			it.Title = switcherSearchTitle(a.Name, plainOr(a.DisplayName, ""), focusTitle(a.Focus))
		} else {
			m := e.mirror
			it.Href, it.Kind = MirrorCardHref(*m), "mirror"
			it.Waiting = waitTokens(m.Attention, false) != ""
			it.Host, it.HostIcon = m.Host, m.HostIcon
			it.HostNamed = pinnedChipLabelNamesHost(*m, labels[i])
			_, it.Code = pinnedChipName(e, true)
			it.Title = switcherSearchTitle(it.Code, focusTitle(m.Focus))
		}
		out[i] = it
	}
	return out
}

// focusTitle is f's title, plained; "" for none.
func focusTitle(f *FocusView) string {
	if f == nil {
		return ""
	}
	return plainOr(f.Title, "")
}

// switcherSearchTitle is the distinct, non-empty names in names that are not
// code, joined by " · ": what the search shows and matches as an entry's title.
func switcherSearchTitle(code string, names ...string) string {
	var out []string
	seen := map[string]bool{code: true, "": true}
	for _, n := range names {
		if n = strings.TrimSpace(n); !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return strings.Join(out, " · ")
}

// view is the items of view key.
func (s *Switcher) view(key string) []SwitcherItem {
	switch key {
	case SwitcherPins:
		return s.Pins
	case SwitcherAll:
		return s.All
	}
	return s.Recent
}

// switcherBar is the bar: the view button, one row per view (Recent shown, the
// others hidden until the script picks them), and the search button — then
// the view menu and the search palette, both popovers. pinned: the bar is
// the last row of a pinned composer that spans the screen (the Rendered
// view's) — it is inset like that composer's rows and padded by the safe-area
// inset, being the last thing above the screen's edge; otherwise (the agent
// page, inside its already-inset pane) it lines up with the form above it.
// Nothing when s is nil.
func switcherBar(s *Switcher, pinned bool) g.Node {
	if s == nil {
		return nil
	}
	rows := make([]g.Node, 0, len(switcherViews))
	for _, v := range switcherViews {
		rows = append(rows, switcherRow(v.key, v.empty, s.view(v.key), v.key != SwitcherRecent))
	}
	return g.Group{
		Nav(
			ID(SwitcherID),
			g.Attr("aria-label", "Switch session"),
			g.Attr("data-switcher-view", SwitcherRecent),
			// A sideways swipe here is the bar's, not the sidebar edge-swipe's
			// (appScript's ownsSideways), as on the Recent strip.
			g.Attr("data-own-swipe", ""),
			// 🔴 ABSOLUTE, ON ROOM ITS CONTAINER RESERVED (switcherRoom): the
			// bar's markup comes after the form, and a browser may paint the
			// page with the form parsed and the bar not yet — measured, the
			// Rendered view's composer then grew 61→106px under the reader
			// (layout shift 0.0047–0.012 at 1280×800). Reserved by the
			// container's padding from its first byte, the bar's arrival moves
			// nothing.
			//
			// 🔴 AND ITS HEIGHT IS FIXED TOO, to exactly that room: a frame can
			// also land with the <nav> parsed and its row not yet, a 1px bar at
			// the bottom that then grew to 45px and moved up by 44 (measured on a
			// 200-message agent page under a parser stall inside the bar).
			Class("absolute inset-x-0 bottom-0 border-t border-line bg-bg "+switcherHeight(pinned)+classIf(pinned, " px-3 pb-[env(safe-area-inset-bottom)]")+" [&[data-switcher-pending]]:invisible"),
			// 🔴 AND INVISIBLE UNTIL ITS SCRIPT (straight after it) HAS RUN: a
			// frame can land mid-bar too, with the toggle's icon parsed and its
			// label not, and the icon then moved aside for the label (0.00059 at
			// 390px, with the title, under a parser stall there). The bar's room
			// is reserved, so its appearing moves nothing; a <noscript> rule
			// shows it without script.
			g.Attr("data-switcher-pending", ""),
			Div(
				Class("flex h-11 min-w-0 items-center gap-2"),
				Button(
					ID("switcher-toggle"),
					Type("button"),
					// Opens the view menu: the browser's popover toggle, so it opens,
					// closes on a second tap, a tap outside and Escape with no script.
					g.Attr("popovertarget", switcherMenuID),
					g.Attr("aria-haspopup", "menu"),
					g.Attr("aria-expanded", "false"),
					g.Attr("aria-controls", switcherMenuID),
					g.Attr("aria-label", switcherToggleLabel(SwitcherRecent)),
					g.Attr("title", switcherToggleLabel(SwitcherRecent)),
					// A fixed width: the label changes on a pick, the row beside it
					// must not move. 44px tall (the row's height), its face 32px.
					Class("press group inline-flex h-11 w-[4.5rem] shrink-0 items-center"),
					Span(
						Class("inline-flex h-8 w-full items-center justify-center gap-1 rounded-lg bg-s2 text-xs font-semibold text-fg2 ring-1 ring-inset ring-line transition group-hover:bg-s3 group-aria-expanded:bg-s3 group-aria-expanded:text-fg"),
						g.Raw(`<svg class="h-3.5 w-3.5 shrink-0" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><path fill-rule="evenodd" d="M2 5.75A.75.75 0 012.75 5h14.5a.75.75 0 010 1.5H2.75A.75.75 0 012 5.75zm0 4.25a.75.75 0 01.75-.75h14.5a.75.75 0 010 1.5H2.75A.75.75 0 012 10zm0 4.25a.75.75 0 01.75-.75h14.5a.75.75 0 010 1.5H2.75a.75.75 0 01-.75-.75z" clip-rule="evenodd"/></svg>`),
						Span(g.Attr("data-switcher-label", ""), g.Text(switcherViews[0].label)),
					),
				),
				g.Group(rows),
				Button(
					ID(switcherSearchOpenID),
					Type("button"),
					g.Attr("aria-haspopup", "dialog"),
					g.Attr("aria-expanded", "false"),
					g.Attr("aria-controls", switcherSearchID),
					g.Attr("aria-keyshortcuts", "/ Control+K Meta+K"),
					g.Attr("aria-label", "Search sessions"),
					g.Attr("title", "Search sessions (/ or Ctrl+K)"),
					Class("press group inline-flex h-11 w-11 shrink-0 items-center justify-center"),
					Span(
						Class("inline-flex h-8 w-8 items-center justify-center rounded-lg bg-s2 text-fg2 ring-1 ring-inset ring-line transition group-hover:bg-s3"),
						g.Raw(searchIconSVG),
					),
				),
			),
			switcherViewMenu(s),
			switcherSearchBox(),
		),
		switcherScript(),
		NoScript(StyleEl(g.Raw(`#`+SwitcherID+` { visibility: visible !important; }`))),
	}
}

// The ids of the view menu, the search palette and its parts.
const (
	switcherMenuID          = "switcher-views"
	switcherSearchID        = "switcher-search"
	switcherSearchOpenID    = "switcher-search-open"
	switcherSearchInputID   = "switcher-search-input"
	switcherSearchResultsID = "switcher-search-results"
)

// searchIconSVG is the magnifier (the 20px solid set the bar's other icon is from).
const searchIconSVG = `<svg class="h-4 w-4 shrink-0" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><path fill-rule="evenodd" d="M9 3.5a5.5 5.5 0 100 11 5.5 5.5 0 000-11zM2 9a7 7 0 1112.452 4.391l3.328 3.329a.75.75 0 11-1.06 1.06l-3.329-3.328A7 7 0 012 9z" clip-rule="evenodd"/></svg>`

// switcherViewMenu is the menu the view button opens: one radio item per view,
// the shown one checked, each with how many entries it holds. A popover (top
// layer, out of flow): it opens upward from the button, placed by the script
// before it is shown, and its open and close are animated in CSS
// (web/css/input.css, #switcher-views) — at once under prefers-reduced-motion.
func switcherViewMenu(s *Switcher) g.Node {
	items := make([]g.Node, 0, len(switcherViews))
	for _, v := range switcherViews {
		items = append(items, Button(
			Type("button"),
			g.Attr("role", "menuitemradio"),
			g.Attr("aria-checked", strconv.FormatBool(v.key == SwitcherRecent)),
			g.Attr("data-switcher-pick", v.key),
			Class("group flex min-h-[44px] w-full items-center gap-2 rounded-lg px-3 text-left text-sm font-medium text-fg outline-none transition hover:bg-s2 focus-visible:bg-s2 aria-checked:text-accent"),
			g.Raw(`<svg class="h-4 w-4 shrink-0 invisible group-aria-checked:visible" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><path fill-rule="evenodd" d="M16.704 4.153a.75.75 0 01.143 1.052l-8 10.5a.75.75 0 01-1.127.075l-4.5-4.5a.75.75 0 011.06-1.06l3.894 3.893 7.48-9.817a.75.75 0 011.05-.143z" clip-rule="evenodd"/></svg>`),
			Span(Class("flex-1"), g.Text(v.label)),
			Span(Class("text-xs tabular-nums text-muted"), g.Attr("aria-label", strconv.Itoa(len(s.view(v.key)))+" sessions"), g.Text(strconv.Itoa(len(s.view(v.key))))),
		))
	}
	return Div(
		ID(switcherMenuID),
		g.Attr("popover", ""),
		g.Attr("role", "menu"),
		g.Attr("aria-label", "Session views"),
		Class("m-0 min-w-[11rem] rounded-xl border-0 bg-s1 p-1 text-fg shadow-lg ring-1 ring-line"),
		g.Group(items),
	)
}

// switcherSearchBox is the search palette: a box over the page, near its top
// (a phone's keyboard takes the bottom), with an input and the list of what it
// finds, filled by the script as the operator types — from the All row's
// entries (their data-switcher-code and data-switcher-title). Ranked: the
// codename starts with what is typed, then contains it, then the title does;
// case-folded; each tier in the All view's order. Arrows move, Enter opens,
// Escape closes.
func switcherSearchBox() g.Node {
	return Div(
		ID(switcherSearchID),
		g.Attr("popover", ""),
		g.Attr("role", "dialog"),
		g.Attr("aria-label", "Search sessions"),
		Class("inset-x-0 bottom-auto top-[max(0.5rem,10vh)] mx-auto my-0 w-[min(36rem,calc(100vw-1rem))] overflow-hidden rounded-xl border-0 bg-s1 p-0 text-fg shadow-2xl ring-1 ring-line backdrop:bg-black/40"),
		Div(
			Class("flex items-center gap-2 border-b border-line px-3 text-muted"),
			g.Raw(searchIconSVG),
			Input(
				ID(switcherSearchInputID),
				Type("text"),
				g.Attr("role", "combobox"),
				g.Attr("aria-expanded", "true"),
				g.Attr("aria-controls", switcherSearchResultsID),
				g.Attr("aria-autocomplete", "list"),
				g.Attr("aria-label", "Search sessions by codename or title"),
				g.Attr("autocomplete", "off"),
				g.Attr("autocapitalize", "off"),
				g.Attr("autocorrect", "off"),
				g.Attr("spellcheck", "false"),
				g.Attr("enterkeyhint", "go"),
				g.Attr("placeholder", "Search sessions…"),
				// text-base: 16px, so a phone does not zoom the page on focus.
				Class("h-12 min-w-0 flex-1 bg-transparent text-base text-fg outline-none placeholder:text-muted"),
			),
			Kbd(Class("hidden rounded border border-line px-1.5 text-[11px] text-muted sm:inline"), g.Text("Esc")),
		),
		Ul(
			ID(switcherSearchResultsID),
			g.Attr("role", "listbox"),
			g.Attr("aria-label", "Sessions"),
			Class("max-h-[min(24rem,60vh)] overflow-y-auto overscroll-contain py-1"),
		),
		P(g.Attr("data-switcher-search-empty", ""), g.Attr("hidden", ""), Class("px-4 py-3 text-sm text-muted"), g.Text("No session matches")),
	)
}

// switcherHeight is the bar's own height, the same room switcherRoom
// reserves (border-box, so the border and the safe-area padding are inside it).
func switcherHeight(pinned bool) string {
	if pinned {
		return "h-[calc(2.8125rem+env(safe-area-inset-bottom))]"
	}
	return "h-[2.8125rem]"
}

// switcherRoom is the bottom padding a container holding the bar reserves
// for it: its 44px row and 1px border (2.8125rem), plus the safe-area inset
// when pinned. The bar is absolutely placed in that padding (switcherBar).
func switcherRoom(pinned bool) string {
	if pinned {
		return "pb-[calc(2.8125rem+env(safe-area-inset-bottom))]"
	}
	return "pb-[2.8125rem]"
}

// classIf is s when ok, "" otherwise: an optional class suffix.
func classIf(ok bool, s string) string {
	if ok {
		return s
	}
	return ""
}

// switcherToggleLabel is the view button's accessible name while key is shown.
func switcherToggleLabel(key string) string {
	for _, v := range switcherViews {
		if v.key == key {
			return "Showing " + v.label + " sessions; choose a view"
		}
	}
	return ""
}

// switcherRow is one view's row: its entries, scrolling sideways, or the
// line saying it holds none.
func switcherRow(key, empty string, items []SwitcherItem, hidden bool) g.Node {
	body := g.Group(g.Map(items, switcherEntry))
	if len(items) == 0 {
		body = g.Group{P(g.Attr("data-switcher-empty", ""), Class("truncate text-xs text-muted"), g.Text(empty))}
	}
	return Div(
		g.Attr("data-switcher-list", key),
		// A page restored from the back/forward cache redraws its rows from
		// its own fresh copy (restore.go), so Pins and Recent are current —
		// and every entry still links the session it names now.
		g.Attr(RestoreRefreshAttr, "switcher-"+key),
		g.If(hidden, g.Attr("hidden", "")),
		Class("flex min-w-0 flex-1 items-center gap-1.5 overflow-x-auto"),
		body,
	)
}

// titleEntryMaxW is how wide an entry of the Agents list's Recent strip
// (recentEntry) may grow.
//
// An operator ask: agents' titles were cut off (the strip was 14rem); 20rem
// still clipped ~50-character task titles, so it is 26rem. An 84-character
// title (605px) still clips there.
//
// ⚠ THE SESSION SWITCHER NO LONGER SHARES IT. Its pills were "often cut off"
// even at 26rem, and the operator asked that they be "always wide enough to
// show the full title": a switcher pill has no cap at all
// (switcherEntryBoxClass) and its row scrolls sideways instead.
//
// 🔴 min(…, 100%): never wider than the row it scrolls in.
const titleEntryMaxW = "max-w-[min(26rem,100%)]"

// switcherEntryBoxClass is an entry's 44px box: as wide as its pill, with NO
// cap — shrink-0, so the row's flex layout never squeezes it either. A label
// longer than the row makes the row scroll sideways (overflow-x-auto, inside
// its min-w-0 box), never the page.
const switcherEntryBoxClass = "inline-flex h-11 shrink-0 items-center"

// switcherLabelClass is an entry's label: one line, never truncated.
const switcherLabelClass = "whitespace-nowrap"

// switcherEntry is one entry: a real-load link (hx-boost="false", like a
// card's), or — the page's own session — the same chip, not a link.
func switcherEntry(it SwitcherItem) g.Node {
	// 🔴 A 44px TAP TARGET AROUND A 32px PILL: the entry fills the bar's h-11
	// row (every control in the app is a 44px target; e2e/tests/layout.spec.ts
	// measures it), and only the pill inside it is drawn.
	const pill = "inline-flex h-8 items-center gap-1.5 rounded-full px-3 text-xs font-semibold ring-1 ring-inset "
	const box = switcherEntryBoxClass
	// The host's icon, as the pinned chip draws it (#252): hostMark's fixed
	// 16px box, server-rendered, so it moves nothing; decorative when the
	// label already names the host.
	icon := hostMark(it.Host, it.HostIcon)
	if it.HostNamed {
		icon = hostMarkDecorative(it.Host, it.HostIcon)
	}
	inner := g.Group{
		g.If(it.Waiting, Span(g.Attr("data-switcher-wait", ""), g.Attr("role", "img"), g.Attr("aria-label", "Waiting on you"),
			Class("h-2 w-2 shrink-0 rounded-full bg-accent"))),
		g.If(it.Host != "", icon),
		Span(Class(switcherLabelClass), g.Text(it.Label)),
	}
	// What the search finds the entry by (switcherSearchBox).
	search := g.Group{
		g.If(it.Code != "", g.Attr("data-switcher-code", it.Code)),
		g.If(it.Title != "", g.Attr("data-switcher-title", it.Title)),
	}
	if it.Current {
		return Span(
			g.Attr("data-switcher-entry", it.Kind),
			g.Attr("data-switcher-current", ""),
			g.Attr("aria-current", "page"),
			g.Attr("title", it.Label+" (this session)"),
			search,
			Class(box),
			Span(Class(pill+"bg-accent/15 text-accent ring-accent/40"), inner),
		)
	}
	return A(
		Href(it.Href),
		hx("hx-boost", "false"),
		// A link to a window's Rendered view is a warming source for the view
		// snapshots: web/static/mirror-snap.js's candidates() reads
		// a[data-snap-warm] (at most WARM_MAX a pass).
		g.If(strings.HasPrefix(it.Href, "/mirror/view?"), g.Attr("data-snap-warm", "")),
		g.Attr("data-switcher-entry", it.Kind),
		g.Attr("title", it.Label),
		search,
		Class("press group "+box),
		Span(Class(pill+"bg-s1/70 text-fg ring-line transition group-hover:bg-s2"), inner),
	)
}

// switcherScript applies the viewer's stored view at once (it runs as the bar
// is parsed, before it is painted), and wires the view menu and the search
// palette. Storage that throws (a private window, blocked site data) leaves
// Recent, and the menu still works for the page.
//
// Without the Popover API (an older browser) the view button cycles the views
// as it did before the menu, and the search button is hidden.
func switcherScript() g.Node {
	views, labels, names := "[", "{", "{"
	for i, v := range switcherViews {
		if i > 0 {
			views, labels, names = views+",", labels+",", names+","
		}
		views += strconv.Quote(v.key)
		labels += strconv.Quote(v.key) + ":" + strconv.Quote(v.label)
		names += strconv.Quote(v.key) + ":" + strconv.Quote(switcherToggleLabel(v.key))
	}
	return Script(g.Raw(`
(function () {
  var bar = document.getElementById('` + SwitcherID + `');
  if (bar) bar.removeAttribute('data-switcher-pending'); // whole now (see switcherBar)
  // A property, not an attribute: htmx's history snapshot carries attributes,
  // and after Back an attribute guard left the restored toggle unbound.
  if (!bar || bar.__musterSwitcherBound) return;
  bar.__musterSwitcherBound = true;
  var VIEWS = ` + views + `], LABELS = ` + labels + `}, NAMES = ` + names + `};
  var KEY = '` + switcherViewKey + `';
  var btn = document.getElementById('switcher-toggle');
  var menu = document.getElementById('` + switcherMenuID + `');
  var pal = document.getElementById('` + switcherSearchID + `');
  var openBtn = document.getElementById('` + switcherSearchOpenID + `');
  var input = document.getElementById('` + switcherSearchInputID + `');
  var list = document.getElementById('` + switcherSearchResultsID + `');
  var empty = pal.querySelector('[data-switcher-search-empty]');
  var popover = typeof menu.showPopover === 'function';
  function picks() { return Array.prototype.slice.call(menu.querySelectorAll('[data-switcher-pick]')); }
  function show(v) {
    if (VIEWS.indexOf(v) < 0) v = VIEWS[0];
    bar.setAttribute('data-switcher-view', v);
    var rows = bar.querySelectorAll('[data-switcher-list]');
    for (var i = 0; i < rows.length; i++) rows[i].hidden = rows[i].getAttribute('data-switcher-list') !== v;
    btn.querySelector('[data-switcher-label]').textContent = LABELS[v];
    btn.setAttribute('aria-label', NAMES[v]);
    btn.setAttribute('title', NAMES[v]);
    picks().forEach(function (p) { p.setAttribute('aria-checked', p.getAttribute('data-switcher-pick') === v ? 'true' : 'false'); });
  }
  function choose(v) {
    show(v);
    try { localStorage.setItem(KEY, v); } catch (e) {}
  }
  var stored = null;
  try { stored = localStorage.getItem(KEY); } catch (e) {}
  if (stored) show(stored);
  function isOpen(p) { try { return p.matches(':popover-open'); } catch (e) { return false; } }
  function hide(p) { try { if (isOpen(p)) p.hidePopover(); } catch (e) {} }

  if (!popover) {
    // No Popover API: the button cycles, and there is no search.
    btn.removeAttribute('aria-haspopup');
    btn.removeAttribute('aria-expanded');
    openBtn.hidden = true;
    btn.addEventListener('click', function () {
      choose(VIEWS[(VIEWS.indexOf(bar.getAttribute('data-switcher-view')) + 1) % VIEWS.length]);
    });
    return;
  }

  // --- the view menu: opens UPWARD from the button (the bar is at the bottom
  // of the screen), left-aligned to it, kept inside an 8px edge.
  function bottomOf() {
    var vv = window.visualViewport;
    return vv ? vv.height + vv.offsetTop : window.innerHeight;
  }
  function placeMenu() {
    var r = btn.getBoundingClientRect();
    menu.style.position = 'fixed';
    menu.style.inset = 'auto';
    menu.style.margin = '0';
    var vw = document.documentElement.clientWidth;
    menu.style.left = Math.max(8, Math.min(Math.round(r.left), vw - 8 - (menu.offsetWidth || 176))) + 'px';
    menu.style.bottom = Math.max(8, Math.round(window.innerHeight - r.top + 4 - (window.innerHeight - bottomOf()))) + 'px';
  }
  var focusPick = false;
  // Placed BEFORE it is shown (beforetoggle): placed on toggle, it would be
  // painted once at the popover's default place and then jump.
  menu.addEventListener('beforetoggle', function (e) { if (e.newState === 'open') placeMenu(); });
  menu.addEventListener('toggle', function (e) {
    var open = e.newState ? e.newState === 'open' : isOpen(menu);
    btn.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (open) {
      placeMenu();
      hide(pal);
      if (focusPick) {
        var cur = menu.querySelector('[aria-checked="true"]') || picks()[0];
        if (cur) cur.focus();
      }
      focusPick = false;
    } else if (menu.contains(document.activeElement)) {
      btn.focus();
    }
  });
  // Opened by a click, a key or a tap, it puts focus on the shown view, so
  // the arrows work at once (a pointer user never sees the ring: focus-visible).
  btn.addEventListener('click', function () { focusPick = true; });
  btn.addEventListener('keydown', function (e) {
    if (e.key === 'ArrowUp' || e.key === 'ArrowDown') {
      e.preventDefault();
      focusPick = true;
      if (!isOpen(menu)) menu.showPopover();
    }
  });
  menu.addEventListener('keydown', function (e) {
    var it = picks(), i = it.indexOf(document.activeElement);
    function to(n) { it[(n + it.length) % it.length].focus(); }
    if (e.key === 'ArrowDown') { e.preventDefault(); to(i + 1); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); to(i - 1); }
    else if (e.key === 'Home') { e.preventDefault(); to(0); }
    else if (e.key === 'End') { e.preventDefault(); to(-1); }
    else if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); hide(menu); btn.focus(); }
    else if (e.key === 'Tab') { hide(menu); }
  });
  menu.addEventListener('click', function (e) {
    var p = e.target && e.target.closest ? e.target.closest('[data-switcher-pick]') : null;
    if (!p) return;
    choose(p.getAttribute('data-switcher-pick'));
    hide(menu);
    btn.focus();
  });

  // --- the search palette.
  var shown = [], active = -1, opener = null;
  function fold(s) { return String(s || '').toLowerCase(); }
  // The All row's entries, read when the palette is used (a bfcache restore
  // redraws the rows: restore.go), the page's own session left out.
  function entries() {
    var es = bar.querySelectorAll('[data-switcher-list="all"] a[data-switcher-entry]');
    var out = [];
    for (var i = 0; i < es.length; i++) {
      var a = es[i];
      var label = a.getAttribute('title') || a.textContent;
      out.push({ href: a.getAttribute('href'), label: label, code: a.getAttribute('data-switcher-code') || label,
        title: a.getAttribute('data-switcher-title') || '', kind: a.getAttribute('data-switcher-entry'), n: i });
    }
    return out;
  }
  // rank: 0 the codename starts with q, 1 it contains q, 2 the title contains
  // q, -1 no match. An empty q is everything, in the All view's order.
  function rank(e, q) {
    if (!q) return 0;
    var c = fold(e.code);
    if (c.indexOf(q) === 0) return 0;
    if (c.indexOf(q) > 0) return 1;
    if (fold(e.title).indexOf(q) >= 0) return 2;
    return -1;
  }
  function el(tag, cls, text) { var x = document.createElement(tag); if (cls) x.className = cls; if (text != null) x.textContent = text; return x; }
  function mark() {
    var opts = list.children;
    for (var i = 0; i < opts.length; i++) {
      var on = i === active;
      opts[i].setAttribute('aria-selected', on ? 'true' : 'false');
      opts[i].classList.toggle('bg-s3', on);
    }
    if (active >= 0 && opts[active]) {
      input.setAttribute('aria-activedescendant', opts[active].id);
      opts[active].scrollIntoView({ block: 'nearest' });
    } else input.removeAttribute('aria-activedescendant');
  }
  function refresh() {
    var q = fold(input.value).trim();
    var found = [];
    entries().forEach(function (e) { var r = rank(e, q); if (r >= 0) { e.r = r; found.push(e); } });
    found.sort(function (a, b) { return a.r - b.r || a.n - b.n; });
    shown = found.slice(0, 50);
    list.textContent = '';
    shown.forEach(function (e, i) {
      var li = el('li', 'flex min-h-[44px] cursor-pointer flex-col justify-center px-4 py-1.5');
      li.id = 'switcher-opt-' + i;
      li.setAttribute('role', 'option');
      li.setAttribute('data-index', String(i));
      li.setAttribute('data-href', e.href);
      li.setAttribute('data-kind', e.kind);
      li.appendChild(el('span', 'truncate text-sm font-semibold text-fg', e.label));
      // The second line: its title, or — when the title is what the first line
      // already says (an agent's display name) — its codename.
      var sub = e.title && e.title !== e.label ? e.title : (e.code !== e.label ? e.code : '');
      if (sub) li.appendChild(el('span', 'truncate text-xs text-muted', sub));
      list.appendChild(li);
    });
    active = shown.length ? 0 : -1;
    empty.hidden = shown.length > 0;
    mark();
  }
  function go(i) {
    var e = shown[i];
    if (!e || !e.href) return;
    hide(pal);
    window.location.assign(e.href);
  }
  function openSearch() {
    if (isOpen(pal)) { input.focus(); input.select(); return; }
    opener = document.activeElement;
    hide(menu);
    input.value = '';
    refresh();
    pal.showPopover();
    // Focused in the same task as the tap, so a phone raises its keyboard.
    input.focus();
  }
  bar.__musterOpenSearch = openSearch;
  openBtn.addEventListener('click', openSearch);
  pal.addEventListener('toggle', function (e) {
    var open = e.newState ? e.newState === 'open' : isOpen(pal);
    openBtn.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (!open && pal.contains(document.activeElement)) {
      var back = opener && opener.isConnected && opener !== document.body ? opener : openBtn;
      try { back.focus(); } catch (err) {}
    }
  });
  input.addEventListener('input', refresh);
  input.addEventListener('keydown', function (e) {
    if (e.isComposing) return;
    var n = shown.length;
    if (e.key === 'ArrowDown') { e.preventDefault(); if (n) { active = (active + 1) % n; mark(); } }
    else if (e.key === 'ArrowUp') { e.preventDefault(); if (n) { active = (active - 1 + n) % n; mark(); } }
    else if (e.key === 'Enter') { e.preventDefault(); go(active); }
    else if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); hide(pal); }
  });
  // A tap opens the option; the mousedown is kept from moving focus, so a
  // phone's keyboard stays up until the page goes.
  list.addEventListener('mousedown', function (e) { e.preventDefault(); });
  list.addEventListener('click', function (e) {
    var li = e.target && e.target.closest ? e.target.closest('[role=option]') : null;
    if (li) go(Number(li.getAttribute('data-index')));
  });
  window.addEventListener('resize', function () { if (isOpen(menu)) placeMenu(); });

  // "/" (focus not in a text field) and Ctrl/Cmd+K open the search, once per
  // document: the listener finds the page's bar when the key is pressed
  // (htmx swaps the page, and with it the bar). Neither is taken inside a
  // terminal (xterm), where both are the shell's.
  if (!window.__musterSwitcherKeys) {
    window.__musterSwitcherKeys = true;
    document.addEventListener('keydown', function (e) {
      if (e.defaultPrevented || e.isComposing || e.altKey) return;
      var b = document.getElementById('` + SwitcherID + `');
      if (!b || !b.__musterOpenSearch || !b.getClientRects().length) return;
      var t = e.target;
      if (t && t.closest && t.closest('.xterm')) return;
      var k = (e.ctrlKey || e.metaKey) && !e.shiftKey && (e.key === 'k' || e.key === 'K');
      var slash = e.key === '/' && !e.ctrlKey && !e.metaKey;
      if (slash && t && (t.isContentEditable || (t.closest && t.closest('input, textarea, select, [contenteditable=""], [contenteditable="true"]')))) return;
      if (!k && !slash) return;
      e.preventDefault();
      b.__musterOpenSearch();
    });
  }
})();
`))
}
