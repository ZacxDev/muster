package ui

import (
	"io"
	"strconv"
	"strings"
	"time"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/textplain"
)

// AgentCardView is the per-agent data the card grid renders.
type AgentCardView struct {
	ID int64
	// SessionID is the agent's latest session (agents.Store.LatestSessionIDs),
	// 0 when it has none or the read failed. Every link the list draws to this
	// agent — the card, a pinned chip, a Recent entry — names it (PageHref).
	SessionID int64
	// Kind and CCAccount are the agent's kind and Claude account name.
	Kind      string
	CCAccount string
	// WorkerPool is the machine-job worker pool the agent belongs to
	// (agents.Agent.WorkerPool); "" for none. Rendered as a "pool" chip.
	WorkerPool  string
	Name        string
	DisplayName string
	Repo        string
	Status      string // pending|provisioning|running|stopped|error
	KickedOff   bool
	// KickoffOwed is agents.KickoffOwed for this agent: a kickoff message is
	// stored and nothing delivered it. It renders as a badge BESIDE the status
	// dot, never as a status value — see kickoffOwedBadge.
	//
	// 🔴 IT IS NOT DERIVED HERE, AND IT MUST NOT BE. The note text is not on this
	// view at all (it is operator-authored instruction text; agents.Agent tags it
	// `json:"-"` and this package would put it in HTML), so the boolean is
	// composed in internal/api's cardViewIndexed from the stored row and arrives
	// already decided.
	KickoffOwed bool
	// KickoffFailed is agents.KickoffFailed: the first turn was handed to a gateway
	// (the row is stamped kicked_off, so KickoffOwed is false) and it did not
	// complete. It renders as a "kickoff failed" badge beside the status dot plus the
	// failure text and the remedy — see kickoffFailedBadge and kickoffFailureDetail.
	// Mutually exclusive with KickoffOwed by construction.
	KickoffFailed bool
	// KickoffFailure is the text to show for a failed kickoff:
	// agents.KickoffFailureText, composed in internal/api's cardViewIndexed with the
	// pending note already SCRUBBED out (agents.ScrubNote). This package never sees
	// the note and must not be handed raw kickoff_error.
	KickoffFailure string
	// KickoffResendSafe is agents.KickoffResendSafe: the failure proves nothing was
	// sent, so the remedy may say re-sending is safe. Otherwise the remedy says to
	// check whether the agent is already working first. See KickoffFailedRemedy.
	KickoffResendSafe bool
	// KickoffAwaitingGrant is agents.KickoffAwaitingGrant: the first turn is held,
	// unsent, until the owner approves this agent's github connector request (its
	// repository cannot be checked out before). It renders as a "waiting for your
	// github approval" line linking to Connectors — see kickoffAwaitingGrantNotice.
	// Composed in internal/api's cardViewIndexed; only the boolean crosses.
	KickoffAwaitingGrant bool
	// GitHubProblem is why the GitHub App cannot currently serve this agent's
	// repository (its credentials refused at the token mint, a rate limit, the
	// repository outside the installation), "" when nothing is known to be wrong
	// or no App is configured. Composed in internal/api's cardViewIndexed from
	// githubapp's operator-facing text, which never carries a token.
	GitHubProblem string
	// Namespace is the agent's stored namespace, used only to spell the log command
	// in the failed-kickoff remedy.
	Namespace string
	// Model is the agent's configured model slug ("" = cluster default). Surfaced
	// as a small read-only line on the card so it's visible at a glance.
	Model string
	// NoteID is the source task this agent was dispatched from (nil = none). When
	// set, the card shows a "Task #<id>" badge linking to the Tasks tab.
	NoteID *int64
	// CreatedAt backs the card's relative "age". Zero renders nothing.
	CreatedAt time.Time
	UpdatedAt time.Time
	// LastActiveAt is agents.LastActivity for this agent — the time the Agents
	// list is ORDERED by — and it is what the footer's "active … ago" prints, so
	// the order and the label on screen are one number. Composed in internal/api's
	// cardViewIndexed; a zero value (a minimal test view) falls back to UpdatedAt.
	LastActiveAt time.Time
	// IsChief marks the fleet's chief agent; its ⋯ menu offers no Delete. It is
	// the answer of internal/api's ONE chief predicate (isChiefAmong), composed in
	// cardViewIndexed and never re-derived here — the server refuses the delete
	// regardless, so this only keeps the UI from offering what it would refuse.
	IsChief bool
	// DeleteProtected: its ⋯ menu offers no Delete, like the chief's, without it
	// being the chief — a declared officer's instance (internal/api's
	// deleteProtection). The server refuses the delete regardless.
	DeleteProtected bool
	// PrimaryChief: this is THE chief the viewer is sent to — where `/` opens,
	// and the agent the sidebar pins first (navBefore). internal/api's
	// primaryChief decides it, for both; it is never re-derived here. At most
	// one card of a list carries it; IsChief may be true on more than one.
	PrimaryChief bool
	// Pinned: the viewing user pinned this agent (internal/pins). The list draws
	// a pinned agent first, as a label-only CHIP instead of its card
	// (pinned_chips.go: unpinned, renamed and started/stopped from the chip's
	// edit controls, behind the row's "Edit pins" toggle); RenderAgentCard draws
	// that chip too. Composed in cardViewIndexed.
	Pinned bool
	// Focus is what the agent is working on (internal/focus), nil for none.
	// Composed in cardViewIndexed; shown as the footer's focus chip, and found
	// by the filter.
	Focus *FocusView
	// Attention is the card's attention badge (cards_attention.go), nil for
	// none. Composed in internal/api from the attention queue's own read.
	Attention *CardAttention
	// AwaitingApproval: the agent is blocked on a permission prompt (or has a
	// request open on the approval queue) — internal/api's AwaitingApproval,
	// the same answer the polled "awaiting approval" badge shows. It is NOT an
	// attention item, so it is carried here: the "Waiting on you" chip reads it
	// (waitTokens), and the status poll asks for a list refresh when it changes
	// (cardStatusIcon).
	AwaitingApproval bool
	// Work is what the agent's pod last reported it has running — subagents and
	// background shells — internal/api's AgentWork, already expired when stale.
	// It draws the same chip a mirrored window's card does (workChip), and the
	// status poll carries it (StatusPollPath) so a change or an expiry re-renders
	// the list. Zero: no chip.
	Work mirror.WindowWork
	// Idle: drawn in the list's Idle section (idle_section.go's InIdleSection),
	// composed by MarkIdle. Its card carries data-idle-section.
	Idle bool
}

// lastActive is the card's "active" time: LastActiveAt, or UpdatedAt when the
// view was built without one.
func (a AgentCardView) lastActive() time.Time {
	if a.LastActiveAt.IsZero() {
		return a.UpdatedAt
	}
	return a.LastActiveAt
}

