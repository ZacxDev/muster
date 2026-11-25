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
// 🔴 ONE "SESSIONS" BUTTON AND A DRAWER FROM THE RIGHT, NOT A STRIP OF PILLS
// (an operator ask: "a slide out from the right side of the screen instead of
// being big pills at the top"; it replaced the strip of chips that had replaced
// a sheet). Both surfaces draw the same bar (chiefChatStrip) over the
// transcript: one small button naming the chat on screen (★ on the primary,
// its model tag, a dot when ANOTHER chat has an unread reply), which slides a
// drawer in from the right edge — of the screen on the page, of the panel's
// column in the panel — listing every chat:
//
//	Sessions                                  ✕
//	[+ New session]
//	★ Primary                                 ⋯
//	side chats, most recent first…  ● sonnet  ⋯
//	Archived (n) ›   (expands inline)
//
// The order is agents.SwitcherOrder's (the server's chiefChatViews hands the
// sessions over in it); the renderer only places them. A tap on a row switches
// to its chat and shuts the drawer; the row of the chat on screen is
// aria-current="true". Each row's actions are in its ⋯ menu (a popover,
// floatingMenuScript): Fresh primary (with its confirm) on the primary; Make
// primary and Archive on a side chat; Unarchive on an archived one — and after
// one of them the drawer is open again on the answer, so the next action is
// one tap away. A member in a team's shared room (ChiefChatBar.SharedRoom) gets
// the rows to switch with, and nothing else.
//
// 🔴 A DIALOG, AND IT BEHAVES AS ONE: the button carries aria-expanded and
// aria-controls; the drawer is role="dialog" aria-modal="true", inert and off
// screen while shut; opening it moves the focus to the chat on screen's row,
// Tab stays inside it, and Escape, the backdrop and ✕ shut it with the focus
// back on the button. Escape on an open ⋯ menu is the menu's (the browser's),
// and an Escape the drawer took is not also the chief panel's.
//
// The data attributes keep their chip-era names (data-chief-chip,
// data-chief-chat-strip, …): they are what both test tiers read, and what they
// name — a chat's row, the bar — did not change.
//
// 🔴 "+" MUST VISIBLY LAND (the operator's "New side chat doesn't work": the
// server opened the chat and the page moved to it, but nothing on screen said
// so). The strip is drawn with the page, so the chat "+" lands on is a chip,
// current, from the first paint; and the composer is focused on arrival
// (the side chat route's answer carries the flag: ComposerFocusParam), on the
// new chat or on the empty one the server reused.
//
// How much context the chat uses is in the header's ⋯ menu, and only when it
// is high (chiefMemorySlot); the header carries no chip.
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
	// NewChatModels are the models the "+" menu offers a new side chat on,
	// after its Default (the chief's own model): agents.ClaudeCodeModels, the
	// list the New session picker offers too, where the deployment offers the
	// claude-code session runtime to this chief (MUSTER_CHIEF_SESSION_RUNTIMES,
	// internal/api chiefChatBarFor). None: no menu, "+" opens a chat at once.
	NewChatModels []string
	// OpenRouter: the "+" menu also offers a side chat on any OpenRouter model
	// (the opencode runtime, found by search: openRouterModelField), where the
	// deployment offers it to this chief (internal/api opencodeOffered).
	OpenRouter bool
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
	// chiefNewChatMenuAttr marks the "+" menu; chiefNewChatModelAttr each of its
	// entries, valued with the model it opens a chat on ("" for Default).
	chiefNewChatMenuAttr  = "data-chief-new-chat-menu"
	chiefNewChatModelAttr = "data-chief-new-chat-model"
	// chiefNewChatOpenRouterAttr marks the "+" menu's OpenRouter form.
	chiefNewChatOpenRouterAttr = "data-chief-new-chat-openrouter"
	// chiefChipModelAttr marks a chip's model tag, valued with the model.
	chiefChipModelAttr = "data-chief-chip-model"
	chiefContextID     = "chief-context"
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

// ChiefContextPath is the ⋯ menu's memory line's partial for chat sid of agent
// name (chiefMemorySlot).
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

// chiefArchivedChipText is the "Archived (n)" toggle's text for n archived chats.
func chiefArchivedChipText(n int) string {
	return chiefArchivedLabel + " (" + strconv.Itoa(n) + ")"
}

