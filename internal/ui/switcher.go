package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/agents"
)

// THE SESSION SWITCHER: a bar BELOW the message input of a session's page — a
// mirrored window's Rendered view and a dispatched agent's chat page — that
// hot-swaps to another session in one tap (operator ask: "add another bar
// component below the message input bar where I can hot swap between active
// sessions").
//
// FOUR VIEWS, picked from a menu the button on the bar's left opens (it
// animates open UPWARD, the bar being at the screen's bottom — switcherViewMenu):
//
//   - Recent: the sessions the viewer last OPENED — the server-side,
//     cross-device list internal/recents keeps (recents.go), newest first.
//     It keeps pinned entries: the pins are a view of their own. The page's
//     own session is left out (it is always the newest: the page recorded the
//     visit before it was drawn).
//   - Waiting: every entry waiting on the viewer, in orderList's order, by the
//     predicate the sidebar marks its waiting rows by (listEntry.waiting,
//     list_order.go).
//   - Pins: the viewer's pins (internal/pins), in orderList's order.
//   - All: every entry, in orderList's order (the sidebar lists the same
//     entries, in navOrder's).
//
// In Waiting, Pins and All the page's own session is drawn as "current"
// (aria-current, not a link), so the bar says where the viewer is.
//
// Every entry is labelled by its WHOLE name (sessionSwitcherLabels: a window's
// codename WITH its ":<index>", an agent's whole name — qualified only when
// another entry of the same view reads the same; plained through
// textplain.Title there, once) and opens what its sidebar row opens
// (MirrorCardHref, the agent's PageHref) with a real load.
//
// 🔴 A PILL IS ALWAYS AS WIDE AS ITS WHOLE LABEL (operator ask: the pills were
// "often cut off … ensure that it's always wide enough to show the full
// title"). There is no width cap and no ellipsis: a long title makes a long
// pill, and the row scrolls sideways (below). This replaced #276's 26rem cap.
// 🔴 AND THE LABEL IS THE WHOLE NAME: the pill used to drop a codename's
// ":<index>" ("mango:8" read "mango") whenever no other entry of its view
// shared the rest — a cut made on the SERVER, which no CSS fix could see
// (operator report: "it's missing the number after the colon").
//
// A SEARCH (the magnifier on the bar's right, the sidebar's, or "/" outside a
// text field) opens the command palette (palette.go): the same box from either
// button, finding any session the All view holds.
//
// ON A SESSION'S PAGE THE BAR ALSO HAS A PIN (switcherPinToggle), beside the
// search: it pins or unpins the page's own session — an agent, or the mirrored
// window — in the viewer's pins (internal/pins), the set the Pins view shows.
//
// The bar draws no "+": the task-less dispatch sheet's "+", which the retired
// Agents list's docked bar carried, is the sidebar's (navDeployButton).
//
// 🔴 THE MENU AND THE PALETTE ARE POPOVERS (the top layer), so opening either
// moves nothing on the page: no layout shift, and no clipping by the
// composer's box.
//
// 🔴 SERVER-RENDERED, EVERY VIEW, AT A FIXED HEIGHT — THAT IS THE
// NO-LAYOUT-SHIFT CONTRACT. The bar paints with the page (never empty, then
// filled), its row is h-11 whatever it holds (an entry, an empty-state line),
// and a view switch only toggles which row is shown. The viewer's choice of
// view is a per-device convenience kept in localStorage; the inline script
// right after the bar applies it before the bar is painted, so a page that
// loads in "All" never shows "Recent" first.
//
// 🔴 IT SCROLLS SIDEWAYS INSIDE ITSELF (each row is overflow-x-auto, min-w-0),
// so thirty long names never widen the page.
//
// ONLY SESSION PAGES DRAW IT. The retired Agents list page also drew it, docked
// to the bottom of the viewport with no page session, and refreshed its rows
// out of band on every list render; out of band they now arrive only with the
// pin toggle's answer (switcherRefresh, RenderSwitcherPin).

// SwitcherID is the bar's element id.
const SwitcherID = "session-switcher"

// switcherViewKey is where the viewer's chosen view is kept (localStorage).
const switcherViewKey = "muster.switcher.view"

// The switcher's views, as data-switcher-view and the storage value spell
// them, in the order the view menu lists them.
const (
	SwitcherRecent  = "recent"
	SwitcherWaiting = "waiting"
	SwitcherPins    = "pins"
	SwitcherAll     = "all"
)