// NoteOption is a selectable existing note for the dispatch modal.
type NoteOption struct {
	ID    int64
	Label string
}

// ComboOption is one option in a custom type-to-filter combobox. Value is the
// submitted value (carried by the hidden input); Label is the visible text the
// user filters against. For repos Value == Label; for notes they differ (Value
// is the note id, Label is the directory / body preview).
type ComboOption struct {
	Value string
	Label string
}

// agentCard is the per-agent card. The card BODY is a real boosted <a> that
// opens the detail page on click/tap (status icon + name + repo chip). There is
// NO chat or log preview on a card — an operator decision: the list is for
// finding an agent, the detail page for reading it. The interactive controls —
// the inline rename pencil and the ⋯ context menu (Start/Stop/Delete) — sit
// OUTSIDE that anchor in a top-right control cluster, so tapping them never
// triggers card navigation. All
// interactivity is boost-safe: a native popover menu (placed by
// floatingMenuScript) and htmx.
//
// 🔴 ITS SHAPE IS mirrorCard's, SLOT FOR SLOT (cardSlot): controls, body,
// header, footer. TestAgentAndMirrorCardsRenderTheSameSlots pins it, so the two
// kinds of card cannot drift into two layouts again.
// PageHref is the card's link to the agent's page: AgentListHref on the agent's
// latest session, so a tap loads the page directly instead of the bare URL and
// its 303 (and is moved to the latest if it went stale). The card, its pinned
// chip and its Recent entry all use it.
func (a AgentCardView) PageHref() string { return AgentListHref(a.Name, a.SessionID) }

// kickoffOwedBadge is the "kickoff owed" badge: an agent whose stored first
// message was never delivered. It renders BESIDE the status dot and says nothing
// about the status itself.
//
// 🔴 IT CARRIES NO PART OF THE NOTE. The text is a FIXED label; the only thing
// this surface learns from the row is a boolean. The note is operator-authored
// instruction text and this repository is public, so putting it in markup would be
// the leak the gate exists for — see Agent.PendingNote's own comment and
// api.TestAnOwedKickoffIsReportedWithoutTheNotesText.
//
// ⚠ ITS CLASSES WERE CHOSEN TO MATCH AN EXISTING PILL'S, down to the spelling. Every Tailwind class in this package is emitted
// into the COMMITTED web/static/app.css, and `make css-check` rebuilds and refuses
// a stale copy — reusing an existing pill's classes means this badge adds zero
// bytes and the committed stylesheet needs no rebuild. A new colour here is not
// free; it is a stylesheet change.
//
// The data attribute is the handle a test parses, so a guard can ask "is THIS
// badge present" rather than grepping for a word any other element could spell.
func kickoffOwedBadge() g.Node {
	return Span(
		g.Attr("data-kickoff-owed", ""),
		g.Attr("title", "This agent's first message was never delivered — it has not been told what to do"),
		g.Attr("aria-label", "Kickoff owed: this agent's first message was never delivered"),
		glyphKnock(glyphWarning),
		Class("inline-flex items-center gap-1.5 rounded-full bg-st-warning-bg px-2.5 py-1 text-xs font-semibold text-st-warning-fg ring-1 ring-inset ring-st-warning-fg/40"),
		statusGlyph(glyphWarning, "h-3 w-3", false),
		g.Text("kickoff owed"),
	)
}

// KickoffRemedyResendSafe is the remedy under a failed kickoff whose record proves
// the task never reached the agent (agents.KickoffResendSafe).
const KickoffRemedyResendSafe = "Not retried automatically. The task never reached the agent, so " +
	"re-sending cannot pay for the task twice: open this agent's chat and send the task again."

// KickoffFailedRemedy is the remedy line under a failed kickoff. Nothing re-sends
// automatically: the deliverer stamps kicked_off before the paid turn so a turn is
// never run twice, and a turn that fails after the stamp is not retried (an
// operator decision).
//
// 🔴 IT TELLS THE OPERATOR TO CHECK BEFORE RE-SENDING UNLESS NOTHING WAS SENT. A
// shutdown, a timeout or a transport error after the request was written can leave
// the runtime still running the turn, and an empty reply can hide a turn the runtime
// retried and finished; re-sending then pays for the task twice. Only a connection
// that was never opened, or a runtime that refused the turn as not_ready for the
// whole delivery, proves otherwise (resendSafe).
//
// ⚠ ITS ONLY DYNAMIC PARTS ARE THE AGENT'S NAME AND NAMESPACE, which the card and
// GET /api/agents already show. No part of the note or of kickoff_error is in it;
// api.TestAnOwedKickoffIsReportedWithoutTheNotesText scans the rendered remedy.
func KickoffFailedRemedy(resendSafe bool, namespace, name string) string {
	if resendSafe {
		return KickoffRemedyResendSafe
	}
	return "Not retried automatically, so it is never paid twice. The agent may still be " +
		"working on this turn. Before re-sending, check its logs (kubectl -n " + namespace +
		" logs deploy/" + name + " -c agent) or its chat transcript, and re-send through its " +
		"chat only if it is not working on the task."
}

// kickoffFailedBadge is the "kickoff failed" badge: the first turn was handed to a
// gateway and did not complete. It renders BESIDE the status dot, exactly like
// kickoffOwedBadge, and for the same reasons — additive, never a status value,
// never inside cardStatusIcon's polling span (TestTheKickoffBadgesAreNotInsideThePollingStatusSpan).
//
// 🔴 IT EXISTS BECAUSE THE OWED BADGE GOES AWAY ON THE STAMP. Before it, a turn
// that failed after the stamp turned "kickoff owed" off and left the card reading
// healthy over an agent that was never told its task.
//
// The data attribute is the handle a test parses — see kickoffOwedBadge.
func kickoffFailedBadge() g.Node {
	return Span(
		g.Attr("data-kickoff-failed", ""),
		g.Attr("title", "This agent's first message was handed to its gateway and the turn did not complete"),
		g.Attr("aria-label", "Kickoff failed: this agent's first turn did not complete and is not retried automatically"),
		glyphKnock(glyphError),
		Class("inline-flex items-center gap-1.5 rounded-full bg-st-error-bg px-2.5 py-1 text-xs font-semibold text-st-error-fg ring-1 ring-inset ring-st-error-fg/40"),
		statusGlyph(glyphError, "h-3 w-3", false),
		g.Text("kickoff failed"),
	)
}