// The bar's own vocabulary: its button, the drawer, the drawer's ✕ and its
// backdrop (valued with the drawer's id).
const (
	chiefSessionsOpenAttr     = "data-chief-sessions-open"
	chiefSessionsDrawerAttr   = "data-chief-sessions-drawer"
	chiefSessionsCloseAttr    = "data-chief-sessions-close"
	chiefSessionsBackdropAttr = "data-chief-sessions-backdrop"
	// chiefSessionsUnreadAttr marks the button's dot: another chat has an
	// unread reply.
	chiefSessionsUnreadAttr = "data-chief-sessions-unread"
)

// ChiefSessionsLabel is the drawer's title, and the start of its button's name.
const ChiefSessionsLabel = "Sessions"

// chiefSessionsDrawerID is the drawer's element id on surface (one per
// document: the page has no panel, and the panel's has its own).
func chiefSessionsDrawerID(surface ChatSurface) string {
	if surface == ChatSurfaceChiefPanel {
		return "chief-panel-sessions"
	}
	return "chief-sessions"
}

// chiefChatStrip is the chief's chats as a "Sessions" button and the drawer it
// opens (see the file's header), for surface: name is the chief's agent name,
// sessions its chats in agents.SwitcherOrder (primary, side chats by activity,
// archived), b the chat the view is on. Nothing off a chief's chat.
//
// 🔴 THE BAR IS ITS FINAL HEIGHT FROM ITS OPENING TAG, and the drawer is out of
// the flow (fixed on the page, absolute over the panel's column), so nothing
// under the bar moves when the drawer opens, a row is added or the archived ones
// expand.
//
// 🔴 ON THE PANEL THE DRAWER IS `absolute`, NOT `fixed`: ChiefPanel carries a
// transform, so a fixed descendant would be placed against the panel by a rule
// this file does not control (chiefThreadList says the same of its sub-view).
// The panel body's relative column is its containing block, and that column's
// overflow-hidden clips it while it is slid away.
func chiefChatStrip(b *ChiefChatBar, name string, sessions []SessionView, surface ChatSurface) g.Node {
	if b == nil {
		return nil
	}
	kind, _ := chiefChatKind(*b)
	var rows, archived []g.Node
	var archivedIDs []string
	var current *SessionView
	otherUnread := false
	for i := range sessions {
		sess := sessions[i]
		if sess.Active {
			current = &sessions[i]
		} else if sess.Unread {
			otherUnread = true
		}
		// The chat on screen is never hidden behind "Archived (n)", archived or
		// not (chiefChatViews lists an open archived chat with the side ones).
		if sess.Archived && !sess.Active {
			archived = append(archived, chiefChip(name, b, sess, surface, true))
			archivedIDs = append(archivedIDs, chiefChipID(surface, sess.ID))
			continue
		}
		rows = append(rows, chiefChip(name, b, sess, surface, false))
	}
	if len(archived) > 0 {
		rows = append(rows, Li(
			Class("flex items-center pt-1"),
			Button(
				Type("button"),
				g.Attr(chiefArchivedAttr, ""),
				g.Attr("aria-expanded", "false"),
				// The rows it shows and hides.
				g.Attr("aria-controls", strings.Join(archivedIDs, " ")),
				Class("group press inline-flex min-h-[44px] items-center gap-1 whitespace-nowrap rounded-lg px-3 text-xs font-semibold uppercase tracking-wide text-muted transition hover:bg-s2 hover:text-fg aria-expanded:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"),
				g.Text(chiefArchivedChipText(len(archived))),
				g.Raw(`<svg class="h-3.5 w-3.5 shrink-0 transition group-aria-expanded:rotate-90" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 18l6-6-6-6"/></svg>`),
			),
		))
		rows = append(rows, archived...)
	}
	drawerID := chiefSessionsDrawerID(surface)
	titleID := drawerID + "-title"
	pad := "px-2 sm:px-4"
	backdrop := "fixed inset-0 z-40 bg-black/50"
	drawer := "fixed inset-y-0 right-0 z-50 w-80 max-w-[88vw] border-l pb-[env(safe-area-inset-bottom)] pt-[env(safe-area-inset-top)]"
	if surface == ChatSurfaceChiefPanel {
		pad = ""
		backdrop = "absolute inset-0 z-20 rounded-lg bg-black/40"
		drawer = "absolute inset-y-0 right-0 z-30 w-72 max-w-[92%] rounded-l-lg border-y border-l"
	}
	return Div(
		g.Attr(chiefChatStripAttr, ""),
		g.Attr(chiefChatsAttr, ""),
		g.Attr(chiefChatKindAttr, kind),
		Class("flex h-[3.25rem] min-w-0 flex-none items-center "+pad),
		chiefSessionsButton(current, drawerID, otherUnread),
		// The backdrop: a tap on it shuts the drawer. Pointer-only, as the
		// thread list's is: Escape and ✕ are the keyboard's.
		Div(
			g.Attr(chiefSessionsBackdropAttr, drawerID),
			g.Attr("hidden", ""),
			g.Attr("aria-hidden", "true"),
			Class(backdrop),
		),
		Div(
			ID(drawerID),
			g.Attr(chiefSessionsDrawerAttr, ""),
			g.Attr("role", "dialog"),
			g.Attr("aria-modal", "true"),
			g.Attr("aria-labelledby", titleID),
			// Shut: inert (out of the tab order and the accessibility tree) and
			// slid off its edge — not `hidden`, which would leave the slide
			// nothing to animate from. chiefChipsScript opens and shuts it.
			g.Attr("inert", ""),
			g.Attr("data-open", "0"),
			Class(drawer+" invisible flex translate-x-full flex-col border-line bg-bg shadow-2xl transition-transform duration-200 ease-out"),
			Div(
				Class("flex flex-none items-center gap-2 border-b border-line py-1 pl-4 pr-1"),
				H2(ID(titleID), Class("mr-auto text-sm font-semibold text-fg"), g.Text(ChiefSessionsLabel)),
				Button(
					Type("button"),
					g.Attr(chiefSessionsCloseAttr, ""),
					g.Attr("aria-label", "Close sessions"),
					g.Attr("title", "Close sessions"),
					Class("press inline-flex h-11 w-11 items-center justify-center rounded-lg text-muted transition hover:bg-s2 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"),
					g.Raw(`<svg class="h-4 w-4" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M5 5l10 10M15 5L5 15"/></svg>`),
				),
			),
			Div(
				Class("flex min-h-0 flex-1 flex-col gap-2 overflow-y-auto overscroll-contain p-2"),
				g.If(!b.SharedRoom, chiefNewChatChip(b, surface)),
				Nav(
					g.Attr("aria-label", "Chief sessions"),
					Ul(
						g.Attr(chiefChipScrollAttr, ""),
						Class("flex flex-col gap-1"),
						g.Group(rows),
					),
				),
			),
		),
		chiefChipsScript(),
		// The ⋯ menus are placed by floatingMenuScript: the chief page carries
		// it already; the shell the panel lives in does not (it is bound once
		// per document, so a second copy is a no-op).
		g.If(surface == ChatSurfaceChiefPanel && !b.SharedRoom, floatingMenuScript()),
	)
}