// switcherViews is the menu's views, in order: each view's key, its button
// label (short: the button is a fixed 4.5rem), its menu item's words, and what
// its row says when it holds nothing.
var switcherViews = []struct{ key, label, menu, empty string }{
	{SwitcherRecent, "Recent", "Recent", "No other recent sessions"},
	{SwitcherWaiting, "Waiting", "Waiting on you", "Nothing is waiting on you"},
	{SwitcherPins, "Pins", "Pins", "Nothing pinned"},
	{SwitcherAll, "All", "All", "No other sessions"},
}

// switcherRecentMax is how many entries Recent shows.
const switcherRecentMax = 8

// SwitcherCurrent names the page's own session: an agent by id, or a mirrored
// window by its address (AgentID 0). A window addressed by its session alone
// (Window "") is the current one whichever of that session's windows an entry
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
	Recent, Waiting, Pins, All []SwitcherItem
	// pin is the page's own session as the bar's pin toggle posts it; nil
	// when the page is no session or its session is not among the entries.
	pin *switcherPin
}

// SwitcherItem is one entry of a view.
type SwitcherItem struct {
	Href, Label string
	// Kind is "agent" or "mirror".
	Kind string
	// Current: the page's own session (drawn, not linked).
	Current bool
	// Waiting: the entry waits on the viewer (listEntry.waiting).
	Waiting bool
	// Host and HostIcon are the entry's host and its icon key — its sidebar row's:
	// the cluster for an agent, the window's host's otherwise. HostNamed: the
	// label already spells the host (labelNamesHost), so the icon is
	// decorative.
	Host, HostIcon string
	HostNamed      bool
	// Code and Title are what the search finds the entry by (palette.go's paletteBox):
	// Code its tmux codename ("mango:2") for a window — its "<session>:<index>"
	// title when the host named none — or its name for an agent; Title what it
	// is called beyond that (its display name, its focus), "" for nothing more.
	Code, Title string
	// Words are what the search finds the entry by AFTER its codename and
	// title, as plain words: its repository, its kind and — a window's — its
	// raw tmux session name; "" for none. Space-separated, lower-cased.
	Words string
}

// switcherName is an entry's WHOLE name, what its pill shows:
//
//   - a window with a codename (Label, unless it only repeats the session): the
//     label as shown, ":<index>" and all ("mango:1");
//   - a window with none: its "<session>:<window index>" title (or the session
//     name, for a per-session host);
//   - an agent: its display name (or name), WHOLE. A pill is as wide as its
//     label (operator ask: "ensure that it's always wide enough to show the
//     full title"), so there is no "…" to add.
//
// 🔴 NOTHING IS CUT FROM IT. It used to have a SHORT form — the codename
// without its ":<index>" — drawn whenever no other entry of the view shared
// it, so a pill read "mango" for "mango:8" (operator report: "it's missing
// the number after the colon").
func switcherName(e listEntry) string {
	if e.agent != nil {
		return plainOr(e.agent.DisplayName, e.agent.Name)
	}
	m := e.mirror
	if m.Label != "" && m.Label != m.Session {
		return plainOr(m.Label, m.Session)
	}
	return plainOr(m.Title, m.Session)
}

// sessionSwitcherLabels is what each entry of one view says, in the entries'
// order: its whole name (switcherName), and when that is shared with another
// entry of the view (one label on two hosts) the name with the window's
// qualifier ("mango:2 · laptop"), the minimal difference the list already
// decided (mirror.SessionView.Qualifier).
//
// The pill also draws its host's icon (switcherEntry), but the label does NOT
// lean on it: the text is disambiguated exactly as it would be without the
// icon, so the search palette, which draws no icons, reads the same.
func sessionSwitcherLabels(es []listEntry) []string {
	out := make([]string, len(es))
	n := map[string]int{}
	for i, e := range es {
		out[i] = switcherName(e)
		n[out[i]]++
	}
	for i, e := range es {
		if n[out[i]] > 1 && e.mirror != nil && e.mirror.Qualifier != "" {
			out[i] = mirrorQualified(out[i], e.mirror.Qualifier)
		}
	}
	return out
}