// kickoffAwaitingGrantNotice says why an owed first turn has not been sent: the
// agent's repository cannot be checked out until the owner approves its github
// connector request, and the turn is held (unsent, unstamped) until then —
// agents.DecideFirstTurn, for at most agents.CheckoutWaitTimeout. The approval is
// the owner's only move, so the line links straight to Connectors, where the
// request is.
//
// Its text is FIXED: no part of the note or of ccd's detail is in it.
func kickoffAwaitingGrantNotice() g.Node {
	// The wrapper's padding matches the card body's (cardBodyClass), with classes
	// already in the committed stylesheet (see kickoffOwedBadge on why that matters).
	return Div(Class("px-4 pb-4"), P(
		g.Attr("data-kickoff-awaiting-grant", ""),
		Class("break-words rounded-lg bg-st-warning-bg px-3 py-2 text-xs text-st-warning-fg ring-1 ring-inset ring-st-warning-fg/40"),
		Span(Class("font-semibold"), g.Text("Waiting for your github approval: ")),
		g.Text("this agent's repository is checked out, and its first message sent, once you approve its "+
			"github connector request. "),
		A(Href("/connectors"), Class("font-semibold underline"), g.Text("Open Connectors")),
	))
}

// githubProblemDetail is the agent card's line for a GitHub App failure that
// keeps this agent from reaching its repository (AgentCardView.GitHubProblem).
// The same classes as kickoffFailureDetail, so it needs nothing new in the
// committed stylesheet.
func githubProblemDetail(msg string) g.Node {
	return P(
		g.Attr("data-github-problem", ""),
		Class("break-words rounded-lg bg-st-error-bg px-3 py-2 text-xs text-st-error-fg ring-1 ring-inset ring-st-error-fg/40"),
		Span(Class("font-semibold"), g.Text("GitHub: ")),
		g.Text(msg),
	)
}

// kickoffFailureDetail renders the recorded failure and the remedy under the
// card header.
//
// 🔴 msg ARRIVES SCRUBBED. It is agents.KickoffFailureText, which removes any run
// of the pending note — a failed turn's error can quote the runtime's response
// body, and a runtime that echoes its request would otherwise publish the note.
// gomponents escapes it as text, so a body carrying markup renders inert.
func kickoffFailureDetail(msg, remedy string) g.Node {
	return P(
		g.Attr("data-kickoff-failure", ""),
		Class("break-words rounded-lg bg-st-error-bg px-3 py-2 text-xs text-st-error-fg ring-1 ring-inset ring-st-error-fg/40"),
		Span(Class("font-semibold"), g.Text("Kickoff failed: ")),
		g.Text(msg),
		g.Text(" "),
		Span(g.Attr("data-kickoff-remedy", ""), Class("font-semibold"), g.Text(remedy)),
	)
}

// statusIcon is the COMPACT status indicator for the agent card: just the glyph
// (no text label box), with the status word carried as a title / aria-label
// tooltip. Same mapping as statusDot.
func statusIcon(status string) g.Node {
	label := statusLabel(status)
	return Span(
		g.Attr("title", label),
		g.Attr("aria-label", "Status: "+label),
		g.Attr("role", "img"),
		Class("relative flex h-3 w-3 shrink-0"),
		statusGlyph(agentGlyphKind(status), "h-3 w-3", status == "provisioning"),
	)
}

// RenderStatusIcon writes the compact status icon (the colored dot, no label) for
// a status. It is the exported render entry for the live-status poll endpoint
// (GET /ui/agents/{id}/status), reusing statusIcon so the polled icon matches the
// first-paint icon in the agent-detail / operator header exactly.
//
// awaitingApproval adds the "awaiting approval" badge beside the dot: the agent's
// terminal is blocked on a permission prompt, or it has a request open on the
// operator's approval queue. 🔴 THE BADGE LIVES IN THIS POLLED RESPONSE ON
// PURPOSE — it is a live state, so the 10s poll is what both shows and clears it.
// It is the opposite of the owed-kickoff badge, which must sit OUTSIDE the
// polling span because this endpoint never rendered it.
func RenderStatusIcon(w io.Writer, status string, awaitingApproval bool) error {
	if !awaitingApproval {
		return statusIcon(status).Render(w)
	}
	return g.Group([]g.Node{statusIcon(status), AwaitingApprovalBadge()}).Render(w)
}

// AwaitingApprovalBadgeID is the data attribute the badge carries, so tests and
// scripts find it by STATE, not by its wording.
const AwaitingApprovalBadgeID = "awaiting-approval"

// AwaitingApprovalBadge is the small pill shown while an agent waits on a human
// to approve a tool call.
func AwaitingApprovalBadge() g.Node {
	return Span(
		g.Attr("data-badge", AwaitingApprovalBadgeID),
		g.Attr("title", "Blocked on a permission prompt: approve or deny it on your phone or in the agent's terminal"),
		Class("ml-1.5 inline-flex items-center gap-1.5 whitespace-nowrap rounded-full bg-st-warning-bg px-2.5 py-1 text-xs font-semibold text-st-warning-fg ring-1 ring-inset ring-st-warning-fg/40"),
		g.Text("awaiting approval"),
	)
}

// StatusPollPath is an agent card's status poll: GET /ui/agents/{id}/status,
// with awaiting=1|0 saying whether the card was drawn awaiting approval, and
// work=WorkPollToken saying which work chip it was drawn with.
func StatusPollPath(id int64, awaiting bool, work mirror.WindowWork) string {
	v := "0"
	if awaiting {
		v = "1"
	}
	return "/ui/agents/" + strconv.FormatInt(id, 10) + "/status?awaiting=" + v + "&work=" + WorkPollToken(work)
}

// WorkPollToken is a work chip's state as the status poll carries it:
// "<subagents>.<shells>", with a trailing "p" for a lower bound ("1+ shell");
// "0.0" for no chip. Two reports that draw the same chip have the same token.
func WorkPollToken(w mirror.WindowWork) string {
	t := strconv.Itoa(w.Subagents) + "." + strconv.Itoa(w.Shells)
	if w.ShellsAtLeast {
		t += "p"
	}
	return t
}

// AgentsAwaitingEvent is the HX-Trigger the status poll answers with when what
// an agent's card was drawn with is no longer true — its awaiting-approval
// state, or its work chip (a new report, or one that expired with no POST to
// announce it); the Agents list re-renders on it (agentsListTrigger).
const AgentsAwaitingEvent = "agents:awaiting"

// statusLabel is the agent-facing word for a stored agent status.
func statusLabel(status string) string {
	switch status {
	case "running", "provisioning", "error", "stopped":
		return status
	default:
		return "pending"
	}
}