// chiefSessionsButton is the bar's one control: the chat on screen (★ and
// "Primary" on the primary, else its title; its model tag), a dot when another
// chat has an unread reply, and what it opens. Its name is "Sessions: <the
// chat>" — the visible label inside it, so a voice command naming what is on
// screen still finds it.
func chiefSessionsButton(current *SessionView, drawerID string, otherUnread bool) g.Node {
	label, primary := ChiefSessionsLabel, false
	var tag g.Node
	if current != nil {
		label = textplain.Title(current.Title)
		if label == "" {
			label = ChiefNewChatLabel
		}
		if current.IsDefault {
			label, primary = "Primary", true
		}
		tag = chiefChipModelTag(*current)
	}
	name := ChiefSessionsLabel + ": " + label
	return Button(
		Type("button"),
		g.Attr(chiefSessionsOpenAttr, ""),
		g.Attr("aria-haspopup", "dialog"),
		g.Attr("aria-expanded", "false"),
		g.Attr("aria-controls", drawerID),
		g.Attr("aria-label", name),
		g.Attr("title", name),
		Class("press inline-flex h-11 min-w-0 max-w-full items-center gap-1.5 whitespace-nowrap rounded-lg bg-s1 pl-2.5 pr-2 text-sm font-medium text-fg ring-1 ring-inset ring-line transition hover:bg-s2 aria-expanded:bg-s2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"),
		g.Raw(`<svg class="h-4 w-4 shrink-0 text-muted" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 6h16M4 12h16M4 18h10"/></svg>`),
		g.If(primary, Span(g.Attr(sessionDefaultAttr, ""), g.Attr("aria-hidden", "true"), Class("shrink-0 text-accent"), g.Text("★"))),
		Span(Class("min-w-0 truncate"), g.Text(label)),
		tag,
		g.If(otherUnread, Span(
			g.Attr(chiefSessionsUnreadAttr, ""),
			g.Attr("role", "img"),
			g.Attr("aria-label", "Unread replies in other sessions"),
			Class("h-2 w-2 shrink-0 rounded-full bg-accent"),
		)),
		g.Raw(`<svg class="h-4 w-4 shrink-0 text-muted" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M15 18l-6-6 6-6"/></svg>`),
	)
}

