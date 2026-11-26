package ui

import (
	"io"
	"net/url"
	"strconv"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/textplain"
)

// THE CHIEF'S PRIMARY AND SIDE CHATS (internal/agents chief_chats.go;
// docs/chief.md#primary-and-side-chats), as the chief page and the chief panel
// draw them.
//
// 🔴 A "SESSIONS" TAB IN THE SHEET THE LIST ICON OPENS, NOT A DRAWER OF ITS OWN
// (an operator ask: "make it part of the same button that opens the slide up
// that shows the focus and the user messages, and just make it another tab";
// it replaced the drawer from the right edge, which had replaced a strip of
// pills, which had replaced a sheet). The list icon — the chief page's header,
// the panel's title bar — opens the sheet (chat_index.go) whose FIRST tab is
// every chat of the chief's (chiefSessionsTab):
//
//	[ Search sessions…                     ]
//	(All) (Unread) (Archived) (qwen3-coder) …  filter chips, one pressed
//	[+ New session]
//	★ Primary                                 ⋯
//	side chats, most recent first…  ● sonnet  ⋯
//	Archived (n) ›   (expands inline)
//	No sessions match     (only while the filters hide every row)
//
// Over the transcript, where the Sessions button was, is the chat on screen
// as PLAIN TEXT (chiefCurrentChat): ★ "Primary" on the primary, else its title
// ("New chat" until its first line titles it), and its model tag. The dot that
// said ANOTHER chat has an unread reply is on the list icon now
// (chiefIndexButton), and the tab's label counts those chats.
//
// The order is agents.SwitcherOrder's (the server's chiefChatViews hands the
// sessions over in it); the renderer only places them. A tap on a row switches
// to its chat and shuts the sheet; the row of the chat on screen is
// aria-current="true". Each row's actions are in its ⋯ menu (a popover,
// floatingMenuScript): Fresh primary (with its confirm) on the primary; Make
// primary and Archive on a side chat; Unarchive on an archived one — and after
// one of them the sheet is open again on the Sessions tab, so the next action
// is one tap away. A member in a team's shared room (ChiefChatBar.SharedRoom)
// gets the rows to switch with, and nothing else.
//
// 🔴 THE SEARCH AND THE CHIPS ARE THE BROWSER'S, ON THE ROWS THE PAGE DREW (no
// route of their own: every chat is already in the tab). The search matches a
// row's words — its title, "Primary", its model — by textContent, never by
// markup, and reaches the archived chats too; the chips are All, Unread,
// Archived (where there is one) and one per OpenRouter model a chat was opened
// on (a Claude model gets none).
// 🔴 THEY APPLY TO EVERY ROW ALIKE, THE CHAT ON SCREEN'S INCLUDED: the bar over
// the transcript names that chat whatever the filters hide, so a filter that
// made an exception for it would only make the list disagree with its own
// chips.
//
// The data attributes keep their chip-era names (data-chief-chip,
// data-chief-chat-strip, …): they are what both test tiers read, and what they
// name — a chat's row, the bar — did not change.
//
// 🔴 "+" MUST VISIBLY LAND (the operator's "New side chat doesn't work": the
// server opened the chat and the page moved to it, but nothing on screen said
// so). The bar and the tab are drawn with the page, so the chat "+" lands on
// is named over the transcript and current in the tab from the first paint;
// and the composer is focused on arrival (the side chat route's answer carries
// the flag: ComposerFocusParam), on the new chat or on the empty one the server
// reused.
//
// How much context the chat uses is in the header's ⋯ menu, and only when it
// is high (chiefMemorySlot); the header carries no chip.
//
// 🔴 THE SAME ROUTES ON BOTH SURFACES, ANSWERED THE WAY EACH SURFACE NEEDS:
// on the page an action's answer is HX-Redirect to the chat it leaves you on;
// in the panel it is the panel body re-rendered on that chat (?surface=
// chief-panel), so the operator never leaves the grid.

