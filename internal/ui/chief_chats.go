package ui

import (
	"io"
	"net/url"
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/textplain"
)

// THE CHIEF'S PRIMARY AND SIDE CHATS (internal/agents chief_chats.go;
// docs/chief.md#primary-and-side-chats), as the chief page and the chief panel
// draw them.
//
// 🔴 ONE PLACE TO MANAGE THEM, NOT A BAR OF BUTTONS (an operator ask: move the
// side chat and primary chat management into one consolidated view instead of
// three buttons). The chief PAGE has no chat bar over its transcript. Its header
// carries the same single list-icon control a mirrored session's does
// (chatIndexButton), opening the same sheet with a Chats tab first
// (chiefChatsPanel): every chat — the primary first, then the side chats
// (agents.SwitcherOrder decides the order; the renderers only place it), the
// archived ones in a collapsed group — each row switching to its chat on a tap,
// with the Primary badge, the unread dot and a ⋯ menu (Archive / Unarchive,
// Make primary); under the list "+ New side chat" and, on the primary, "Fresh
// primary". The context chip is in the header, under the title
// (chiefContextSlot), where a mirrored session's is.
//
// The chief PANEL (the slide-out over the grid) keeps its own thread list
// (chiefThreadList) as its sheet, with the same actions under the list
// (chiefPanelChatActions) and the context chip in its title bar.
//
// 🔴 THE SAME ROUTES ON BOTH SURFACES, ANSWERED THE WAY EACH SURFACE NEEDS:
// on the page an action's answer is HX-Redirect to the chat it leaves you on;
// in the panel it is the panel body re-rendered on that chat (?surface=
// chief-panel), so the operator never leaves the grid.

// ChiefChatBar is the chief chat a page or panel is on: set on a chief's chat
// only. (Named for the bar it used to draw; it now feeds the Chats tab, the
// header's context chip and the panel's chat actions.)
type ChiefChatBar struct {
	AgentID   int64
	AgentName string
	SessionID int64
	// Primary: this chat is the chief's primary (its default session).
	Primary bool
	// Archived: this chat is archived (an old primary, or a side chat put away).
	Archived bool
}