// chiefChip is one chat's row in the drawer: a tap switches to it (a boosted
// link on the page, an hx-get of the panel body in the panel), current when it
// is the chat on screen, with the ★ on the primary, its model tag and the
// unread dot — and, unless the viewer is in a team's shared room, its ⋯ menu
// BESIDE the link, never inside it: a form in a link is not valid HTML, and a
// tap meant to open the chat must not archive it. hidden: an archived chat's
// row, behind "Archived (n)".
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
	cls := "press flex min-h-[44px] min-w-0 flex-1 items-center gap-1.5 rounded-lg pl-3 text-left text-sm transition focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus "
	if b.SharedRoom {
		cls += "pr-3 "
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
		chiefChipModelTag(sess),
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
	ring := "ring-transparent hover:bg-s1"
	if sess.Active {
		ring = "bg-accent/15 ring-accent/60"
	}
	return Li(
		g.Attr(chiefChipAttr, id),
		g.Attr(chiefChipKindAttr, kind),
		g.If(hidden, g.Attr(chiefArchivedChipAttr, "")),
		g.If(hidden, g.Attr("hidden", "")),
		g.If(hidden, ID(chiefChipID(surface, sess.ID))),
		Class("flex min-h-[44px] items-center rounded-lg ring-1 ring-inset transition "+ring),
		link,
		g.If(!b.SharedRoom, chiefChipMenu(b, sess, kind, label, surface)),
	)
}

// chiefChipID is the element id of chat sid's row on surface: an archived
// row's, which the "Archived (n)" toggle names in aria-controls.
func chiefChipID(surface ChatSurface, sid int64) string {
	if surface == ChatSurfaceChiefPanel {
		return "chief-panel-chip-" + strconv.FormatInt(sid, 10)
	}
	return "chief-chip-" + strconv.FormatInt(sid, 10)
}

// chiefChipMenu is a chip's ⋯ menu: Fresh primary (with its confirm) on the
// primary; Make primary (where the deployment offers it) and Archive on a side
// chat; Unarchive on an archived one. Every action names the chat on screen
// (ChiefCurrentParam), so acting on another chat leaves the operator where they
// are. A popover in the top layer, placed by floatingMenuScript, so the strip's
// scroller never clips it; Escape and a tap outside close it (the browser's).
//
// label is the chip's own (its chat's title, or "Primary"): the ⋯ is named
// "Actions for <label>", so a screen reader tells one chip's ⋯ from another's.
func chiefChipMenu(b *ChiefChatBar, sess SessionView, kind, label string, surface ChatSurface) g.Node {
	name := "Actions for " + label
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
			g.Attr("aria-label", name),
			g.Attr("title", name),
			Class("press inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-full text-fg2 transition hover:bg-s2 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"),
			g.Raw(`<svg class="h-4 w-4" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><circle cx="5" cy="12" r="1.75"/><circle cx="12" cy="12" r="1.75"/><circle cx="19" cy="12" r="1.75"/></svg>`),
		),
		chiefPopoverMenu(id, chiefChatMenuAttr, name, items),
	}
}

// chiefPopoverMenu is the strip's popover menu (a chip's ⋯, the "+" menu): in
// the top layer, placed beside its popovertarget by floatingMenuScript, closed
// by Escape and a tap outside (the browser's). attr marks it.
func chiefPopoverMenu(id, attr, name string, items []g.Node) g.Node {
	return chiefPopoverMenuWidth(id, attr, name, "w-48", items)
}

// chiefPopoverMenuWidth is chiefPopoverMenu at width class width (the "+"
// menu with its OpenRouter search is wider).
func chiefPopoverMenuWidth(id, attr, name, width string, items []g.Node) g.Node {
	return Div(
		ID(id),
		g.Attr(attr, ""),
		g.Attr("popover", "auto"),
		g.Attr("role", "menu"),
		g.Attr("aria-label", name),
		g.Attr("data-floating-menu", ""),
		Class("invisible hidden [&:popover-open]:flex m-0 "+width+" flex-col gap-1 rounded-xl border border-line bg-s1 p-1.5 text-fg shadow-2xl ring-1 ring-black/20"),
		g.Group(items),
	)
}