// labelNamesHost is whether a window's label already spells its host: the
// label carries the window's qualifier (sessionSwitcherLabels appended it) and
// the qualifier leads with the host — the cross-host form, mirror.qualify's
// first part ("mango:1 · hosta", "Gold:1 · hosta · scratch3:1"). A one-host
// qualifier ("Gold:1 · scratch3:1") names no host. Then the pill's host icon is
// decorative: a screen reader would hear the host twice.
func labelNamesHost(m MirrorCardView, label string) bool {
	if m.Qualifier == "" || !strings.HasSuffix(label, mirrorQualifierSep+m.Qualifier) {
		return false
	}
	first, _, _ := strings.Cut(m.Qualifier, mirrorQualifierSep)
	return first == m.Host
}

// NewSwitcher resolves the bar against the entries (list, mirrors — internal/api
// builds them as it builds the sidebar's) and the viewer's recently opened refs
// (newest first).
func NewSwitcher(list []AgentCardView, mirrors []MirrorCardView, refs []RecentRef, cur SwitcherCurrent) *Switcher {
	entries := orderList(list, mirrors)
	byAgent := map[int64]listEntry{}
	byMirror := map[MirrorRef]listEntry{}
	var pins, waiting []listEntry
	var pin *switcherPin
	for _, e := range entries {
		if e.agent != nil {
			byAgent[e.agent.ID] = e
		} else {
			byMirror[MirrorRef{Host: e.mirror.Host, Session: e.mirror.Session, Window: e.mirror.Window}] = e
		}
		if e.pinned() {
			pins = append(pins, e)
		}
		if e.waiting() {
			waiting = append(waiting, e)
		}
		if pin == nil && cur.is(e) {
			pin = newSwitcherPin(e)
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
	return &Switcher{Recent: switcherItems(recent, cur), Waiting: switcherItems(waiting, cur), Pins: switcherItems(pins, cur), All: switcherItems(entries, cur), pin: pin}
}

// switcherItems is es as one view's items, labelled against each other.
func switcherItems(es []listEntry, cur SwitcherCurrent) []SwitcherItem {
	labels := sessionSwitcherLabels(es)
	out := make([]SwitcherItem, len(es))
	for i, e := range es {
		it := SwitcherItem{Label: labels[i], Current: cur.is(e), Waiting: e.waiting()}
		if e.agent != nil {
			a := e.agent
			it.Href, it.Kind = a.PageHref(), "agent"
			it.Host, it.HostIcon = podHost, clusterIcon.Key
			it.Code = a.Name
			it.Title = switcherSearchTitle(a.Name, plainOr(a.DisplayName, ""), focusTitle(a.Focus))
			// The host is a word too (the retired Agents list's host tabs): a
			// dispatched agent's is the cluster.
			it.Words = switcherSearchWords(a.Repo, agents.KindLabel(a.Kind), podHost)
		} else {
			m := e.mirror
			it.Href, it.Kind = MirrorCardHref(*m), "mirror"
			it.Host, it.HostIcon = m.Host, m.HostIcon
			it.HostNamed = labelNamesHost(*m, labels[i])
			it.Code = switcherName(e)
			it.Title = switcherSearchTitle(it.Code, focusTitle(m.Focus))
			it.Words = switcherSearchWords(m.Repo, mirrorKind(*m), m.Session, m.Host)
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

// switcherSearchWords is the non-empty parts, lower-cased and joined by
// spaces: an entry's data-switcher-words.
func switcherSearchWords(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.ToLower(strings.Join(strings.Fields(p), " ")); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}

// view is the items of view key.
func (s *Switcher) view(key string) []SwitcherItem {
	switch key {
	case SwitcherWaiting:
		return s.Waiting
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
		rows = append(rows, switcherRow(s, v.key, v.empty, v.key != SwitcherRecent))
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
				switcherPinToggle(s),
				Button(
					ID(switcherSearchOpenID),
					Type("button"),
					// Opens the command palette (palette.go), the one the
					// sidebar's magnifier opens.
					g.Attr(paletteOpenAttr, ""),
					g.Attr("aria-haspopup", "dialog"),
					g.Attr("aria-expanded", "false"),
					g.Attr("aria-controls", paletteID),
					g.Attr("aria-keyshortcuts", "/"),
					g.Attr("aria-label", "Search sessions"),
					g.Attr("title", "Search sessions (/)"),
					Class("press group inline-flex h-11 w-11 shrink-0 items-center justify-center"),
					Span(
						Class("inline-flex h-8 w-8 items-center justify-center rounded-lg bg-s2 text-fg2 ring-1 ring-inset ring-line transition group-hover:bg-s3"),
						g.Raw(searchIconSVG),
					),
				),
			),
			switcherViewMenu(s),
		),
		switcherScript(),
		NoScript(StyleEl(g.Raw(`#` + SwitcherID + ` { visibility: visible !important; }`))),
	}
}

// The ids of the view menu and the bar's search button (the palette's own are
// palette.go's).
const (
	switcherMenuID       = "switcher-views"
	switcherSearchOpenID = "switcher-search-open"
)

// switcherViewMenu is the menu the view button opens: one radio item per view,
// the shown one checked, each with how many entries it holds (refreshed with
// the rows on a restore: RestoreRefreshAttr). A popover (top
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
			Span(Class("flex-1"), g.Text(v.menu)),
			// The count is redrawn with the rows on a back/forward-cache restore
			// (restore.go), from the same fresh copy, so it never disagrees with
			// the row it counts.
			Span(ID(switcherCountID(v.key)), Class("text-xs tabular-nums text-muted"), g.Attr("data-switcher-count", v.key), g.Attr(RestoreRefreshAttr, "switcher-count-"+v.key), switcherCountText(s, v.key)),
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

// switcherRowID and switcherCountID are a view's row and its count in the
// menu: the ids the out-of-band refresh swaps into (switcherRefresh).
func switcherRowID(key string) string   { return "switcher-list-" + key }
func switcherCountID(key string) string { return "switcher-count-" + key }

// switcherCountText is how many entries view key holds.
func switcherCountText(s *Switcher, key string) g.Node {
	return g.Text(strconv.Itoa(len(s.view(key))))
}

// switcherRowBody is what a view's row holds: its entries, or the line saying
// it holds none.
func switcherRowBody(s *Switcher, key, empty string) g.Node {
	items := s.view(key)
	if len(items) == 0 {
		return P(g.Attr("data-switcher-empty", ""), Class("truncate text-xs text-muted"), g.Text(empty))
	}
	return g.Group(g.Map(items, switcherEntry))
}

// switcherRow is one view's row: its entries, scrolling sideways, or the
// line saying it holds none.
func switcherRow(s *Switcher, key, empty string, hidden bool) g.Node {
	body := switcherRowBody(s, key, empty)
	return Div(
		ID(switcherRowID(key)),
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

// switcherRefresh is the bar's rows and counts as the pin toggle's answer
// carries them, out of band (RenderSwitcherPin): each swaps its CHILDREN into the element of
// its id, so the shown row (a `hidden` the script set) and the view stay as
// the viewer left them. The script skips a copy identical to what is drawn
// (switcherScript), so a refresh that changes nothing leaves focus and the
// row's sideways scroll alone.
func switcherRefresh(s *Switcher) g.Node {
	out := make(g.Group, 0, 2*len(switcherViews))
	for _, v := range switcherViews {
		out = append(out,
			Div(ID(switcherRowID(v.key)), hx("hx-swap-oob", "innerHTML"), switcherRowBody(s, v.key, v.empty)),
			Span(ID(switcherCountID(v.key)), hx("hx-swap-oob", "innerHTML"), switcherCountText(s, v.key)),
		)
	}
	return out
}

// switcherEntryBoxClass is an entry's 44px box: as wide as its pill, with NO
// cap — shrink-0, so the row's flex layout never squeezes it either. A label
// longer than the row makes the row scroll sideways (overflow-x-auto, inside
// its min-w-0 box), never the page.
const switcherEntryBoxClass = "inline-flex h-11 shrink-0 items-center"

// switcherLabelClass is an entry's label: one line, never truncated.
const switcherLabelClass = "whitespace-nowrap"

// switcherEntry is one entry: a real-load link (hx-boost="false", like a
// sidebar row's), or — the page's own session — the same chip, not a link.
func switcherEntry(it SwitcherItem) g.Node {
	// 🔴 A 44px TAP TARGET AROUND A 32px PILL: the entry fills the bar's h-11
	// row (every control in the app is a 44px target; e2e/tests/layout.spec.ts
	// measures it), and only the pill inside it is drawn.
	const pill = "inline-flex h-8 items-center gap-1.5 rounded-full px-3 text-xs font-semibold ring-1 ring-inset "
	const box = switcherEntryBoxClass
	// The host's icon (#252): hostMark's fixed
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
	// What the search finds the entry by (palette.go's paletteBox).
	search := g.Group{
		g.If(it.Code != "", g.Attr("data-switcher-code", it.Code)),
		g.If(it.Title != "", g.Attr("data-switcher-title", it.Title)),
		g.If(it.Words != "", g.Attr("data-switcher-words", it.Words)),
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
// INLINE on a session's page (the agent page, the Rendered view) — the only
// pages that draw the bar now — as it was measured there. (The app shell, whose
// retired Agents panel docked the bar, carried it as a static file instead.)
// One text, switcherJS.
func switcherScript() g.Node { return Script(g.Raw(switcherJS)) }

// switcherOOBHandler takes the out-of-band copies of the bar's rows and
// counts (switcherRefresh, in the pin toggle's answer) and swaps them ITSELF, not htmx: the
// copy's CHILDREN go into the element of its id, which keeps the row itself
// (its hidden, the view), and a copy that says what is drawn already is not
// swapped at all.
//
// 🔴 NOT htmx's own innerHTML swap: htmx 2.0.4 inserted the copy ELEMENT into
// the row (two elements of one id, the row's entries one level down),
// measured in e2e/tests/host-icons.spec.ts. What it hands over here is the
// copy, or a fragment holding it; either way its children are used.
//
// 🔴 A SWAP KEEPS THE READER'S PLACE: a pill that had focus has it again
// after the swap (the pill of the same href, wherever the new order put it),
// and the row keeps its sideways scroll.
const switcherOOBHandler = `function (e) {
      var d = e.detail, t = d && d.target;
      if (!t || !t.closest || !t.closest('#` + SwitcherID + `') || !d.fragment) return;
      d.shouldSwap = false;
      var src = d.fragment;
      if (src.nodeType === 11 && src.childNodes.length === 1 && src.firstChild.nodeType === 1 && src.firstChild.id === t.id) src = src.firstChild;
      var box = document.createElement('div');
      for (var c = src.firstChild; c; c = c.nextSibling) box.appendChild(c.cloneNode(true));
      if (box.innerHTML === t.innerHTML) return;
      var a = document.activeElement, href = a && t.contains(a) && a.closest('a[data-switcher-entry]') ? a.closest('a[data-switcher-entry]').getAttribute('href') : null;
      var x = t.scrollLeft;
      t.replaceChildren.apply(t, Array.prototype.slice.call(box.childNodes));
      if (window.htmx) window.htmx.process(t);
      t.scrollLeft = x;
      if (href !== null) {
        var es = t.querySelectorAll('a[data-switcher-entry]');
        for (var i = 0; i < es.length; i++) if (es[i].getAttribute('href') === href) { es[i].focus({ preventScroll: true }); break; }
      }
    }`

// switcherOOBVersion is a hash of switcherOOBHandler: a window holding an
// older handler (or none, under an older build's "/" flag) rebinds.
func switcherOOBVersion() string {
	sum := sha256.Sum256([]byte(switcherOOBHandler))
	return hex.EncodeToString(sum[:6])
}

// switcherOOBJS binds switcherOOBHandler once per window and version,
// replacing an older version's.
var switcherOOBJS = `if (!window.__musterSwitcherOOB || window.__musterSwitcherOOB.v !== '` + switcherOOBVersion() + `') {
    if (window.__musterSwitcherOOB) document.removeEventListener('htmx:oobBeforeSwap', window.__musterSwitcherOOB.fn);
    window.__musterSwitcherOOB = { v: '` + switcherOOBVersion() + `', fn: ` + switcherOOBHandler + ` };
    document.addEventListener('htmx:oobBeforeSwap', window.__musterSwitcherOOB.fn);
  }`

// switcherJS is the bar's script (switcherScript).
var switcherJS = func() string {
	views, labels, names := "[", "{", "{"
	for i, v := range switcherViews {
		if i > 0 {
			views, labels, names = views+",", labels+",", names+","
		}
		views += strconv.Quote(v.key)
		labels += strconv.Quote(v.key) + ":" + strconv.Quote(v.label)
		names += strconv.Quote(v.key) + ":" + strconv.Quote(switcherToggleLabel(v.key))
	}
	return `
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
      // The menu and the palette (palette.go) are never open together.
      var pal = document.getElementById('` + paletteID + `');
      if (pal) hide(pal);
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

  window.addEventListener('resize', function () { if (isOpen(menu)) placeMenu(); });

  // The out-of-band refresh rows (switcherOOBJS): bound per window AND
  // VERSION, not under a once-per-window flag — a window still running an
  // older build's switcher would have that flag set, and never bind this.
  ` + switcherOOBJS + `

})();
`
}()