// The chats' data attributes: what both test tiers read.
const (
	// chiefChatsAttr marks the Chats tab's content (the page) and the panel's
	// chat actions; chiefChatKindAttr on it names the chat the view is on.
	chiefChatsAttr        = "data-chief-chats"
	chiefChatKindAttr     = "data-chief-chat-kind"
	chiefChatRowAttr      = "data-chief-chat-row"
	chiefChatCurrentAttr  = "data-chief-chat-current"
	chiefChatMenuAttr     = "data-chief-chat-menu"
	chiefMakePrimaryAttr  = "data-chief-make-primary"
	chiefArchivedAttr     = "data-chief-archived"
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

// chiefChatAction is one chat action: a labelled button in a form posting to
// path, answered per surface (see the file's header).
func chiefChatAction(surface ChatSurface, path, attr, label, confirm string, primary bool, lead ...g.Node) g.Node {
	cls := "press inline-flex min-h-[36px] shrink-0 items-center gap-1 whitespace-nowrap rounded-lg px-2.5 py-1 text-xs font-medium ring-1 ring-inset transition focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus "
	if primary {
		cls += "bg-accent/15 text-fg ring-accent/50 hover:bg-accent/25"
	} else {
		cls += "bg-s1 text-fg2 ring-line hover:bg-s2 hover:text-fg"
	}
	var answer g.Node = g.Group{hx("hx-post", path), hx("hx-swap", "none")}
	if surface == ChatSurfaceChiefPanel {
		answer = g.Group{
			hx("hx-post", path+"?"+ChatSurfaceQueryParam+"="+string(ChatSurfaceChiefPanel)),
			hx("hx-target", "#"+chiefPanelBodyID),
			hx("hx-swap", "innerHTML"),
		}
	}
	return Form(
		answer,
		g.If(confirm != "", hx("hx-confirm", confirm)),
		Class("contents"),
		Button(Type("submit"), g.Attr(attr, ""), Class(cls), g.Group(lead), g.Text(label)),
	)
}

// ChiefSideChatButton is the labelled "New side chat" control on a chief's
// chat, for surface. 🔴 VISIBLE TEXT, NOT AN ICON: the icon-only ＋ it replaced
// on the chief page carried its name only in aria-label, and the operator could
// not find it.
func ChiefSideChatButton(agentID int64, surface ChatSurface) g.Node {
	// The ＋ is drawn, not named: the button's name is "New side chat".
	return chiefChatAction(surface, ChiefSideChatPath(agentID), chiefSideChatAttr, "New side chat", "", true,
		Span(g.Attr("aria-hidden", "true"), g.Text("+")))
}

// chiefFreshPrimaryButton is "Fresh primary", with its confirmation: offered on
// the primary only (a fresh primary replaces the primary).
func chiefFreshPrimaryButton(agentID int64, surface ChatSurface) g.Node {
	return chiefChatAction(surface, ChiefFreshPrimaryPath(agentID), chiefFreshPrimaryAttr, "Fresh primary", FreshPrimaryConfirm, false)
}

// chiefChatsPanel is the chief page's Chats tab (chatIndexSheet's first tab):
// the chats, then the actions that make one. Nothing for a chat that is not a
// chief's.
//
// 🔴 THE LIST SCROLLS AND THE ACTIONS DO NOT: the actions are the content's last
// row, outside the list's scroller, so "+ New side chat" is on screen however
// many chats there are.
func chiefChatsPanel(v AgentDetailView) g.Node {
	b := v.ChiefChat
	if b == nil {
		return nil
	}
	kind, _ := chiefChatKind(*b)
	var live, archived []g.Node
	for _, sess := range v.Sessions {
		row := chiefChatRow(v.Name, b.AgentID, sess)
		// The chat on screen is never hidden in the collapsed group, archived
		// or not (chiefChatViews lists an open archived chat with the side ones).
		if sess.Archived && !sess.Active {
			archived = append(archived, row)
			continue
		}
		live = append(live, row)
	}
	return Div(
		g.Attr(chiefChatsAttr, ""),
		g.Attr(chiefChatKindAttr, kind),
		Class("flex min-h-0 flex-1 flex-col"),
		Div(
			Class("flex min-h-0 flex-1 flex-col gap-2 overflow-y-auto overscroll-contain p-3"),
			Ul(g.Attr("aria-label", "Chats"), Class("flex flex-col gap-2"), g.Group(live)),
			// The archived chats, put away: a collapsed group (a native <details>,
			// so it opens without script).
			g.If(len(archived) > 0, g.El("details",
				g.Attr(chiefArchivedAttr, ""),
				Class("group mt-1"),
				g.El("summary",
					Class("press flex min-h-[44px] cursor-pointer list-none items-center gap-2 rounded-lg px-1 text-[11px] font-semibold uppercase tracking-wide text-muted transition hover:text-fg [&::-webkit-details-marker]:hidden"),
					g.Raw(`<svg class="h-3.5 w-3.5 shrink-0 transition group-open:rotate-90" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 18l6-6-6-6"/></svg>`),
					g.Text(chiefArchivedLabel+" ("+strconv.Itoa(len(archived))+")"),
				),
				Ul(g.Attr("aria-label", "Archived chats"), Class("mt-1 flex flex-col gap-2"), g.Group(archived)),
			)),
		),
		Div(
			Class("flex flex-none flex-wrap items-center gap-2 border-t border-line px-3 py-2"),
			ChiefSideChatButton(b.AgentID, ChatSurfacePage),
			g.If(kind == ChiefChatKindPrimary, chiefFreshPrimaryButton(b.AgentID, ChatSurfacePage)),
		),
	)
}

// chiefChatRow is one chat in the Chats tab: a link to it (a boosted
// navigation, as the switcher's rows are), marked "Current" when it is the chat
// on screen, with the Primary badge and the unread dot — and, when the chat has
// any, its ⋯ menu BESIDE the link, never inside it: a form in a link is not
// valid HTML, and a tap meant to open the chat must not archive it.
func chiefChatRow(name string, agentID int64, sess SessionView) g.Node {
	title := textplain.Title(sess.Title)
	if title == "" {
		title = "New chat"
	}
	cls := "press flex min-h-[44px] min-w-0 flex-1 items-center gap-2 rounded-xl px-3 py-2 text-sm transition ring-1 ring-inset "
	if sess.Active {
		cls += "bg-accent/15 text-fg ring-accent/50"
	} else {
		cls += "bg-s1 text-fg2 ring-line hover:bg-s2"
	}
	return Li(
		Class("flex items-center gap-1"),
		A(
			Href(AgentSessionHref(name, sess.ID)),
			g.Attr(chiefChatRowAttr, strconv.FormatInt(sess.ID, 10)),
			g.If(sess.Active, g.Attr("aria-current", "page")),
			Class(cls),
			Span(Class("mr-auto min-w-0 truncate"), g.Text(title)),
			g.If(sess.Active, Span(g.Attr(chiefChatCurrentAttr, ""), Class("shrink-0 text-[11px] font-medium text-accent"), g.Text("Current"))),
			chiefChatRowExtras(sess),
			g.If(!sess.LastActive.IsZero(),
				Span(Class("shrink-0 text-xs text-muted"), g.Text(relTimeString(sess.LastActive)+" ago")),
			),
		),
		chiefChatRowMenu(agentID, sess),
	)
}

// chiefChatRowMenu is a chat row's ⋯ menu: Unarchive on an archived chat; Make
// primary (where the deployment offers it) and Archive on a side chat. Nothing
// for the primary: it cannot be archived, and it is already the primary.
//
// A popover in the top layer, placed by floatingMenuScript, so the sheet's
// scrolling list never clips it. Its actions answer HX-Redirect (the page
// surface), which htmx follows.
func chiefChatRowMenu(agentID int64, sess SessionView) g.Node {
	if sess.IsDefault {
		return nil
	}
	id := "chief-chat-menu-" + strconv.FormatInt(sess.ID, 10)
	action := func(path, attr, label string) g.Node {
		return Form(
			hx("hx-post", path),
			hx("hx-swap", "none"),
			Class("contents"),
			Button(Type("submit"), g.Attr(attr, ""), g.Attr("role", "menuitem"),
				Class("press flex min-h-[44px] w-full items-center rounded-lg px-3 text-left text-sm font-medium text-fg transition hover:bg-s2"),
				g.Text(label)),
		)
	}
	var items []g.Node
	if sess.Archived {
		items = append(items, action(ChiefArchivePath(agentID, sess.ID, false), chiefUnarchiveAttr, "Unarchive"))
	} else {
		if sess.DefaultPath != "" {
			items = append(items, action(sess.DefaultPath, chiefMakePrimaryAttr, "Make primary"))
		}
		items = append(items, action(ChiefArchivePath(agentID, sess.ID, true), chiefArchiveAttr, "Archive"))
	}
	return g.Group{
		Button(
			Type("button"),
			g.Attr("popovertarget", id),
			g.Attr("aria-haspopup", "menu"),
			g.Attr("aria-expanded", "false"),
			g.Attr("aria-label", "Chat actions"),
			g.Attr("title", "Chat actions"),
			Class("press inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-lg text-fg2 transition hover:bg-s2"),
			g.Raw(`<svg class="h-5 w-5" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><circle cx="5" cy="12" r="1.75"/><circle cx="12" cy="12" r="1.75"/><circle cx="19" cy="12" r="1.75"/></svg>`),
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

// chiefPanelChatActions is the chief PANEL's chat actions, under its thread list
// (chiefThreadList): "+ New side chat", and the open chat's own — "Fresh
// primary" on the primary, "Archive this chat" on a side chat, "Unarchive this
// chat" on an archived one — each answered with the panel body re-rendered
// (ChatSurfaceChiefPanel). Nothing for a panel not on a chief's chat.
func chiefPanelChatActions(b *ChiefChatBar) g.Node {
	if b == nil {
		return nil
	}
	kind, _ := chiefChatKind(*b)
	var own g.Node
	switch kind {
	case ChiefChatKindPrimary:
		own = chiefFreshPrimaryButton(b.AgentID, ChatSurfaceChiefPanel)
	case ChiefChatKindSide:
		own = chiefChatAction(ChatSurfaceChiefPanel, ChiefArchivePath(b.AgentID, b.SessionID, true), chiefArchiveAttr, "Archive this chat", "", false)
	case ChiefChatKindArchived:
		own = chiefChatAction(ChatSurfaceChiefPanel, ChiefArchivePath(b.AgentID, b.SessionID, false), chiefUnarchiveAttr, "Unarchive this chat", "", false)
	}
	return Div(
		g.Attr(chiefChatsAttr, ""),
		g.Attr(chiefChatKindAttr, kind),
		Class("flex flex-none flex-wrap items-center gap-2 border-t border-line pt-2"),
		ChiefSideChatButton(b.AgentID, ChatSurfaceChiefPanel),
		own,
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