// chiefChipModelTag is a chip's model tag ("sonnet"): the model a side chat was
// opened on from the "+" menu, fixed for its life. Nothing for a chat on the
// chief's own model (Model "", the "+" menu's Default and every chat made
// before the menu). Compact, so it barely widens the chip.
func chiefChipModelTag(sess SessionView) g.Node {
	if sess.Model == "" {
		return nil
	}
	return Span(
		g.Attr(chiefChipModelAttr, sess.Model),
		g.Attr("title", "Model: "+strings.TrimPrefix(sess.Model, OpenRouterModelPrefix)),
		Class("max-w-[5.5rem] shrink-0 truncate rounded px-1 text-[10px] font-medium leading-4 text-muted ring-1 ring-inset ring-line"),
		g.Text(chipModelText(sess.Model)),
	)
}

// chipModelText is a chip's model tag text: an OpenRouter model's own name,
// the catalogue id's last part ("openrouter/deepseek/deepseek-v4.1-flash" →
// "deepseek-v4.1-flash"), so the narrow tag shows the model rather than the
// route prefix; any other model as it is.
func chipModelText(model string) string {
	if rest, ok := strings.CutPrefix(model, OpenRouterModelPrefix); ok && rest != "" {
		return rest[strings.LastIndex(rest, "/")+1:]
	}
	return model
}

// ChiefNewSessionLabel is the drawer's new-chat control's words (and name).
const ChiefNewSessionLabel = "New session"

// chiefNewChatChip is the drawer's "+ New session": a new side chat (or the newest one,
// reused while it is still empty and on the same model: internal/api
// openSideChat). Its answer moves the view to that chat, with the cursor in its
// message box (the route's answer asks for it: ComposerFocusParam).
//
// 🔴 WHERE A MODEL MAY BE PICKED (b.NewChatModels), "+" OPENS A SMALL MENU, NOT
// A MODAL (the operator's ask: chief side chats on different models, picked
// when the chat is made): Default (the chief's own model, what "+" opened
// before) and each model, each entry posting it as the route's model. A chat's
// model is fixed once it is made. Elsewhere "+" opens a chat at once, as
// before.
func chiefNewChatChip(b *ChiefChatBar, surface ChatSurface) g.Node {
	post := chiefChatPost(surface, onSurface(surface, ChiefSideChatPath(b.AgentID)))
	cls := "press flex min-h-[44px] w-full items-center gap-2 rounded-lg bg-s1 px-3 text-left text-sm font-medium text-fg ring-1 ring-inset ring-line transition hover:bg-s2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"
	// The name is the visible words, "New session".
	plus := g.Group{
		Span(g.Attr("aria-hidden", "true"), Class("text-lg leading-none text-accent"), g.Text("+")),
		Span(g.Text(ChiefNewSessionLabel)),
	}
	if len(b.NewChatModels) == 0 && !b.OpenRouter {
		return Form(
			post,
			Class("contents"),
			Button(
				Type("submit"),
				g.Attr(chiefSideChatAttr, ""),
				g.Attr("title", ChiefNewSessionLabel),
				Class(cls),
				plus,
			),
		)
	}
	id := "chief-new-chat-menu"
	if surface == ChatSurfaceChiefPanel {
		id = "chief-panel-new-chat-menu"
	}
	entry := func(model, label string) g.Node {
		return Form(
			post,
			Class("contents"),
			g.If(model != "", Input(Type("hidden"), Name("model"), Value(model))),
			Button(Type("submit"), g.Attr(chiefNewChatModelAttr, model), g.Attr("role", "menuitem"),
				Class("press flex min-h-[44px] w-full items-center rounded-lg px-3 text-left text-sm font-medium text-fg transition hover:bg-s2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"),
				g.Text(label)),
		)
	}
	items := []g.Node{entry("", "Default")}
	for _, m := range b.NewChatModels {
		items = append(items, entry(m, chiefModelLabel(m)))
	}
	menu := chiefPopoverMenu(id, chiefNewChatMenuAttr, "New side chat on", items)
	if b.OpenRouter {
		// Any OpenRouter model: a search field and Start, posting the opencode
		// runtime and the picked model to the same route.
		items = append(items,
			Div(Class("my-1 border-t border-line")),
			Form(
				post,
				g.Attr(chiefNewChatOpenRouterAttr, ""),
				Class("flex flex-col gap-1.5 px-1 pb-1"),
				Input(Type("hidden"), Name("runtime"), Value(OpenRouterRuntime)),
				Span(Class("px-1 text-xs font-semibold text-fg2"), g.Text("OpenRouter model")),
				openRouterModelField(id+"-or"),
				Button(Type("submit"),
					Class("press min-h-[40px] rounded-lg bg-accent px-3 text-xs font-semibold text-on-accent transition hover:bg-accent/90 disabled:opacity-60"),
					g.Text("Start")),
			),
		)
		menu = chiefPopoverMenuWidth(id, chiefNewChatMenuAttr, "New side chat on", "w-80", items)
	}
	return g.Group{
		Button(
			Type("button"),
			g.Attr(chiefSideChatAttr, ""),
			g.Attr("popovertarget", id),
			g.Attr("aria-haspopup", "menu"),
			g.Attr("aria-expanded", "false"),
			g.Attr("title", ChiefNewSessionLabel),
			Class(cls),
			plus,
		),
		menu,
	}
}

