package ui

import (
	"io"
	"net/url"
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE CHIEF'S PRIMARY AND SIDE CHATS (internal/agents chief_chats.go;
// docs/chief.md#primary-and-side-chats), as the chief page and the chief panel
// draw them:
//
//   - the CHAT BAR above the transcript (chiefChatBar): which chat this is
//     (Primary, Side chat, Archived), its context chip, and the actions — a
//     labelled "New side chat" on every chat, "Fresh primary" on the primary,
//     Archive / Unarchive on a side chat;
//   - the SWITCHER rows (chiefChatRowExtras): the primary first, then the side
//     chats, each with its last activity and an unread dot, the archived ones
//     apart (agents.SwitcherOrder decides the order; the renderers only place
//     it).
//
// 🔴 THE SAME CONTROLS ON BOTH SURFACES, ANSWERED THE WAY EACH SURFACE NEEDS:
// on the page an action's answer is HX-Redirect to the chat it leaves you on;
// in the panel it is the panel body re-rendered on that chat (?surface=
// chief-panel), so the operator never leaves the grid. The routes are the same.

// ChiefChatBar is the chat bar's view: set on a chief's chat only.
type ChiefChatBar struct {
	AgentID   int64
	AgentName string
	SessionID int64
	// Primary: this chat is the chief's primary (its default session).
	Primary bool
	// Archived: this chat is archived (an old primary, or a side chat put away).
	Archived bool
}

// The chat bar's and the switcher's data attributes: what both test tiers read.
const (
	chiefChatBarAttr      = "data-chief-chat-bar"
	chiefChatKindAttr     = "data-chief-chat-kind"
	chiefSideChatAttr     = "data-chief-side-chat"
	chiefFreshPrimaryAttr = "data-chief-fresh-primary"
	chiefArchiveAttr      = "data-chief-archive"
	chiefUnarchiveAttr    = "data-chief-unarchive"
	chiefUnreadAttr       = "data-chief-unread"
	chiefContextID        = "chief-context"
	chiefContextChipID    = "chief-context-chip"
	chiefContextPopID     = "chief-context-detail"
)

// The kinds a chat bar names (data-chief-chat-kind), and what it says.
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

// chiefChatKind is the bar's kind for v.
func chiefChatKind(b ChiefChatBar) (kind, label string) {
	switch {
	case b.Primary:
		return ChiefChatKindPrimary, "Primary chat"
	case b.Archived:
		return ChiefChatKindArchived, "Archived chat"
	}
	return ChiefChatKindSide, "Side chat"
}

// chiefChatAction is one of the bar's actions: a labelled button in a form
// posting to path, answered per surface (see the file's header).
func chiefChatAction(surface ChatSurface, path, attr, label, confirm string, primary bool) g.Node {
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
		Button(Type("submit"), g.Attr(attr, ""), Class(cls), g.Text(label)),
	)
}

// ChiefSideChatButton is the labelled "New side chat" control on a chief's
// chat, for surface. 🔴 VISIBLE TEXT, NOT AN ICON: the icon-only ＋ it replaces
// on the chief page carried its name only in aria-label, and the operator could
// not find it.
func ChiefSideChatButton(agentID int64, surface ChatSurface) g.Node {
	return chiefChatAction(surface, ChiefSideChatPath(agentID), chiefSideChatAttr, "New side chat", "", true)
}

// chiefChatBar is the bar above a chief chat's transcript; nothing for a chat
// that is not a chief's.
func chiefChatBar(v AgentDetailView) g.Node {
	b := v.ChiefChat
	if b == nil {
		return nil
	}
	kind, label := chiefChatKind(*b)
	actions := []g.Node{ChiefSideChatButton(b.AgentID, v.Surface)}
	switch kind {
	case ChiefChatKindPrimary:
		actions = append(actions, chiefChatAction(v.Surface, ChiefFreshPrimaryPath(b.AgentID), chiefFreshPrimaryAttr,
			"Fresh primary", FreshPrimaryConfirm, false))
	case ChiefChatKindSide:
		actions = append(actions, chiefChatAction(v.Surface, ChiefArchivePath(b.AgentID, b.SessionID, true), chiefArchiveAttr,
			"Archive", "", false))
	case ChiefChatKindArchived:
		actions = append(actions, chiefChatAction(v.Surface, ChiefArchivePath(b.AgentID, b.SessionID, false), chiefUnarchiveAttr,
			"Unarchive", "", false))
	}
	return Div(
		g.Attr(chiefChatBarAttr, ""),
		g.Attr(chiefChatKindAttr, kind),
		// flex-wrap: at a phone's width the actions drop under the kind and the
		// chip rather than pushing the row past the screen.
		Class("flex flex-none flex-wrap items-center gap-x-2 gap-y-1.5"),
		Div(
			Class("flex min-w-0 flex-1 items-center gap-2"),
			Span(Class("shrink-0 text-xs font-semibold text-fg2"), g.Text(label)),
			chiefContextSlot(b.AgentName, b.SessionID),
		),
		Div(Class("flex shrink-0 items-center gap-1.5"), g.Group(actions)),
	)
}

// chiefContextSlot is where the context chip lives: an empty slot that loads
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

// chiefArchivedLabel is the heading over the archived chats in a switcher.
const chiefArchivedLabel = "Archived"
