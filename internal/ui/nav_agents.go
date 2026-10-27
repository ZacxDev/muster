package ui

import (
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE SIDEBAR IS A LIST OF AGENTS (operator decision: "like channels in a chat
// app"), on every signed-in page that draws the nav (sidebarIn): the chief
// pinned first, then every session waiting on the operator, then the rest by
// recency (navOrder, list_order.go). Each row is one entry of the Agents list
// — its identity colour on the row's left edge, its recency dot, its host
// icon, its short name (sessionSwitcherLabels, the switcher's labels) and its
// attention count — and opens what its card opens. The page's own session is
// marked.
//
// The pages (Tasks, the Agents list page, Connectors, Asks, Install, the
// theme, Account, Admin) moved into ONE collapsed "More" disclosure at the
// bottom of the sidebar (navMore); the Asks count rides on its header, so it
// shows while More is shut.
//
// 🔴 THE ROWS ARE A LAZY PARTIAL (GET NavAgentsPath), NOT THE FIRST PAINT. The
// sidebar is on every document — a mirrored view, a terminal, the account page
// — and building the list there would put the Agents list's reads in front of
// each of those documents' first byte. The partial is built like the session
// switcher (internal/api's agentsListCardsLive with no live instance read):
// names, recency and attention, never a pod's status.
//
// 🔴 ITS ROOM IS THE SIDEBAR'S, NOT THE CONTENT'S, so its arrival moves nothing
// on the page: the list slot is the flex-1 middle of a fixed full-height panel
// whose header and More are fixed boxes above and below it — rows filling in
// only fill the slot (e2e/tests/cls.spec.ts measures every page at the desktop
// size, where the rail is on screen).
//
// WHEN IT LOADS (navAgentsTrigger): a page with the desktop rail loads it on
// load, then refreshes it once a minute and on the attention queue's SSE event
// (heard only on a document that already holds a stream — the nav opens none,
// TestTheNavAddsNoStream). A page whose sidebar is an overlay at every width
// (the mirrored Rendered view and the terminal, whose open path is measured)
// loads it only when the sidebar is OPENED (appScript's setOpen fires
// NavOpenEvent), so opening one of those pages costs no extra read.

// NavAgentsPath is the sidebar list's partial.
const NavAgentsPath = "/ui/nav/agents"

// NavAgentsListID is the element the partial is swapped into.
const NavAgentsListID = "sidebar-agents-list"

// navAgentsLabelID names the list (the <ul> is labelled by it).
const navAgentsLabelID = "sidebar-agents-label"

// NavOpenEvent is what appScript's setOpen dispatches on document each time the
// sidebar opens.
const NavOpenEvent = "muster:nav-open"

// navAgentsRailTrigger is the list's refresh on a page with the desktop rail;
// the SSE half is attentionBadgeTrigger's, debounced for the same reason.
const navAgentsRailTrigger = "load, sse:attention_queue.changed from:body delay:50ms, every 60s"

// navAgentsOverlayTrigger is the list's load on an overlay page: each open.
const navAgentsOverlayTrigger = NavOpenEvent + " from:document"

// navAgentsTrigger is the list's hx-trigger for a sidebar drawn as an overlay
// (overlay) or with the desktop rail.
func navAgentsTrigger(overlay bool) string {
	if overlay {
		return navAgentsOverlayTrigger
	}
	return navAgentsRailTrigger
}

// navAgentsSlot is the sidebar's list region: its label and the element the
// partial fills. It scrolls inside itself, so thirty agents never push More
// off the panel.
func navAgentsSlot(overlay bool) g.Node {
	return Div(
		ID("sidebar-agents"),
		// A sideways swipe here is still the sidebar's (its edge-swipe close);
		// a vertical one scrolls the list.
		Class("flex min-h-0 flex-1 flex-col overflow-y-auto overscroll-contain px-2 pb-2"),
		P(ID(navAgentsLabelID), Class("px-3 pb-1 pt-3 text-xs font-medium uppercase tracking-wide text-muted"), g.Text("Agents")),
		Div(
			ID(NavAgentsListID),
			hx("hx-get", NavAgentsPath),
			hx("hx-trigger", navAgentsTrigger(overlay)),
			hx("hx-swap", "innerHTML"),
			// Its own target: the shell's body boost and the list's inherited
			// attributes must not send a row's load anywhere else.
			hx("hx-target", "this"),
			P(g.Attr("data-nav-agents-pending", ""), Class("px-3 py-2 text-xs text-muted"), g.Text("Loading…")),
		),
	)
}

// navCurrent is the page the partial was asked for from (htmx's
// HX-Current-URL), as the session it shows: an agent's page or terminal
// (/agents/<name>…) by the agent of that name among list, a mirrored window's
// Rendered view or terminal (/mirror/view, /mirror/terminal) by its address.
// Anything else — or a URL that does not parse — is no session.
func navCurrent(raw string, list []AgentCardView) SwitcherCurrent {
	u, err := url.Parse(raw)
	if err != nil || raw == "" {
		return SwitcherCurrent{}
	}
	switch p := u.Path; {
	case strings.HasPrefix(p, "/agents/"):
		seg, _, _ := strings.Cut(strings.TrimPrefix(p, "/agents/"), "/")
		name, err := url.PathUnescape(seg)
		if err != nil || name == "" {
			return SwitcherCurrent{}
		}
		for _, a := range list {
			if a.Name == name {
				return SwitcherCurrent{AgentID: a.ID}
			}
		}
	case p == "/mirror/view" || p == "/mirror/terminal":
		q := u.Query()
		if q.Get("host") == "" {
			return SwitcherCurrent{}
		}
		return SwitcherCurrent{Mirror: MirrorRef{Host: q.Get("host"), Session: q.Get("session"), Window: q.Get("window")}}
	}
	return SwitcherCurrent{}
}

// NavAgentsList is the partial: the entries in navOrder, the page's own
// session (currentURL, see navCurrent) marked.
func NavAgentsList(list []AgentCardView, mirrors []MirrorCardView, currentURL string, now time.Time) g.Node {
	es := navOrder(list, mirrors)
	if len(es) == 0 {
		return P(g.Attr("data-nav-agents-empty", ""), Class("px-3 py-2 text-xs text-muted"), g.Text("No agents yet"))
	}
	cur := navCurrent(currentURL, list)
	labels := sessionSwitcherLabels(es)
	rows := make([]g.Node, len(es))
	for i, e := range es {
		rows[i] = navAgentRow(e, labels[i], cur.is(e), now)
	}
	return Ul(g.Attr("aria-labelledby", navAgentsLabelID), Class("flex flex-col gap-0.5"), g.Group(rows))
}

// RenderNavAgents writes NavAgentsList.
func RenderNavAgents(w io.Writer, list []AgentCardView, mirrors []MirrorCardView, currentURL string) error {
	return NavAgentsList(list, mirrors, currentURL, time.Now()).Render(w)
}

// navAgentRow is one row: a real-load link (hx-boost="false", as a card's and
// a switcher pill's), 44px tall, the whole row the tap target.
//
// 🔴 THE CURRENT ROW SAYS aria-current="true", NOT "page". The page's own nav
// entry (the Agents link, for a session page — sidebar's currentTab) already
// claims "page", and one document has one current page
// (TestSidebarMarksExactlyOneCurrentPage); the row is the current item of the
// agents set, which is what "true" means. It is drawn as the active row
// (navRowTone) — never the accent highlight, which marks the nav entry.
func navAgentRow(e listEntry, label string, current bool, now time.Time) g.Node {
	var href, kind, color, host, icon string
	var att *CardAttention
	if e.agent != nil {
		a := e.agent
		href, kind, att = a.PageHref(), "agent", a.Attention
		host, icon = podHost, clusterIcon.Key
	} else {
		m := e.mirror
		href, kind, att, color = MirrorCardHref(*m), "mirror", m.Attention, m.Color
		host, icon = m.Host, m.HostIcon
	}
	// The identity colour on the row's left edge: the global --id-bd that
	// identityAttrs' data-identity resolves for the theme in force
	// (web/css/input.css). No colour: a transparent edge of the same width,
	// so a coloured row and a plain one line up.
	edge := "border-l-transparent"
	if _, ok := identityTint(color); ok {
		edge = "border-l-[rgb(var(--id-bd))]"
	}
	return Li(
		Class("min-w-0"),
		A(
			Href(href),
			hx("hx-boost", "false"),
			g.Attr("data-nav-agent", kind),
			g.If(e.chief(), g.Attr("data-nav-chief", "")),
			g.If(e.waiting(), g.Attr("data-nav-wait", "")),
			g.If(current, g.Group([]g.Node{g.Attr("aria-current", "true"), g.Attr("data-nav-current", "")})),
			// A link to a window's Rendered view warms its snapshot, as a
			// switcher pill's does (mirror-snap.js reads a[data-snap-warm]).
			g.If(strings.HasPrefix(href, "/mirror/view?"), g.Attr("data-snap-warm", "")),
			identityAttrs(color),
			g.Attr("title", label),
			Class("press flex min-h-[44px] w-full min-w-0 items-center gap-2 rounded-lg border-l-4 px-3 text-left text-sm font-medium transition "+edge+" "+navRowTone(current)),
			// The dot's box is reserved whether or not a time is known, so the
			// names line up.
			Span(Class("inline-flex w-2 shrink-0 justify-center"), recencyDot(now, e.activeAt())),
			hostMarkDecorative(host, icon),
			Span(Class("min-w-0 flex-1 truncate"), g.Text(label)),
			navAttentionPill(att, e.waiting()),
		),
	)
}

// navRowTone is a row's colour: the current row raised, every other quiet.
func navRowTone(current bool) string {
	if current {
		return "bg-s2 text-fg ring-1 ring-inset ring-line"
	}
	return "text-fg2 hover:bg-s2 hover:text-fg"
}

// navAttentionPill is a row's attention: the card badge's icon and count
// (AttentionKindIcons) — NOT a link, the row is one (an anchor cannot nest
// in it) — or, for an entry waiting with no item to count (an agent at a
// permission prompt), the switcher's waiting dot. Nothing otherwise.
func navAttentionPill(a *CardAttention, waiting bool) g.Node {
	if a != nil && a.Count > 0 {
		ic := AttentionKindIcons[a.Kind]
		n := strconv.Itoa(a.Count)
		return Span(
			g.Attr("data-nav-attention", ""),
			g.Attr("data-kind", a.Kind),
			g.Attr("data-count", n),
			g.Attr("role", "img"),
			g.Attr("aria-label", n+" waiting on you; the most urgent is "+ic.Words),
			Class("inline-flex shrink-0 items-center gap-1 text-xs"),
			Span(g.Attr("aria-hidden", "true"), g.Text(ic.Icon)),
			Span(g.Attr("aria-hidden", "true"), Class("inline-flex min-w-[1.25rem] items-center justify-center rounded-full bg-accent px-1.5 text-[11px] font-semibold text-on-accent"), g.Text(n)),
		)
	}
	if waiting {
		return Span(g.Attr("data-nav-wait-dot", ""), g.Attr("role", "img"), g.Attr("aria-label", "Waiting on you"),
			Class("h-2 w-2 shrink-0 rounded-full bg-accent"))
	}
	return nil
}

// navMoreID is the "More" disclosure; navMoreKey where this device keeps
// whether it is open (localStorage).
const (
	navMoreID  = "sidebar-more"
	navMoreKey = "muster.nav.more"
)

// navMoreJS restores More's open state as the disclosure is parsed (the
// script follows it, so a page that loads with More open never paints it shut
// and then moves the rail) and saves each toggle. Storage that throws leaves
// it shut, and the toggle still works for the page. Inline, and small: it
// must run before the panel is painted, which a static file's load cannot
// promise (TestTheShellCarriesNoInlineScriptOverTheBudget holds its size).
const navMoreJS = `(function(){var d=document.getElementById('` + navMoreID + `');if(!d||d.__musterMore)return;d.__musterMore=1;` +
	`try{if(localStorage.getItem('` + navMoreKey + `')==='1')d.open=true}catch(e){}` +
	`d.addEventListener('toggle',function(){try{localStorage.setItem('` + navMoreKey + `',d.open?'1':'0')}catch(e){}})})();`

// navMore is the sidebar's "More": the pages, collapsed by default, with the
// Asks count on its header (attentionBadgeSlot) so the one actionable count
// shows while it is shut. body is what it holds when open.
func navMore(feat Features, body ...g.Node) g.Node {
	return g.Group{
		Details(
			ID(navMoreID),
			Class("group shrink-0 border-t border-line pb-[env(safe-area-inset-bottom)]"),
			Summary(
				ID(navMoreID+"-summary"),
				Class("press flex min-h-[44px] cursor-pointer list-none items-center gap-3 px-5 text-sm font-medium text-fg2 transition hover:bg-s2 hover:text-fg [&::-webkit-details-marker]:hidden"),
				Span(Class("text-base"), g.Attr("aria-hidden", "true"), g.Text("⋯")),
				Span(g.Text("More")),
				// The Asks count (it was on the Asks link, now inside More).
				g.If(feat.Attention, attentionBadgeSlot()),
				g.Raw(`<svg class="h-4 w-4 shrink-0 text-muted transition-transform group-open:rotate-180`+classIf(!feat.Attention, " ml-auto")+`" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><path fill-rule="evenodd" d="M14.77 12.79a.75.75 0 01-1.06-.02L10 8.832 6.29 12.77a.75.75 0 11-1.08-1.04l4.25-4.5a.75.75 0 011.08 0l4.25 4.5a.75.75 0 01-.02 1.06z" clip-rule="evenodd"/></svg>`),
			),
			Div(
				ID(navMoreID+"-body"),
				Class("flex max-h-[60dvh] flex-col gap-1 overflow-y-auto overscroll-contain p-2"),
				g.Group(body),
			),
		),
		Script(g.Raw(navMoreJS)),
	}
}