// ChiefChatBar is the chief chat a page or panel is on: set on a chief's chat
// only. (Named for the bar it used to draw; it now feeds the Sessions tab, the
// bar naming the chat on screen, the header's memory line and the panel's
// Continue picker.)
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
	// teamAgentRoutes), so the chief PAGE's Sessions tab draws none of them:
	// no "+", no ⋯; the rows switch, and that is all. (The chief PANEL never opens a
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
	// chiefChatsAttr marks the Sessions tab's panel, chiefChatStripAttr the bar
	// naming the chat on screen; chiefChatKindAttr on both names the chat the
	// view is on.
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

// The Sessions tab's own vocabulary.
const (
	// chiefSessionsUnreadAttr marks the list icon's dot: another chat has an
	// unread reply.
	chiefSessionsUnreadAttr = "data-chief-sessions-unread"
	// chiefSessionsCountAttr marks the tab label's count of those chats,
	// valued with it.
	chiefSessionsCountAttr = "data-chief-sessions-count"
	// chiefCurrentChatAttr marks the bar's plain-text name of the chat on
	// screen, valued with its id.
	chiefCurrentChatAttr = "data-chief-current-chat"
	// chiefSessionsSearchAttr marks the tab's search box; chiefSessionsFilterAttr
	// each filter chip, valued with its filter ("all", "unread", "archived",
	// "model:<model>"); chiefSessionsNoneAttr the line shown when they hide
	// every row.
	chiefSessionsSearchAttr = "data-chief-sessions-search"
	chiefSessionsFilterAttr = "data-chief-filter"
	chiefSessionsNoneAttr   = "data-chief-sessions-none"
	// chiefArchivedToggleRowAttr marks the "Archived (n)" toggle's row (hidden
	// while a search or a chip lists the archived chats among the others).
	chiefArchivedToggleRowAttr = "data-chief-archived-row"
)

// ChiefSessionsLabel is the Sessions tab's label.
const ChiefSessionsLabel = "Sessions"

// chiefSessionsTabID is the Sessions tab's ID in the sheet's tablist (what the
// tab choice stores, and what a ⋯ action's answer selects again).
const chiefSessionsTabID = "sessions"

// ChiefSessionsNoMatch is the tab's line when the search and chips hide every
// row.
const ChiefSessionsNoMatch = "No sessions match"

// chiefIndexID is the chief's sheet's id on surface (chat_index.go): the page's
// #chat-index, the panel's own.
func chiefIndexID(surface ChatSurface) string {
	if surface == ChatSurfaceChiefPanel {
		return chiefPanelIndexID
	}
	return chatIndexPageID
}

// chiefUnreadElsewhere is how many of sessions other than the one on screen
// have an unread reply: the Sessions tab's count, and (above 0) the list
// icon's dot. One count, so the two cannot disagree.
func chiefUnreadElsewhere(sessions []SessionView) int {
	n := 0
	for _, s := range sessions {
		if s.Unread && !s.Active {
			n++
		}
	}
	return n
}

// chiefCurrentChat is the bar over the transcript where the Sessions button
// was: the chat on screen, as plain text — ★ and "Primary" on the primary,
// else its title, and its model tag. Not a control: the list icon opens the
// chats. Nothing off a chief's chat.
//
// 🔴 THE BAR IS ITS FINAL HEIGHT FROM ITS OPENING TAG (h-10), and the sheet is
// out of the flow (fixed on the page, absolute over the panel's column), so
// nothing under the bar moves when the sheet opens, a tab is switched or a
// filter narrows the rows.
func chiefCurrentChat(b *ChiefChatBar, sessions []SessionView, surface ChatSurface) g.Node {
	if b == nil {
		return nil
	}
	kind, _ := chiefChatKind(*b)
	var current *SessionView
	for i := range sessions {
		if sessions[i].Active {
			current = &sessions[i]
		}
	}
	var name g.Node
	if current != nil {
		label := textplain.Title(current.Title)
		if label == "" {
			label = ChiefNewChatLabel
		}
		primary := current.IsDefault
		if primary {
			label = "Primary"
		}
		name = P(
			g.Attr(chiefCurrentChatAttr, strconv.FormatInt(current.ID, 10)),
			Class("flex min-w-0 items-center gap-1.5 whitespace-nowrap text-sm font-medium text-fg"),
			Span(Class("sr-only"), g.Text("Session: ")),
			g.If(primary, Span(g.Attr(sessionDefaultAttr, ""), g.Attr("aria-hidden", "true"), Class("shrink-0 text-accent"), g.Text("★"))),
			Span(Class("min-w-0 truncate"), g.Text(label)),
			chiefChipModelTag(*current),
		)
	}
	pad := "px-2 sm:px-4"
	if surface == ChatSurfaceChiefPanel {
		pad = "px-1"
	}
	return Div(
		g.Attr(chiefChatStripAttr, ""),
		g.Attr(chiefChatKindAttr, kind),
		Class("flex h-10 min-w-0 flex-none items-center "+pad),
		name,
	)
}