// agentModalShell is the static (always-present, hidden) dispatch modal. Its
// body is loaded on demand by the FAB so it stays out of the 4s poll cycle.
func agentModalShell() g.Node {
	return Div(
		ID("agent-modal"),
		Class("hidden fixed inset-0 z-40 flex items-end justify-center sm:items-center"),
		Div(
			Class("absolute inset-0 bg-black/60"),
			// Ambiguous dismiss → the SAME dirty guard the task sheet uses, so a
			// typed ad-hoc prompt is not destroyed by a stray tap.
			hx("hx-on:click", "if(window.cgModalDismiss){window.cgModalDismiss('agent-modal')}else{document.getElementById('agent-modal').classList.add('hidden')}"),
		),
		Div(
			ID("agent-modal-body"),
			// Same height cap as #task-modal-body: an uncapped `items-end` sheet
			// pushes its own header (and the ✕) off the top of a phone viewport and
			// leaves no tappable backdrop, so the dialog becomes a dead end.
			// pb uses the safe-area inset for the same reason <main> and the FABs do:
			// this sheet sits flush against the viewport bottom, so a flat p-5 put its
			// last control (Dispatch) under the iOS home indicator.
			Class("relative z-10 max-h-[85dvh] w-full max-w-lg overflow-y-auto overscroll-contain rounded-t-2xl bg-s1 p-5 pb-[calc(1.25rem+env(safe-area-inset-bottom))] shadow-2xl ring-1 ring-line sm:rounded-2xl sm:pb-5"),
			// body injected via /ui/agents/new
		),
		modalDiscardBar("agent-modal"),
	)
}

// DispatchModalBody is the /ui/agents/new partial (FAB path): the full dispatch
// form. It renders INSTANTLY — the repo + task option lists lazy-load (with
// animated skeletons) and the model field searches OpenRouter on type, so the
// form never blocks on a slow upstream. Morphed into #agent-modal-body when the
// FAB is tapped. The Task-card Dispatch button uses the task-scoped variant
// (DispatchModalTaskBody) instead; this FAB form is unchanged.
func DispatchModalBody() g.Node { return DispatchModalBodyFor(KindChoices{}) }

// ProvisionerGuideURL is docs/provisioner.md on GitHub: how to give muster an
// agent runtime so it can dispatch.
const ProvisionerGuideURL = "https://github.com/ZacxDev/muster/blob/main/docs/provisioner.md"

// DispatchUnavailableAttr marks the sheet a server with no agent runtime
// answers GET /ui/agents/new with, in place of the dispatch form.
const DispatchUnavailableAttr = "data-dispatch-unavailable"

// DispatchOffHintAttr marks the Agents header's note on a server that cannot
// dispatch (dispatchOffHint).
const DispatchOffHintAttr = "data-dispatch-off-hint"

// dispatchOffHint is the Agents list header's one-line note on a server with no
// agent runtime: the "+" is absent from the bar, and this says why and links
// the guide. Muted text, not a control that looks like an action. h-11 because
// it is a link, and every link a thumb can hit is a 44px target.
func dispatchOffHint() g.Node {
	return A(
		g.Attr(DispatchOffHintAttr, ""),
		Href(ProvisionerGuideURL), Target("_blank"), Rel("noopener"),
		g.Attr("title", "This server has no agent runtime, so it cannot start agents. Agents you run in tmux still show up here."),
		Class("ml-auto inline-flex h-11 shrink-0 items-center px-1 text-xs text-muted underline decoration-dotted underline-offset-2 hover:text-fg"),
		g.Text("Agent dispatch not set up"),
	)
}

// DispatchUnavailableBody is the /ui/agents/new partial on a server that cannot
// dispatch (Features.Dispatch false): what is missing and where to set it up,
// instead of a form whose submit can only answer 503.
//
// No document drawn by that server offers the sheet (no "+", no card Dispatch
// button), so this is reached by a page rendered before the server lost its
// provisioner, or a hand-typed request. It carries no form, so nothing on it
// can POST /agents.
func DispatchUnavailableBody() g.Node {
	return Div(
		g.Attr(DispatchUnavailableAttr, ""),
		Div(
			Class("mb-4 flex items-center gap-2"),
			H2(Class("text-base font-semibold text-fg"), g.Text("Agent dispatch is not set up")),
			Span(Class("flex-1")),
			dispatchCloseButton(),
		),
		Div(
			Class("space-y-2 text-sm leading-relaxed text-muted"),
			P(g.Text("This server has no agent runtime configured, so it cannot start agents. "+
				"Agents you run in tmux on a connected machine still show up in muster.")),
			P(
				g.Text("Dispatch needs an agent provisioner (MUSTER_AGENT_PROVISIONER); the docker compose install ships without one. See "),
				A(Href(ProvisionerGuideURL), Target("_blank"), Rel("noopener"), Class("text-accent underline"), g.Text("docs/provisioner.md")),
				g.Text("."),
			),
		),
	)
}

