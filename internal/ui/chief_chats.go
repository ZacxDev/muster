package ui

import (
	"io"
	"net/url"
	"strconv"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/textplain"
)

// THE CHIEF'S PRIMARY AND SIDE CHATS (internal/agents chief_chats.go;
// docs/chief.md#primary-and-side-chats), as the chief page and the chief panel
// draw them.
//
// 🔴 A STRIP OF CHIPS UNDER THE HEADER, NOT A SHEET (an operator ask: "a
// simpler ui/ux to manage sidechats, no modal"). Both surfaces draw the same
// strip (chiefChatStrip) over the transcript:
//
//	[★ Primary] [side chats, most recent first…] [Archived (n)] … [+]
//
// The order is agents.SwitcherOrder's (the server's chiefChatViews hands the
// sessions over in it); the renderer only places them. A tap on a chip switches
// to its chat; the chip of the chat on screen is aria-current="true". A side
// chat's chip carries its unread dot. The archived chats are behind the last
// chip, "Archived (n)", which expands INLINE into their chips (no dialog). The
// "+" sits at the strip's end, outside the chips' scroller, so it is on screen
// however many chats there are. Each chip's actions are in its ⋯ menu (a
// popover, floatingMenuScript): Fresh primary (with its confirm) on the
// primary; Make primary and Archive on a side chat; Unarchive on an archived
// one. A member in a team's shared room (ChiefChatBar.SharedRoom) gets the
// chips to switch with, and nothing else.
//
// 🔴 "+" MUST VISIBLY LAND (the operator's "New side chat doesn't work": the
// server opened the chat and the page moved to it, but nothing on screen said
// so). The strip is drawn with the page, so the chat "+" lands on is a chip,
// current, from the first paint; and the composer is focused on arrival
// (the side chat route's answer carries the flag: ComposerFocusParam), on the
// new chat or on the empty one the server reused.
//
// The context chip stays in the header (chiefContextSlot).
//
// 🔴 THE SAME ROUTES ON BOTH SURFACES, ANSWERED THE WAY EACH SURFACE NEEDS:
// on the page an action's answer is HX-Redirect to the chat it leaves you on;
// in the panel it is the panel body re-rendered on that chat (?surface=
// chief-panel), so the operator never leaves the grid.

// ChiefChatBar is the chief chat a page or panel is on: set on a chief's chat
// only. (Named for the bar it used to draw; it now feeds the chip strip, the
// header's context chip and the panel's Continue picker.)
type ChiefChatBar struct {
	AgentID   int64
	AgentName string
	SessionID int64
	// Primary: this chat is the chief's primary (its default session).
	Primary bool
	// Archived: this chat is archived (an old primary, or a side chat put away).
	Archived bool
	// SharedRoom: the viewer is a member on a TEAM's chief, which has ONE
	// shared conversation (internal/api memberOnTeamAgent). The server refuses
	// a member every chat-making and chat-managing route on it (New side chat,
	// Fresh primary, Make primary, Archive, Unarchive: internal/api
	// teamAgentRoutes), so the chief PAGE's strip draws none of them: no "+",
	// no ⋯; the chips switch, and that is all. (The chief PANEL never opens a
	// team's chief: it is the viewer's own, chiefAgent.)
	SharedRoom bool
}

// The chats' data attributes: what both test tiers read.
const (
	// chiefChatsAttr marks the strip; chiefChatKindAttr on it names the chat
	// the view is on.
	chiefChatsAttr       = "data-chief-chats"
	chiefChatKindAttr    = "data-chief-chat-kind"
	chiefChatStripAttr   = "data-chief-chat-strip"
	chiefChipScrollAttr  = "data-chief-chip-scroller"
	chiefChipAttr        = "data-chief-chip"
	chiefChipKindAttr    = "data-chief-chip-kind"
	chiefChatRowAttr     = "data-chief-chat-row"
	chiefChatMenuAttr    = "data-chief-chat-menu"
	chiefMakePrimaryAttr = "data-chief-make-primary"
	// chiefArchivedAttr marks the "Archived (n)" chip (a toggle);
	// chiefArchivedChipAttr each archived chat's chip behind it.
	chiefArchivedAttr     = "data-chief-archived"
	chiefArchivedChipAttr = "data-chief-archived-chip"
	chiefSideChatAttr     = "data-chief-side-chat"
	chiefFreshPrimaryAttr = "data-chief-fresh-primary"
	chiefArchiveAttr      = "data-chief-archive"
	chiefUnarchiveAttr    = "data-chief-unarchive"
	chiefUnreadAttr       = "data-chief-unread"
	chiefContextID        = "chief-context"
	chiefContextChipID    = "chief-context-chip"
	chiefContextPopID     = "chief-context-detail"
)