// chiefSessionsTab is the sheet's Sessions tab on surface, in the sheet
// sheetID: name is the chief's agent name, sessions its chats in
// agents.SwitcherOrder (primary, side chats by activity, archived), b the chat
// the view is on. Its label counts the chats with an unread reply other than
// the one on screen.
func chiefSessionsTab(b *ChiefChatBar, name string, sessions []SessionView, surface ChatSurface, sheetID string) Tab {
	kind, _ := chiefChatKind(*b)
	var rows, archived []g.Node
	var archivedIDs, models []string
	seen := map[string]bool{}
	anyArchived := false
	for i := range sessions {
		sess := sessions[i]
		// 🔴 OPENROUTER MODELS ONLY (the operator's option: "a per-model chip
		// when you have OpenRouter sessions"): a chat on a Claude model gets no
		// chip. agents.SessionOnOpenRouter is the one predicate for it.
		if agents.SessionOnOpenRouter(sess.Model) && !seen[sess.Model] {
			seen[sess.Model] = true
			models = append(models, sess.Model)
		}
		if sess.Archived {
			anyArchived = true
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
			g.Attr(chiefArchivedToggleRowAttr, ""),
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
	// The chips: All (pressed as drawn), Unread, Archived where there is an
	// archived chat, and one per OpenRouter model a chat was opened on, in the
	// order the chats first name them.
	chip := func(value, label, title string, pressed bool) g.Node {
		p := "false"
		if pressed {
			p = "true"
		}
		return Button(
			Type("button"),
			g.Attr(chiefSessionsFilterAttr, value),
			g.Attr("aria-pressed", p),
			g.If(title != "", g.Attr("title", title)),
			Class("press inline-flex min-h-[36px] max-w-[10rem] shrink-0 items-center truncate rounded-full px-3 text-xs font-medium text-fg2 ring-1 ring-inset ring-line transition hover:bg-s2 hover:text-fg aria-pressed:bg-accent/15 aria-pressed:text-fg aria-pressed:ring-accent/60 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"),
			g.Text(label),
		)
	}
	chips := []g.Node{chip("all", "All", "", true), chip("unread", "Unread", "", false)}
	if anyArchived {
		chips = append(chips, chip("archived", chiefArchivedLabel, "", false))
	}
	for _, m := range models {
		chips = append(chips, chip("model:"+m, chipModelText(m), "Model: "+strings.TrimPrefix(m, OpenRouterModelPrefix), false))
	}
	searchID := sheetID + "-search"
	listID := sheetID + "-sessions"
	panel := Div(
		g.Attr(chiefChatsAttr, ""),
		g.Attr(chiefChatKindAttr, kind),
		Class("flex flex-col"),
		// 🔴 PINNED OVER THE ROWS (sticky in the tab's own scroll), so a long
		// list scrolls under the search and the chips instead of taking them
		// away.
		Div(
			Class("sticky top-0 z-10 flex flex-col gap-2 border-b border-line bg-s1 px-3 py-2"),
			Label(For(searchID), Class("sr-only"), g.Text("Search sessions")),
			Input(
				ID(searchID),
				Type("search"),
				g.Attr(chiefSessionsSearchAttr, ""),
				g.Attr("autocomplete", "off"),
				g.Attr("aria-controls", listID),
				Placeholder("Search sessions…"),
				Class("min-h-[40px] w-full rounded-lg border-0 bg-bg px-3 py-2 text-sm text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
			),
			Div(
				g.Attr("role", "group"),
				g.Attr("aria-label", "Show sessions"),
				Class("flex flex-wrap gap-1.5"),
				g.Group(chips),
			),
		),
		Div(
			Class("flex flex-col gap-2 p-2"),
			g.If(!b.SharedRoom, chiefNewChatChip(b, surface)),
			Nav(
				g.Attr("aria-label", "Chief sessions"),
				Ul(
					ID(listID),
					g.Attr(chiefChipScrollAttr, ""),
					Class("flex flex-col gap-1"),
					g.Group(rows),
				),
			),
			P(
				g.Attr(chiefSessionsNoneAttr, ""),
				g.Attr("hidden", ""),
				g.Attr("role", "status"),
				Class("px-3 py-6 text-center text-sm text-muted"),
				g.Text(ChiefSessionsNoMatch),
			),
		),
		chiefSessionsScript(),
		// The ⋯ menus are placed by floatingMenuScript: the chief page carries
		// it already; the shell the panel lives in does not (it is bound once
		// per document, so a second copy is a no-op).
		g.If(surface == ChatSurfaceChiefPanel && !b.SharedRoom, floatingMenuScript()),
	)
	var count g.Node
	if n := chiefUnreadElsewhere(sessions); n > 0 {
		ns := strconv.Itoa(n)
		count = Span(
			g.Attr(chiefSessionsCountAttr, ns),
			Class("ml-1.5 inline-flex h-5 min-w-[1.25rem] items-center justify-center rounded-full bg-accent px-1.5 text-[11px] font-semibold tabular-nums text-on-accent"),
			Span(g.Attr("aria-hidden", "true"), g.Text(ns)),
			Span(Class("sr-only"), g.Text(", "+ns+" unread")),
		)
	}
	return Tab{ID: chiefSessionsTabID, Label: ChiefSessionsLabel, Count: count, Panel: panel}
}

// chiefIndexSheet is the chief's sheet on v's surface (chat_index.go): its
// Sessions tab first, then Your messages and What it worked on, and its
// script. The page's is #chat-index, fixed over the screen; the panel's is its
// own, absolute in the panel body's column. Nothing off a chief's chat.
func chiefIndexSheet(v AgentDetailView) g.Node {
	if v.ChiefChat == nil {
		return nil
	}
	id := chiefIndexID(v.Surface)
	tab := chiefSessionsTab(v.ChiefChat, v.Name, v.Sessions, v.Surface, id)
	sp := chatIndexSpec{ID: id, Label: chiefIndexLabel, HistoryPath: v.FocusHistory,
		TabsKey: "agent-chats:" + v.Name, Partial: chatIndexPartialPage, Sessions: &tab}
	if v.Surface == ChatSurfaceChiefPanel {
		sp.TabsKey, sp.Partial, sp.InPanel = "chief-panel-chats:"+v.Name, chatIndexPartialPanel, true
	}
	return g.Group{chatIndexSheetOf(sp), chatIndexScript()}
}

// chiefChip is one chat's row in the Sessions tab: a tap switches to it (a boosted
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
// are. A popover in the top layer, placed by floatingMenuScript, so the tab's
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

// chiefPopoverMenu is the Sessions tab's popover menu (a chip's ⋯, the "+" menu): in
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

// ChiefNewSessionLabel is the Sessions tab's new-chat control's words (and name).
const ChiefNewSessionLabel = "New session"

// chiefNewChatChip is the Sessions tab's "+ New session": a new side chat (or the newest one,
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
// panel body — opens the sheet again on its Sessions tab (chiefSessionsScript).
const chiefSessionsReopenKey = "muster.chief.v1.sessions-reopen"

// chiefSessionsScript is the Sessions tab's behaviour, bound once per document
// (htmx re-executes it on every swap that brings a tab; the guard leaves it
// bound once). The sheet itself — open, ✕, the backdrop, Escape, Tab kept
// inside — is chatIndexScript's.
//
//   - THE SEARCH AND THE CHIPS FILTER THE ROWS (see the file's header): a row
//     is shown when its words hold the search (its link's textContent and its
//     title attribute, in lower case) and it passes the pressed chip. With no
//     search and All pressed the archived rows are behind "Archived (n)" as
//     before; with either they are listed with the others and the toggle's row
//     is hidden. When nothing is left, "No sessions match".
//   - A chip is a toggle (aria-pressed), one pressed at a time: pressing the
//     pressed one (other than All) goes back to All.
//   - A TAP ON A ROW SHUTS THE SHEET as the switch starts (the answer replaces
//     the page or the panel body anyway; shutting at once is what makes the
//     pick feel immediate).
//   - "Archived (n)" toggles its chats' rows inline (aria-expanded).
//   - A ⋯ ACTION'S ANSWER OPENS THE SHEET AGAIN ON SESSIONS: the action's
//     request records the sheet (chiefSessionsReopenKey, sessionStorage, with
//     the time), and the page or panel body that answers opens it. A record
//     older than 15s, or one whose request failed, is dropped, so a later load
//     never opens it unasked.
func chiefSessionsScript() g.Node {
	return Script(g.Raw(`
(function () {
  var TAB = '[` + chiefChatsAttr + `]', SHEET = '` + chatIndexSheetAttr + `';
  var ROW = '` + chiefChatRowAttr + `', CHIP = '` + chiefChipAttr + `', KIND = '` + chiefChipKindAttr + `';
  var MENU = '` + chiefChatMenuAttr + `', UNREAD = '` + chiefUnreadAttr + `', MODEL = '` + chiefChipModelAttr + `';
  var ARCH = '` + chiefArchivedAttr + `', ARCH_ROW = '` + chiefArchivedChipAttr + `', ARCH_LI = '` + chiefArchivedToggleRowAttr + `';
  var SEARCH = '` + chiefSessionsSearchAttr + `', FILTER = '` + chiefSessionsFilterAttr + `', NONE = '` + chiefSessionsNoneAttr + `';
  var SESSIONS_TAB = '` + chiefSessionsTabID + `', KEY = '` + chiefSessionsReopenKey + `';
  if (window.__musterChiefSessions) return;
  window.__musterChiefSessions = true;
  function ss() { try { return window.sessionStorage; } catch (e) { return null; } }
  function clearReopen() { var s = ss(); if (s) { try { s.removeItem(KEY); } catch (e) {} } }

  // words: what a row's search matches — its link's text and its title (the
  // chat's own, which the primary's "Primary" label hides), by textContent
  // and an attribute's value: never markup.
  function words(r) {
    var a = r.querySelector('[' + ROW + ']');
    if (!a) return '';
    return ((a.textContent || '') + ' ' + (a.getAttribute('title') || '')).toLowerCase();
  }
  function passes(r, f) {
    if (f === 'unread') return !!r.querySelector('[' + UNREAD + ']');
    if (f === 'archived') return r.getAttribute(KIND) === 'archived';
    if (f.indexOf('model:') === 0) {
      var tag = r.querySelector('[' + MODEL + ']');
      return !!tag && tag.getAttribute(MODEL) === f.slice(6);
    }
    return true;
  }
  // apply shows tab's rows by its search and its pressed chip.
  function apply(tab) {
    var box = tab.querySelector('[' + SEARCH + ']');
    var q = box ? (box.value || '').trim().toLowerCase() : '';
    var on = tab.querySelector('[' + FILTER + '][aria-pressed="true"]');
    var f = on ? on.getAttribute(FILTER) : 'all';
    var plain = !q && f === 'all';
    var tg = tab.querySelector('button[' + ARCH + ']');
    var expanded = !!tg && tg.getAttribute('aria-expanded') === 'true';
    var rows = tab.querySelectorAll('[' + CHIP + ']'), n = 0;
    for (var i = 0; i < rows.length; i++) {
      var r = rows[i], ok;
      if (plain) ok = !r.hasAttribute(ARCH_ROW) || expanded;
      else ok = (!q || words(r).indexOf(q) >= 0) && passes(r, f);
      r.hidden = !ok;
      if (ok) n++;
    }
    var li = tab.querySelector('[' + ARCH_LI + ']');
    if (li) li.hidden = !plain;
    var none = tab.querySelector('[' + NONE + ']');
    if (none) none.hidden = n > 0;
  }
  function onSearch(e) {
    var t = e.target;
    if (!t || !t.matches || !t.matches('[' + SEARCH + ']')) return;
    var tab = t.closest(TAB);
    if (tab) apply(tab);
  }
  document.addEventListener('input', onSearch);
  // The search box's own clear (×) fires 'search', which does not bubble.
  document.addEventListener('search', onSearch, true);
  document.addEventListener('click', function (e) {
    var t = e.target;
    if (!t || !t.closest) return;
    var tab = t.closest(TAB);
    if (!tab) return;
    var chip = t.closest('[' + FILTER + ']');
    if (chip) {
      var all = tab.querySelector('[' + FILTER + '="all"]');
      var pick = chip.getAttribute('aria-pressed') === 'true' && chip !== all ? all : chip;
      var cs = tab.querySelectorAll('[' + FILTER + ']');
      for (var i = 0; i < cs.length; i++) cs[i].setAttribute('aria-pressed', cs[i] === pick ? 'true' : 'false');
      apply(tab);
      return;
    }
    var a = t.closest('button[' + ARCH + ']');
    if (a) {
      a.setAttribute('aria-expanded', a.getAttribute('aria-expanded') === 'true' ? 'false' : 'true');
      apply(tab);
      return;
    }
    if (t.closest('[' + ROW + ']') && window.muIndex) window.muIndex.close({ focus: false });
  });
  // reopen: a ⋯ action's answer has landed; its sheet opens again on Sessions.
  // evt: the settle that brought it, when one did — only a settle whose target
  // holds the sheet is the answer (a transcript refresh settling meanwhile is
  // not, and must not spend the record on the sheet about to be replaced).
  function reopen(evt) {
    var s = ss(), raw = null;
    if (s) { try { raw = s.getItem(KEY); } catch (e) {} }
    if (!raw) return;
    var v = null;
    try { v = JSON.parse(raw); } catch (e) {}
    if (!v || !(Date.now() - v.t < 15000)) { clearReopen(); return; }
    var d = v.id && document.getElementById(v.id);
    if (!d || !window.muIndex) return;
    var t = evt && evt.target;
    if (t && t !== document && t !== document.body && !(t.contains && t.contains(d))) return;
    clearReopen();
    var tab = d.querySelector('[role="tab"][data-tab="' + SESSIONS_TAB + '"]');
    if (tab && window.muTabs) window.muTabs.select(tab.closest('[data-tabs]'), tab, { persist: true });
    window.muIndex.open(d);
  }
  document.addEventListener('htmx:beforeRequest', function (e) {
    var el = e.target;
    if (!el || !el.closest || !el.closest('[' + MENU + ']')) return;
    var d = el.closest('[' + SHEET + ']');
    var s = ss();
    if (!d || !s) return;
    try { s.setItem(KEY, JSON.stringify({ id: d.id, t: Date.now() })); } catch (err) {}
  });
  document.addEventListener('htmx:responseError', clearReopen);
  document.addEventListener('htmx:sendError', clearReopen);
  // 🔴 A SWAPPED-IN SHEET IS OPENED AFTER THE SWAP SETTLES, NOT WHEN THIS
  // SCRIPT RUNS INSIDE IT: htmx's settle puts back the attributes the server
  // drew (hidden) over whatever ran in between, so a sheet opened here would be
  // open by the script's account and hidden by its attribute. afterSettle does
  // it; a full load, DOMContentLoaded (chatIndexScript, later in the document,
  // has run by then).
  document.addEventListener('htmx:afterSettle', reopen);
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', function () { reopen(); });
})();
`))
}

// chiefPageStrip is the chief PAGE's bar: a row of its own under the header,
// in the chat column's width, naming the chat on screen. Nothing off a chief's
// chat.
func chiefPageStrip(v AgentDetailView) g.Node {
	if v.ChiefChat == nil || v.Surface != ChatSurfacePage {
		return nil
	}
	return Div(
		g.Attr("data-chief-strip-row", ""),
		Class("flex-none border-b border-line bg-bg lg:pl-72"),
		Div(Class(contentMaxW()), chiefCurrentChat(v.ChiefChat, v.Sessions, ChatSurfacePage)),
	)
}

// chiefPanelContinue is the chief PANEL's "Continue in a new session", under
// its thread list (chiefThreadList), for the side chat the panel is on
// (chiefContinuePicker, where the New session picker is offered: runtimes,
// claudeModels). Every other chat action is on the Sessions tab's rows.
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