// DispatchModalBodyFor is DispatchModalBody with the deployment's agent kinds;
// more than one renders the kind picker (see kindPicker).
func DispatchModalBodyFor(k KindChoices) g.Node {
	return g.Group{
		Div(
			Class("mb-4 flex items-center gap-3"),
			H2(Class("text-base font-semibold text-fg"), g.Text("Dispatch agent")),
			Span(Class("flex-1")),
			dispatchCloseButton(),
		),
		Form(
			hx("hx-post", "/agents"),
			// Nothing to swap: the answer sends the page to the new agent
			// (HX-Redirect, internal/api handleAgentCreate). It used to morph
			// #agents-list, which only the retired Agents list page had — on any
			// other document htmx would have sent nothing at all.
			hx("hx-swap", "none"),
			// Close the modal after a successful dispatch — but ONLY for THIS form's
			// own request. htmx:afterRequest bubbles, so the lazy repo/task combobox
			// loads (hx-get on child <ul>s) would otherwise fire this handler and slam
			// the modal shut ~immediately after it opens (event.target is the child,
			// not the form). Guarding on event.target===this scopes it to the submit.
			hx("hx-on::after-request", dispatchCloseOnOwnSubmit),
			Class("flex flex-col gap-3"),
			// The "what to dispatch" (an existing task or an ad-hoc description) is the
			// primary decision, so it stays visible. Model/Repository are
			// tucked under a collapsed "Advanced" disclosure — they default to the
			// cluster's MUSTER_AGENT_MODEL + last-used repo (localStorage recents)
			// so a one-tap dispatch is sane without ever opening Advanced.
			labelledField("Existing task", lazyCombobox("note", "Existing task", "Search tasks… (optional)", "note_id", "/ui/agents/notes", "")),
			labelledField("Or ad-hoc task", Textarea(
				Name("note_text"),
				Rows("3"),
				Placeholder("Task for the agent (used if no task selected)…"),
				Class("w-full resize-y rounded-lg border-0 bg-bg px-3 py-2 text-sm text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
			)),
			kindPicker(k),
			// The model is gateway-only (a claude-code dispatch is refused one, see
			// kindToggleScript); the repository is for both kinds — a claude-code
			// agent's session starts in a checkout of it.
			advancedDisclosure(
				gatewayOnly(labelledField("Model", modelField())),
				labelledField("Repository", lazyCombobox("repo", "Repository", "Search repos… (optional)", "repo", "/ui/agents/repos", "repos")),
			),
			Div(
				// Sticky as a ROW: both actions stay in view when Advanced pushes them
				// below the sheet's fold (see TestEverySheetKeepsItsPrimaryActionInView).
				Class("sticky bottom-0 z-10 mt-1 grid grid-cols-2 gap-2 bg-s1 shadow-[0_-16px_16px_-4px_rgb(var(--mu-s1))]"),
				Button(
					Type("submit"),
					Name("action"), Value("dispatch"),
					Class("press inline-flex items-center justify-center rounded-xl bg-accent px-4 py-3 text-sm font-semibold text-on-accent transition hover:bg-accent/90 active:scale-[0.98] disabled:opacity-60"),
					// 🔴 No hx-disabled-elt="this" here. htmx reads hx-disabled-elt from
					// the element that ISSUES the request, and this POST is issued by the
					// FORM's submit event — so the attribute that used to sit on both
					// submit buttons could provably never fire (same finding as the task
					// edit form, notes.go). It was removed rather than left in place
					// reading as a working double-submit guard.
					//
					// 🔴 The justification that used to follow — "moving it to the <form>
					// is not a drop-in because hx-disabled-elt is inherited, so the lazy
					// children's hx-get on `load` would disable both buttons, and a
					// form-level guard needs hx-disinherit" — is RETRACTED. The attribute
					// IS inherited, but its VALUE is resolved against the issuing element
					// (htmx 2.0.4, findAttributeTargets): "this" resolves to the closest
					// ancestor-or-self CARRYING the attribute (the form, which has no
					// disabled state), and "find X" is scoped to the issuer, so a lazy
					// <ul>/<div> containing no submit button matches nothing. Neither
					// reaches these buttons. No hx-disinherit is required.
					//
					// What is actually true of the code below: the form has NO in-flight
					// double-submit guard at all. The task edit form's working idiom is
					// hx-disabled-elt="find button[type='submit']" ON THE FORM (notes.go).
					// That is the change to make here; it is simply not made yet, and it
					// is untested against this form's two lazy children (the task and
					// repo comboboxes each fire hx-get on `load`; the model field is
					// seeded, not lazy).
					g.Text("Dispatch"),
				),
				Button(
					Type("submit"),
					Name("action"), Value("save"),
					Class("press inline-flex items-center justify-center rounded-xl px-4 py-3 text-sm font-semibold text-fg ring-1 ring-inset ring-edge transition hover:bg-s2 active:scale-[0.98] disabled:opacity-60"),
					// See the Dispatch button above: hx-disabled-elt="this" on a submit
					// button is inert.
					g.Text("Save for later"),
				),
			),
		),
	}
}

// dispatchCloseOnOwnSubmit closes the dispatch modal after THIS form's own
// successful submit (guarded by event.target===this so bubbled child combobox
// loads don't slam it shut) and fires the dispatch analytics event. Shared by
// the FAB and task-scoped dispatch forms.
const dispatchCloseOnOwnSubmit = "if(event.target===this && event.detail.successful){if(window.cgModalClose){window.cgModalClose('agent-modal')}else{document.getElementById('agent-modal').classList.add('hidden')}try{window.cgTrack('agent.dispatch',{});}catch(e){}}"

// dispatchCloseButton is the modal's top-right close (×).
func dispatchCloseButton() g.Node {
	return Button(
		Type("button"),
		g.Attr("aria-label", "Close"),
		Class("press inline-flex h-11 w-11 items-center justify-center rounded-lg text-muted transition hover:bg-s2 hover:text-fg"),
		hx("hx-on:click", "if(window.cgModalClose){window.cgModalClose('agent-modal')}else{document.getElementById('agent-modal').classList.add('hidden')}"),
		g.Raw(`<svg class="h-5 w-5" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M5 5l10 10M15 5L5 15"/></svg>`),
	)
}

// TaskDispatchView is the resolved dispatch config for a Task-scoped dispatch
// confirm (the from-card path). Every field is pre-resolved server-side so the
// modal is a pre-filled confirm: Model/Repo/RepoBranch are the effective values
// (empty = cluster default / none / repo default).
type TaskDispatchView struct {
	NoteID     int64
	Label      string // the task's first line / directory (header subtitle)
	Model      string // resolved OpenRouter slug ("" = cluster default)
	Repo       string // resolved repo full name ("" = none / not inferred)
	RepoBranch string // resolved branch ("" = repo default, resolved server-side)

	// Kinds is the deployment's agent kinds; more than one renders the picker.
	Kinds KindChoices
}

// dispatchTarget is the (selector, swap) pair the task-scoped dispatch form
// posts into: the task's own card, on the board and on /tasks/{id} alike, and
// POST /agents answers a request targeting it with that card (internal/api's
// handleAgentCreate reads HX-Target): the dispatch's visible effect is the task
// flipping to in-progress with its new agent's status chip.
//
// 🔴 htmx resolves hx-target BEFORE it issues the request; when the selector
// matches nothing it raises htmx:targetError and sends NOTHING — no request,
// no error, no toast, a sheet that does not even close. The board's form used
// to target #agents-list, which only existed because the retired Agents tab's
// panel was drawn (hidden) into every shell document; the card is there on
// every document that draws the Dispatch button.
func (v TaskDispatchView) dispatchTarget() (selector, swap string) {
	return "#task-" + strconv.FormatInt(v.NoteID, 10), "morph:outerHTML"
}

// DispatchModalTaskBody is the /ui/agents/new?note=<id> partial: a TASK-SCOPED
// dispatch confirm. The task is known, so instead of the full form's "Existing
// task" combobox + ad-hoc textarea, it shows two fixed rows — Model and
// Repository — each with the task's resolved value as static text plus an inline
// "Edit" that reveals the corresponding editable control (pre-seeded to the
// current value). The editable controls carry the same field names the dispatch
// POST reads (model / repo), and — being present-but-hidden — they
// submit the resolved values even when never opened, so this is a one-tap confirm.
// Overrides are THIS-DISPATCH-ONLY (never persisted back to the task). The task is
// submitted via a hidden note_id; repo_branch rides along as a hidden field.
func DispatchModalTaskBody(v TaskDispatchView) g.Node {
	ids := strconv.FormatInt(v.NoteID, 10)
	target, swap := v.dispatchTarget()

	// Static row displays.
	modelStatic := shortModelLabel(v.Model)
	if modelStatic == "" {
		modelStatic = defaultModelShortLabel()
	}
	// The task's title, as a reader of it sees it (textplain).
	taskLabel := textplain.Title(v.Label)
	repoStatic := v.Repo
	if repoStatic == "" {
		repoStatic = "none"
	}

	return g.Group{
		Div(
			Class("mb-4 flex items-start gap-3"),
			Div(
				Class("min-w-0"),
				H2(Class("text-base font-semibold text-fg"), g.Text("Dispatch · Task #"+ids)),
				g.If(taskLabel != "", P(Class("mt-0.5 truncate text-xs text-muted"), g.Text(taskLabel))),
			),
			Span(Class("flex-1")),
			dispatchCloseButton(),
		),
		Form(
			hx("hx-post", "/agents"),
			// The task's card (dispatchTarget): an unresolvable hx-target makes htmx
			// drop the request silently rather than fail loudly.
			hx("hx-target", target),
			hx("hx-swap", swap),
			hx("hx-on::after-request", dispatchCloseOnOwnSubmit),
			Class("flex flex-col gap-3"),
			// The task is known — submit it by id (no combobox, no ad-hoc textarea).
			Input(Type("hidden"), Name("note_id"), Value(ids)),
			// Resolved branch rides along (no visible row; the FAB form has no branch
			// field either — it resolves server-side when empty).
			Input(Type("hidden"), Name("repo_branch"), Value(v.RepoBranch)),
			kindPicker(v.Kinds),
			// Gateway-only, like the FAB form's model field: the task's model is
			// refused for a claude-code dispatch, so it leaves the screen when
			// Claude Code is picked (kindToggleScript). The repository stays: both
			// kinds work in it.
			gatewayOnly(
				Class("flex flex-col gap-3"),
				dispatchConfirmRow("Model", modelStatic, modelFieldFor(v.Model, "")),
			),
			dispatchConfirmRow("Repository", repoStatic,
				lazyComboboxPreselect("repo", "Repository", "Search repos… (optional)", "repo", "/ui/agents/repos", "repos", v.Repo, v.Repo)),
			// Mobile-first action stack: a small secondary Cancel sits ABOVE the
			// full-width primary Dispatch, so the thumb lands on Dispatch and Cancel
			// is a deliberate, smaller reach.
			Div(
				Class("mt-1 flex flex-col gap-2"),
				Button(
					Type("button"),
					Class("press min-h-[44px] self-end inline-flex items-center justify-center rounded-lg px-3 py-1.5 text-xs font-medium text-muted ring-1 ring-inset ring-edge transition hover:bg-s2 active:scale-[0.98]"),
					hx("hx-on:click", "if(window.cgModalClose){window.cgModalClose('agent-modal')}else{document.getElementById('agent-modal').classList.add('hidden')}"),
					g.Text("Cancel"),
				),
				Button(
					Type("submit"),
					Name("action"), Value("dispatch"),
					Class("sticky bottom-0 z-10 shadow-[0_-16px_16px_-4px_rgb(var(--mu-s1))] press w-full inline-flex items-center justify-center rounded-xl bg-accent px-4 py-3 text-sm font-semibold text-on-accent transition hover:bg-accent/90 active:scale-[0.98] disabled:opacity-60"),
					hx("hx-disabled-elt", "this"),
					g.Text("Dispatch"),
				),
			),
		),
	}
}

// dispatchConfirmRow is one row of the task-scoped confirm: a label, the resolved
// value as static text, and an inline "Edit" that reveals the (pre-seeded)
// editable control. The editable control is present-but-hidden so it submits the
// resolved value even when never opened; Edit only unhides it for an override.
// The toggle is a CSP-safe hx-on:click (the app's CSP allows hx-on via
// unsafe-eval) that hides the static value + Edit and shows the control — the same
// data-attribute reveal pattern the delete control uses.
func dispatchConfirmRow(label, static string, editable g.Node) g.Node {
	const toggle = "var r=this.closest('[data-dispatch-row]');" +
		"r.querySelector('[data-dispatch-static]').classList.add('hidden');" +
		"r.querySelector('[data-dispatch-editable]').classList.remove('hidden');" +
		"this.classList.add('hidden');"
	return Div(
		g.Attr("data-dispatch-row", ""),
		Class("flex flex-col gap-1.5"),
		Div(
			Class("flex items-center gap-2"),
			Label(Class("text-xs font-medium text-muted"), g.Text(label)),
			Span(Class("flex-1")),
			Button(
				Type("button"),
				g.Attr("data-dispatch-edit", ""),
				g.Attr("aria-label", "Edit "+label),
				Class("press min-h-[44px] rounded-lg px-2 py-0.5 text-xs font-medium text-accent transition hover:bg-accent/10 hover:text-fg"),
				hx("hx-on:click", toggle),
				g.Text("Edit"),
			),
		),
		Div(
			g.Attr("data-dispatch-static", ""),
			Class("truncate text-sm text-fg"),
			g.Text(static),
		),
		Div(
			g.Attr("data-dispatch-editable", ""),
			Class("hidden"),
			editable,
		),
	)
}

// comboOption renders one combobox <li> (data-value + data-label).
func comboOption(o ComboOption) g.Node {
	return Li(
		g.Attr("role", "option"),
		g.Attr("data-combobox-option", ""),
		g.Attr("data-value", o.Value),
		g.Attr("data-label", o.Label),
		Class("cursor-pointer truncate px-3 py-2 text-sm text-fg hover:bg-accent/15 hover:text-fg data-[active=true]:bg-accent/15 data-[active=true]:text-fg"),
		g.Text(o.Label),
	)
}

// seededComboOption is a combobox <li> marked as a SEEDED quick-pick
// (data-combobox-seed): in a remote combobox these are preserved at the top of
// the list across remote searches (the combobox JS filters them by query and
// dedupes them against remote results) instead of being wiped on each fetch.
func seededComboOption(o ComboOption) g.Node {
	return Li(
		g.Attr("role", "option"),
		g.Attr("data-combobox-option", ""),
		g.Attr("data-combobox-seed", ""),
		g.Attr("data-value", o.Value),
		g.Attr("data-label", o.Label),
		Class("cursor-pointer truncate px-3 py-2 text-sm text-fg hover:bg-accent/15 hover:text-fg data-[active=true]:bg-accent/15 data-[active=true]:text-fg"),
		g.Text(o.Label),
	)
}

// RepoView is the minimal repo shape the dispatch dialog's repository picker
// renders (decoupled from the GitHub API client type).
type RepoView struct {
	FullName string // owner/name
	Private  bool
	HTMLURL  string
}

// RenderRepoOptions / RenderNoteOptions write the lazy-loaded combobox option
// lists (swapped in over the skeleton).
func RenderRepoOptions(w io.Writer, repos []RepoView) error {
	return comboOptionList(repoOptions(repos)).Render(w)
}

func RenderNoteOptions(w io.Writer, notes []NoteOption) error {
	return comboOptionList(noteOptions(notes)).Render(w)
}

func comboOptionList(opts []ComboOption) g.Node {
	if len(opts) == 0 {
		return Li(Class("px-3 py-2 text-sm text-muted"), g.Text("none"))
	}
	return g.Group(g.Map(opts, comboOption))
}

func repoOptions(repos []RepoView) []ComboOption {
	opts := make([]ComboOption, 0, len(repos))
	for _, r := range repos {
		opts = append(opts, ComboOption{Value: r.FullName, Label: r.FullName})
	}
	return opts
}

func noteOptions(notes []NoteOption) []ComboOption {
	opts := make([]ComboOption, 0, len(notes))
	for _, n := range notes {
		opts = append(opts, ComboOption{Value: strconv.FormatInt(n.ID, 10), Label: textplain.Title(n.Label)})
	}
	return opts
}

// comboboxSkeleton is the subtly-animated placeholder shown in a lazy combobox
// list (and inside the dropdown) until the real options load.
func comboboxSkeleton() g.Node {
	bar := func(w string) g.Node {
		return Li(Class("px-3 py-2"), Div(Class("h-3 "+w+" animate-pulse rounded bg-s3/60")))
	}
	return g.Group{bar("w-3/4"), bar("w-1/2"), bar("w-2/3")}
}

// CuratedModel is one option in the curated model dropdown.
type CuratedModel struct {
	Value string // submitted model slug; empty means "use the server default"
	Label string // visible option text
}

// curatedModels are the known-good models offered in the dispatch modal's common
// path. The first (empty value) is the default: posting an empty model lets the
// server's MUSTER_AGENT_MODEL win. The rest are named, vetted slugs. Power users
// can still reach ANY OpenRouter slug via the "search OpenRouter…" expander.
func curatedModels() []CuratedModel {
	out := []CuratedModel{
		{Value: "", Label: "default (" + defaultModelShortLabel() + ")"},
		{Value: "openrouter/anthropic/claude-haiku-4.5", Label: shortModelLabel("openrouter/anthropic/claude-haiku-4.5")},
		{Value: "openrouter/anthropic/claude-sonnet-4.6", Label: shortModelLabel("openrouter/anthropic/claude-sonnet-4.6")},
	}
	// Fold in the other known-good slugs the agents package already references
	// (deduped against the default), so this list tracks the dispatch suggestions.
	seen := map[string]bool{}
	for _, m := range out {
		seen[m.Value] = true
	}
	def := agents.DefaultModelOption()
	for _, m := range agents.ModelOptions {
		if m.Value == def || seen[m.Value] {
			continue
		}
		seen[m.Value] = true
		out = append(out, CuratedModel{Value: m.Value, Label: shortModelLabel(m.Value)})
	}
	return out
}

// defaultModelShortLabel is the short label for the server-default option.
func defaultModelShortLabel() string {
	if def := agents.DefaultModelOption(); def != "" {
		return shortModelLabel(def)
	}
	return "server default"
}

// shortModelLabel trims the leading "openrouter/" provider prefix from a slug for
// display, keeping the rest of the path intact
// (e.g. "openrouter/anthropic/claude-haiku-4.5" → "anthropic/claude-haiku-4.5").
// Slugs without the prefix (and non-model values like repo names) pass through.
func shortModelLabel(slug string) string {
	return strings.TrimPrefix(slug, "openrouter/")
}

// modelName is the AGENT-VIEW model display: just the model's own name — the
// last '/'-segment of an OpenRouter slug — e.g. "openrouter/deepseek/deepseek-v4-pro"
// → "deepseek-v4-pro". Empty in → empty out. Unlike shortModelLabel (which only
// strips the "openrouter/" provider prefix, keeping the vendor path) this drops
// everything before the final segment. Used ONLY for the agent-detail model
// control's visible label; the hidden input still submits the full slug.
func modelName(slug string) string {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return ""
	}
	if i := strings.LastIndex(slug, "/"); i >= 0 {
		return slug[i+1:]
	}
	return slug
}

