package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/agents"
)

// A CLAUDE-CODE AGENT'S PAGE, IN THE LAYOUT OF A MIRRORED SESSION.
//
// An operator report (a phone screenshot of the chief at 390px): an agent's
// page looked and felt like a different app from a mirrored session's Rendered
// view. Its header had a second STATUS ROW — the status dot, the ⋯, the Claude
// account, "↻ switched" and a large Terminal button — a floating "● N waiting"
// pill sat above the thread, the thread was a bordered card with a divider above
// the composer, and the working indicator was a bubble. A mirrored session has
// one header row, its messages straight on the page and a one-line "working…".
//
// So a claude-code agent's page — the chief's and every dispatched one's — draws
// (foldedHeader):
//
//   - ONE header row: ☰, the status dot, the title, the asks badge, the Open
//     button (its pages and asks, plugin_chip.go), New chat, Chat history,
//     What it worked on, and the ⋯ — at the row's END, where a mirrored
//     session's last control is. Below `sm` the three tools are compact
//     (agentHeaderTools), so the title keeps its room with the Open button
//     drawn. The CHIEF's row has the one list icon a mirrored session's has
//     instead of the three (its sheet holds its chats: chief_chats.go), and
//     its ⋯ says how much memory the chat uses when that is high
//     (chiefMemorySlot);
//   - the status row's other controls IN THE ⋯ MENU (foldedMenuItems): the Claude
//     account, the failover ("Account switched" and its whole line — in the menu
//     itself, no second popover), the worker pool, and Terminal (the same
//     view-swap link the Rendered | Terminal switch draws, so the terminal still
//     opens in the kept frame);
//   - the agent's open asks as a count badge in the header (AskerAsksBadge),
//     which opens the Asks page filtered to them; the page draws no asks block.
//
// The transcript, the composer, the working row and the relay rows are every
// agent page's, whatever its kind (agentChatPane, chatLogInner).
//
// 🔴 EVERY CLAUDE-CODE AGENT, NOT ONLY THE CHIEF (operator decision on PR #527,
// which first folded the chief alone). A GATEWAY agent's page — chief or not —
// keeps the two-row header: it has no account, failover or terminal to fold,
// and its second row is the model picker, which a menu cannot hold.

// headerFolded reports whether v's page draws the folded header: a claude-code
// agent's. It is the ONLY header a claude-code agent's page has (round 0 D1
// deleted the two-row one it used to fall back to without a card); a view with
// no card (a minimal test view — every page the server draws has one) draws
// the row without the status strip and the ⋯ menu, which both need the card.
func headerFolded(v AgentDetailView) bool {
	return agents.ResolveKind(v.Kind) == agents.KindClaudeCode
}

// foldedHeader is a claude-code agent's page header: one row (see above).
//
// ⚠ IT KEEPS THE PAGE HEADER'S FRAME — sticky, the sidebar offset, the column's
// width ladder (contentMaxW) — so from `sm` up its left edge is the chat
// column's, as agentDetailHeader's is.
//
// 🔴 BELOW `sm` ITS GUTTER IS px-2, ITS GAPS gap-1 AND ITS TOOLS COMPACT, AND
// THAT IS MEASURED ROOM, NOT TASTE. The row holds the ☰, the Open button, the
// three tools (36px wide there: agentHeaderTools), the ⋯, the 12px status dot
// and — while asks are open — the asks badge. With the page header's px-4
// and gap-3 tools, at 390px the badge left the title 0px: "chief" drew as its
// "…" alone (the e2e chief-fold-in shot). A mirrored session's header is px-2
// for the same reason.
func foldedHeader(v AgentDetailView) g.Node {
	return Header(
		g.Attr("data-folded-header", ""),
		Class(StickyHeaderClass+" lg:pl-72"),
		Div(
			// py-1.5: a mirrored session's header's (MirrorViewPage) — the row is
			// its 44px controls, so the two headers are one height.
			// gap-1 below `sm` (headerPhoneTools: the row's room at 390px).
			Class(contentMaxW()+" flex items-center gap-1 px-2 py-1.5 sm:gap-2 sm:px-4"),
			sidebarOpenButton(),
			// The status dot (and any badge the strip carries), live: the same
			// strip the second row drew, refreshing itself (agentStrip).
			g.If(v.Card.ID != 0, agentStrip(v.Card)),
			foldedTitle(v),
			sessionTagsHeadNode(SessionTagTarget{AgentID: v.ID}, v.Tags),
			g.If(v.AsksSrc != "", agentAsksBadge(v.Asks, v.Open)),
			// The agent's Open button (its pages), while it has any
			// (plugin_chip.go).
			previewButtonSlot(v.Open),
			agentHeaderToolsWith(v, true),
			g.If(v.Card.ID != 0, Span(ID(AgentMenuSlot), Class("inline-flex shrink-0"), agentActionsMenuWith(v.Card, "", foldedMenuItems(v)))),
		),
		// The Terminal item is a view-swap link (viewToggle's): the script that
		// opens it in the kept frame travels with whatever draws such a link.
		viewFrameScript(),
		updatingChip("agent-updating", "right-3 top-full mt-1 z-10"),
	)
}

