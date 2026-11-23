package ui

import (
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/textplain"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE SESSION VIEWS' OPEN BUTTON AND PANEL (docs/plugins.md#the-session-view-chip;
// docs/previews.md; docs/pages.md#a-sessions-pages): what a session has made
// to look at — a mirrored window's previews (the owner's live viewer plugin,
// the preview gateway) and the pages the session CREATED — in ONE header
// button and ONE panel (operator decision: the header had no room for a second
// button, so pages fold in with the preview button and the two are tabs of
// its panel).
//
//   - THE BUTTON: "Open" (an icon alone below `sm`) and ONE count — the
//     window's offers plus its open previews (mirror.WindowPreviews, which the
//     window's host reports; counted only while the owner has a live viewer
//     plugin to open them with, as before) plus the session's pages plus its
//     OPEN asks (session_asks.go in internal/api) — drawn only while that
//     count is above zero, or while the session has an answered ask to read
//     (drawn bare then, no count). It is in the header row at every
//     width, at a FIXED width with its count laid over its corner, so neither
//     its arrival (the title, which is flex-1, gives it the room; nothing
//     right of it moves) nor a new count moves anything. ONE slot re-reads it
//     (OpenButtonSrc) every previewPollEvery while the tab is shown, so the
//     page holds no event stream.
//   - 🔴 PAGES NEED NO VIEWER PLUGIN. The slot and the panel are drawn while
//     the owner has a live viewer plugin OR the pages feature is on (PagesOn);
//     without a plugin the Previews tab says there is nothing to preview and
//     nothing is minted.
//   - THE PANEL'S TABS: "Previews (n) · Pages (n) · Asks (n)" (tabs.go's
//     tablist, with the shared tab swipe on the panel below it,
//     tab_swipe.go; Asks only while the queue is served, AsksOn). It opens
//     on a tab that has something: the one this viewer used last when it has
//     (tabs.go's stored choice, per viewer: openTabsKey), else the first that
//     has. The Asks tab lists the session's asks, open first, each linking
//     to its own page (SessionAsksList); it answers nothing. The Previews tab
//     is the preview panel as it was; the Pages tab lists the session's pages
//     newest first (SessionPagesList), and a tap on one shows it in the panel
//     — the owner's own view of it (/pages/{id}/view?embed=1, its version
//     chip in its header) — under a line with the way back to the list and
//     "Open full page" (/pages/{id}/view).
//   - THE PANEL opens on the button: the plugin's page (the gateway's session
//     page) in a sandboxed frame. Close (or Escape, when
//     no other dialog or popover is open to take it) unloads the page. "New
//     tab" in its header is the hand-off in a new tab instead — except in the
//     Android app, where that same control reads "Full screen": the same frame fills the
//     screen, never a new tab (the app would hand that to the system browser;
//     see enterFull in the script).
//   - 🔴 THE MESSAGE BAR STAYS USABLE WHILE THE PANEL IS OPEN (operator ask:
//     "chat and send messages while I'm looking at the preview"). The page's
//     ONE composer, #mv-compose, is kept: never a second one
//     (TestEveryMessageBoxIsOneComposer). The panel's layout (previewMode):
//   - "bar", below `sm`: the panel fills the screen down to the TOP of
//     #mv-compose, which stays live under it. Its bottom is measured from
//     the bar, never a constant: the keyboard raises the bar (the Rendered
//     view's pin script, or a page that resizes) and the panel shrinks with
//     it.
//   - "side", from `sm` up: the panel runs the full height at the right, and
//     the conversation AND the bar end at its left edge (the body's right
//     padding, and a pinned bar's `right`), so Send and the replies stay in
//     view beside it.
//   - "full", full screen (the Android app): the bar is hidden; a
//     semi-transparent round button at the bottom right ("Show message bar")
//     shows it, and the panel is then laid out as "bar" above it. The same
//     button hides it again ("Hide message bar"); Exit, Escape and Back
//     leave full screen whatever the bar's state, and full screen always
//     opens with the bar hidden.
//     While the bar is up its z-order is above the panel's (z-41 against
//     z-40), so its popups — the `/` menu, the suggested replies, the
//     attachments, the mic's line, all placed bottom-full of it — open over
//     the panel. What the panel still covers is made inert, so Tab reaches
//     the panel and the bar only. Dialogs, popovers and the toasts' host are
//     never made inert, WHEREVER THEY SIT (the command palette the bar's
//     search opens is a popover inside #sidebar-root; a step-up confirm
//     opened by a Send must answer): an element that holds one is not made
//     inert whole, its other children are. Beside the page ("side") nothing
//     is inert.
//   - THE NEW-REPLY DOT: while the panel COVERS the conversation ("bar" and
//     "full") a reply from the agent (a new assistant bubble in #mv-log, the
//     Rendered view's transcript) puts a dot on the panel's title, and on the
//     show-bar button in full screen, whose header is hidden. It clears when
//     the conversation is in view again: the panel closes, or becomes a side
//     panel. A reconnect's redraw of the log (a reset, then batches) is not
//     a reply: a reply is a bubble the log holds more copies of than were
//     ever seen. Beside the page the replies are in view, so it never shows
//     there. A Terminal view has no transcript to watch, so no dot.
//   - Both mint a one-time hand-off at CLICK time (POST /plugin-handoffs,
//     aimed at this window) and go to the entry URL it answers; drawing the
//     button mints nothing. One mint at a time.
//   - 🔴 New tab opens its tab SYNCHRONOUSLY, inside the tap (window.open of
//     about:blank), and only then mints; the tab is sent to the minted URL
//     once it answers. A popup blocker (Safari's among them, which drops a tab
//     opened after an await) therefore refuses at the tap, before any code is
//     minted, and the panel says so. A refused mint closes the blank tab and
//     says why. NOOPENER IS KEPT BY HAND: window.open with "noopener" returns
//     no handle to navigate, so the tab is opened with one, its opener is set
//     to null while it is still about:blank (same-origin with muster, so
//     muster may), and only then is it navigated — so the plugin's page sees
//     window.opener === null, as rel="noopener" would give. What is not the
//     same: the tab stays in muster's browsing context group (no process
//     isolation from it), which hands the plugin no reference to muster's
//     window. Referrer: the blank document inherits muster's Referrer-Policy
//     no-referrer, so the plugin gets no Referer. The link still carries
//     target=_blank rel="noopener noreferrer" for what its markup says.
//   - 🔴 The panel's frame is sandboxed WITHOUT allow-top-navigation, so the
//     plugin's content cannot navigate muster's tab; allow="" grants it no
//     camera, microphone or location; referrerpolicy="no-referrer".
//     allow-same-origin is safe because the frame's origin is the plugin's,
//     never muster's (a Host under MUSTER_PLUGIN_ORIGINS is refused 421).
//   - 🔴 THE PAGE'S CSP CARRIES frame-src 'self' <the plugins' origins>
//     WHENEVER THE OWNER HAS A LIVE VIEWER PLUGIN, and not only while the
//     count is above zero: the button can appear after the page loaded, and
//     its panel must then be allowed to frame the plugin (api's
//     allowSameOriginFrame). Without one the CSP is the switch's own, whose
//     default-src 'self' frames the Pages tab's page (muster's own origin).
//   - The panel is position:fixed, and the button takes room only inside the
//     header row, which already has its 44px height, so neither moves the
//     conversation: the views with a live preview are measured by cls.spec.ts,
//     and session-open.spec.ts measures the header unchanged when the button
//     appears and when its count changes, at both widths. Every layout change
//     above is keyed on attributes the script sets on <html> only while the
//     panel is open (previewPanelStyle), so a page loads exactly as before;
//     preview-composer.spec.ts measures the open panel at rest.

// PluginChipSandbox is the panel's frame's sandbox, exactly: no
// allow-top-navigation of any kind.
const PluginChipSandbox = "allow-scripts allow-same-origin allow-forms allow-popups allow-downloads allow-modals"

// SessionPageSandbox is the Pages tab's frame's sandbox: the owner's own view
// of a page (muster's shell, which frames the page's content in its own
// sandbox, pages.ContentCSP). Same-origin and scripts, as the shell needs.
// 🔴 NO allow-top-navigation OF ANY KIND, as the preview's frame
// (TestThePanelsNewTabAndFrameAreLockedDown): the shell's own links to
// muster's screens open in a new tab when embedded (pages.ShellView.Embed).
const SessionPageSandbox = "allow-scripts allow-same-origin allow-forms allow-popups allow-popups-to-escape-sandbox allow-downloads"

// PreviewButtonPath is a mirrored window's Open button's refresh: GET with the
// window's host, session and window (and owner= for a team's), answering
// OpenButton for its counts NOW.
const PreviewButtonPath = "/ui/mirror/preview-button"

// SessionPagesPath is a mirrored window's Pages tab: GET, addressed as
// PreviewButtonPath, answering SessionPagesList.
const SessionPagesPath = "/ui/mirror/session-pages"

// SessionAsksPath is a mirrored window's Asks tab: GET, addressed as
// PreviewButtonPath, answering SessionAsksList.
const SessionAsksPath = "/ui/mirror/session-asks"

// AgentOpenButtonPath is an agent page's Open button's refresh, and
// AgentSessionPagesPath its Pages tab, for the agent named name.
func AgentOpenButtonPath(name string) string {
	return "/ui/agents/" + url.PathEscape(name) + "/open-button"
}

// AgentSessionPagesPath is an agent page's Pages tab (see AgentOpenButtonPath).
func AgentSessionPagesPath(name string) string {
	return "/ui/agents/" + url.PathEscape(name) + "/session-pages"
}

// AgentSessionAsksPath is an agent page's Asks tab (see AgentOpenButtonPath).
func AgentSessionAsksPath(name string) string {
	return "/ui/agents/" + url.PathEscape(name) + "/session-asks"
}

// previewPollEvery is how often the slot re-reads the button while the tab is
// shown. The host reports its counts every refresh (5 s) after its own 5 s
// listener scan, so this is the same order of delay.
const previewPollEvery = "10s"

// mirrorQuery is a window's address as PreviewButtonPath and
// SessionPagesPath take it: host, session, window, and owner= for a TEAM
// host's (owner != 0, team sharing 2b.3b).
func mirrorQuery(owner int64, host, session, window string) string {
	q := url.Values{}
	q.Set("host", host)
	q.Set("session", session)
	q.Set("window", window)
	if owner != 0 {
		q.Set("owner", strconv.FormatInt(owner, 10))
	}
	return q.Encode()
}

// PreviewButtonSrc is PreviewButtonPath for one window's button.
func PreviewButtonSrc(host, session, window string) string {
	return PreviewButtonPath + "?" + mirrorQuery(0, host, session, window)
}

// PreviewButtonSrcOf is PreviewButtonSrc for owner's entry: a TEAM host's
// (owner != 0, team sharing 2b.3b) names it as owner=.
func PreviewButtonSrcOf(owner int64, host, session, window string) string {
	return PreviewButtonPath + "?" + mirrorQuery(owner, host, session, window)
}

// PluginChip is one live viewer plugin the panel can open.
type PluginChip struct {
	ID    int64
	Label string
}

// PluginChips is a session view's Open control: the owner's live viewer
// plugins (newest first), the mint route, the window every hand-off is aimed
// at, what that window has to preview now, and the session's pages. nil draws
// nothing.
type PluginChips struct {
	MintPath              string
	Host, Session, Window string
	// Owner is a TEAM host's owner (team sharing 2b.3b: the team's plugins),
	// 0 for the viewer's own: the mint and the button's poll name it.
	Owner int64
	Chips []PluginChip
	// Previews is the window's counts when the page was drawn
	// (mirror.SessionView.Previews): the button's first state.
	Previews mirror.WindowPreviews
	// PagesOn: the pages feature is on, so the session's pages are listed
	// (the slot and the panel are drawn even with no viewer plugin). Pages is
	// how many it has made, when the page was drawn.
	PagesOn bool
	Pages   int
	// AsksOn: the attention queue is served, so the session's asks are
	// listed (its Asks tab; the slot and the panel are drawn even with no
	// viewer plugin and pages off). Asks is how many of them are open, and
	// AsksListed how many the tab lists (open and answered, at most
	// attention.MaxSessionAsks), when the page was drawn.
	AsksOn     bool
	Asks       int
	AsksListed int
	// AsksCoverBadge: the asks this control counts are every ask the page
	// header's asks badge counts (a personal agent's page, the viewer's own:
	// agentAsksBadge hides the badge on a phone then). false on a team
	// agent's page, whose badge also counts the team's asks waiting on the
	// viewer.
	AsksCoverBadge bool
	// AsksPoll: the page has no live source of the queue's changes (no asks
	// badge, no stream — the mirrored Terminal view), so the slot polls for
	// its asks (slotTrigger). Set by the page that draws it.
	AsksPoll bool
	// Agent is a dispatched agent's name on ITS page (no window, no
	// previews): the button's and the list's routes are the agent's.
	Agent string
	// Viewer is the signed-in viewer, whose last-used tab is remembered.
	Viewer int64
}

// on: there is a plugin to open, or pages or asks to list, so the slot and
// the panel are drawn.
func (c *PluginChips) on() bool { return c != nil && (len(c.Chips) > 0 || c.PagesOn || c.AsksOn) }

// slotTrigger is when the slot re-reads the button.
//
// 🔴 ASKS FOLLOW THE PAGE'S ASKS BADGE; ONLY PREVIEWS AND PAGES ARE POLLED.
// A page whose header has the asks badge keeps that badge live on the
// queue's event, with a catch-up (a reconnect, a wake, once a minute), and
// the button re-reads whenever the badge does:
//   - the agent page's badge is an htmx refresh (askerAsksRefresh: the SSE
//     event and every 60 s) into #asker-asks-badge, so its htmx:afterSwap
//     there is the button's cue — the catch-up included, which the bare SSE
//     event is not (a stream that missed an event while asleep sent none);
//   - the mirrored Rendered view's badge is its own stream's
//     (AttentionLiveScript), which dispatches asks:changed on document on
//     every refresh: the event, a reconnect's catch-up, once a minute.
//
// A page with NO such source (c.AsksPoll: the mirrored Terminal view, and its
// copy kept in the view swap's frame, draw no badge and open no stream) polls
// every previewPollEvery instead, as every slot did before. So does every
// page while there are previews or pages to count, which have no event.
// Otherwise nothing is polled: drawing the slot on every session page for its
// asks had added a request every 10 s to each (e2e telemetry.spec: 12
// batches a minute from one open view, against its 6).
func (c *PluginChips) slotTrigger() string {
	t := "htmx:afterSwap from:#" + AskerAsksBadgeID + " delay:50ms, asks:changed from:document delay:50ms"
	if c.previewsOn() || c.PagesOn || c.AsksPoll {
		t += ", every " + previewPollEvery + " [!document.hidden]"
	}
	return t
}

// withAsksPoll is c for a page with no live source of the queue's changes
// (AsksPoll); nil stays nil.
func withAsksPoll(c *PluginChips) *PluginChips {
	if c == nil {
		return nil
	}
	cc := *c
	cc.AsksPoll = true
	return &cc
}

// previewsOn: there is a viewer plugin to open the window's previews with.
func (c *PluginChips) previewsOn() bool { return c != nil && len(c.Chips) > 0 }

// counts is the button's first state.
func (c *PluginChips) counts() OpenCounts {
	o := OpenCounts{Pages: c.Pages, Asks: c.Asks, AsksListed: c.AsksListed}
	if c.previewsOn() {
		o.Previews = c.Previews
	}
	return o
}

// buttonSrc is the slot's refresh route.
func (c *PluginChips) buttonSrc() string {
	if c.Agent != "" {
		return AgentOpenButtonPath(c.Agent)
	}
	return PreviewButtonSrcOf(c.Owner, c.Host, c.Session, c.Window)
}

// pagesSrc is the Pages tab's route.
func (c *PluginChips) pagesSrc() string {
	if c.Agent != "" {
		return AgentSessionPagesPath(c.Agent)
	}
	return SessionPagesPath + "?" + mirrorQuery(c.Owner, c.Host, c.Session, c.Window)
}

// asksSrc is the Asks tab's route.
func (c *PluginChips) asksSrc() string {
	if c.Agent != "" {
		return AgentSessionAsksPath(c.Agent)
	}
	return SessionAsksPath + "?" + mirrorQuery(c.Owner, c.Host, c.Session, c.Window)
}

// OpenCounts is what the Open button counts: the window's previews (zero
// without a live viewer plugin), the session's pages and its OPEN asks.
// AsksListed is how many asks its Asks tab lists (open and answered): an
// answered one is not counted, but a session with one still draws the
// button, so its answers stay one tap away.
type OpenCounts struct {
	Previews   mirror.WindowPreviews
	Pages      int
	Asks       int
	AsksListed int
}

// Total is the button's one count.
func (o OpenCounts) Total() int { return o.Previews.Total() + o.Pages + o.Asks }

// shown reports whether there is anything to open: a count, or an answered
// ask to read.
func (o OpenCounts) shown() bool { return o.Total() > 0 || o.AsksListed > 0 }

// openWords is what a session has to open, as words: "1 preview open,
// 2 servers to preview, 3 pages".
func openWords(o OpenCounts) string {
	var parts []string
	plural := func(n int, one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return strconv.Itoa(n) + " " + many
	}
	p := o.Previews
	if p.Open > 0 {
		parts = append(parts, plural(p.Open, "preview open", "previews open"))
	}
	if p.Offered > 0 {
		parts = append(parts, plural(p.Offered, "server to preview", "servers to preview"))
	}
	if o.Pages > 0 {
		parts = append(parts, plural(o.Pages, "page", "pages"))
	}
	if o.Asks > 0 {
		parts = append(parts, plural(o.Asks, "open ask", "open asks"))
	}
	if n := o.AsksListed - o.Asks; n > 0 {
		parts = append(parts, plural(n, "answered ask", "answered asks"))
	}
	return strings.Join(parts, ", ")
}

// previewCount is a count as drawn: "99+" past it.
func previewCount(n int) string {
	if n > mirror.MaxPreviewCount {
		return strconv.Itoa(mirror.MaxPreviewCount) + "+"
	}
	return strconv.Itoa(n)
}

const previewIcon = `<svg class="h-5 w-5 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M3 9h18M7 6.5h.01M10 6.5h.01"/></svg>`

// OpenButtonLabel is the Open button's word and the start of its name.
const OpenButtonLabel = "Open"

// OpenButton is the Open button for counts o: nil while there is nothing to
// open (shown). It carries no htmx wiring (the slot does), so a swapped-in
// button never re-arms a trigger. Its count is drawn only while it is above
// zero: a session whose asks are all answered draws the button bare.
//
// 🔴 ITS WIDTH IS FIXED (w-11; from `sm` w-[5.5rem] with its word), and its
// count is laid over its top right corner: a count of 1 and one of 99+ are one
// box, so a new count moves nothing in the header.
func OpenButton(o OpenCounts) g.Node {
	if !o.shown() {
		return nil
	}
	n := o.Total()
	words := openWords(o)
	return Button(
		Type("button"),
		ID("preview-toggle"),
		g.Attr("data-preview-toggle", ""),
		g.Attr("data-preview-count", strconv.Itoa(n)),
		g.Attr("data-open-previews", strconv.Itoa(o.Previews.Total())),
		g.Attr("data-open-pages", strconv.Itoa(o.Pages)),
		g.Attr("data-open-asks", strconv.Itoa(o.Asks)),
		g.Attr("data-open-asks-listed", strconv.Itoa(o.AsksListed)),
		g.Attr("aria-controls", "preview-panel"),
		g.Attr("aria-expanded", "false"),
		g.Attr("aria-label", OpenButtonLabel+": "+words),
		g.Attr("title", OpenButtonLabel+": "+words),
		Class("press relative inline-flex h-11 w-11 shrink-0 items-center justify-center gap-1.5 rounded-lg bg-s2 text-xs font-semibold text-fg ring-1 ring-inset ring-line transition hover:bg-s3 aria-expanded:ring-2 aria-expanded:ring-accent sm:w-[5.5rem]"),
		g.Raw(previewIcon),
		Span(Class("max-sm:hidden"), g.Text(OpenButtonLabel)),
		g.If(n > 0, Span(g.Attr("aria-hidden", "true"), g.Attr("data-open-badge", ""),
			Class("pointer-events-none absolute -right-1 -top-1 inline-flex h-[1.125rem] min-w-[1.125rem] items-center justify-center rounded-full bg-accent px-1 text-[11px] font-semibold leading-none tabular-nums text-on-accent ring-2 ring-bg"),
			g.Text(previewCount(n)))),
	)
}

// RenderOpenButton writes OpenButton(o) (nothing at zero).
func RenderOpenButton(w io.Writer, o OpenCounts) error {
	n := OpenButton(o)
	if n == nil {
		return nil
	}
	return n.Render(w)
}

// previewButtonSlot is the header's slot: the button as the page was drawn,
// re-read on the queue's event (slotTrigger) and, while there are previews or
// pages to count, every previewPollEvery while the tab is shown. Hidden while
// empty (empty:hidden: an empty slot takes no room and adds no gap to its
// row).
func previewButtonSlot(c *PluginChips) g.Node {
	if !c.on() {
		return nil
	}
	return Span(
		ID("preview-slot"),
		Class("inline-flex shrink-0 empty:hidden"),
		hx("hx-get", c.buttonSrc()),
		hx("hx-trigger", c.slotTrigger()),
		hx("hx-swap", "innerHTML"),
		hx("hx-target", "this"),
		OpenButton(c.counts()),
	)
}

// PreviewFullAttr marks the panel while it is full screen (the Android app
// only: its header's one hand-off control reads Full screen there).
const PreviewFullAttr = "data-preview-full"

// The <html> attributes the panel's script sets while it is open, one per
// layout (see the file comment): previewBarAttr below `sm` and in full screen
// with the bar shown, previewNoBarAttr in full screen with it hidden,
// previewSideAttr beside the page.
const (
	previewBarAttr   = "data-preview-bar"
	previewNoBarAttr = "data-preview-nobar"
	previewSideAttr  = "data-preview-side"
)

// previewSideWidth is the side panel's width: the panel's own class
// (sm:w-[…], spelled out for Tailwind) and the room previewPanelStyle takes
// from the page beside it are this one value (TestThePreviewSidePanelTakesItsOwnWidth).
const previewSideWidth = "min(40rem,60vw)"

// previewPanelStyle lays the page out around the open panel, keyed on the
// attributes above (nothing applies while it is closed):
//   - the bar above the panel (z-41 over its z-40), with its popups;
//   - the bar hidden in full screen (visibility: its room is kept, so nothing
//     moves when it comes back);
//   - beside the side panel, the page's column ends at the panel's left edge.
//     A pinned bar (position:fixed) is not in that column: the script gives it
//     the same room as its `right`.
var previewPanelStyle = `html[` + previewBarAttr + `] #mv-compose{z-index:41}` +
	`html[` + previewNoBarAttr + `] #mv-compose{visibility:hidden}` +
	`html[` + previewSideAttr + `] body{padding-right:` + previewSideWidth + `}`

const exitFullIcon = `<svg class="h-4 w-4 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 4v5H4M15 4v5h5M9 20v-5H4M15 20v-5h5"/></svg>`

// chatIcon is the show-bar button's face (a speech bubble); hideBarIcon its
// face while the bar is shown (a chevron down).
const chatIcon = `<svg class="h-5 w-5 shrink-0 group-aria-expanded/bar:hidden" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 12a8 8 0 0 1-11.6 7.1L4 20l1-4.6A8 8 0 1 1 21 12z"/></svg>`
const hideBarIcon = `<svg class="hidden h-5 w-5 shrink-0 group-aria-expanded/bar:block" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 9l6 6 6-6"/></svg>`

// PreviewBarToggleLabel is the full screen's floating button's name while the
// message bar is hidden; previewBarHideLabel while it is shown.
const (
	PreviewBarToggleLabel = "Show message bar"
	previewBarHideLabel   = "Hide message bar"
)

// The panel's tabs (tabs.go): the tablist's id, its tabs' ids, and the key
// the viewer's last choice is stored under (per viewer: openTabsKey).
const (
	openTabsID      = "open-tabs"
	OpenTabPreviews = "previews"
	OpenTabPages    = "pages"
	OpenTabAsks     = "asks"
)

// openTabsKey is the tablist's storage key for viewer (tabs.go stores it as
// "muster:tabs:<key>"): one choice per signed-in viewer on this browser.
func openTabsKey(viewer int64) string {
	if viewer <= 0 {
		return "session-open"
	}
	return "session-open:" + strconv.FormatInt(viewer, 10)
}

// openTabCount is a tab's count, "(n)", which the script keeps current.
func openTabCount(tab string, n int) g.Node {
	return Span(g.Attr("data-open-count", tab), Class("ml-1 tabular-nums"), g.Text("("+previewCount(n)+")"))
}

// NothingToPreview is the Previews tab with no viewer plugin to open a
// preview with, and SessionPagesNone the Pages tab of a session that made none.
const (
	NothingToPreview = "Nothing to preview in this session."
	SessionPagesNone = "This session has made no pages."
	SessionAsksNone  = "This session has asked you nothing."
)

// previewPanel is the panel the button opens (closed), and its script.
func previewPanel(c *PluginChips) g.Node {
	if !c.on() {
		return nil
	}
	previews := c.previewsOn()
	// More than one live viewer plugin: the panel's header picks which one
	// it shows (newest first, the first chosen).
	var picker g.Node
	if previews && len(c.Chips) > 1 {
		opts := make([]g.Node, 0, len(c.Chips))
		for _, p := range c.Chips {
			opts = append(opts, Option(Value(strconv.FormatInt(p.ID, 10)), g.Text(textplain.Title(p.Label))))
		}
		picker = Select(g.Attr("data-preview-plugin", ""), g.Attr("aria-label", "Plugin"),
			Class("min-h-[44px] max-w-[9rem] shrink-0 truncate rounded-lg bg-s2 px-2 text-xs text-fg ring-1 ring-inset ring-line group-data-[open-tab=pages]:hidden"),
			g.Group(opts))
	}
	btn := "press inline-flex min-h-[44px] shrink-0 items-center rounded-lg px-3 text-sm font-semibold hover:bg-s2"
	var previewsBody g.Node
	if previews {
		previewsBody = g.Group{
			P(ID("preview-error"), g.Attr("role", "status"), g.Attr("hidden", ""),
				Class("shrink-0 border-b border-line bg-danger/15 px-3 py-2 text-xs text-fg break-words")),
			IFrame(
				g.Attr("data-preview-frame", ""),
				g.Attr("sandbox", PluginChipSandbox),
				g.Attr("allow", ""),
				g.Attr("referrerpolicy", "no-referrer"),
				g.Attr("title", "Preview"),
				Class("min-h-0 w-full flex-1 border-0 bg-bg"),
			),
		}
	} else {
		previewsBody = P(g.Attr("data-open-empty", OpenTabPreviews), Class("px-4 py-6 text-sm text-fg2"), g.Text(NothingToPreview))
	}
	pagesBody := Div(
		Class("flex min-h-0 flex-1 flex-col"),
		// The list (SessionPagesList), read each time the tab is shown.
		Div(g.Attr("data-session-pages-list", ""), g.Attr("aria-live", "polite"), Class("min-h-0 flex-1 overflow-y-auto overscroll-contain"),
			P(Class("px-4 py-6 text-sm text-fg2"), g.Text("Loading…"))),
		// One page, in the owner's own view.
		Div(g.Attr("data-page-open", ""), g.Attr("hidden", ""), Class("min-h-0 flex-1 flex-col [&:not([hidden])]:flex"),
			Div(Class("flex h-11 shrink-0 items-center gap-1 border-b border-line pl-1 pr-1"),
				Button(Type("button"), g.Attr("data-page-back", ""), g.Attr("aria-label", "Back to this session's pages"),
					Class(btn+" gap-1 px-2 text-fg2"), g.Raw(`<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M15 18l-6-6 6-6"/></svg>`), g.Text("Pages")),
				Span(g.Attr("data-page-title", ""), Class("min-w-0 flex-1 truncate text-sm font-semibold")),
				Span(g.Attr("data-page-version", ""), Class(sessionPageChipClass)),
				A(g.Attr("data-page-full", ""), Href("#"), Target("_top"), Class(btn+" text-accent"), g.Text("Open full page")),
			),
			// 🔴 A PLACEHOLDER, NOT A FRAME, UNTIL A PAGE IS OPENED: the script
			// makes the frame from it (loadInto copies its attributes, the
			// sandbox first, onto a new <iframe>) and parks it back here when
			// the page is left. An idle about:blank frame was drawn on every
			// session page once the Open panel listed asks, and a same-origin
			// frame is a document of its own — it runs a page's per-document
			// scripts and its pagehide (e2e: update.spec's load count,
			// switcher-dock.spec's bfcache record and dictation.spec's frames
			// each read it as the page's own).
			Div(
				g.Attr("data-page-frame", ""),
				g.Attr("sandbox", SessionPageSandbox),
				g.Attr("title", "Page"),
				Class("min-h-0 w-full flex-1 border-0 bg-bg"),
			),
		),
	)
	list := []Tab{
		{ID: OpenTabPreviews, Label: "Previews", Count: openTabCount(OpenTabPreviews, c.counts().Previews.Total()), Panel: previewsBody},
		{ID: OpenTabPages, Label: "Pages", Count: openTabCount(OpenTabPages, c.Pages), Panel: pagesBody},
	}
	if c.AsksOn {
		// The session's asks (SessionAsksList), read each time the tab is
		// shown; its count is the OPEN ones, as the button's.
		list = append(list, Tab{ID: OpenTabAsks, Label: "Asks", Count: openTabCount(OpenTabAsks, c.Asks),
			Panel: Div(g.Attr("data-session-asks-list", ""), g.Attr("aria-live", "polite"), Class("min-h-0 flex-1 overflow-y-auto overscroll-contain"),
				P(Class("px-4 py-6 text-sm text-fg2"), g.Text("Loading…")))})
	}
	tabs := TabsSelected(openTabsID, openTabsKey(c.Viewer), "Previews, pages and asks", OpenTabPreviews, list...)
	var first PluginChip
	if previews {
		first = c.Chips[0]
	}
	return g.Group{
		Aside(
			ID("preview-panel"),
			g.Attr("hidden", ""),
			g.Attr("aria-labelledby", "preview-panel-title"),
			g.If(previews, g.Group{
				g.Attr("data-mint", c.MintPath),
				g.Attr("data-host", c.Host),
				g.Attr("data-window", c.Window),
				g.If(c.Owner != 0, g.Attr("data-owner", strconv.FormatInt(c.Owner, 10))),
				g.Attr("data-plugin", strconv.FormatInt(first.ID, 10)),
			}),
			g.Attr("data-pages-src", c.pagesSrc()),
			g.If(c.AsksOn, g.Attr("data-asks-src", c.asksSrc())),
			g.Attr("data-open-tab", OpenTabPreviews),
			// The screen above the message bar below `sm` (the script sets
			// its bottom to the bar's top); a full-height side panel from
			// `sm` up, the page narrowed beside it (z-40: above the page,
			// below the bar while the bar is over it, and the dialogs). Full
			// screen (PreviewFullAttr, the Android app only) it is the whole
			// screen at every width, its header hidden — down to the bar's
			// top while the bar is shown.
			// 🔴 sm:w-[…] IS previewSideWidth, spelled out for Tailwind.
			Class("group fixed inset-0 z-40 flex-col bg-s1 text-fg shadow-2xl ring-1 ring-line [&:not([hidden])]:flex sm:left-auto sm:w-[min(40rem,60vw)] sm:border-l sm:border-line "+
				"data-[preview-full]:ring-0 data-[preview-full]:sm:left-0 data-[preview-full]:sm:w-full data-[preview-full]:sm:border-l-0"),
			// The header is a swipe area too (tab_swipe.go): a frame's touches
			// are its own document's, so over a preview or a page the tabs are
			// swiped here.
			Div(g.Attr(TabSwipeAttr, openTabsID), Class("flex h-12 shrink-0 items-center gap-1 border-b border-line pl-1 pr-1 group-data-[preview-full]:hidden"),
				H2(ID("preview-panel-title"), Class("sr-only"), g.Text("Previews, pages and asks")),
				// The tabs (tabs.go), and the new-reply dot (see the file comment).
				Div(Class("flex min-w-0 flex-1 items-center gap-1 overflow-hidden"),
					tabs.List,
					Span(g.Attr("data-preview-dot", "header"), g.Attr("title", "New reply"),
						Class("hidden h-2.5 w-2.5 shrink-0 rounded-full bg-accent group-data-[preview-unread]:inline-block"),
						Span(Class("sr-only"), g.Text("New reply"))),
				),
				picker,
				// ONE hand-off control: New tab in a browser; in the Android
				// app the script relabels it Full screen when the panel opens,
				// and its tap keeps the preview in this frame. The Previews
				// tab's only.
				g.If(previews, A(
					g.Attr("data-preview-newtab", ""),
					Href("#"),
					Target("_blank"),
					Rel("noopener noreferrer"),
					g.Attr("aria-label", "Open the preview in a new tab"),
					Class(btn+" text-accent group-data-[open-tab=pages]:hidden"),
					g.Text("New tab"),
				)),
				Button(Type("button"), g.Attr("data-preview-close", ""), g.Attr("aria-label", "Close the panel"),
					Class(btn+" text-fg2"), g.Text("Close")),
			),
			// Full screen's way out, over the preview's top right corner:
			// drawn only while full screen.
			Button(Type("button"), g.Attr("data-preview-exit-full", ""), g.Attr("aria-label", "Exit full screen"),
				Class("press absolute right-2 top-[max(0.5rem,env(safe-area-inset-top))] z-10 hidden min-h-[44px] items-center gap-1.5 rounded-full bg-s1/90 px-3 text-sm font-semibold text-fg shadow-lg ring-1 ring-line group-data-[preview-full]:inline-flex"),
				g.Raw(exitFullIcon), Span(g.Text("Exit"))),
			// Full screen's message-bar button, over the preview's bottom
			// right corner (inside the safe areas, a 44px target), drawn only
			// while full screen: it shows the bar (the panel then ends at the
			// bar's top, the button above it) and hides it again. Faint
			// until pressed or focused, so the preview under it still reads.
			// The script hides it on a page with no message bar.
			Button(Type("button"), g.Attr("data-preview-bar-toggle", ""), g.Attr("aria-label", PreviewBarToggleLabel),
				g.Attr("aria-expanded", "false"), g.Attr("title", PreviewBarToggleLabel),
				Class("press group/bar absolute bottom-[max(0.75rem,env(safe-area-inset-bottom))] right-[max(0.75rem,env(safe-area-inset-right))] z-10 hidden h-11 w-11 items-center justify-center rounded-full bg-s1/80 text-fg opacity-60 shadow-lg ring-1 ring-line transition-opacity hover:opacity-100 focus-visible:opacity-100 group-data-[preview-full]:inline-flex"),
				g.Raw(chatIcon), g.Raw(hideBarIcon),
				Span(g.Attr("data-preview-dot", "full"), g.Attr("aria-hidden", "true"),
					Class("absolute right-0.5 top-0.5 hidden h-3 w-3 rounded-full bg-accent ring-2 ring-s1 group-data-[preview-unread]:block"))),
			// The tabs' panels, in the shared tab swipe's area (tab_swipe.go).
			Div(g.Attr(TabSwipeAttr, openTabsID), Class("flex min-h-0 flex-1 flex-col"), tabs.Panels),
		),
		StyleEl(g.Raw(previewPanelStyle)),
		tabsScript(),
		tabSwipeScript(),
		staticScript(previewPanelScriptSrc),
	}
}

// sessionPageChipClass is a page's version chip in the Pages tab.
const sessionPageChipClass = "inline-flex h-6 shrink-0 items-center rounded-full bg-s2 px-2 text-xs font-semibold tabular-nums text-fg2 ring-1 ring-inset ring-line"

// SessionPage is one page in a session's Pages tab.
type SessionPage struct {
	ID      int64
	Title   string
	Version int
	// Shared: it has a live link (someone without an account can open it).
	Shared bool
	// Made is when it was made, in words ("today", ui.PlainWhen).
	Made string
}

// SessionPageViewPath is a page's full owner's view; SessionPageEmbedPath it
// framed in the panel.
func SessionPageViewPath(id int64) string  { return PageViewPath(id) }
func SessionPageEmbedPath(id int64) string { return PageViewPath(id) + "?embed=1" }

// SessionPagesList is the Pages tab's list: the session's pages as given
// (newest first), each a button that shows it in the panel, with its current
// version and whether it is shared. A page's title is an agent's words:
// plained (textplain.Title) and escaped.
func SessionPagesList(ps []SessionPage) g.Node {
	if len(ps) == 0 {
		return P(g.Attr("data-open-empty", OpenTabPages), Class("px-4 py-6 text-sm text-fg2"), g.Text(SessionPagesNone))
	}
	items := make([]g.Node, 0, len(ps))
	for _, p := range ps {
		title := textplain.Title(p.Title)
		v := "v" + strconv.Itoa(p.Version)
		share, shareCls := "Not shared", "text-muted"
		if p.Shared {
			share, shareCls = "Shared · live link", "text-st-complete-fg"
		}
		items = append(items, Li(Button(
			Type("button"),
			g.Attr("data-session-page", strconv.FormatInt(p.ID, 10)),
			g.Attr("data-page-embed", SessionPageEmbedPath(p.ID)),
			g.Attr("data-page-view", SessionPageViewPath(p.ID)),
			g.Attr("data-page-title", title),
			g.Attr("data-page-version", v),
			g.Attr("aria-label", title+", version "+strconv.Itoa(p.Version)+", "+strings.ToLower(share)),
			Class("press flex min-h-[44px] w-full flex-col gap-1 px-4 py-2.5 text-left transition hover:bg-s2"),
			Span(Class("flex min-w-0 w-full items-center gap-2"),
				Span(Class("min-w-0 flex-1 truncate text-sm font-semibold text-fg"), g.Text(title)),
				Span(Class(sessionPageChipClass), g.Text(v)),
			),
			Span(Class("flex min-w-0 items-center gap-2 text-xs"),
				Span(g.Attr("data-page-share", strconv.FormatBool(p.Shared)), Class(shareCls), g.Text(share)),
				g.If(p.Made != "", Span(Class("truncate text-muted"), g.Text("made "+p.Made))),
			),
		)))
	}
	return Ul(g.Attr("data-session-pages", ""), Class("divide-y divide-line"), g.Group(items))
}

// SessionAsk is one ask in a session's Asks tab.
type SessionAsk struct {
	ID       int64
	Question string
	// Open: it waits on an answer. Answer is an answered one's answer.
	Open   bool
	Answer string
	// When is when it was asked, in words (ui.PlainWhen).
	When string
	// Page is the ask's own page (/attention/asks/{id}), "" where its reader
	// may not open one (api askHasPage): it is then listed unlinked.
	Page string
}

// SessionAsksList is the Asks tab's list: the session's asks as given (open
// first, newest first in each), each a link to its own page. An open one says
// it waits on an answer; an answered one shows its answer. The question is an
// agent's words, plained (textplain.Title); the answer the owner's, as typed;
// both escaped.
//
// 🔴 A FULL NAVIGATION (hx-boost="false"): the ask page is a standalone
// document, never a body swap of the agent page's shell (AskerAsksBadgeHTML's
// rule).
//
// more, when set, is the Asks page filtered to this session: the session has
// more asks than the tab lists (attention.MaxSessionAsks), and a last line
// links there.
func SessionAsksList(as []SessionAsk, more string) g.Node {
	if len(as) == 0 {
		return P(g.Attr("data-open-empty", OpenTabAsks), Class("px-4 py-6 text-sm text-fg2"), g.Text(SessionAsksNone))
	}
	items := make([]g.Node, 0, len(as))
	for _, a := range as {
		q := textplain.Title(a.Question)
		state, stateCls := "Answered", "text-muted"
		if a.Open {
			state, stateCls = "Waiting on your answer", "text-accent"
		}
		body := g.Group{
			Span(Class("flex min-w-0 w-full items-start gap-2"),
				Span(g.Attr("data-session-ask-question", ""), Class("line-clamp-2 min-w-0 flex-1 break-words text-sm font-semibold text-fg"), g.Text(q)),
				g.If(a.When != "", Span(Class("shrink-0 text-xs text-muted"), g.Text(a.When))),
			),
			Span(g.Attr("data-session-ask-state", strconv.FormatBool(a.Open)), Class("text-xs font-medium "+stateCls), g.Text(state)),
			// The answer is the owner's own words, drawn as they typed them
			// (escaped), not plained as a title: "<b>" in an answer is text.
			g.If(!a.Open && a.Answer != "", Span(g.Attr("data-session-ask-answer", ""), Class("line-clamp-3 break-words text-sm text-fg2"), g.Text(a.Answer))),
		}
		cls := "flex min-h-[44px] w-full flex-col gap-1 px-4 py-2.5 text-left"
		id := strconv.FormatInt(a.ID, 10)
		if a.Page == "" {
			items = append(items, Li(Div(g.Attr("data-session-ask", id), Class(cls), body)))
			continue
		}
		label := q + ", " + strings.ToLower(state)
		items = append(items, Li(A(Href(a.Page), hx("hx-boost", "false"), g.Attr("data-session-ask", id), g.Attr("aria-label", label),
			Class("press "+cls+" transition hover:bg-s2"), body)))
	}
	if more != "" {
		items = append(items, Li(A(Href(more), hx("hx-boost", "false"), g.Attr("data-session-asks-more", ""),
			Class("press flex min-h-[44px] w-full items-center px-4 py-2.5 text-sm font-semibold text-accent transition hover:bg-s2"),
			g.Text(SessionAsksMore))))
	}
	return Ul(g.Attr("data-session-asks", ""), Class("divide-y divide-line"), g.Group(items))
}

// SessionAsksMore is the Asks tab's last line when it lists only the newest
// attention.MaxSessionAsks of a session's asks.
const SessionAsksMore = "More on the Asks page"

// previewPanelScriptSrc opens and closes the panel, mints at click time and
// sends the frame or the new tab on (see the file comment for the tab). A
// refused mint (a revoked token, the window gone, the rate cap), an expired
// sign-in and a blocked tab each say why in the panel and go nowhere.
var previewPanelScriptSrc = staticJS("preview-panel", `
(function () {
  if (window.__musterPreviewPanel) return;
  window.__musterPreviewPanel = true;
  var SIGN_IN = 'Your sign-in has expired: reload the page and sign in again.';
  var BLOCKED = 'The browser blocked the new tab. Allow pop-ups for this site, or use the preview here.';
  var FULL = '`+PreviewFullAttr+`';
  var busy = false;
  function panel() { return document.getElementById('preview-panel'); }
  // The button on screen: the header's, or the second row's below sm.
  function toggles() { return document.querySelectorAll('[data-preview-toggle]'); }
  function toggle() {
    var ts = toggles();
    for (var i = 0; i < ts.length; i++) if (ts[i].getClientRects().length) return ts[i];
    return null;
  }
  function frame() { return panel().querySelector('[data-preview-frame]'); }
  function say(msg) {
    var e = document.getElementById('preview-error');
    if (!e) return;
    e.textContent = msg || '';
    e.hidden = !msg;
  }
  function expanded() {
    var ts = toggles(), p = panel(), on = p && !p.hidden ? 'true' : 'false';
    for (var i = 0; i < ts.length; i++) ts[i].setAttribute('aria-expanded', on);
  }
  function plugin() {
    var p = panel(), s = p.querySelector('[data-preview-plugin]');
    return Number(s ? s.value : p.getAttribute('data-plugin'));
  }
  function mint() {
    var p = panel();
    var body = { plugin: plugin(), target: { mirror: { host: p.getAttribute('data-host'), window: p.getAttribute('data-window') } } };
    if (p.getAttribute('data-owner')) body.owner = Number(p.getAttribute('data-owner'));
    return fetch(p.getAttribute('data-mint'), {
      method: 'POST', credentials: 'same-origin', cache: 'no-store', redirect: 'manual',
      headers: { 'Content-Type': 'application/json', 'Accept': 'application/json' },
      body: JSON.stringify(body)
    }).then(function (res) {
      // An expired session is sent to the sign-in page: a redirect, or an
      // HTML page that is not this route's JSON.
      var json = (res.headers.get('Content-Type') || '').indexOf('application/json') === 0;
      if (res.type === 'opaqueredirect' || res.status === 401 || !json) throw new Error(SIGN_IN);
      return res.json().catch(function () { return {}; }).then(function (b) {
        if (!res.ok || !b || typeof b.url !== 'string') throw new Error((b && b.error) || ('the hand-off was refused (' + res.status + ')'));
        return b.url;
      });
    });
  }
  // 🔴 THE FRAME ADDS NOTHING TO THE TAB'S HISTORY. Assigning src to the
  // live frame is a navigation of it, and a frame's navigations are entries
  // in the tab's joint session history: Back (the Android shell's goBack
  // among them) then walked the frame back to an already-spent hand-off URL,
  // a white, broken frame. So each load is a NEW frame, with the old one's
  // attributes (the sandbox first of all) and its src set BEFORE it is
  // inserted, put in the old one's place: a new frame's first load is no
  // entry. ⚠ Only the panel's own loads: the previewed page's own
  // navigations in the frame (an app's pushState, a link it follows) still
  // add entries, which outlive the frame (docs/plugins.md).
  function load(url) { loadInto(frame(), url); }
  // parkFrame puts a placeholder (a <div> with the frame's attributes) in
  // place of a frame: its document ends, and no idle frame is left behind.
  function parkFrame(old) {
    if (!old || old.tagName !== 'IFRAME') return;
    var d = document.createElement('div');
    for (var i = 0; i < old.attributes.length; i++) {
      var a = old.attributes[i];
      if (a.name !== 'src') d.setAttribute(a.name, a.value);
    }
    old.replaceWith(d);
  }
  function loadInto(old, url) {
    if (!old) return;
    var f = document.createElement('iframe');
    for (var i = 0; i < old.attributes.length; i++) {
      var a = old.attributes[i];
      if (a.name !== 'src') f.setAttribute(a.name, a.value);
    }
    f.src = url;
    old.replaceWith(f);
  }
  // previewsOn: the owner has a viewer plugin to open previews with (the
  // panel then carries the mint's data); without one the Previews tab says
  // there is nothing to preview, and nothing is minted.
  function previewsOn() { var p = panel(); return !!(p && p.getAttribute('data-mint')); }
  // minted: the Previews tab has loaded its hand-off since the panel opened
  // (so switching back to it does not mint again).
  var minted = false;
  function show() {
    if (busy || !previewsOn()) return;
    busy = true;
    minted = true;
    say('');
    mint().then(function (url) {
      if (!panel().hidden && tabNow() === 'previews') load(url);
    }).catch(function (e) {
      say(e && e.message ? e.message : 'the hand-off failed');
    }).then(function () { busy = false; });
  }
  // 🔴 THE LAYOUT AROUND THE OPEN PANEL (the file comment): previewMode is
  // '' (closed), 'bar' (the panel above the live message bar: below sm, or
  // full screen with the bar shown), 'full' (full screen, the bar hidden) or
  // 'side' (beside the page, sm and up). shield() applies it — every time
  // anything it reads may have moved; it is idempotent, so a call that
  // changes nothing writes nothing.
  var barUp = false;
  function compose() { return document.getElementById('mv-compose'); }
  function previewMode() {
    var p = panel();
    if (!p || p.hidden) return '';
    if (p.hasAttribute(FULL)) return barUp && compose() ? 'bar' : 'full';
    return window.innerWidth < 640 ? (compose() ? 'bar' : 'full') : 'side';
  }
  function flag(el, name, on) {
    if (on === el.hasAttribute(name)) return;
    if (on) el.setAttribute(name, ''); else el.removeAttribute(name);
  }
  function setStyle(el, prop, v) { if (el.style[prop] !== v) el.style[prop] = v; }
  // What the panel covers is made inert, so Tab cannot reach a control it
  // hides; the bar, while it is up, is not, and neither is a dialog or a
  // popover (each is in the top layer, over the panel). Only what this set
  // inert is restored.
  var inerted = [], raised = false;
  // KEEP: what the panel never makes inert — the top layer's dialogs and
  // popovers (closed, they take no focus anyway) and the toasts' host.
  var KEEP = 'dialog,[popover],#action-toasts';
  function shield() {
    var p = panel();
    if (!p) return;
    var m = previewMode(), c = compose(), root = document.documentElement;
    var bar = m === 'bar', covers = bar || m === 'full';
    flag(root, '`+previewBarAttr+`', bar);
    flag(root, '`+previewNoBarAttr+`', m === 'full' && !!c);
    flag(root, '`+previewSideAttr+`', m === 'side');
    if (c) {
      // The bar over the panel needs a stacking position: a pinned bar has
      // one (fixed); an in-flow one (a Terminal view's) is given relative.
      if (bar && !raised && getComputedStyle(c).position === 'static') { c.style.position = 'relative'; raised = true; }
      else if (!bar && raised) { c.style.position = ''; raised = false; }
      // A pinned bar is not in the page's column: beside the side panel it
      // ends where the panel starts, as the column does.
      var pinned = !raised && getComputedStyle(c).position === 'fixed';
      setStyle(c, 'right', m === 'side' && pinned ? p.getBoundingClientRect().width + 'px' : '');
    }
    // 🔴 THE PANEL ENDS AT THE BAR'S TOP, MEASURED: the keyboard moves the
    // bar (and the bar's own rows change its height), so no constant would
    // hold. A fixed element's bottom is measured from the layout viewport's.
    setStyle(p, 'bottom', bar ? Math.max(0, document.documentElement.clientHeight - c.getBoundingClientRect().top) + 'px' : '');
    var t = p.querySelector('[data-preview-bar-toggle]');
    if (t) {
      setStyle(t, 'display', c ? '' : 'none');
      var up = barUp && !!c;
      if (t.getAttribute('aria-expanded') !== String(up)) {
        t.setAttribute('aria-expanded', String(up));
        var name = up ? '`+previewBarHideLabel+`' : '`+PreviewBarToggleLabel+`';
        t.setAttribute('aria-label', name);
        t.setAttribute('title', name);
      }
    }
    var want = [];
    // 🔴 AN OVERLAY IS KEPT WHEREVER IT SITS, not only as a child of the body:
    // the command palette (a popover) and the toasts' host are inside
    // #sidebar-root, whose own panel and backdrop the panel covers. A child
    // that holds a kept element (or the panel, or the live bar) is not made
    // inert whole: its children are, each by the same rule, so everything
    // covered stays out of Tab's reach and only the overlays stay live.
    function cover(el) {
      var kids = el.children;
      for (var i = 0; i < kids.length; i++) {
        var k = kids[i];
        if (k === p || k.tagName === 'SCRIPT' || k.tagName === 'STYLE' || k.matches(KEEP) || (bar && k === c)) continue;
        if (k.inert && inerted.indexOf(k) < 0) continue;
        if (k.contains(p) || (bar && k.contains(c)) || k.querySelector(KEEP)) { cover(k); continue; }
        want.push(k);
      }
    }
    if (covers) cover(document.body);
    inerted.forEach(function (k) { if (want.indexOf(k) < 0) k.inert = false; });
    want.forEach(function (k) { if (!k.inert) k.inert = true; });
    inerted = want;
    // The conversation is in view again: the dot has said its piece.
    if (!covers) flag(p, 'data-preview-unread', false);
  }
  // relayout runs shield after the frame's other layout work: two frames on,
  // so the pinned bar's own measure (composerPinScript, one frame on from
  // the same viewport event) has placed it first.
  var queued = false, watching = false;
  function relayout() {
    if (queued) return;
    queued = true;
    requestAnimationFrame(function () { requestAnimationFrame(function () { queued = false; shield(); }); });
  }
  // watch starts following what moves the bar, once (at the first open):
  // the window, the keyboard's viewport (the top window's inside the view
  // switch's frame, as the pinned bar measures it) and the bar's own size.
  function watch() {
    watchViewport();
    if (watching) return;
    watching = true;
    window.addEventListener('resize', relayout);
    // Back from the back/forward cache with the panel open: the keyboard's
    // viewport followed again (a frame's pagehide let go of it) and measured.
    window.addEventListener('pageshow', function () { var p = panel(); if (p && !p.hidden) watchViewport(); relayout(); });
    var c = compose();
    if (c && typeof ResizeObserver !== 'undefined') new ResizeObserver(relayout).observe(c);
  }
  // watchViewport follows the keyboard's viewport. Inside the view switch's
  // frame that is the top window's, let go of on the frame's pagehide (a
  // frame that goes away leaves nothing listening there) — and taken again by
  // the next watch, so a frame restored from the back/forward cache follows it.
  var vvWatching = false;
  function watchViewport() {
    if (vvWatching) return;
    var vw = window;
    try { if (window.frameElement && window.frameElement.hasAttribute('`+ViewFrameAttr+`')) vw = window.parent; } catch (e) {}
    if (!vw.visualViewport) return;
    vvWatching = true;
    vw.visualViewport.addEventListener('resize', relayout);
    vw.visualViewport.addEventListener('scroll', relayout);
    if (vw !== window) window.addEventListener('pagehide', function off() {
      window.removeEventListener('pagehide', off);
      vw.visualViewport.removeEventListener('resize', relayout);
      vw.visualViewport.removeEventListener('scroll', relayout);
      vvWatching = false;
    });
  }
  // THE NEW-REPLY DOT: while the panel covers the conversation, a reply in
  // it sets the dot (shield clears it once the conversation is in view). A
  // reply is an assistant bubble the log holds MORE copies of than were ever
  // seen at once, counted by its text. 🔴 A RESET IS NOT A REPLY: a
  // reconnect clears the log and redraws it in batches of up to 64 events,
  // each in its own task, so between batches the log holds a part of what it
  // held — never more copies of any reply than were seen — and the whole of
  // it once redrawn. Only a reply that was not there before adds one.
  var logObs = null, seen = Object.create(null);
  function replies() {
    var log = document.getElementById('mv-log'), out = Object.create(null);
    var bs = log ? log.querySelectorAll('[data-bubble="assistant"]') : [];
    for (var i = 0; i < bs.length; i++) { var t = bs[i].textContent; out[t] = (out[t] || 0) + 1; }
    return out;
  }
  function watchReplies(on) {
    if (logObs) { logObs.disconnect(); logObs = null; }
    var log = document.getElementById('mv-log');
    if (!on || !log || typeof MutationObserver === 'undefined') return;
    seen = replies();
    logObs = new MutationObserver(function () {
      var now = replies(), m = previewMode(), fresh = false;
      for (var t in now) if (now[t] > (seen[t] || 0)) { seen[t] = now[t]; fresh = true; }
      if (fresh && (m === 'bar' || m === 'full')) flag(panel(), 'data-preview-unread', true);
    });
    logObs.observe(log, { childList: true, subtree: true });
  }
  // showBar is full screen's message-bar button: the bar shown (the panel
  // above it) or hidden again.
  function showBar(on) {
    barUp = !!on;
    shield();
  }

  // 🔴 INSIDE THE ANDROID APP NEW TAB IS FULL SCREEN (operator decision): the
  // app's WebView sends every origin but muster's to the system browser, so a
  // new tab of the plugin's page left the app. There the same control reads
  // Full screen, and its tap makes the SAME frame, never reloaded (its
  // partitioned cookie and state carry over), fill the screen at every width,
  // the header hidden, with an Exit button over its corner. Escape, Exit, Close and the app's
  // Back each leave it (Close also closes the panel).
  //
  // THE APP is the bridge object the app's WebView injects (AppBridgeName),
  // read in the MAIN frame: the panel can be inside the view-swap frame, and
  // the app refuses a bridge message from a subframe. In a browser there is
  // no such object, and New tab is exactly what it was.
  //
  // 🔴 BACK BELONGS TO THE PANEL WHILE IT IS OPEN (operator report: Back in
  // the panel "goes to a white screen and then breaks"). The shell offers its
  // Back press to the page while the page holds a back-claim
  // (docs/design/switcher-dock.md "The shell change"), asking the main
  // frame's window.musterBack; with no claim it is the WebView's goBack. So
  // the panel claims Back from open to close, full screen or not, through
  // the page's one claim function (backClaimJS, shared with the switcher
  // dock, which can open over the side panel), and the main frame's
  // musterBack is wrapped (once): full screen, a press leaves it (the claim
  // stays: the panel is still open); else the dock's close, if the dock was
  // open; else it closes the panel. In a browser there is no bridge, and
  // nothing is posted.
  function shellWin() {
    var w = window;
    try { while (w !== w.top && w.parent && w.parent.document) w = w.parent; } catch (e) {}
    return w;
  }
  function bridge() {
    try { var b = shellWin().`+AppBridgeName+`; return b && typeof b.postMessage === 'function' ? b : null; } catch (e) { return null; }
  }
  function inApp() { return !!bridge(); }
  `+backClaimJS+`
  // This document's panel's name among the claim's holders (a view-swap
  // frame has its own panel, and its own name).
  var WHO = 'preview-' + Math.random().toString(36).slice(2);
  var SLOT = '__musterPreview:' + WHO;
  var holding = false;
  // Each holding panel is registered on the main frame's window under its own
  // name (SLOT), beside the holder list: both views can have a panel open
  // at once (the rendered page's and the view-swap frame's, from sm up).
  function hold(on) {
    if (holding === on) return;
    holding = on;
    var S = shellWin();
    try { S[SLOT] = on ? { win: window, exitFull: exitFull, close: close, onScreen: onScreen } : null; } catch (e) {}
    musterClaimBack(WHO, on);
  }
  // onScreen: this panel is open and in the view on screen — not under the
  // view-swap frame (the page under it is made inert) and not in that frame
  // while it is hidden (visibility:hidden, inert).
  function onScreen() {
    var p = panel();
    if (!p || p.hidden) return false;
    try {
      var el = p, w = window;
      for (;;) {
        if (el.closest('[inert]') || w.getComputedStyle(el).visibility === 'hidden') return false;
        if (w === w.top || !w.frameElement) break;
        el = w.frameElement;
        w = w.parent;
      }
    } catch (e) { return false; }
    return true;
  }
  // Wrapped when the panel opens (the dock has defined its own by then), and
  // again if anything has replaced the wrapper since. It acts on the panel ON
  // SCREEN: the most recently opened holder whose panel is in the view shown
  // (one wrapper serves the main frame's panel and a view-swap frame's). A
  // panel in the hidden view is never closed by Back.
  function hookBack() {
    var S = shellWin();
    if (S.musterBack && S.musterBack === S.__musterPreviewBack) return;
    var prev = S.musterBack;
    S.musterBack = S.__musterPreviewBack = function () {
      var held = String(S.__musterBackHolders || '').split(' ').filter(Boolean), v = null;
      for (var i = held.length - 1; i >= 0 && !v; i--) {
        var o = S['__musterPreview:' + held[i]];
        if (!o) continue;
        var live = false;
        try { live = !o.win.closed; } catch (e) {}
        // A panel whose document is gone (a removed frame) holds nothing.
        if (!live) { S['__musterPreview:' + held[i]] = null; musterClaimBack(held[i], false); continue; }
        try { if (o.onScreen()) v = o; } catch (e) {}
      }
      if (v) { try { if (v.exitFull()) return true; } catch (e) {} }
      // The dock is modal over the panel when both are open: it goes first.
      if (typeof prev === 'function' && prev() === true) return true;
      if (v) { try { if (v.close()) return true; } catch (e) {} }
      return false;
    };
  }
  function isFull() { var p = panel(); return !!(p && p.hasAttribute(FULL)); }
  function enterFull() {
    var p = panel();
    if (!p || p.hidden || isFull()) return;
    p.setAttribute(FULL, '');
    // Full screen opens with the message bar hidden (its button shows it).
    barUp = false;
    shield();
    hookBack();
    var x = p.querySelector('[data-preview-exit-full]');
    if (x) x.focus({ preventScroll: true });
  }
  // exitFull leaves full screen; true when it was. Focus goes back to Full
  // screen unless the panel is closing (close() moves it to the button).
  function exitFull(closing) {
    var p = panel();
    if (!p || !p.hasAttribute(FULL)) return false;
    p.removeAttribute(FULL);
    barUp = false;
    shield();
    if (closing !== true) {
      var f = p.querySelector('[data-preview-newtab]');
      if (f) f.focus({ preventScroll: true });
    }
    return true;
  }
  // htmx's history snapshot is the body's HTML: never take the panel in it
  // open (it would come back framing a spent hand-off), nor its claim on.
  document.addEventListener('htmx:beforeHistorySave', function () { close(); hold(false); });
  window.addEventListener('pagehide', function () { if (isFull()) exitFull(true); hold(false); });
  // Back from the back/forward cache with the panel still open: claim again.
  window.addEventListener('pageshow', function (e) {
    var p = panel();
    if (e.persisted && p && !p.hidden) { hookBack(); hold(true); }
  });

  // label is the hand-off control's words for where the panel runs: New tab
  // in a browser (as drawn), Full screen in the app.
  function label(p) {
    var a = p.querySelector('[data-preview-newtab]');
    if (!a || !inApp()) return;
    a.textContent = 'Full screen';
    a.setAttribute('aria-label', 'Show the preview full screen');
  }
  function open() {
    var p = panel();
    if (!p.hidden) return;
    label(p);
    p.hidden = false;
    watch();
    watchReplies(true);
    shield();
    expanded();
    var c = p.querySelector('[data-preview-close]');
    if (c) c.focus();
    hookBack();
    hold(true);
    minted = false;
    chooseTab();
    onTab();
  }
  // close closes the panel; true when it was open.
  function close() {
    var p = panel();
    if (!p || p.hidden) return false;
    exitFull(true);
    p.hidden = true;
    watchReplies(false);
    shield();
    // The plugin's page ends with the panel: nothing of it runs on hidden.
    // A new blank frame, never a navigation of this one (see load). So does
    // the Pages tab's page.
    load('about:blank');
    minted = false;
    pageBack(true);
    hold(false);
    say('');
    expanded();
    var t = toggle();
    if (t) t.focus();
    return true;
  }
  function newTab() {
    if (busy) return;
    say('');
    // Inside the tap, before any await (see the file comment).
    var tab = window.open('about:blank', '_blank');
    if (!tab) { say(BLOCKED); return; }
    try { tab.opener = null; } catch (e) {}
    busy = true;
    mint().then(function (url) {
      if (tab.closed) { say(BLOCKED); return; }
      tab.location.replace(url);
    }).catch(function (e) {
      try { tab.close(); } catch (x) {}
      say(e && e.message ? e.message : 'the hand-off failed');
    }).then(function () { busy = false; });
  }
  document.addEventListener('click', function (ev) {
    var t = ev.target;
    if (!t || !t.closest || !panel()) return;
    if (t.closest('[data-preview-toggle]')) { ev.preventDefault(); if (panel().hidden) open(); else close(); return; }
    if (t.closest('[data-preview-close]')) { ev.preventDefault(); close(); return; }
    // The one hand-off control: in the app a new tab would leave it, so
    // there it is full screen, never window.open.
    if (t.closest('[data-preview-newtab]')) { ev.preventDefault(); if (inApp()) enterFull(); else newTab(); return; }
    if (t.closest('[data-preview-exit-full]')) { ev.preventDefault(); exitFull(); return; }
    if (t.closest('[data-preview-bar-toggle]')) { ev.preventDefault(); showBar(!barUp); }
  });
  document.addEventListener('change', function (ev) {
    var t = ev.target;
    if (t && t.matches && t.matches('[data-preview-plugin]') && !panel().hidden) show();
  });
  // Escape closes the panel only when it is the panel's: not one another
  // handler already took, and not while another dialog or popover is open
  // (Escape is that one's to close).
  function otherOpen() {
    if (document.querySelector('dialog[open]')) return true;
    try { return !!document.querySelector(':popover-open'); } catch (e) { return false; }
  }
  document.addEventListener('keydown', function (ev) {
    if (ev.key !== 'Escape' || ev.defaultPrevented || !panel() || panel().hidden || otherOpen()) return;
    ev.preventDefault();
    // Full screen, the first Escape leaves it; the next closes the panel.
    if (!exitFull()) close();
  });
  // The slot's refresh swaps the button in fresh: keep it saying whether the
  // panel is open.
  document.addEventListener('htmx:afterSwap', function (ev) {
    if (ev.target && ev.target.id === 'preview-slot') { expanded(); recount(); }
  });

  // 🔴 THE TABS (Previews | Pages | Asks): tabs.go's tablist, its choice
  // stored per viewer. The panel opens on a tab that HAS something: the
  // stored one when it has (tabs.go restored it on load, and a tap or a swipe
  // stores it again), else the first that has, in the tabs' order; when none
  // has, on Previews. Asks "has something" while the session has an ask to
  // list (open or answered). Choosing on open stores nothing: only the
  // viewer's own choice is remembered.
  var TABS = ['previews', 'pages', 'asks'];
  function tabsList() { return document.getElementById('`+openTabsID+`'); }
  function tabEl(name) { var l = tabsList(); return l ? l.querySelector('[data-tab="' + name + '"]') : null; }
  function tabNow() {
    var l = tabsList(), t = l && l.querySelector('[role="tab"][aria-selected="true"]');
    return t ? t.getAttribute('data-tab') : 'previews';
  }
  function storedTab() {
    var l = tabsList(), k = l && l.getAttribute('data-tabs-key');
    try { return k ? window.localStorage.getItem('muster:tabs:' + k) : null; } catch (e) { return null; }
  }
  // countsNow is what the button on screen counts (the slot's last answer),
  // else what the tabs were drawn with.
  // asksListed is how many asks the Asks tab lists (open and answered).
  function countsNow() {
    var t = toggle();
    if (t) return { previews: Number(t.getAttribute('data-open-previews')) || 0, pages: Number(t.getAttribute('data-open-pages')) || 0,
      asks: Number(t.getAttribute('data-open-asks')) || 0, asksListed: Number(t.getAttribute('data-open-asks-listed')) || 0 };
    var out = { previews: 0, pages: 0, asks: 0 };
    TABS.forEach(function (n) {
      var c = panel() && panel().querySelector('[data-open-count="' + n + '"]');
      out[n] = c ? Number(String(c.textContent).replace(/[^0-9]/g, '')) || 0 : 0;
    });
    out.asksListed = out.asks;
    return out;
  }
  function recount() {
    var n = countsNow(), p = panel();
    if (!p) return;
    TABS.forEach(function (k) {
      var c = p.querySelector('[data-open-count="' + k + '"]');
      var v = '(' + (n[k] > `+strconv.Itoa(mirror.MaxPreviewCount)+` ? '`+strconv.Itoa(mirror.MaxPreviewCount)+`+' : n[k]) + ')';
      if (c && c.textContent !== v) c.textContent = v;
    });
  }
  function chooseTab() {
    recount();
    var n = countsNow(), has = function (k) { return tabEl(k) && (k === 'asks' ? n.asksListed : n[k]) > 0; };
    var stored = storedTab(), want = 'previews';
    if (stored && TABS.indexOf(stored) >= 0 && has(stored)) want = stored;
    else for (var i = 0; i < TABS.length; i++) if (has(TABS[i])) { want = TABS[i]; break; }
    var t = tabEl(want), l = tabsList();
    if (t && l && window.muTabs && t.getAttribute('aria-selected') !== 'true') window.muTabs.select(l, t, {});
  }
  // onTab follows the tab shown: the panel's header shows the Previews
  // tab's controls only on it (data-open-tab); the Previews tab mints once
  // per opening, the Pages and Asks tabs read their lists each time they are
  // shown.
  function onTab() {
    var p = panel();
    if (!p || p.hidden) return;
    var t = tabNow();
    p.setAttribute('data-open-tab', t);
    if (t === 'previews') { if (!minted) show(); }
    else if (t === 'asks') loadAsks();
    else loadPages();
  }
  document.addEventListener('click', function (ev) {
    if (ev.target && ev.target.closest && ev.target.closest('#`+openTabsID+` [role="tab"]')) setTimeout(onTab, 0);
  });
  document.addEventListener('keydown', function (ev) {
    if (ev.target && ev.target.closest && ev.target.closest('#`+openTabsID+` [role="tab"]')) setTimeout(onTab, 0);
  });
  // THE PAGES TAB: the session's pages (the list route, read fresh), and one
  // of them shown in the panel — the owner's own view, framed.
  var pagesSeq = 0;
  function pagesList() { return panel().querySelector('[data-session-pages-list]'); }
  function pageView() { return panel().querySelector('[data-page-open]'); }
  function loadPages() {
    var p = panel(), list = pagesList();
    if (!list || (pageView() && !pageView().hidden)) return;
    var seq = ++pagesSeq;
    fetch(p.getAttribute('data-pages-src'), { credentials: 'same-origin', cache: 'no-store', headers: { 'Accept': 'text/html' } })
      .then(function (res) { if (!res.ok) throw new Error(String(res.status)); return res.text(); })
      .then(function (html) { if (seq === pagesSeq) list.innerHTML = html; })
      .catch(function () { if (seq === pagesSeq) list.textContent = 'Could not read this session\'s pages. Close the panel and open it again.'; });
  }
  // THE ASKS TAB: the session's asks (the list route, read fresh), each a
  // link to its own page.
  var asksSeq = 0;
  function loadAsks() {
    var p = panel(), list = p.querySelector('[data-session-asks-list]');
    if (!list || !p.getAttribute('data-asks-src')) return;
    var seq = ++asksSeq;
    fetch(p.getAttribute('data-asks-src'), { credentials: 'same-origin', cache: 'no-store', headers: { 'Accept': 'text/html' } })
      .then(function (res) { if (!res.ok) throw new Error(String(res.status)); return res.text(); })
      .then(function (html) { if (seq === asksSeq) list.innerHTML = html; })
      .catch(function () { if (seq === asksSeq) list.textContent = 'Could not read this session\'s asks. Close the panel and open it again.'; });
  }
  function openPage(b) {
    var v = pageView(), list = pagesList();
    if (!v || !list) return;
    v.querySelector('[data-page-title]').textContent = b.getAttribute('data-page-title') || '';
    v.querySelector('[data-page-version]').textContent = b.getAttribute('data-page-version') || '';
    v.querySelector('[data-page-full]').setAttribute('href', b.getAttribute('data-page-view') || '#');
    list.hidden = true;
    v.hidden = false;
    loadInto(v.querySelector('[data-page-frame]'), b.getAttribute('data-page-embed'));
    var back = v.querySelector('[data-page-back]');
    if (back) back.focus({ preventScroll: true });
  }
  // pageBack leaves the page for the list (quiet: no focus move, the panel
  // is closing) and unloads it.
  function pageBack(quiet) {
    var p = panel(), v = p && pageView(), list = p && pagesList();
    if (!v || v.hidden) return;
    v.hidden = true;
    if (list) list.hidden = false;
    parkFrame(v.querySelector('[data-page-frame]'));
    if (quiet !== true) { loadPages(); var f = list && list.querySelector('[data-session-page]'); if (f) f.focus({ preventScroll: true }); }
  }
  document.addEventListener('click', function (ev) {
    var t = ev.target, b;
    if (!t || !t.closest || !panel()) return;
    if ((b = t.closest('[data-session-page]'))) { ev.preventDefault(); openPage(b); return; }
    if (t.closest('[data-page-back]')) { ev.preventDefault(); pageBack(); }
  });
})();
`)