// agentModelLabel is the short display label for an agent's configured model:
// the slug with its leading "openrouter/" prefix trimmed, or "default" when unset
// (cluster default wins).
func agentModelLabel(slug string) string {
	if strings.TrimSpace(slug) == "" {
		return "default"
	}
	return shortModelLabel(slug)
}

// modelField is the LLM model selector: ONE consolidated searchable combobox
// (the 0.8.0 component). It shows the curated quick-picks as initial options,
// filters them as you type, AND searches any OpenRouter slug live (proxied via
// /api/openrouter/models). The visible input IS the submitted `model` field
// (id="model-input"); an empty value means "use the cluster default".
func modelField() g.Node { return modelFieldFor("", "") }

// modelFieldWithDisplay is modelFieldFor with an explicit visible-label override.
// When display is non-empty it is shown in the visible input verbatim (the hidden
// input still submits the FULL `selected` slug); when empty the default display
// (shortModelLabel / cluster-default label) is used. The agent-detail control
// passes modelName(selected) so the visible label is just the model name while
// switching still posts the full slug.
func modelFieldWithDisplay(selected, note, display string) g.Node {
	return modelFieldForDisplay(selected, note, display)
}

// modelFieldFor renders the consolidated searchable model combobox, pre-seeded
// with `selected` (an OpenRouter slug; "" = the cluster default). Used by both
// the dispatch modal (selected="") and the agent-detail change control
// (selected=agent.Model) so the control reflects the agent's current model.
//
// It reuses the existing custom combobox component (data-combobox +
// data-combobox-input + data-combobox-list + data-combobox-remote) in
// hidden-value mode (data-combobox-hidden). The list is pre-seeded with the
// curated quick-picks as inline <li data-combobox-option> entries so they show
// without a remote round-trip; typing filters those AND fetches matching
// OpenRouter slugs from the proxy. A hidden input named `model` carries the
// submitted FULL slug; the visible input (id="model-input") shows the trimmed
// display label only. On pick, the combobox JS writes the option's data-value
// (full slug) into the hidden field and its data-label (trimmed) into the visible
// input. The hidden field is pre-filled with `selected` (the current slug) so an
// unchanged submit re-posts it; empty when nothing is selected (server default
// wins).
func modelFieldFor(selected, note string) g.Node {
	return modelFieldForDisplay(selected, note, "")
}

