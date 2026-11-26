package ui

import (
	"io"
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// ---------------------------------------------------------------------------
// THE CHIEF SLIDE-OUT
//
// A right-side, resizable panel for talking to the chief agent without leaving
// the tmux grid. Its width and open/closed state are persisted and restored.
//
// 🔴 THREE CONSTRAINTS THIS ARC HAS ALREADY PAID FOR. NONE IS STYLISTIC, AND
// EACH ONE NAMES A SHIPPED DEFECT:
//
//  1. THE PANEL CANNOT HOST ITS OWN PERSISTENCE SCRIPT. #chief-panel-body is an
//     `hx-swap: innerHTML` target, so a <script> inside it is destroyed by the
//     very swap it exists to survive. chiefPanelScript therefore lives in the
//     PAGE SHELL (components.go's Page), beside the other shell scripts, and the
//     panel body carries only DOM.
//
//  2. THE STORAGE KEY IS A STABLE REFERENCE, NEVER A DESCRIPTION. Rank 11
//     shipped a drawer keyed on an archived COUNT — a number that changes on
//     exactly the swaps the memory was meant to survive, so it forgot precisely
//     when it mattered. These keys are built from the panel's own ELEMENT ID,
//     which is a constant of this file.
//
//  3. BIND ONCE, AND LISTEN ON `document`, NOT `document.body`. A once-guard
//     plus a body-scoped listener is worse than either alone: an hx-boost body
//     swap kills the listener while the guard suppresses the rebind, leaving a
//     panel that is permanently inert and a console with nothing in it.
//
// 🔴 AND IT DOES NOT SWALLOW INTERACTIONS IT ONLY OBSERVES — rank 11's mutant
// PD. The document-level click handler returns immediately for anything that is
// not one of this panel's own controls, and calls preventDefault on nothing
// else, so a <summary> click still toggles its <details> and a card's view
// toggle still flips.
//
// 🔴 IT RE-MOUNTS THE EXISTING AGENT CHAT RATHER THAN BUILDING A SECOND ONE.
// The body is agentChatPane + agentChatScript — the same #chat-log fed by
// GET /ui/agents/{name}/chat-log on `sse:chat.reply`, and the same WebSocket
// send path. A second chat would be a second transcript, a second unread ledger
// and a second place for a streaming bug to live.
// ---------------------------------------------------------------------------

const (
	// chiefPanelID is BOTH the element id and the reference every stored key is
	// built from. One constant, so the memory cannot come to be keyed on
	// something that describes the panel's contents.
	chiefPanelID     = "chief-panel"
	chiefPanelBodyID = "chief-panel-body"

	// chiefPanelActionsID is the TITLE-BAR slot the two launchers land in.
	//
	// 🔴 IT IS A SLOT FILLED OUT-OF-BAND, NOT MARKUP THE SHELL CAN RENDER, AND THE
	// REASON IS THE AGENT ID. ChiefPanel() is rendered by the shell on every tab,
	// and the shell deliberately does NOT resolve the chief — that lookup is what
	// makes the panel body lazy, and paying it on every page load would undo the
	// whole point. The new-thread control needs the chief's row id (it reuses
	// POST /agents/{id}/sessions), so it can only be built where that id is known:
	// in the body partial. The body's response therefore carries an
	// `hx-swap-oob` copy of this container, which is the established idiom in this
	// package (18 other uses in components.go).
	chiefPanelActionsID = "chief-panel-actions"

	// chiefQuickActionAttr carries a quick action's prompt text. The attribute IS
	// the payload: the delegated handler reads it off the element it was clicked
	// on, so a button rebuilt by a swap needs no rebind (constraint 3).
	chiefQuickActionAttr = "data-chief-quick-action"
	// chiefIntroAttr marks the zero-message intro block.
	chiefIntroAttr = "data-chief-intro"
	// chiefActiveSessionAttr carries the session id the SERVER just answered with
	// (what both test tiers read to know which chat the panel is on).
	//
	// 🔴 NOTHING IN THE BROWSER REMEMBERS IT ANY MORE. The panel used to keep the
	// picked thread in localStorage and ask for it on the next open — a memory the
	// chief PAGE never read, so the page and the panel disagreed about where the
	// owner had been. The server keeps that now, for both (internal/api
	// chiefOpenSession, chat_sessions.viewed_at): the body asks with no ?session=
	// on every open and is answered with the chat the owner was on last.
	chiefActiveSessionAttr = "data-chief-active-session"

	// The control vocabulary. Spelled as constants because this renderer, the Go
	// tests and the Playwright specs all assert on them.
	chiefPanelAttr        = "data-chief-panel"
	chiefPanelToggleAttr  = "data-chief-panel-toggle"
	chiefPanelCloseAttr   = "data-chief-panel-close"
	chiefPanelResizeAttr  = "data-chief-panel-resize"
	chiefPanelOpenAttr    = "data-chief-panel-open"
	chiefPanelMissingAttr = "data-chief-panel-missing"

	// chiefPanelBodyLoadedAttr marks a body element that a swap has filled. It is
	// set by the CLIENT and never rendered by the server: its whole job is to tell
	// a fresh element (no mark, must load) from a refreshed one (marked, must not
	// reload), and a server-rendered mark would make every body look already-loaded.
	chiefPanelBodyLoadedAttr = "data-chief-panel-loaded"

	// chiefPanelOpenEvent is dispatched on `document` when the panel opens. The
	// body's hx-trigger listens for it, which is what makes the chat LAZY: a
	// reader who never opens the panel never pays for the transcript fetch or for
	// the chat script.
	chiefPanelOpenEvent = "muster:chief-open"

	// Width bounds, in px. The default is wide enough for a conversation without
	// covering the card column it sits beside.
	chiefPanelMinWidth     = 320
	chiefPanelMaxWidth     = 900
	chiefPanelDefaultWidth = 460
)

// chiefQuickActions is THE list. One place, in Go.
//
// 🔴 A TAP FILLS THE INPUT AND FOCUSES IT. IT DOES NOT SEND. The panel is used
// from a phone, where a stray tap on a control that dispatched an orchestrator
// turn would spend a paid turn with no undo — so the operator always presses Send
// themselves.
//
// 🔴 AND "NO UNDO" IS THE WHOLE REASON, SO THE PREFILL MUST NOT DESTROY A DRAFT
// EITHER. The first version of this wrote the prompt over #chat-input
// unconditionally, which means a stray tap still cost something irrecoverable — the
// half-typed message — on the one surface where the intro renders, an EMPTY thread,
// with the three buttons sitting directly above the box at 390px. A non-empty draft
// is therefore kept and the prompt appended to it; see the click handler in
// chiefPanelScript.
//
// The prefill goes into the EXISTING #chat-input and rides #chat-form's
// existing WebSocket submit: the optimistic bubble, the mid-stream refresh guard
// and data-session are all already correct there, and a second write path would
// have to re-derive every one of them.
//
// ⚠ THE STRINGS ARE THE OPERATOR'S OWN WORDS AND THE ORDER IS THEIRS.
// TestTheChiefQuickActionsAreTheLedgeredThree pins this slice two-way, so adding
// a fourth without a test entry — or leaving an entry naming nothing — fails. It is
// `package ui` and reads this variable directly: unexported on purpose, because an
// exported accessor for one same-package test is public API nobody outside needs.
var chiefQuickActions = []string{
	"What needs my attention?",
	"Recap the fleet",
	"What's blocked right now?",
}

// ChiefPanelView is what the panel body needs.
type ChiefPanelView struct {
	// Configured is false when no agent carries the chief display name. The panel
	// then renders an honest explanation rather than an empty chat.
	Configured bool
	// Reason explains a missing chief, shown verbatim.
	Reason string
	// Chat is the chief's agent-detail view, re-used as-is.
	Chat AgentDetailView
	// Prefill is a draft for the chat box (the fleet digest's "Ask the chief
	// about this", fleet_digest.go): the panel script puts it in the box once
	// the body lands, after any draft already there, and sends nothing.
	Prefill string
}

// ChiefPanelToggle is the launcher, rendered by the tmux panel body.
//
// ⚠ IT IS RENDERED INSIDE A SWAP TARGET AND THAT IS FINE. It is a plain
// <button> carrying a data attribute; the handler is delegated on `document`, so
// a button destroyed and rebuilt on every panel refresh keeps working without a
// rebind. That is the whole reason constraint 3 is worth obeying.
func ChiefPanelToggle() g.Node {
	return Button(
		Type("button"),
		g.Attr(chiefPanelToggleAttr, ""),
		g.Attr("aria-controls", chiefPanelID),
		g.Attr("aria-expanded", "false"),
		Class("press min-h-[44px] inline-flex items-center gap-1 rounded-md border border-edge bg-s1 px-2 py-1 text-[11px] font-medium text-fg2 transition hover:bg-s2 hover:text-fg"),
		g.Text("Ask chief"),
	)
}

// ChiefPanelSubtitle is the line under "Chief" in its panel's header: what
// the chief does, in words (the jargon audit's F045; it said "the fleet's
// orchestrator").
const ChiefPanelSubtitle = "keeps an eye on all your agents"

// ChiefPanel is the slide-out itself, rendered at BODY level by the shell.
//
// 🔴 BODY LEVEL IS WHAT SATISFIES CRITERION 3. #panel-tmux replaces its entire
// innerHTML every 60s and on every snapshot push; the other four tab panels swap
// independently. A panel rendered inside <main> would be destroyed by any of
// them — along with the operator's half-typed message and the live WebSocket —
// so it is a sibling of the FABs and popovers, which are body-level for exactly
// this reason.
//
// fleet draws the fleet digest card's slot (Features.FleetDigest,
// MUSTER_FLEET_DIGEST); off, the panel is exactly as it was before the digest.
// propose (Features.FleetPropose, MUSTER_FLEET_ORCHESTRATION=propose) draws
// the card's Proposed group in its skeleton too, and the slot the step-up
// dialog is loaded into when an approval needs a fresh sign-in.
func ChiefPanel(fleet, propose bool) g.Node {
	return Aside(
		ID(chiefPanelID),
		g.Attr(chiefPanelAttr, ""),
		// 🔴 THE PANEL IS ALWAYS IN THE DOM AND `inert` + translated OFF-SCREEN
		// WHEN SHUT, rather than `hidden`. `hidden` would drop the CSS transition
		// (there is nothing to animate from) and, more importantly, would unmount
		// the chat's scroll position every time the operator glanced away. `inert`
		// is what keeps a closed panel out of the tab order and off a screen
		// reader's radar, which `translate-x-full` alone does not do.
		g.Attr("inert", "inert"),
		g.Attr("aria-label", "Chief"),
		Class("fixed inset-y-0 right-0 z-40 flex w-full max-w-full translate-x-full flex-col border-l border-line bg-bg shadow-2xl transition-transform duration-200 sm:w-[28rem]"),
		// The resize grip: a thin strip down the left edge.
		//
		// ⚠ `role=separator` + `aria-orientation=vertical` + tabindex is the
		// accessible shape of a splitter, and the keyboard half is not optional —
		// a drag-only control is unreachable without a pointer. Left/Right arrows
		// move it; the script owns the step.
		Div(
			g.Attr(chiefPanelResizeAttr, ""),
			g.Attr("role", "separator"),
			g.Attr("aria-orientation", "vertical"),
			g.Attr("aria-label", "Resize the chief panel"),
			g.Attr("tabindex", "0"),
			Class("absolute inset-y-0 left-0 z-10 w-1.5 cursor-col-resize bg-transparent transition hover:bg-accent/40 focus:bg-accent/60 focus:outline-none"),
		),
		// ⚠ A <div>, NOT A <header>, AND THAT IS A GUARD'S FINDING RATHER THAN A
		// preference. This panel is rendered into the SHELL, which serves every tab
		// URL from one document and already has exactly one <header>;
		// TestHeaderAndContentShareOneWidth and TestRouteHeaderMatchesItsOwnContentColumn
		// both locate that header by being the only one, so a second banner element
		// here broke them on both counts. The title bar of a slide-out is not the
		// document's banner in any case.
		Div(
			Class("flex flex-none items-center justify-between gap-2 border-b border-line px-3 py-2"),
			Div(
				Class("flex items-baseline gap-2"),
				Span(Class("text-sm font-semibold text-fg"), g.Text("Chief")),
				Span(Class("text-[10px] text-muted"), g.Text(ChiefPanelSubtitle)),
			),
			// The launcher slot: the list icon (or new-thread), filled out of band by the
			// body partial (see chiefPanelActionsID). It renders EMPTY here rather
			// than with disabled placeholders — a control that looks tappable and
			// does nothing until a fetch lands is worse than one that appears when
			// it works, and the panel is shut (and `inert`) until the body loads
			// anyway.
			Div(ID(chiefPanelActionsID), Class("ml-auto flex items-center gap-0.5")),
			Button(
				Type("button"),
				g.Attr(chiefPanelCloseAttr, ""),
				g.Attr("aria-label", "Close the chief panel"),
				Class("press inline-flex h-11 w-11 items-center justify-center rounded-md text-muted transition hover:bg-s2 hover:text-fg"),
				g.Text("✕"),
			),
		),
		// The fleet digest (fleet_digest.go): pinned above whichever chat the
		// body below shows, in a slot of its own, so a thread switch swaps the
		// chat and leaves the card.
		g.If(fleet, fleetDigestSlot(propose)),
		g.If(fleet && propose, Div(ID(fleetStepUpSlotID))),
		// The body: an hx-swap target, fetched the first time the panel opens and
		// on every open after that.
		//
		// 🔴 NO `once` ON THE TRIGGER, DELIBERATELY. A panel opened, left for an
		// hour and opened again should show the conversation as it is NOW; `once`
		// would show it as it was the first time, with the live `sse:chat.reply`
		// refresh inside it being the only thing that had moved. Re-fetching on
		// each open costs one request the operator explicitly asked for.
		Div(
			ID(chiefPanelBodyID),
			Class("flex min-h-0 flex-1 flex-col overflow-hidden p-3"),
			hx("hx-get", "/ui/chief/panel"),
			// 🔴 SPELLED AS A LITERAL, NOT CONCATENATED WITH chiefPanelOpenEvent.
			// The hx-trigger ledger (api.TestTheTmuxChangedSeamIsWiredEndToEnd) folds
			// literals and REFUSES anything it cannot fold, because a trigger it
			// cannot read is a subscription it cannot verify — it reported this
			// function by name the moment it was a concatenation. The constant is
			// still the single name the JS dispatches, and
			// TestTheChiefPanelTriggerAndTheDispatchedEventAreOneName pins the two
			// together, so the literal and the constant cannot silently disagree.
			hx("hx-trigger", "muster:chief-open from:document"),
			hx("hx-target", "this"),
			hx("hx-swap", "innerHTML"),
			P(Class("text-xs text-muted"), g.Text("Loading…")),
		),
	)
}

// RenderChiefPanelBody writes the panel body partial.
func RenderChiefPanelBody(w io.Writer, v ChiefPanelView) error {
	return chiefPanelBody(v).Render(w)
}

// chiefPanelBody is the chat, or an honest account of why there is none.
func chiefPanelBody(v ChiefPanelView) g.Node {
	if !v.Configured {
		reason := v.Reason
		if reason == "" {
			reason = "No agent carries the chief display name."
		}
		return g.Group{
			// 🔴 THE SLOT IS CLEARED, NOT SKIPPED. hx-swap-oob replaces the element it
			// names, so rendering an EMPTY actions container is what removes launchers
			// a previous (configured) render put there. Omitting the fragment would
			// leave a new-thread button in the title bar of a panel that has no agent
			// to create a thread on.
			chiefPanelActions(v),
			Div(
				g.Attr(chiefPanelMissingAttr, ""),
				Class("space-y-2 text-xs leading-relaxed text-muted"),
				P(g.Text(reason)),
				// 🔴 IT SAYS WHAT IS MISSING AND WHERE TO FIX IT. "Chief is
				// unavailable" with no further text is the shape that gets reported as
				// a broken feature; naming the DISPLAY NAME is what makes it a
				// configuration statement a reader can act on.
				P(g.Text("Dispatch an agent and set its display name to “chief”, then reopen this panel. "+
					"The agent's own slug name is generated and does not have to be “chief”.")),
			),
		}
	}
	return g.Group{
		chiefPanelActions(v),
		Div(
			// 🔴 `relative` IS LOAD-BEARING, NOT COSMETIC. It makes THIS element the
			// containing block for the chief's sheet's `absolute inset-0`
			// (chiefIndexSheet), which is how the sheet becomes a part of the panel's
			// own column rather than a second viewport-relative overlay. Drop it and the
			// sheet anchors to whatever ancestor happens to be positioned — which,
			// because ChiefPanel carries a transform, is the panel: it would still LOOK
			// right while being anchored to something this file does not control.
			// `overflow-hidden` clips the sheet to this column: the panel's own
			// overflow-hidden would not, since the column sits inside the panel's
			// padding (#chief-panel-body's p-3).
			Class("relative flex min-h-0 flex-1 flex-col overflow-hidden"),
			// The session the SERVER resolved. See chiefActiveSessionAttr.
			g.Attr(chiefActiveSessionAttr, strconv.FormatInt(v.Chat.ActiveSessionID, 10)),
			// The chat on screen, named over the transcript, where the page names
			// it (chief_chats.go chiefCurrentChat).
			chiefCurrentChat(v.Chat.ChiefChat, v.Chat.Sessions, ChatSurfaceChiefPanel),
			// The EXISTING chat pane, unmodified: #chat-log fed by
			// /ui/agents/{name}/chat-log on sse:chat.reply, plus #chat-form's WebSocket.
			agentChatPane(v.Chat),
			// 🔴 THE CHAT SCRIPT LIVES IN THE PANEL BODY AND THE PERSISTENCE SCRIPT
			// DOES NOT — the two are opposite cases and the difference is not a
			// judgement call. agentChatScript is BUILT to be re-executed: it guards its
			// document-lifetime hooks on window.__cgChatGlobals and its per-form wiring
			// on the form's own data-cg-chat marker, so running it again after a swap
			// re-binds the fresh form and does nothing else. chiefPanelScript keeps the
			// panel's OPEN/WIDTH memory, which is the thing the swap exists to survive,
			// so it cannot be here (constraint 1).
			//
			// ⚠ IT IS ALSO WHAT MAKES THE CHAT LAZY. This script is several hundred
			// lines including a markdown renderer; putting it in the shell would charge
			// every page load in the app for a panel most of them never open.
			// 🔴 THE BUBBLE CLAMP, SO A LONG CHIEF REPLY SHOWS ITS END: the panel's
			// bubbles are chatBubbleOf's, so a reply carries data-clamp-from="tail"
			// and is drawn with its last lines, "Show more" above it
			// (bubble_clamp.go). Before agentChatScript, so the transcript the
			// swap brought in is clamped before that script pins the log to its
			// end. Re-executable like it: a second open only rescans, and its
			// observers (bound once, on <body>) clamp every #chat-log refresh.
			bubbleClampScript(),
			agentChatScript(v.Chat.Name),
			DispatchCardGuard(),
			chiefPrefill(v.Prefill),
			// 🔴 THE PANEL'S OWN SHEET, IN THIS COLUMN (chiefIndexSheet): Sessions,
			// Your messages and What it worked on, opened by the list icon in the
			// title bar (chiefPanelActions). `absolute` here, never `fixed` — the
			// panel's transform would place a fixed one by the panel — so this
			// relative, overflow-hidden column is its containing block; and its ids
			// are its own (#chief-panel-index), so it is never the page's #chat-index.
			chiefIndexSheet(v.Chat),
		),
	}
}

// chiefPanelActions renders the TITLE-BAR launcher, out of band: the chief's
// list icon (chiefIndexButton, the page header's own control, opening the
// panel's sheet), or — a panel with no chief chat bar — the new-thread control,
// which reuses POST /agents/{id}/sessions, the route the page surfaces already
// post to, so its empty-session reuse comes along rather than being re-derived.
func chiefPanelActions(v ChiefPanelView) g.Node {
	return Div(
		ID(chiefPanelActionsID),
		hx("hx-swap-oob", "true"),
		Class("ml-auto flex min-w-0 items-center gap-0.5"),
		// No context chip (removed in #871: the page says it in its ⋯ menu,
		// and only when it is high). The chief's chats — switching, New session,
		// Continue, the search through their messages — are the Sessions tab of
		// the sheet the list icon here opens (chiefIndexButton, with the dot when
		// another chat has an unread reply): the ONE list of them, on the page and
		// here (it replaced the panel's own thread list, an operator decision).
		g.If(v.Configured && v.Chat.ChiefChat == nil, chiefNewThreadButton(v.Chat.ID)),
		g.If(v.Configured && v.Chat.ChiefChat != nil, chiefIndexButton(chiefPanelIndexID, chiefUnreadElsewhere(v.Chat.Sessions) > 0)),
	)
}

// chiefNewThreadButton opens a fresh thread WITHOUT leaving the grid.
//
// 🔴 IT TARGETS #chief-panel-body BECAUSE THE PAGE CONTRACT WOULD NAVIGATE AWAY.
// handleAgentSessionCreate answers HX-Redirect, which htmx follows with a real
// navigation — correct on /agents/{name} and /operator, and on /tmux it would
// throw away the grid, the panel and the operator's place in it. The handler grows
// a SURFACE-aware branch (?surface=chief-panel) that returns this partial instead;
// the redirect stays exactly as it was for the two page surfaces, which have their
// own tests pinning it.
func chiefNewThreadButton(agentID int64) g.Node {
	return Form(
		hx("hx-post", agentSessionsPath(agentID)+"?"+ChatSurfaceQueryParam+"="+string(ChatSurfaceChiefPanel)),
		hx("hx-target", "#"+chiefPanelBodyID),
		hx("hx-swap", "innerHTML"),
		// class="contents" so the form introduces no box into the title bar's flex
		// row — the Button is the visible, tappable control. Same shape as
		// newChatButton, for the same reason.
		Class("contents"),
		Button(
			Type("submit"),
			g.Attr("aria-label", "New thread"),
			g.Attr("title", "New thread"),
			Class("press inline-flex h-11 w-11 items-center justify-center rounded-md text-muted transition hover:bg-s2 hover:text-fg"),
			g.Raw(`<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 5v14M5 12h14"/></svg>`),
		),
	)
}

// chiefIntro is the zero-message opener: the three quick actions, plus the one line
// saying what tapping one does.
//
// 🔴 IT DESCRIBES THE CONTROL, NOT THE AGENT'S CAPABILITIES. A prose restatement of
// what chief can reach lived here and was deleted: the reachable set is decided by
// the route table and by whether the chief door is armed on the live pod
// (the CLI's chief-capability reach test is the machine-checked ledger of it, and
// the deployment manifest carries the token as `optional: true`, so removing the
// secret key disarms the door with no manifest change). ChiefPanelView carries no armed field, so this renderer cannot qualify
// such a claim, and nothing binds the prose to the ledger — which is how the same
// sentence has already rotted elsewhere. Do not re-add it.
func chiefIntro() g.Node {
	actions := make([]g.Node, 0, len(chiefQuickActions))
	for _, a := range chiefQuickActions {
		actions = append(actions, Button(
			// 🔴 type="button", NOT submit. The quick actions sit inside #chat-log,
			// which is a sibling of #chat-form rather than a descendant — so a submit
			// here would not post today either. Spelling it is what keeps that true if
			// the two are ever nested: the operator must always press Send themselves.
			Type("button"),
			g.Attr(chiefQuickActionAttr, a),
			Class("press inline-flex min-h-[36px] items-center rounded-lg bg-s2/80 px-3 py-1.5 text-left text-xs text-fg ring-1 ring-inset ring-edge transition hover:bg-s3 hover:text-fg"),
			g.Text(a),
		))
	}
	return Div(
		g.Attr(chiefIntroAttr, ""),
		Class("flex flex-col gap-3 rounded-xl bg-s1/60 p-3 ring-1 ring-inset ring-line"),
		Div(
			Class("flex flex-wrap gap-2"),
			g.Group(actions),
		),
		P(
			Class("text-[10px] text-muted"),
			g.Text("Tapping one fills the box below — you send it."),
		),
	)
}

// chiefPanelScript owns the panel's open/closed state and its width.
//
// 🔴 IT LIVES IN THE SHELL, NOT IN THE PANEL BODY — constraint 1, and the reason
// this function is called from components.go's Page rather than from
// chiefPanelBody.
//
// 🔴 EVERY LISTENER IS ON `document`, BOUND ONCE — constraint 3. htmx's boost
// swaps the body's children; a listener on `document` is unaffected by that
// whatever htmx does with the body element itself, which is the property worth
// having. The once-guard is on `window`, which outlives every swap.
//
// 🔴 restore() RUNS ON EVERY EXECUTION, OUTSIDE THE GUARD. The guard suppresses
// re-BINDING; it must not suppress re-APPLYING. A boosted navigation re-executes
// this script against a fresh DOM, and that fresh panel starts shut at the
// default width until restore() puts the stored state back.
func chiefPanelScript() g.Node { return staticScript(chiefPanelScriptSrc) }

// chiefPanelScriptSrc is the path chiefPanelScript's code is served at, /static/js/chief-panel.js (static_script.go).
var chiefPanelScriptSrc = staticJS("chief-panel", `
(function () {
  var PANEL_ID = '` + chiefPanelID + `';
  // 🔴 THE KEYS ARE BUILT FROM THE ELEMENT ID — A STABLE REFERENCE. Rank 11
  // keyed a drawer on an archived COUNT, which changes on exactly the swaps the
  // memory was meant to survive, so it forgot precisely when it mattered. An id
  // is a constant of the markup: it changes only when someone renames the panel,
  // which is a deliberate act, and it cannot be moved by anything the operator
  // or the fleet does.
  //
  // 🔴 THE PREFIX IS 'muster.', AND IT USED TO BE THE PREFIX OF THE SERVICE THIS
  // ONE WAS EXTRACTED FROM. Every other key this app writes is already 'muster.'
  // (appScript's nav tab, nav scroll and tag filter), so three keys under the old
  // namespace were the last place a reader of the page source could see the
  // extraction had not finished. It is inert rather than dangerous — localStorage
  // is per-ORIGIN, so the two services could never have read each other's — which
  // is exactly why it survived: nothing ever broke.
  //
  // ⚠ THE RENAME DROPS WHAT WAS STORED, DELIBERATELY, AND NOTHING MIGRATES IT.
  // The two values are the panel's open state and its width; each is rewritten
  // by the next interaction with the panel, so the whole cost is that the panel
  // opens shut, at its default width, ONCE. A migration would be a read of the
  // old key, a write of the new one and a removal deadline nobody would come
  // back for — more code than the preference is worth, and the 'v1' segment
  // exists so a reset is sayable. Orphaned entries under the old prefix are left
  // in place rather than swept: a cleanup pass is a write on every page load to
  // reclaim short strings.
  //
  // 🔴 THE CHAT THE PANEL IS ON IS NOT STORED HERE. It was (a third key, the
  // picked thread), and the chief PAGE never read it, so the page and the panel
  // each opened on a chat of their own. The server remembers it for both
  // (internal/api chiefOpenSession): #chief-panel-body asks /ui/chief/panel with
  // no ?session= on every open and is answered with the chat the owner was on
  // last, here or on the page. A stale 'muster.chief.v1.session.*' entry is
  // read by nothing.
  var KEY_OPEN = 'muster.chief.v1.open.' + PANEL_ID;
  var KEY_WIDTH = 'muster.chief.v1.width.' + PANEL_ID;
  var MIN = ` + strconv.Itoa(chiefPanelMinWidth) + `;
  var MAX = ` + strconv.Itoa(chiefPanelMaxWidth) + `;
  var DEFAULT = ` + strconv.Itoa(chiefPanelDefaultWidth) + `;
  var STEP = 32;
  var OPEN_EVENT = '` + chiefPanelOpenEvent + `';
  var BODY_ID = '` + chiefPanelBodyID + `';
  var LOADED_ATTR = '` + chiefPanelBodyLoadedAttr + `';
  var FLEET_ID = '` + fleetDigestSlotID + `';
  var FLEET_PATH = '` + FleetDigestPath + `';
  var FLEET_STEPUP_PATH = '` + FleetStepUpPath + `';
  var FLEET_OPEN_EVENT = '` + fleetOpenEvent + `';
  var FLEET_REQUESTED = '` + fleetRequestedAttr + `';
  // openFleet asks the digest card to load: look is true only for a real
  // open (the panel tapped open), which records a look.
  function openFleet(look) {
    document.dispatchEvent(new CustomEvent(FLEET_OPEN_EVENT, { bubbles: true, detail: { look: !!look } }));
  }
  var PREFILL_ATTR = '` + ChiefPrefillAttr + `';

  // fillInput puts text in #chat-input and focuses it — 🔴 NEVER SENDING, AND
  // NEVER DESTROYING A DRAFT: a non-empty box is kept and text goes after it on
  // a line of its own (a whitespace-only box counts as empty). The quick
  // actions and the fleet digest's "Ask the chief about this" both fill the
  // box through here; #chat-form's Send stays the only way a message leaves.
  function fillInput(text) {
    var input = document.getElementById('chat-input');
    if (!input) return false;
    var draft = input.value || '';
    // The newline is safe in this control: #chat-input is a <textarea> whose
    // Enter-to-send lives on a keydown handler, so text CONTAINING a newline
    // sends nothing by itself.
    input.value = draft.trim() === '' ? text : draft.replace(/\s+$/, '') + '\n' + text;
    // An 'input' event so the box's listeners see the change — the composer's
    // autosize fallback (composer_fit.go) and the draft keeper among them. A
    // value assigned by script fires nothing on its own.
    try { input.dispatchEvent(new Event('input', { bubbles: true })); } catch (err) {}
    input.focus();
    // Caret at the end, so a follow-on keystroke appends rather than replacing
    // a fully-selected value.
    try { input.setSelectionRange(input.value.length, input.value.length); } catch (err) {}
    return true;
  }

  // The fleet digest's phone sheets (fleet_digest.go fleetSheet).
  function fleetSlot() { return document.getElementById(FLEET_ID); }
  function closeSheets() {
    var f = fleetSlot();
    if (!f) return false;
    var open = f.querySelectorAll('[` + fleetSheetAttr + `]:not([hidden])');
    for (var i = 0; i < open.length; i++) open[i].setAttribute('hidden', '');
    var chips = f.querySelectorAll('[` + fleetChipAttr + `]');
    for (var j = 0; j < chips.length; j++) chips[j].setAttribute('aria-expanded', 'false');
    return open.length > 0;
  }
  function openSheet(key) {
    var f = fleetSlot();
    if (!f) return;
    closeSheets();
    var sh = f.querySelector('[` + fleetSheetAttr + `="' + key + '"]');
    if (!sh) return;
    sh.removeAttribute('hidden');
    var chip = f.querySelector('[` + fleetChipAttr + `="' + key + '"]');
    if (chip) chip.setAttribute('aria-expanded', 'true');
    var close = sh.querySelector('[` + fleetSheetCloseAttr + `]');
    if (close) { try { close.focus(); } catch (err) {} }
  }
  // musterFleetShown is the digest's live-refresh filter: refresh only while
  // the panel is open and the owner is not inside a group, a sheet or a
  // drill-in — a refresh replaces the card, and would close what they opened.
  window.musterFleetShown = function () {
    var p = panel();
    if (!p || p.getAttribute('` + chiefPanelOpenAttr + `') !== '1') return false;
    var f = fleetSlot();
    if (!f) return false;
    return !f.querySelector('details[open], [` + fleetSheetAttr + `]:not([hidden]), [` + fleetDrillSlotAttr + `]:not(:empty)');
  };

  // Every storage access is wrapped: localStorage throws on access in a
  // partitioned context and on setItem when the quota is full. A storage failure
  // must cost the operator their panel MEMORY, never the panel — the direction
  // to fail in is "the panel opens at its default", not "the page is broken".
  function store() { try { return window.localStorage; } catch (e) { return null; } }
  function get(k) { var s = store(); if (!s) return null; try { return s.getItem(k); } catch (e) { return null; } }
  function set(k, v) { var s = store(); if (!s) return; try { s.setItem(k, v); } catch (e) {} }

  function panel() { return document.getElementById(PANEL_ID); }
  function body() { return document.getElementById(BODY_ID); }

  function clamp(n) { return Math.max(MIN, Math.min(MAX, n)); }

  function storedWidth() {
    var raw = get(KEY_WIDTH);
    var n = parseInt(raw, 10);
    // 🔴 A STORED VALUE THAT IS NOT A NUMBER FALLS BACK TO THE DEFAULT RATHER
    // THAN TO NaN. clamp(NaN) is NaN, and 'width: NaNpx' is an invalid
    // declaration the browser DROPS — so a corrupted key would produce a panel
    // at whatever the stylesheet says, silently, for ever.
    if (!isFinite(n) || n <= 0) return DEFAULT;
    return clamp(n);
  }
  function storedOpen() { return get(KEY_OPEN) === '1'; }

  // applyWidth paints a width WITHOUT writing it. The separation is deliberate:
  // restore() must never write, or a restore against a DOM that has not settled
  // would overwrite the operator's real choice with whatever it happened to read.
  function applyWidth(p, px) {
    p.style.width = clamp(px) + 'px';
    var grip = p.querySelector('[` + chiefPanelResizeAttr + `]');
    if (grip) grip.setAttribute('aria-valuenow', String(clamp(px)));
  }

  // applyOpen paints open/closed WITHOUT writing it, for the same reason.
  function applyOpen(p, open) {
    p.classList.toggle('translate-x-full', !open);
    p.classList.toggle('translate-x-0', !!open);
    if (open) { p.removeAttribute('inert'); } else { p.setAttribute('inert', 'inert'); }
    p.setAttribute('` + chiefPanelOpenAttr + `', open ? '1' : '0');
    // Every launcher on the page reports the state. There can be more than one
    // (the tmux body re-renders its own), and they are found by attribute rather
    // than held in a variable precisely because the panel outlives them.
    var toggles = document.querySelectorAll('[` + chiefPanelToggleAttr + `]');
    for (var i = 0; i < toggles.length; i++) {
      toggles[i].setAttribute('aria-expanded', open ? 'true' : 'false');
    }
  }

  // setOpen is the ONE writer of the open key. cardLoaded: the digest card was
  // already fetched for this open (a fleet link's arrival, openFromURL), so it
  // is not asked for again.
  function setOpen(open, cardLoaded) {
    var p = panel();
    if (!p) return;
    applyOpen(p, open);
    set(KEY_OPEN, open ? '1' : '0');
    if (open) {
      // Ask the body to (re)fetch. Dispatched on 'document' so the body's
      // 'from:document' trigger catches it wherever the panel sits.
      document.dispatchEvent(new CustomEvent(OPEN_EVENT, { bubbles: true }));
      // The digest card: a real open, so a look.
      if (!cardLoaded) openFleet(true);
    }
  }

  // --- a fleet link's arrival (FleetProposalsURL) ------------------------------
  //
  // ?fleet=proposed[&proposal=<id>]: the card is asked for WITH them
  // (window.__musterFleetFocus; the server draws the Proposed group open — on a phone,
  // its sheet — and the proposal highlighted, or the line saying it no longer
  // waits), and the panel opens only once it has landed, then scrolls to it.
  //
  // 🔴 THE CARD LANDS WHILE THE PANEL IS SHUT, SO NOTHING SHIFTS. The shut
  // panel is translated off the screen, so its card growing to an open group
  // moves nothing in view; the panel then slides in (a transform, which is no
  // layout shift) already drawn. Opened first, the desktop's open group would
  // push the chat down under the reader's eyes (chief-fleet-proposals.spec.ts
  // measures the arrival). window.__musterFleetFocus is set until that card lands: any
  // request for the card meanwhile carries it too (htmx:configRequest), so a
  // second fetch cannot land a closed group over it.
  if (typeof window.__musterFleetFocus !== 'string') window.__musterFleetFocus = '';
  function fleetFocusLanded() {
    var f = fleetSlot();
    if (!f) return;
    var chip = f.querySelector('[` + fleetChipAttr + `="` + FleetGroupProposed + `"]');
    if (chip) chip.setAttribute('aria-expanded', 'true');
    var marks = f.querySelectorAll('[` + fleetHighlightAttr + `], [` + fleetGoneAttr + `]');
    for (var i = 0; i < marks.length; i++) {
      // The card is drawn twice (the desktop group, the phone sheet): the one
      // on screen.
      if (marks[i].getClientRects().length === 0) continue;
      try { marks[i].scrollIntoView({ block: 'nearest' }); } catch (err) {}
      break;
    }
  }

  // fleetClocks draws each <time data-fleet-clock> in the slot (a card's
  // "expires in 3h (22:07)", "That proposal expired at 22:07") in the
  // reader's own clock; the server's UTC one stays if it cannot.
  function fleetClocks() {
    var f = fleetSlot();
    if (!f) return;
    var ts = f.querySelectorAll('time[` + fleetClockAttr + `]');
    var today = new Date().toDateString();
    for (var i = 0; i < ts.length; i++) {
      try {
        var ms = parseInt(ts[i].getAttribute('` + fleetClockAttr + `'), 10);
        if (!ms) continue;
        var d = new Date(ms);
        ts[i].textContent = d.toDateString() === today
          ? d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })
          : d.toLocaleString([], { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
      } catch (err) {}
    }
  }

  // restore applies the stored state to whatever panel is in the DOM NOW. It
  // WRITES NOTHING.
  //
  // 🔴 IT STANDS DOWN WHILE A DRAG IS IN PROGRESS, AND THAT IS A MEASURED FIX,
  // NOT A PRECAUTION. restore() is bound to 'htmx:afterSettle', which fires on
  // EVERY settle anywhere in the app — and #panel-tmux settles on a 60s poll and
  // on every snapshot push from either host. A settle landing mid-drag re-applied
  // the STORED width (still the old one, because endDrag has not run yet), so the
  // panel snapped back under the operator's cursor and endDrag then read that
  // snapped-back value off the DOM and persisted it. The drag was silently
  // discarded. Reproduced in the e2e tier at 1 failure in 4 runs; the pointer
  // counters proved the events were arriving, which is what located it here
  // rather than in the event path.
  function restore() {
    if (dragging) return;
    var p = panel();
    if (!p) return;
    applyWidth(p, storedWidth());
    var open = storedOpen();
    applyOpen(p, open);
    // 🔴 A PANEL RESTORED OPEN MUST ALSO LOAD ITS BODY, or a reload leaves the
    // operator looking at 'Loading…' for ever: the body's only trigger is the
    // open event, and a restore is not a click. Dispatched here rather than by
    // calling setOpen, because setOpen WRITES and this path must not.
    //
    // 🔴 ONLY WHEN THE BODY HAS NOT LOADED. restore() is bound to
    // 'htmx:afterSettle', which fires on EVERY settle anywhere in the app — and
    // on the tmux page that is constant (60s poll, a snapshot push from either
    // host, the SSE-driven swaps, and one recap settle per card across ~78
    // cards). An unconditional dispatch here re-fetched /ui/chief/panel on every
    // one of them, and the body is an innerHTML swap whose content IS the chat
    // pane — so the operator's half-typed message was destroyed and re-rendered
    // continuously, and the input could not be typed into at all.
    //
    // MEASURED, both tiers. Live pod before the fix: 111 requests to
    // /ui/chief/panel in 5 minutes, one or two in every single second. In the e2e
    // tier, THREE grid settles produced 449 and 546 refetches on two runs of the
    // same probe — the cascade is far worse under a burst than the live average
    // suggests, because each refetch settles and re-arms the next.
    //
    // The guard is the loaded MARK below, not a 'once' on the trigger: a 'once'
    // would break the re-open case the body's own comment protects, where a
    // panel reopened an hour later must show the conversation as it is NOW.
    // setOpen still dispatches on every OPEN, which is that case; this path
    // fires only for a body that has never been filled — a fresh page, or the
    // fresh element a boosted navigation builds.
    if (open && !bodyLoaded()) {
      document.dispatchEvent(new CustomEvent(OPEN_EVENT, { bubbles: true }));
    }
    // 🔴 THE CARD IS FETCHED ONCE PER SLOT ELEMENT, NOT ON EVERY SETTLE: the
    // mark is set when its request STARTS (htmx:beforeRequest, below), not
    // when it lands, so neither the card's own settle nor a fetch that keeps
    // failing asks again. Until a request has started, a restore asks — the
    // first restore can run before htmx has bound the slot's trigger. A
    // boosted navigation's fresh slot carries no mark and is fetched once.
    // Not a look.
    var fs = fleetSlot();
    if (open && fs && !fs.hasAttribute(FLEET_REQUESTED)) {
      openFleet(false);
    }
  }

  // bodyLoaded reports whether the CURRENT body element has been filled by a
  // successful swap.
  //
  // 🔴 THE MARK LIVES ON THE ELEMENT, NOT IN A VARIABLE, AND THAT IS WHAT MAKES
  // THE BOOSTED-NAVIGATION CASE WORK. hx-swap="innerHTML" replaces the body's
  // CHILDREN and keeps the element, so a mark set on it survives every refresh;
  // a boosted navigation builds a BRAND NEW element, which carries no mark, so
  // the next restore() loads it exactly as it must. A module-level boolean
  // would still read true against that new element and leave it on 'Loading…'
  // for ever — the failure this whole path exists to prevent.
  function bodyLoaded() {
    var b = document.getElementById(BODY_ID);
    return !!(b && b.hasAttribute(LOADED_ATTR));
  }

  // --- drag-to-resize ---------------------------------------------------------
  //
  // 🔴 dragging IS DECLARED BEFORE restore() USES IT AND BEFORE THE ONCE-GUARD.
  // Both restore() and the pointer handlers close over this one variable; a
  // second copy inside the guard would leave restore() reading a 'dragging' that
  // is never set.
  var dragging = false;
  // dragWidth is the width the CURRENT drag has computed.
  //
  // 🔴 endDrag PERSISTS THIS, NOT A READ-BACK OF p.style.width. Reading the DOM
  // at the end of a drag persists whatever the element happens to say at that
  // instant — which, if anything else wrote to it mid-drag, is not what the
  // operator dragged to. That is exactly the failure the restore() guard above
  // describes, and fixing only one of the two leaves the other as a silent way to
  // persist a width nobody chose.
  var dragWidth = null;
  function widthFromPointer(clientX) {
    // The panel is right-anchored, so its width is the distance from the pointer
    // to the right edge of the viewport.
    return clamp(Math.round(window.innerWidth - clientX));
  }

  if (!window.__cgChiefPanelInit) {
    window.__cgChiefPanelInit = true;

    // 🔴 THE CLICK HANDLER OBSERVES AND DOES NOT SWALLOW — rank 11's mutant PD.
    // It returns immediately for anything that is not one of this panel's own
    // controls and calls preventDefault on NOTHING else, so a <summary> click
    // still toggles its <details> and a card's view toggle still flips. This is
    // the single most load-bearing line in the file and the easiest to "tidy"
    // into a page-wide preventDefault.
    document.addEventListener('click', function (e) {
      var t = e.target;
      if (!t || !t.closest) return;
      if (t.closest('[` + chiefPanelCloseAttr + `]')) { setOpen(false); return; }
      // 🔴 A QUICK ACTION PREFILLS AND FOCUSES. IT DOES NOT SEND, AND IT SUPPRESSES
      // NO DEFAULT — the button is type=button, so there is nothing to suppress,
      // and suppressing one here is exactly how this handler becomes the page-wide
      // swallow its own header warns about. The text goes into the EXISTING
      // #chat-input and the operator presses Send, so #chat-form's WebSocket path
      // stays the only way a chat message leaves this panel.
      // (TestTheChiefPanelObservesClicksWithoutSwallowingThem greps this handler's
      // SOURCE for the suppressing call's name, comments included — so describe the
      // property without spelling it.)
      // 🔴 AND IT NEVER DESTROYS A HALF-TYPED DRAFT. The reason prefill was chosen
      // over send-immediately is that a stray tap on a phone must not cost the
      // operator something they cannot undo — and an unconditional write to the box
      // costs them their draft, which is exactly that harm in a different currency.
      // The three buttons sit DIRECTLY ABOVE the input at 390px, and the intro
      // renders on an EMPTY thread, which is where the first message is being
      // composed. So a non-empty draft is KEPT and the prompt is appended after it,
      // on its own line: the tap still does something visible (no dead control), the
      // words the operator typed are still there, and #chat-form's Send is still the
      // only thing that puts either on the wire. Appending rather than inserting at
      // the caret is deliberate — a caret parked at position 0 would jam the prompt
      // in front of the draft, and selectionStart is not reliable across the focus
      // this handler is about to take.
      //
      // ⚠ A WHITESPACE-ONLY BOX COUNTS AS EMPTY. It carries no words to lose, and
      // treating it as a draft would leave a blank line above every prefill for ever.
      var qa = t.closest('[` + chiefQuickActionAttr + `]');
      if (qa) {
        fillInput(qa.getAttribute('` + chiefQuickActionAttr + `') || '');
        return;
      }
      // The fleet digest's chips and sheets (phone): a chip opens its group's
      // sheet, the sheet's close button shuts it. Both are this card's buttons.
      var chip = t.closest('[` + fleetChipAttr + `]');
      if (chip) {
        if (!chip.disabled) openSheet(chip.getAttribute('` + fleetChipAttr + `'));
        return;
      }
      if (t.closest('[` + fleetSheetCloseAttr + `]')) { closeSheets(); return; }
      // A proposal's Edit opens (or shuts) its text box: the one in ITS card
      // (each card is drawn in the desktop group and in the phone sheet).
      var ed = t.closest('[data-fleet-edit]');
      if (ed) {
        var cardEl = ed.closest('[` + fleetProposalAttr + `]');
        var form = cardEl && cardEl.querySelector('[data-fleet-edit-form]');
        if (form) {
          var show = form.hasAttribute('hidden');
          if (show) form.removeAttribute('hidden'); else form.setAttribute('hidden', '');
          ed.setAttribute('aria-expanded', show ? 'true' : 'false');
          if (show) { var ta = form.querySelector('textarea'); if (ta) ta.focus(); }
        }
        return;
      }
      var toggle = t.closest('[` + chiefPanelToggleAttr + `]');
      if (!toggle) return;               // not ours — observed, not swallowed
      var p = panel();
      setOpen(!(p && p.getAttribute('` + chiefPanelOpenAttr + `') === '1'));
    });

    // Escape closes, but ONLY when the panel is open and only when the key was
    // not consumed by something else. A global Escape that always fired would
    // close the panel while the operator was dismissing a modal on top of it.
    document.addEventListener('keydown', function (e) {
      if (e.key !== 'Escape' || e.defaultPrevented) return;
      // An open popover (a chat chip's ⋯ menu, the context chip's detail) is
      // the top layer: that Escape is the popover's, which the browser closes,
      // and the panel stays.
      try { if (document.querySelector(':popover-open')) return; } catch (err) {}
      // An open fleet sheet is the top layer: Escape shuts it, not the panel.
      if (closeSheets()) return;
      var p = panel();
      if (p && p.getAttribute('` + chiefPanelOpenAttr + `') === '1') setOpen(false);
    });

    // --- the grip: pointer drag + keyboard -----------------------------------
    document.addEventListener('pointerdown', function (e) {
      var t = e.target;
      if (!t || !t.closest || !t.closest('[` + chiefPanelResizeAttr + `]')) return;
      dragging = true;
      dragWidth = null;
      // preventDefault HERE is correct and is not the mutant above: a drag on a
      // splitter must not also select text across the page. It is scoped to this
      // one element by the closest() test on the line before.
      e.preventDefault();
      try { t.setPointerCapture(e.pointerId); } catch (err) {}
    });
    document.addEventListener('pointermove', function (e) {
      if (!dragging) return;
      var p = panel();
      if (!p) return;
      dragWidth = widthFromPointer(e.clientX);
      applyWidth(p, dragWidth);
    });
    function endDrag() {
      if (!dragging) return;
      dragging = false;
      var p = panel();
      if (!p) return;
      // 🔴 THE WIDTH IS WRITTEN ONCE, AT THE END OF THE DRAG, NOT ON EVERY MOVE.
      // A pointermove fires per frame; writing localStorage per frame is a
      // synchronous main-thread write on the page that already measures 900ms
      // long tasks per panel swap.
      //
      // 🔴 AND IT WRITES WHAT THE DRAG COMPUTED, NOT WHAT THE DOM SAYS. Reading
      // p.style.width back persists whatever the element happens to carry at this
      // instant, which is not the operator's choice if anything else wrote to it
      // mid-drag — see the restore() guard above for the settle that did exactly
      // that. dragWidth is null when the press produced no movement at all, and
      // the DOM read is the fallback for that case only.
      var chosen = dragWidth;
      if (chosen === null) chosen = parseInt(p.style.width, 10) || DEFAULT;
      dragWidth = null;
      set(KEY_WIDTH, String(clamp(chosen)));
      applyWidth(p, chosen);
    }
    document.addEventListener('pointerup', endDrag);
    document.addEventListener('pointercancel', endDrag);

    document.addEventListener('keydown', function (e) {
      var t = e.target;
      if (!t || !t.closest || !t.closest('[` + chiefPanelResizeAttr + `]')) return;
      var d = 0;
      if (e.key === 'ArrowLeft') d = STEP;     // grip left = wider (right-anchored)
      else if (e.key === 'ArrowRight') d = -STEP;
      else return;
      e.preventDefault();
      var p = panel();
      if (!p) return;
      var next = clamp((parseInt(p.style.width, 10) || DEFAULT) + d);
      applyWidth(p, next);
      set(KEY_WIDTH, String(next));
    });

    // 🔴 MARK THE BODY THE MOMENT IT IS FILLED. This listener is what stops the
    // restore() dispatch above from firing on every settle; without it the mark
    // is never set, bodyLoaded() is false for ever, and the re-fetch storm this
    // fix removes comes straight back.
    //
    // It keys on the TARGET's id rather than on the event's detail, because a
    // swap anywhere else in the app also raises htmx:afterSwap on document and
    // must not be mistaken for this one.
    document.addEventListener('htmx:afterSwap', function (evt) {
      var t = evt && evt.target;
      if (!t || t.id !== BODY_ID) return;
      t.setAttribute(LOADED_ATTR, '');
    });

    // The fleet digest's fetch ON OPEN records a look (the "since you last
    // looked" line's baseline, fleet.Store.Look); its live refreshes do not.
    // Scoped by the slot's id, like the body's rewrite above.
    document.addEventListener('htmx:configRequest', function (evt) {
      var el = evt && evt.target;
      if (!el || el.id !== FLEET_ID || !evt.detail) return;
      var te = evt.detail.triggeringEvent;
      if (te && te.type === FLEET_OPEN_EVENT && te.detail && te.detail.look) evt.detail.path = FLEET_PATH + '?look=1';
      // A fleet link's arrival not yet landed: every card request carries it.
      if (window.__musterFleetFocus) evt.detail.path = FLEET_PATH + window.__musterFleetFocus;
    });

    // The card landed: its clocks in the reader's own; and a fleet link's
    // arrival, once its card is in, opens the panel and scrolls to the
    // proposal (see window.__musterFleetFocus).
    document.addEventListener('htmx:afterSettle', function (evt) {
      var t = evt && evt.target;
      if (!t || t.id !== FLEET_ID) return;
      fleetClocks();
      if (!window.__musterFleetFocus) return;
      window.__musterFleetFocus = '';
      setOpen(true, true);
      fleetFocusLanded();
    });
    document.addEventListener('htmx:responseError', function (evt) {
      var el = evt && evt.detail && evt.detail.elt;
      if (!window.__musterFleetFocus || !el || el.id !== FLEET_ID) return;
      // The card did not come: the panel opens anyway, on whatever it has.
      window.__musterFleetFocus = '';
      setOpen(true);
    });

    // 🔴 A PROPOSAL'S APPROVAL (OR EDIT) ON A STALE SIGN-IN: the server
    // answered 403 with X-Muster-Step-Up and sent nothing. "Confirm it's you"
    // is asked — the page's own dialog, or, on a page with none, the one
    // FLEET_STEPUP_PATH draws into the panel's slot — and the same tap is made
    // again once confirmed. The server re-checks the sign-in whatever this
    // does.
    document.addEventListener('htmx:responseError', function (evt) {
      var d = evt && evt.detail, x = d && d.xhr, el = d && d.elt;
      if (!x || x.status !== 403 || !x.getResponseHeader('X-Muster-Step-Up') || !el || !el.closest || !el.closest('#' + FLEET_ID)) return;
      var again = function () { if (el.tagName === 'FORM' && el.requestSubmit) el.requestSubmit(); else el.click(); };
      var ask = function () { window.musterStepUp.stale(); window.musterStepUp.ask('', again); };
      if (window.musterStepUp) { ask(); return; }
      if (!window.htmx) return;
      window.htmx.ajax('GET', FLEET_STEPUP_PATH, { target: '#` + fleetStepUpSlotID + `', swap: 'innerHTML' }).then(function () {
        if (window.musterStepUp) ask();
      });
    });

    // The card's request has started: restore() asks no more (see restore).
    document.addEventListener('htmx:beforeRequest', function (evt) {
      var el = evt && evt.target;
      if (el && el.id === FLEET_ID) el.setAttribute(FLEET_REQUESTED, '');
    });

    // A drill-in's name toggles it: open (the fetch), or shut when it is open —
    // the second tap empties it and cancels the fetch.
    document.addEventListener('htmx:confirm', function (evt) {
      var el = evt && evt.target;
      if (!el || !el.hasAttribute || !el.hasAttribute('` + fleetDrillAttr + `')) return;
      var slot = el.nextElementSibling;
      if (slot && slot.hasAttribute('` + fleetDrillSlotAttr + `') && slot.innerHTML.trim() !== '') {
        evt.preventDefault();
        slot.innerHTML = '';
        el.setAttribute('aria-expanded', 'false');
      }
    });
    document.addEventListener('htmx:afterSwap', function (evt) {
      var t = evt && evt.target;
      if (!t || !t.hasAttribute || !t.hasAttribute('` + fleetDrillSlotAttr + `')) return;
      var btn = t.previousElementSibling;
      if (btn) btn.setAttribute('aria-expanded', 'true');
    });

    // A body that carries a draft (the Fleet chat's "Ask the chief about
    // this") puts it in the box once it has settled — after the box's own
    // draft is restored — and the mark is removed so a refresh cannot apply
    // it twice. A sheet the tap came from is shut, so the chat is in view.
    document.addEventListener('htmx:afterSettle', function (evt) {
      var t = evt && evt.target;
      if (!t || t.id !== BODY_ID) return;
      var mark = t.querySelector('[' + PREFILL_ATTR + ']');
      if (!mark) return;
      var text = mark.getAttribute(PREFILL_ATTR) || '';
      mark.removeAttribute(PREFILL_ATTR);
      closeSheets();
      setTimeout(function () { fillInput(text); }, 0);
    });

    // Re-apply after ANY htmx settle. The panel itself is body-level and is not
    // swapped, but a boosted navigation rebuilds the body's children — including
    // it — so the fresh element needs the stored state put back.
    document.addEventListener('htmx:afterSettle', restore);
  }

  // ?` + ChiefOpenParam + `=open (the fleet's "N proposals waiting" push and
  // its Attention notice open it, FleetProposalsURL): the panel opens — with
  // ` + FleetFocusParam + `=` + FleetGroupProposed + `, on the digest's Proposed group (see window.__musterFleetFocus)
  // — and the parameters leave the address bar so a reload does not reopen it.
  function openFromURL() {
    try {
      var u = new URL(window.location.href);
      if (u.searchParams.get('` + ChiefOpenParam + `') !== 'open') return;
      var focus = u.searchParams.get('` + FleetFocusParam + `') === '` + FleetGroupProposed + `';
      var id = parseInt(u.searchParams.get('` + FleetProposalParam + `') || '', 10);
      u.searchParams.delete('` + ChiefOpenParam + `');
      u.searchParams.delete('` + FleetFocusParam + `');
      u.searchParams.delete('` + FleetProposalParam + `');
      window.history.replaceState(window.history.state, '', u.pathname + u.search + u.hash);
      var f = fleetSlot(), p = panel();
      if (!focus || !f || !p || !window.htmx) { setOpen(true); return; }
      window.__musterFleetFocus = '?look=1&` + FleetFocusParam + `=` + FleetGroupProposed + `' + (isFinite(id) && id > 0 ? '&` + FleetProposalParam + `=' + id : '');
      // Shut until the card lands (a panel left open is shut for that moment).
      set(KEY_OPEN, '0');
      applyOpen(p, false);
      window.htmx.ajax('GET', FLEET_PATH + window.__musterFleetFocus, { source: f, target: f, swap: 'innerHTML' });
    } catch (e) {
      window.__musterFleetFocus = '';
      setOpen(true);
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', restore);
    document.addEventListener('DOMContentLoaded', openFromURL);
  } else {
    restore();
    openFromURL();
  }
})();
`)