// The kinds data-chief-chat-kind names, and what each is called.
const (
	ChiefChatKindPrimary  = "primary"
	ChiefChatKindSide     = "side"
	ChiefChatKindArchived = "archived"
)

// ChiefSideChatPath is the side chat route: POST opens a new side chat.
func ChiefSideChatPath(agentID int64) string {
	return agentSessionsPath(agentID) + "/side"
}

// ChiefFreshPrimaryPath is the fresh-primary route.
func ChiefFreshPrimaryPath(agentID int64) string {
	return agentSessionsPath(agentID) + "/fresh-primary"
}

// ChiefArchivePath is the archive (archived true) or unarchive route of chat sid.
func ChiefArchivePath(agentID, sid int64, archived bool) string {
	verb := "/unarchive"
	if archived {
		verb = "/archive"
	}
	return agentSessionsPath(agentID) + "/" + strconv.FormatInt(sid, 10) + verb
}

// ChiefCurrentParam names the chat the operator is ON when a chip's action
// (archive, unarchive, make primary) acts on another one: the answer leaves
// them on it (internal/api stayOnCurrent) instead of moving them.
const ChiefCurrentParam = "current"

// withCurrent is path with the open chat cur as ChiefCurrentParam.
func withCurrent(path string, cur int64) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + ChiefCurrentParam + "=" + strconv.FormatInt(cur, 10)
}

// ChiefContextPath is the context chip's partial for chat sid of agent name.
func ChiefContextPath(name string, sid int64) string {
	return "/ui/agents/" + url.PathEscape(name) + "/chat-context?session=" + strconv.FormatInt(sid, 10)
}

// FreshPrimaryConfirm is the fresh-primary action's confirmation.
const FreshPrimaryConfirm = "Start a fresh primary chat? This one is archived (its history is kept), " +
	"the chief writes a short handover here and starts the new one with it, and every delivery " +
	"goes to the new chat from now on."

// chiefChatKind is the kind of the chat b names.
func chiefChatKind(b ChiefChatBar) (kind, label string) {
	switch {
	case b.Primary:
		return ChiefChatKindPrimary, "Primary chat"
	case b.Archived:
		return ChiefChatKindArchived, "Archived chat"
	}
	return ChiefChatKindSide, "Side chat"
}

// chiefChatPost is how a chat action posts to path on surface: on the page
// hx-swap none (the answer is HX-Redirect, which htmx follows); in the panel the
// panel body re-rendered (?surface=chief-panel), so the grid stays.
func chiefChatPost(surface ChatSurface, path string) g.Node {
	if surface == ChatSurfaceChiefPanel {
		return g.Group{
			hx("hx-post", path),
			hx("hx-target", "#"+chiefPanelBodyID),
			hx("hx-swap", "innerHTML"),
		}
	}
	return g.Group{hx("hx-post", path), hx("hx-swap", "none")}
}