// modelFieldForDisplay is modelFieldFor with an optional visible-label override
// (displayOverride). Empty override → the default display (shortModelLabel of the
// selected slug, or the cluster-default label when unset). The hidden `model`
// input always carries the FULL `selected` slug regardless of the visible label.
func modelFieldForDisplay(selected, note, displayOverride string) g.Node {
	listID := "combobox-list-model"
	curated := curatedModels()

	// Seed the list with the curated quick-picks. The first entry (empty value)
	// is the cluster default; render it with the ◉ marker and a clear label so an
	// empty submit obviously means "default". The combobox is in hidden-value mode
	// (data-combobox-hidden), so each option's data-value is the FULL slug written
	// into the hidden `model` field on pick, while its data-label is the trimmed
	// display shown in the visible input. The empty default writes an empty value.
	seeded := make([]g.Node, 0, len(curated))
	for i, m := range curated {
		label := m.Label
		if i == 0 {
			label = "◉ " + label
		}
		seeded = append(seeded, seededComboOption(ComboOption{Value: m.Value, Label: label}))
	}

	// The visible input shows the CURRENT model rather than a bare placeholder:
	// the trimmed slug, or — when nothing is set — the effective cluster-default
	// model's short label (just the name, no "default (…)" wrapper). The hidden
	// field still carries the real submit value ("" = default), so display is
	// purely cosmetic; the dropdown's ◉ option is where "reset to default" lives.
	// An explicit override (agent-view name-only label) wins; otherwise the
	// default display: the trimmed slug, or the cluster-default label when unset.
	display := displayOverride
	if display == "" {
		display = shortModelLabel(selected)
		if display == "" {
			display = defaultModelShortLabel()
		}
	}

	// A passive (non-option) sticky note pinned to the top of the dropdown — used
	// for the agent-detail "selecting a model restarts the agent" warning. Empty
	// in the dispatch modal (creating a new agent restarts nothing). It carries no
	// data-combobox-option, so the filter/keyboard nav ignores it.
	var noteNode g.Node = g.Text("")
	if note != "" {
		noteNode = Li(
			g.Attr("aria-hidden", "true"),
			Class("sticky top-0 z-10 border-b border-line bg-s1 px-3 py-1.5 text-[11px] text-st-warning-fg"),
			g.Text("↳ "+note),
		)
	}

	return Div(
		g.Attr("data-combobox", ""),
		g.Attr("data-combobox-remote", "/api/openrouter/models"),
		Class("relative"),
		// Hidden field carries the SUBMITTED full slug ("" = cluster default). The
		// visible input below only drives display/search and does not submit.
		Input(Type("hidden"), Name("model"), g.Attr("data-combobox-hidden", ""), Value(selected)),
		Input(
			Type("text"),
			ID("model-input"),
			g.Attr("data-combobox-input", ""),
			g.Attr("role", "combobox"),
			g.Attr("aria-expanded", "false"),
			g.Attr("aria-controls", listID),
			g.Attr("aria-label", "Model — pick a quick-pick or type to filter/search"),
			g.Attr("autocomplete", "off"),
			// Pre-filled with the TRIMMED display of the current slug so the user sees
			// a clean label; the hidden field holds the full slug that submits. Empty
			// when nothing is selected → the cluster default wins unless changed.
			Value(display),
			Placeholder("Model — quick-picks, or type to filter/search…"),
			Class("w-full rounded-lg border-0 bg-bg px-3 py-2.5 text-sm text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
		),
		Ul(
			ID(listID),
			g.Attr("data-combobox-list", ""),
			g.Attr("role", "listbox"),
			// Pre-seeded curated quick-picks; typing filters these and also fetches
			// matching OpenRouter slugs from the remote proxy.
			Class("hidden absolute z-10 mt-1 max-h-56 w-full overflow-auto rounded-lg bg-s1 py-1 shadow-2xl ring-1 ring-line"),
			noteNode,
			g.Group(seeded),
		),
	)
}

// lazyCombobox is a type-to-filter combobox whose option list lazy-loads from
// optionsURL (hx-trigger load) into an animated skeleton — so the modal renders
// instantly and never blocks on the GitHub API / DB. A hidden input (hiddenName)
// carries the submitted value (full name for repos, note id for tasks). When
// recentsKey is non-empty, recently-picked values float to the top (localStorage,
// wired in appScript). Behaviour is delegated in appScript via data-combobox*.
func lazyCombobox(fieldName, label, placeholder, hiddenName, optionsURL, recentsKey string) g.Node {
	return lazyComboboxPreselect(fieldName, label, placeholder, hiddenName, optionsURL, recentsKey, "", "")
}

// lazyComboboxPreselect is lazyCombobox with an optional pre-selected value. When
// presetValue is non-empty the hidden input is pre-filled with presetValue and the
// visible input shows presetLabel — these are SIBLINGS of the lazy <ul>, so they
// survive the options swap and an unchanged submit re-posts the selection. A
// SEEDED quick-pick option (data-combobox-seed) for the same value is also rendered
// at the top of the list so the pick is visible immediately on open (the lazy
// hx-get later swaps the <ul> with the real task list, which includes this task as
// a normal option). When presetValue is empty it renders EXACTLY like the plain
// lazyCombobox (the FAB path stays byte-for-byte unchanged).
func lazyComboboxPreselect(fieldName, label, placeholder, hiddenName, optionsURL, recentsKey, presetValue, presetLabel string) g.Node {
	listID := "combobox-list-" + fieldName
	wrapAttrs := []g.Node{g.Attr("data-combobox", ""), Class("relative")}
	if recentsKey != "" {
		wrapAttrs = append(wrapAttrs, g.Attr("data-combobox-recents", recentsKey))
	}

	hiddenAttrs := []g.Node{Type("hidden"), Name(hiddenName), g.Attr("data-combobox-hidden", "")}
	inputAttrs := []g.Node{
		Type("text"),
		g.Attr("data-combobox-input", ""),
		g.Attr("role", "combobox"),
		g.Attr("aria-expanded", "false"),
		g.Attr("aria-controls", listID),
		g.Attr("aria-label", label),
		g.Attr("autocomplete", "off"),
		Placeholder(placeholder),
		Class("w-full rounded-lg border-0 bg-bg px-3 py-2 text-sm text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"),
	}
	// The seeded preselect option lives at the TOP of the list (before the
	// skeleton). The lazy load (hx-get) swaps the list's innerHTML, but
	// RenderNoteOptions renders only non-seed options and the combobox JS dedupes
	// against seeds — so the seed survives and the hidden value stays valid.
	var seed g.Node = g.Text("")
	if presetValue != "" {
		hiddenAttrs = append(hiddenAttrs, Value(presetValue))
		inputAttrs = append(inputAttrs, Value(presetLabel))
		seed = seededComboOption(ComboOption{Value: presetValue, Label: presetLabel})
	}

	wrapAttrs = append(wrapAttrs,
		Input(hiddenAttrs...),
		Input(inputAttrs...),
		Ul(
			ID(listID),
			g.Attr("data-combobox-list", ""),
			g.Attr("role", "listbox"),
			// Lazy-load the options when the modal opens (load fires on swap-in);
			// the skeleton shows until they arrive.
			hx("hx-get", optionsURL),
			hx("hx-trigger", "load"),
			// Target THIS <ul> explicitly. htmx hx-target is inherited by
			// descendants, and this combobox lives inside a dispatch form whose
			// hx-target is the task's card (the task-scoped one) — without this,
			// the options would load into that card and the skeleton here would
			// never resolve.
			hx("hx-target", "this"),
			hx("hx-swap", "innerHTML"),
			Class("hidden absolute z-10 mt-1 max-h-56 w-full overflow-auto rounded-lg bg-s1 py-1 shadow-2xl ring-1 ring-line"),
			seed,
			comboboxSkeleton(),
		),
	)
	return Div(wrapAttrs...)
}

func displayOr(a, b string) string {
	if strings.TrimSpace(a) == "" {
		return b
	}
	return a
}