// chiefModelLabel is a "+" menu entry's label for model: an alias capitalised
// ("sonnet" → "Sonnet"), any other model as it is.
func chiefModelLabel(model string) string {
	if model == "" || strings.ContainsAny(model, "-.[") {
		return model
	}
	return strings.ToUpper(model[:1]) + model[1:]
}

// ComposerFocusParam is the query parameter (value "1") with which the side
// chat route's page answer (HX-Redirect) asks the page it lands on to put the
// cursor in the message box (AgentDetailView.FocusComposer). The panel's answer
// sets the flag on the body it renders instead. Nothing else sets it, so a
// plain load or reload of ?session=N never takes the focus.
const ComposerFocusParam = "focus"

// chiefSessionsReopenKey is the sessionStorage key a ⋯ action sets as its
// request starts, so the answer — a reload of the page (HX-Redirect), a new
// panel body — opens the drawer again (chiefChipsScript).
const chiefSessionsReopenKey = "muster.chief.v1.sessions-reopen"

// chiefChipsScript is the drawer's behaviour, bound once per document (htmx
// re-executes it on every swap that brings a bar; the guard leaves only the
// re-open step to run again):
//
//   - THE BUTTON OPENS ITS DRAWER (aria-controls), the focus moves to the row
//     of the chat on screen, and Tab is kept inside it while it is open.
//   - ✕, THE BACKDROP AND ESCAPE SHUT IT, the focus back on the button. The
//     Escape listener is a CAPTURE listener on document, so it runs before the
//     chief panel's own (which shuts the whole panel), and it takes the key
//     (preventDefault, stopPropagation) only when a drawer is open and no
//     popover is: an open ⋯ menu is the top layer, and its Escape is the
//     browser's.
//   - A TAP ON A ROW SHUTS IT as the switch starts (the answer replaces the
//     bar anyway; shutting at once is what makes the pick feel immediate).
//   - "Archived (n)" toggles its chats' rows inline (aria-expanded).
//   - A ⋯ ACTION'S ANSWER OPENS IT AGAIN: the action's request records the
//     drawer (chiefSessionsReopenKey, sessionStorage, with the time), and the
//     page or panel body that answers opens it. A record older than 15s, or one
//     whose request failed, is dropped, so a later load never opens it
//     unasked.
func chiefChipsScript() g.Node {
	return Script(g.Raw(`
(function () {
  var OPEN = '` + chiefSessionsOpenAttr + `', DRAWER = '` + chiefSessionsDrawerAttr + `';
  var CLOSE = '` + chiefSessionsCloseAttr + `', BACK = '` + chiefSessionsBackdropAttr + `';
  var ROW = '` + chiefChatRowAttr + `', MENU = '` + chiefChatMenuAttr + `';
  var ARCH = '` + chiefArchivedAttr + `', ARCH_ROW = '` + chiefArchivedChipAttr + `';
  var KEY = '` + chiefSessionsReopenKey + `';
  function ss() { try { return window.sessionStorage; } catch (e) { return null; } }
  function clearReopen() { var s = ss(); if (s) { try { s.removeItem(KEY); } catch (e) {} } }
  function trigger(d) { return document.querySelector('[' + OPEN + '][aria-controls="' + d.id + '"]'); }
  function isOpen(d) { return d.getAttribute('data-open') === '1'; }
  function setOpen(d, open, focusBack) {
    d.setAttribute('data-open', open ? '1' : '0');
    d.classList.toggle('translate-x-full', !open);
    d.classList.toggle('translate-x-0', open);
    // 🔴 VISIBLE AT ONCE, HIDDEN ONLY ONCE THE SLIDE IS DONE. A transitioned
    // visibility is still hidden on a transition's first frame, so the focus
    // moved in below was refused; and hidden at once, the slide out would not
    // be seen.
    clearTimeout(d.__hideT);
    if (open) d.classList.remove('invisible');
    else d.__hideT = setTimeout(function () { if (!isOpen(d)) d.classList.add('invisible'); }, 250);
    if (open) d.removeAttribute('inert'); else d.setAttribute('inert', '');
    var bk = document.querySelector('[' + BACK + '="' + d.id + '"]');
    if (bk) bk.hidden = !open;
    var t = trigger(d);
    if (t) t.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (open) {
      var f = d.querySelector('[' + ROW + '][aria-current="true"]') || d.querySelector('[' + CLOSE + ']');
      try { if (f) f.focus({ preventScroll: true }); } catch (e) {}
    } else if (focusBack && t) {
      try { t.focus({ preventScroll: true }); } catch (e) {}
    }
  }
  function openDrawer() {
    var ds = document.querySelectorAll('[' + DRAWER + '][data-open="1"]');
    return ds.length ? ds[ds.length - 1] : null;
  }
  // The focusable controls of d now on screen (a shut ⋯ menu and a hidden
  // archived row are not).
  function focusables(d) {
    var all = d.querySelectorAll('a[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), select, textarea, [tabindex]:not([tabindex="-1"])');
    var out = [];
    for (var i = 0; i < all.length; i++) if (all[i].getClientRects().length) out.push(all[i]);
    return out;
  }
  // reopen: a ⋯ action's answer has landed; its drawer opens again.
  // evt: the settle that brought it, when one did — only a settle whose target
  // holds the drawer is the answer (a transcript refresh settling meanwhile is
  // not, and must not spend the record on the drawer about to be replaced).
  function reopen(evt) {
    var s = ss(), raw = null;
    if (s) { try { raw = s.getItem(KEY); } catch (e) {} }
    if (!raw) return;
    var v = null;
    try { v = JSON.parse(raw); } catch (e) {}
    var d = v && v.id && document.getElementById(v.id);
    if (!v || !(Date.now() - v.t < 15000)) { clearReopen(); return; }
    if (!d) return;
    var t = evt && evt.target;
    if (t && t !== document && t !== document.body && !(t.contains && t.contains(d))) return;
    clearReopen();
    setOpen(d, true);
  }
  // 🔴 A SWAPPED-IN DRAWER IS OPENED AFTER THE SWAP SETTLES, NOT WHEN THIS
  // SCRIPT RUNS INSIDE IT: htmx's settle puts back the class the server drew
  // (shut, translate-x-full) over whatever ran in between, so a drawer opened
  // here was open by its attributes and off screen by its class. The
  // afterSettle listener below does it; a full load, DOMContentLoaded.
  if (window.__musterChiefSessions) return;
  window.__musterChiefSessions = true;
  document.addEventListener('click', function (e) {
    var t = e.target;
    if (!t || !t.closest) return;
    var o = t.closest('[' + OPEN + ']');
    if (o) {
      var d0 = document.getElementById(o.getAttribute('aria-controls') || '');
      if (d0) setOpen(d0, !isOpen(d0), true);
      return;
    }
    var c = t.closest('[' + CLOSE + ']');
    if (c) { var d1 = c.closest('[' + DRAWER + ']'); if (d1) setOpen(d1, false, true); return; }
    var bk = t.closest('[' + BACK + ']');
    if (bk) { var d2 = document.getElementById(bk.getAttribute(BACK)); if (d2) setOpen(d2, false, true); return; }
    var a = t.closest('button[' + ARCH + ']');
    if (a) {
      var dr = a.closest('[' + DRAWER + ']');
      if (!dr) return;
      var show = a.getAttribute('aria-expanded') !== 'true';
      a.setAttribute('aria-expanded', show ? 'true' : 'false');
      var rs = dr.querySelectorAll('[' + ARCH_ROW + ']');
      for (var i = 0; i < rs.length; i++) rs[i].hidden = !show;
      return;
    }
    var row = t.closest('[' + ROW + ']');
    if (row) { var d3 = row.closest('[' + DRAWER + ']'); if (d3 && isOpen(d3)) setOpen(d3, false, false); }
  });
  document.addEventListener('keydown', function (e) {
    var d = openDrawer();
    if (!d) return;
    if (e.key === 'Escape') {
      if (e.defaultPrevented) return;
      try { if (document.querySelector(':popover-open')) return; } catch (err) {}
      e.preventDefault();
      e.stopPropagation();
      setOpen(d, false, true);
      return;
    }
    if (e.key !== 'Tab') return;
    var f = focusables(d);
    if (!f.length) return;
    var first = f[0], last = f[f.length - 1], at = document.activeElement;
    var inside = at && d.contains(at);
    if (e.shiftKey && (at === first || !inside)) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && (at === last || !inside)) { e.preventDefault(); first.focus(); }
  }, true);
  document.addEventListener('htmx:beforeRequest', function (e) {
    var el = e.target;
    if (!el || !el.closest || !el.closest('[' + MENU + ']')) return;
    var d = el.closest('[' + DRAWER + ']');
    var s = ss();
    if (!d || !s) return;
    try { s.setItem(KEY, JSON.stringify({ id: d.id, t: Date.now() })); } catch (err) {}
  });
  document.addEventListener('htmx:responseError', clearReopen);
  document.addEventListener('htmx:sendError', clearReopen);
  document.addEventListener('htmx:afterSettle', reopen);
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', reopen);
  else reopen();
})();
`))
}