// foldedTitle is the folded header's title: agentTitle. A chief's chat drew
// its context chip under it before #871; how much context the chat uses is
// in the ⋯ menu now, and only when it is high (chiefMemorySlot).
func foldedTitle(v AgentDetailView) g.Node { return agentTitle(v) }

// foldedChiefMemory is a claude-code chief's ⋯ memory slot, among the
// menu's other extras (so not its divider's peer); nil off a chief's chat page.
func foldedChiefMemory(v AgentDetailView) g.Node {
	if v.ChiefChat == nil || v.Surface != ChatSurfacePage {
		return nil
	}
	return chiefMemorySlot(v.ChiefChat.AgentName, v.ChiefChat.SessionID, false)
}

// foldedMenuItems is what the status row held, as the ⋯ menu's first items.
//
// The account, the failover and the pool are STATEMENTS, not actions, so they
// are a group of text rather than menu items: the failover's whole line ("from
// <account> · <reason> · resets …") is drawn under its marker, in the menu —
// the second row's "↻ switched" opened it in a popover of its own, which inside
// a menu would be a popover in a popover. Terminal is the action.
func foldedMenuItems(v AgentDetailView) g.Node {
	return g.Group{
		Div(
			g.Attr("role", "group"),
			g.Attr("aria-label", "Claude account"),
			Class("flex flex-col gap-1 px-3 py-2 text-xs"),
			Div(
				Class("flex min-w-0 items-center gap-2"),
				Span(Class("shrink-0 text-muted"), g.Text("Claude account")),
				Span(g.Attr("data-cc-account", v.CCAccount), g.Attr("title", "Claude account: "+v.CCAccount),
					Class("ml-auto min-w-0 truncate font-medium text-fg2"), g.Text(v.CCAccount)),
			),
			g.If(v.LastFailover != "", Div(
				g.Attr("data-failover", ""),
				Class("flex items-center gap-1 font-medium text-st-warning-fg"),
				Span(g.Attr("aria-hidden", "true"), g.Text("↻")),
				g.Text("Account switched"),
			)),
			g.If(v.LastFailover != "", P(g.Attr("data-failover-detail", ""), Class("[overflow-wrap:anywhere] text-fg2"), g.Text(v.LastFailover))),
			g.If(v.WorkerPool != "", Div(
				Class("flex min-w-0 items-center gap-2"),
				Span(Class("shrink-0 text-muted"), g.Text("Worker pool")),
				Span(g.Attr("data-worker-pool", v.WorkerPool), Class("ml-auto min-w-0 truncate text-fg2"), g.Text(v.WorkerPool)),
			)),
		),
		// A chief's chat: its memory line, when its context is high
		// (chiefMemorySlot).
		foldedChiefMemory(v),
		// The Rendered | Terminal switch's link (viewToggle), as a menu item: the
		// same id and data-view-swap, so the terminal opens in the kept frame
		// (view_swap.go) and is a plain link without script. The view swap's
		// click handler closes the menu first — it is in the top layer, over the
		// frame it opens (viewFrameScript).
		A(
			ID("view-terminal"),
			Href(AgentTerminalPath(v.Name)),
			hx("hx-boost", "false"),
			g.Attr("data-view-swap", ViewTerminal),
			g.Attr("data-view-toggle", ""),
			g.Attr("role", "menuitem"),
			Class("press flex min-h-[44px] w-full items-center gap-2 rounded-lg px-3 text-sm font-medium text-fg transition hover:bg-s2"),
			g.Raw(`<svg class="h-4 w-4 shrink-0 text-fg2" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M4 17l6-5-6-5"/><path d="M12 19h8"/></svg>`),
			g.Text("Terminal"),
		),
	}
}