// onSurface is path asking for surface's answer: ?surface=chief-panel in the
// panel, path itself on the page.
func onSurface(surface ChatSurface, path string) string {
	if surface != ChatSurfaceChiefPanel {
		return path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + ChatSurfaceQueryParam + "=" + string(ChatSurfaceChiefPanel)
}

// ChiefNewChatLabel is a chat's chip label until its first line titles it.
const ChiefNewChatLabel = "New chat"

// chiefArchivedChipText is the "Archived (n)" chip's text for n archived chats.
func chiefArchivedChipText(n int) string {
	return chiefArchivedLabel + " (" + strconv.Itoa(n) + ")"
}

// chiefChatStrip is the chief's chats as a strip of chips (see the file's
// header), for surface: name is the chief's agent name, sessions its chats in
// agents.SwitcherOrder (primary, side chats by activity, archived), b the chat
// the view is on. Nothing off a chief's chat.
//
// 🔴 ONE ROW, A FIXED HEIGHT: the chips scroll sideways inside it (never wrap),
// and the row is its final height from its opening tag, so nothing under it
// moves when a chip is added or the archived ones expand.
func chiefChatStrip(b *ChiefChatBar, name string, sessions []SessionView, surface ChatSurface) g.Node {
	if b == nil {
		return nil
	}
	kind, _ := chiefChatKind(*b)
	var chips, archived []g.Node
	for _, sess := range sessions {
		// The chat on screen is never hidden behind "Archived (n)", archived or
		// not (chiefChatViews lists an open archived chat with the side ones).
		if sess.Archived && !sess.Active {
			archived = append(archived, chiefChip(name, b, sess, surface, true))
			continue
		}
		chips = append(chips, chiefChip(name, b, sess, surface, false))
	}
	if len(archived) > 0 {
		chips = append(chips, Li(
			Class("flex shrink-0 items-center"),
			Button(
				Type("button"),
				g.Attr(chiefArchivedAttr, ""),
				g.Attr("aria-expanded", "false"),
				Class("group press inline-flex h-11 shrink-0 items-center gap-1 whitespace-nowrap rounded-full px-3.5 text-xs font-medium text-muted ring-1 ring-inset ring-line transition hover:bg-s2 hover:text-fg aria-expanded:bg-s2 aria-expanded:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"),
				g.Text(chiefArchivedChipText(len(archived))),
				g.Raw(`<svg class="h-3.5 w-3.5 shrink-0 transition group-aria-expanded:rotate-90" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 18l6-6-6-6"/></svg>`),
			),
		))
		chips = append(chips, archived...)
	}
	pad := "px-2 sm:px-4"
	if surface == ChatSurfaceChiefPanel {
		pad = ""
	}
	return Nav(
		g.Attr(chiefChatStripAttr, ""),
		g.Attr(chiefChatsAttr, ""),
		g.Attr(chiefChatKindAttr, kind),
		g.Attr("aria-label", "Chats"),
		Class("flex h-[3.25rem] min-w-0 flex-none items-center gap-1.5 "+pad),
		Ul(
			g.Attr(chiefChipScrollAttr, ""),
			Class("flex h-full min-w-0 flex-1 items-center gap-1.5 overflow-x-auto overflow-y-hidden overscroll-x-contain [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"),
			g.Group(chips),
		),
		g.If(!b.SharedRoom, chiefNewChatChip(b.AgentID, surface)),
		chiefChipsScript(),
		// The ⋯ menus are placed by floatingMenuScript: the chief page carries
		// it already; the shell the panel lives in does not (it is bound once
		// per document, so a second copy is a no-op).
		g.If(surface == ChatSurfaceChiefPanel && !b.SharedRoom, floatingMenuScript()),
	)
}

// chiefChip is one chat's chip: a tap switches to it (a boosted link on the
// page, an hx-get of the panel body in the panel), current when it is the chat
// on screen, with the ★ on the primary and the unread dot — and, unless the
// viewer is in a team's shared room, its ⋯ menu BESIDE the link, never inside
// it: a form in a link is not valid HTML, and a tap meant to open the chat
// must not archive it. hidden: an archived chat's chip, behind "Archived (n)".
func chiefChip(name string, b *ChiefChatBar, sess SessionView, surface ChatSurface, hidden bool) g.Node {
	title := textplain.Title(sess.Title)
	if title == "" {
		title = ChiefNewChatLabel
	}
	kind := ChiefChatKindSide
	switch {
	case sess.IsDefault:
		kind = ChiefChatKindPrimary
	case sess.Archived:
		kind = ChiefChatKindArchived
	}
	label := title
	if kind == ChiefChatKindPrimary {
		label = "Primary"
	}
	id := strconv.FormatInt(sess.ID, 10)
	cls := "press inline-flex h-11 min-w-0 max-w-[11rem] items-center gap-1.5 whitespace-nowrap rounded-full pl-3.5 text-sm transition focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus "
	if b.SharedRoom {
		cls += "pr-3.5 "
	} else {
		cls += "pr-1 "
	}
	if sess.Active {
		cls += "font-semibold text-fg"
	} else {
		cls += "text-fg2 hover:text-fg"
	}
	inner := g.Group{
		g.If(kind == ChiefChatKindPrimary, Span(g.Attr(sessionDefaultAttr, ""), g.Attr("aria-hidden", "true"), Class("shrink-0 text-accent"), g.Text("★"))),
		Span(Class("min-w-0 truncate"), g.Text(label)),
		g.If(sess.Unread, Span(
			g.Attr(chiefUnreadAttr, ""),
			g.Attr("role", "img"),
			g.Attr("aria-label", "Unread reply"),
			Class("h-2 w-2 shrink-0 rounded-full bg-accent"),
		)),
	}
	common := g.Group{
		g.Attr(chiefChatRowAttr, id),
		g.Attr("title", title),
		g.If(sess.Active, g.Attr("aria-current", "true")),
		Class(cls),
	}
	var link g.Node
	if surface == ChatSurfaceChiefPanel {
		// A row of the panel's: an hx-get into the panel body, never a
		// navigation (chiefThreadRows says why).
		link = Button(Type("button"), common,
			hx("hx-get", "/ui/chief/panel?session="+id),
			hx("hx-target", "#"+chiefPanelBodyID),
			hx("hx-swap", "innerHTML"),
			inner)
	} else {
		link = A(Href(AgentSessionHref(name, sess.ID)), common, inner)
	}
	ring := "bg-s1 ring-line hover:bg-s2"
	if sess.Active {
		ring = "bg-accent/15 ring-accent/60"
	}
	return Li(
		g.Attr(chiefChipAttr, id),
		g.Attr(chiefChipKindAttr, kind),
		g.If(hidden, g.Attr(chiefArchivedChipAttr, "")),
		g.If(hidden, g.Attr("hidden", "")),
		Class("flex h-11 shrink-0 items-center rounded-full ring-1 ring-inset transition "+ring),
		link,
		g.If(!b.SharedRoom, chiefChipMenu(b, sess, kind, surface)),
	)
}

// chiefChipMenu is a chip's ⋯ menu: Fresh primary (with its confirm) on the
// primary; Make primary (where the deployment offers it) and Archive on a side
// chat; Unarchive on an archived one. Every action names the chat on screen
// (ChiefCurrentParam), so acting on another chat leaves the operator where they
// are. A popover in the top layer, placed by floatingMenuScript, so the strip's
// scroller never clips it; Escape and a tap outside close it (the browser's).
func chiefChipMenu(b *ChiefChatBar, sess SessionView, kind string, surface ChatSurface) g.Node {
	prefix := "chief-chat-menu-"
	if surface == ChatSurfaceChiefPanel {
		prefix = "chief-panel-chat-menu-"
	}
	id := prefix + strconv.FormatInt(sess.ID, 10)
	action := func(path, attr, label, confirm string) g.Node {
		return Form(
			chiefChatPost(surface, withCurrent(onSurface(surface, path), b.SessionID)),
			g.If(confirm != "", hx("hx-confirm", confirm)),
			Class("contents"),
			Button(Type("submit"), g.Attr(attr, ""), g.Attr("role", "menuitem"),
				Class("press flex min-h-[44px] w-full items-center rounded-lg px-3 text-left text-sm font-medium text-fg transition hover:bg-s2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"),
				g.Text(label)),
		)
	}
	var items []g.Node
	switch kind {
	case ChiefChatKindPrimary:
		items = append(items, action(ChiefFreshPrimaryPath(b.AgentID), chiefFreshPrimaryAttr, "Fresh primary", FreshPrimaryConfirm))
	case ChiefChatKindArchived:
		items = append(items, action(ChiefArchivePath(b.AgentID, sess.ID, false), chiefUnarchiveAttr, "Unarchive", ""))
	default:
		if sess.DefaultPath != "" {
			items = append(items, action(sess.DefaultPath, chiefMakePrimaryAttr, "Make primary", ""))
		}
		items = append(items, action(ChiefArchivePath(b.AgentID, sess.ID, true), chiefArchiveAttr, "Archive", ""))
	}
	return g.Group{
		Button(
			Type("button"),
			g.Attr("popovertarget", id),
			g.Attr("aria-haspopup", "menu"),
			g.Attr("aria-expanded", "false"),
			g.Attr("aria-label", "Chat actions"),
			g.Attr("title", "Chat actions"),
			Class("press inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-full text-fg2 transition hover:bg-s2 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"),
			g.Raw(`<svg class="h-4 w-4" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><circle cx="5" cy="12" r="1.75"/><circle cx="12" cy="12" r="1.75"/><circle cx="19" cy="12" r="1.75"/></svg>`),
		),
		Div(
			ID(id),
			g.Attr(chiefChatMenuAttr, ""),
			g.Attr("popover", "auto"),
			g.Attr("role", "menu"),
			g.Attr("aria-label", "Chat actions"),
			g.Attr("data-floating-menu", ""),
			Class("invisible hidden [&:popover-open]:flex m-0 w-48 flex-col gap-1 rounded-xl border border-line bg-s1 p-1.5 text-fg shadow-2xl ring-1 ring-black/20"),
			g.Group(items),
		),
	}
}

// chiefNewChatChip is the strip's "+": a new side chat (or the newest one,
// reused while it is still empty: internal/api openSideChat). Its answer moves
// the view to that chat, with the cursor in its message box (the route's
// answer asks for it: ComposerFocusParam).
func chiefNewChatChip(agentID int64, surface ChatSurface) g.Node {
	return Form(
		chiefChatPost(surface, onSurface(surface, ChiefSideChatPath(agentID))),
		Class("contents"),
		Button(
			Type("submit"),
			g.Attr(chiefSideChatAttr, ""),
			g.Attr("aria-label", "New side chat"),
			g.Attr("title", "New side chat"),
			Class("press inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-full bg-s1 text-xl leading-none text-fg ring-1 ring-inset ring-line transition hover:bg-s2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"),
			Span(g.Attr("aria-hidden", "true"), g.Text("+")),
		),
	)
}

// ComposerFocusParam is the query parameter (value "1") with which the side
// chat route's page answer (HX-Redirect) asks the page it lands on to put the
// cursor in the message box (AgentDetailView.FocusComposer). The panel's answer
// sets the flag on the body it renders instead. Nothing else sets it, so a
// plain load or reload of ?session=N never takes the focus.
const ComposerFocusParam = "focus"

// chiefChipsScript is the strip's behaviour, bound once per document (htmx
// re-executes it on every swap that brings a strip; the guard leaves only the
// scroll step to run again):
//
//   - THE CURRENT CHIP IS SCROLLED INTO VIEW on arrival (a phone shows two or
//     three chips), inside the strip's own scroller — never the page's.
//   - "Archived (n)" toggles its chats' chips inline (aria-expanded).
func chiefChipsScript() g.Node {
	return Script(g.Raw(`
(function () {
  function reveal() {
    var ss = document.querySelectorAll('[` + chiefChipScrollAttr + `]');
    for (var i = 0; i < ss.length; i++) {
      var sc = ss[i], cur = sc.querySelector('[aria-current="true"]');
      if (!cur) continue;
      var chip = cur.closest('[` + chiefChipAttr + `]') || cur;
      var sr = sc.getBoundingClientRect(), cr = chip.getBoundingClientRect();
      if (cr.left < sr.left) sc.scrollLeft += cr.left - sr.left - 8;
      else if (cr.right > sr.right) sc.scrollLeft += cr.right - sr.right + 8;
    }
  }
  if (window.__musterChiefChips) { reveal(); return; }
  window.__musterChiefChips = true;
  // Only a swap that brought a strip (the panel body) scrolls it again: the
  // page's other swaps (the context chip, the transcript) must not scroll the
  // strip back under the operator's thumb.
  document.addEventListener('htmx:afterSettle', function (e) {
    var t = e.detail && e.detail.target;
    if (t && t.querySelector && t.querySelector('[` + chiefChatStripAttr + `]')) reveal();
  });
  document.addEventListener('click', function (e) {
    var b = e.target && e.target.closest && e.target.closest('button[` + chiefArchivedAttr + `]');
    if (!b) return;
    var strip = b.closest('[` + chiefChatStripAttr + `]');
    if (!strip) return;
    var open = b.getAttribute('aria-expanded') !== 'true';
    b.setAttribute('aria-expanded', open ? 'true' : 'false');
    var cs = strip.querySelectorAll('[` + chiefArchivedChipAttr + `]');
    for (var i = 0; i < cs.length; i++) cs[i].hidden = !open;
    if (open && cs.length) {
      var sc = b.closest('[` + chiefChipScrollAttr + `]');
      var sr = sc.getBoundingClientRect(), last = cs[cs.length - 1].getBoundingClientRect();
      if (last.right > sr.right) sc.scrollLeft += Math.min(last.right - sr.right + 8, b.getBoundingClientRect().left - sr.left);
    }
  });
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', reveal);
  else reveal();
})();
`))
}

// chiefPageStrip is the chief PAGE's strip: a row of its own under the header,
// in the chat column's width. Nothing off a chief's chat.
func chiefPageStrip(v AgentDetailView) g.Node {
	if v.ChiefChat == nil || v.Surface != ChatSurfacePage {
		return nil
	}
	return Div(
		g.Attr("data-chief-strip-row", ""),
		Class("flex-none border-b border-line bg-bg lg:pl-72"),
		Div(Class(contentMaxW()), chiefChatStrip(v.ChiefChat, v.Name, v.Sessions, ChatSurfacePage)),
	)
}

// chiefPanelContinue is the chief PANEL's "Continue in a new session", under
// its thread list (chiefThreadList), for the side chat the panel is on
// (chiefContinuePicker, where the New session picker is offered: runtimes,
// claudeModels). Every other chat action is on the strip's chips.
func chiefPanelContinue(b *ChiefChatBar, runtimes []SessionRuntimeView, claudeModels []string) g.Node {
	if b == nil {
		return nil
	}
	if kind, _ := chiefChatKind(*b); kind != ChiefChatKindSide {
		return nil
	}
	return Div(
		Class("flex flex-none flex-wrap items-center gap-2 border-t border-line pt-2"),
		chiefContinuePicker(b.AgentID, b.SessionID, runtimes, claudeModels),
	)
}

// ChiefContinuePath is the continue route of side chat sid: POST runtime +
// model continues it in a new session (internal/api chief_continue.go).
func ChiefContinuePath(agentID, sid int64) string {
	return agentSessionsPath(agentID) + "/" + strconv.FormatInt(sid, 10) + "/continue"
}

// chiefContinueAttr marks the Continue picker; chiefContinueFormAttr each of
// its forms, valued with the runtime it continues on.
const (
	chiefContinueAttr     = "data-chief-continue"
	chiefContinueFormAttr = "data-chief-continue-form"
)

// ContinueConfirm is the Continue action's confirmation.
const ContinueConfirm = "Continue this chat in a new session? This chat writes a handover for it, the new session " +
	"starts from that handover, and this chat is then archived (Unarchive brings it back)."

// chiefContinuePicker is "Continue in a new session" on a side chat: a
// collapsed group (a native <details>, so it opens without script) holding the
// same runtime + model forms as the New session picker (chiefNewSessionPicker),
// each posting to the continue route. Nothing where no runtime is offered.
//
// PENDING AND FAILED ARE VISIBLE: the form's controls are disabled while its
// request is in flight (hx-disabled-elt); the answer is the panel on the NEW
// chat, drawn "working…" while the old chat writes its handover; a refusal
// (409 while one is already running, 400 for a model the server refuses) is
// toasted by the shell's htmx:responseError handler; and a handover that
// failed says so in the new chat's first turn.
func chiefContinuePicker(agentID, sid int64, runtimes []SessionRuntimeView, claudeModels []string) g.Node {
	// The New session picker's forms (runtimeModelForms), posting to this chat's
	// continue route, with a confirm.
	forms := runtimeModelForms(runtimes, claudeModels, ChiefContinuePath(agentID, sid), chiefContinueFormAttr, "Continue",
		hx("hx-confirm", ContinueConfirm))
	if len(forms) == 0 {
		return nil
	}
	return g.El("details",
		g.Attr(chiefContinueAttr, ""),
		Class("group w-full"),
		g.El("summary",
			Class("press inline-flex min-h-[36px] cursor-pointer list-none items-center gap-1 whitespace-nowrap rounded-lg bg-s1 px-2.5 py-1 text-xs font-medium text-fg2 ring-1 ring-inset ring-line transition hover:bg-s2 hover:text-fg [&::-webkit-details-marker]:hidden"),
			g.Text("Continue in a new session"),
		),
		Div(
			Class("mt-2 flex flex-col gap-2 rounded-lg bg-s1 p-2 ring-1 ring-inset ring-line"),
			P(Class("text-xs text-muted"), g.Text("A new side chat on the model you pick starts from this chat's handover; this chat is then archived.")),
			g.Group(forms),
		),
	)
}

// chiefContextSlot is where the context chip lives — the chief page's header,
// under the title (where a mirrored session's chip is), and the chief panel's
// title bar: an empty slot that loads
// the chip (ChiefContextChip) on load and again whenever a turn of the chief
// starts or ends or a reply is stored — so the chip follows the turn that just
// ended without a reload. innerHTML into the slot: the slot keeps its triggers
// (an outerHTML swap of an element with a `load` trigger would load for ever).
func chiefContextSlot(name string, sid int64) g.Node {
	quoted := strconv.Quote(name)
	return Span(
		ID(chiefContextID),
		Class("inline-flex min-w-0"),
		hx("hx-get", ChiefContextPath(name, sid)),
		hx("hx-trigger", "load, sse:chat.turn[JSON.parse(detail.data).id==="+quoted+"] from:body, "+
			"sse:chat.reply[JSON.parse(detail.data).id==="+quoted+"] from:body"),
		// 🔴 ITS OWN TARGET, SPELLED: inside the chief panel, #chief-panel-body
		// declares hx-target="this", which htmx INHERITS — without this the chip's
		// answer replaced the whole panel body (the chat with it).
		hx("hx-target", "this"),
		hx("hx-swap", "innerHTML"),
	)
}

// RenderChiefContextChip writes the context chip for c — nothing when c is nil
// (no turn of the chat has reported usage yet).
func RenderChiefContextChip(w io.Writer, c *ContextView) error {
	if c == nil {
		return nil
	}
	return chiefContextChip(c).Render(w)
}

// ChiefContextView is the chief chat's context readout: contextView, THE
// readout #601's chip draws for a mirrored session, so the chief's chip says
// the same thing the same way — the model then used/window, amber from
// ContextWarnPct, red from ContextDangerPct, the exact counts on tap.
func ChiefContextView(tokens, window int64, model string) *ContextView {
	return contextView(tokens, window, model, ChiefContextTitle)
}

// ChiefContextTitle is how the chief chip's number is derived.
const ChiefContextTitle = "Context used by this chat's latest turn: its last request's input + cache read + cache write tokens " +
	"(output is not counted: it is in the next request's input), of the model's context window."

// chiefContextChip is the chip itself: mirrorContextChip's markup and classes,
// filled on the server rather than by a page script.
func chiefContextChip(c *ContextView) g.Node {
	if c == nil {
		return nil
	}
	return g.Group{
		Button(
			ID(chiefContextChipID),
			Type("button"),
			g.Attr("title", c.Detail),
			g.Attr("popovertarget", chiefContextPopID),
			g.Attr("aria-haspopup", "dialog"),
			g.Attr("aria-controls", chiefContextPopID),
			g.Attr("aria-expanded", "false"),
			g.If(c.Level != "", g.Attr("data-level", c.Level)),
			Class("press relative inline-flex min-w-0 max-w-[16rem] items-center gap-1 rounded-full bg-s2 px-2 py-0.5 text-[11px] tabular-nums text-fg2 ring-1 ring-inset ring-line focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus "+
				"data-[level=warn]:bg-st-warning-bg data-[level=warn]:text-st-warning-fg data-[level=warn]:ring-st-warning-fg/40 "+
				"data-[level=danger]:bg-st-error-bg data-[level=danger]:text-st-error-fg data-[level=danger]:ring-st-error-fg/40"),
			g.If(c.Name != "", Span(g.Attr("data-ctx-name", ""), Class("min-w-0 truncate"), g.Text(c.Name))),
			g.If(c.Name != "", Span(g.Attr("data-ctx-sep", ""), Class("shrink-0"), g.Text(contextSep))),
			Span(g.Attr("data-ctx-count", ""), Class("shrink-0 whitespace-nowrap"), g.Text(c.Count)),
		),
		Div(
			ID(chiefContextPopID),
			g.Attr("popover", "auto"),
			g.Attr("role", "dialog"),
			g.Attr("aria-label", "Context used"),
			g.Attr("data-floating-menu", ""),
			Class(fullTitlePopoverClass),
			Span(g.Attr("data-ctx-detail", ""), Class("[overflow-wrap:anywhere] text-sm font-semibold tabular-nums"), g.Text(c.Detail)),
			g.If(c.Title != "", Span(g.Attr("data-ctx-how", ""), Class("text-xs text-muted"), g.Text(c.Title))),
		),
	}
}

// chiefChatRowExtras is what a chief chat's switcher row adds after its title:
// the primary badge (sessionDefaultBadge) and the unread dot.
func chiefChatRowExtras(sess SessionView) g.Node {
	return g.Group{
		sessionDefaultBadge(sess),
		g.If(sess.Unread, Span(
			g.Attr(chiefUnreadAttr, ""),
			g.Attr("role", "img"),
			g.Attr("aria-label", "Unread reply"),
			Class("h-2 w-2 shrink-0 rounded-full bg-accent"),
		)),
	}
}

// chiefArchivedLabel is the heading over the archived chats in a list.
const chiefArchivedLabel = "Archived"