// chiefPageStrip is the chief PAGE's bar: a row of its own under the header,
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

// THE CHIEF'S MEMORY IN THE ⋯ MENU, ONLY WHEN IT IS HIGH (operator decision,
// #871). The header used to carry a raw token-count chip ("45k") under
// the chief's title, always. It is gone from the header and from the panel's
// title bar; the chief page's ⋯ menu says "Memory used: 362k" in plain words
// — and only while the chat's latest turn sent more than ChiefMemoryShowAbove
// tokens, the point where a fresh chat is worth thinking about.
//
// The menu holds an empty slot (chiefMemorySlot) that loads its line
// (RenderChiefMemoryLine, internal/api handleChiefChatContext) on load and
// again whenever a turn of the chief starts or ends or a reply is stored, so
// the line follows the turn that just ended without a reload. innerHTML into
// the slot: the slot keeps its triggers.

// ChiefMemoryShowAbove is the context, in tokens, past which the ⋯ menu says
// how much of it the chief's chat uses. At or below it, nothing.
const ChiefMemoryShowAbove int64 = 350_000

// ChiefMemoryAttr marks the menu's memory line.
const ChiefMemoryAttr = "data-chief-memory"

// chiefMemorySlot is the ⋯ menu's slot for chat sid's memory line. peer: the
// slot is the menu's only extra item, so the divider under the extras hides
// while it is empty (agentActionsMenuWith's peer-empty:hidden).
func chiefMemorySlot(name string, sid int64, peer bool) g.Node {
	quoted := strconv.Quote(name)
	cls := "block empty:hidden"
	if peer {
		cls += " peer"
	}
	return Div(
		ID(chiefContextID),
		Class(cls),
		hx("hx-get", ChiefContextPath(name, sid)),
		hx("hx-trigger", "load, sse:chat.turn[JSON.parse(detail.data).id==="+quoted+"] from:body, "+
			"sse:chat.reply[JSON.parse(detail.data).id==="+quoted+"] from:body"),
		// 🔴 ITS OWN TARGET, SPELLED: an inherited hx-target would put the line's
		// answer over whatever declared it.
		hx("hx-target", "this"),
		hx("hx-swap", "innerHTML"),
	)
}

// RenderChiefMemoryLine writes the ⋯ menu's memory line for a chat whose
// latest turn sent tokens of context — nothing at or below
// ChiefMemoryShowAbove.
func RenderChiefMemoryLine(w io.Writer, tokens int64) error {
	if tokens <= ChiefMemoryShowAbove {
		return nil
	}
	return P(
		g.Attr(ChiefMemoryAttr, strconv.FormatInt(tokens, 10)),
		Class("px-3 py-2 text-xs text-fg2"),
		g.Text("Memory used: "+chiefMemoryWords(tokens)),
	).Render(w)
}

// chiefMemoryWords is tokens in thousands, rounded: "362k".
func chiefMemoryWords(tokens int64) string {
	return strconv.FormatInt((tokens+500)/1000, 10) + "k"
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
