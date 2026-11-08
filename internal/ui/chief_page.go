package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/agents"
)

// THE CHIEF'S PAGE, IN THE LAYOUT OF EVERY OTHER SESSION.
//
// An operator report (a phone screenshot at 390px): the chief's page looked and
// felt like a different app from a mirrored session's Rendered view. Its header
// had a second, chief-only STATUS ROW — the status dot, the ⋯, the Claude
// account, "↻ switched" and a large Terminal button — a floating "● N waiting"
// pill sat above the thread, the thread was a bordered card with a divider above
// the composer, and the working indicator was a bubble. A mirrored session has
// one header row, its messages straight on the page, its asks between the
// conversation and the composer and a one-line "working…".
//
// So the chief's page draws (chiefHeader):
//
//   - ONE header row: ☰, the status dot, the title, the asks badge, New chat,
//     Chat history, Focus history, and the ⋯ — at the row's END, where a
//     mirrored session's last control is;
//   - the status row's other controls IN THE ⋯ MENU (chiefMenuItems): the Claude
//     account, the failover ("Account switched" and its whole line — in the menu
//     itself, no second popover), the worker pool, and Terminal (the same
//     view-swap link the Rendered | Terminal switch draws, so the terminal still
//     opens in the kept frame);
//   - the "N waiting" chip as a count badge in the header (askerAsksBadge), and
//     the asks block under the transcript (agentChatPane).
//
// The transcript, the composer, the working row and the relay rows are not
// chief-only: every agent page draws them the mirrored session's way
// (agentChatPane, chatLogInner).
//
// 🔴 ONLY THE CHIEF, AND ONLY A CLAUDE-CODE ONE (chiefFolded). The status row is
// what every claude-code agent's page has, and a dispatched agent's page keeps it
// for now: its Terminal switch and account are pinned in place by a dozen specs
// (terminal, view-swap, switcher-dock, kinds, byo-credentials, …), and moving
// them there is a separate decision. A gateway chief has no account, failover or
// terminal — its second row is the model picker — and keeps its header too.

// chiefFolded reports whether v is drawn as the chief's page: the fleet's chief
// (its card says so — AgentCardView.IsChief, internal/api's one chief
// predicate), of the claude-code kind.
func chiefFolded(v AgentDetailView) bool {
	return v.Card.ID != 0 && v.Card.IsChief && agents.ResolveKind(v.Kind) == agents.KindClaudeCode
}

// chiefHeader is the chief's page header: one row (see above).
//
// ⚠ IT KEEPS THE PAGE HEADER'S FRAME — sticky, the sidebar offset, the column's
// width ladder (contentMaxW) — so from `sm` up its left edge is the chat
// column's, as agentDetailHeader's is.
//
// 🔴 BELOW `sm` ITS GUTTER IS px-2 AND ITS TOOLS ARE gap-1, AND THAT IS
// MEASURED ROOM, NOT TASTE. The row holds five 44px controls (☰, New chat,
// Chat history, Focus history, ⋯), the 12px status dot and — while asks are
// hidden — the badge. With the page header's px-4 and gap-3 tools, at 390px the
// badge left the title 0px: "chief" drew as its "…" alone (the e2e
// chief-fold-in shot). A mirrored session's header is px-2 for the same reason.
func chiefHeader(v AgentDetailView) g.Node {
	return Header(
		g.Attr("data-chief-header", ""),
		Class("sticky top-0 z-20 border-b border-line bg-bg/80 backdrop-blur lg:pl-72"),
		Div(
			// py-1.5: a mirrored session's header's (MirrorViewPage) — the row is
			// its 44px controls, so the two headers are one height.
			Class(contentMaxW()+" flex items-center gap-2 px-2 py-1.5 sm:px-4"),
			sidebarOpenButton(),
			// The status dot (and any badge the strip carries), live: the same
			// strip the second row drew, refreshing itself (agentStrip).
			agentStrip(v.Card),
			agentTitle(v),
			sessionTagsHeadNode(SessionTagTarget{AgentID: v.ID}, v.Tags),
			g.If(v.AsksSrc != "", askerAsksBadge()),
			agentHeaderToolsWith(v, true),
			Span(ID(AgentMenuSlot), Class("inline-flex shrink-0"), agentActionsMenuWith(v.Card, "", chiefMenuItems(v))),
		),
		// The Terminal item is a view-swap link (viewToggle's): the script that
		// opens it in the kept frame travels with whatever draws such a link.
		viewFrameScript(),
		updatingChip("agent-updating", "right-3 top-full mt-1"),
	)
}

// chiefMenuItems is what the status row held, as the ⋯ menu's first items.
//
// The account, the failover and the pool are STATEMENTS, not actions, so they
// are a group of text rather than menu items: the failover's whole line ("from
// <account> · <reason> · resets …") is drawn under its marker, in the menu —
// the second row's "↻ switched" opened it in a popover of its own, which inside
// a menu would be a popover in a popover. Terminal is the action.
func chiefMenuItems(v AgentDetailView) g.Node {
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
		// The Rendered | Terminal switch's link (viewToggle), as a menu item: the
		// same id and data-view-swap, so the terminal opens in the kept frame
		// (view_swap.go) and is a plain link without script. The click closes the
		// menu first — it is in the top layer, over the frame it opens.
		A(
			ID("view-terminal"),
			Href(AgentTerminalPath(v.Name)),
			hx("hx-boost", "false"),
			g.Attr("data-view-swap", ViewTerminal),
			g.Attr("data-view-toggle", ""),
			g.Attr("role", "menuitem"),
			g.Attr("hx-on:click", "var m=this.closest('[popover]');if(m){try{m.hidePopover()}catch(e){}}"),
			Class("press flex min-h-[44px] w-full items-center gap-2 rounded-lg px-3 text-sm font-medium text-fg transition hover:bg-s2"),
			g.Raw(`<svg class="h-4 w-4 shrink-0 text-fg2" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M4 17l6-5-6-5"/><path d="M12 19h8"/></svg>`),
			g.Text("Terminal"),
		),
	}
}
