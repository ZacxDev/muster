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

// ---------------------------------------------------------------------------
// THE FLEET DIGEST CARD (internal/fleet; docs/chief.md "Fleet digest").
//
// 🔴 PINNED ABOVE WHICHEVER CHAT IS OPEN, AND NOT A CHAT MESSAGE. The card has
// its own slot in the chief panel (fleetDigestSlot, rendered by ChiefPanel
// between the title bar and #chief-panel-body), so switching threads swaps the
// chat under it and leaves it in place. It is muster's own rendering of the
// fleet reader (internal/api fleet_read.go): no chief turn, no model.
//
// 🔴 THE SLOT IS NEVER EMPTY, SO THE CHAT NEVER MOVES. The shell renders the
// card's skeleton (FleetDigestSkeleton) at the card's own height, and the
// fetch on open replaces it with the real card at the same height: every row
// is one line, the "since" line is always drawn, the groups start closed. A
// card that grew in when it landed would shift the conversation under the
// operator's thumb (e2e/tests/cls.spec.ts measures the panel opening).
//
// Desktop (sm and up): each group is a <details> that opens inline. A phone
// (below sm): one row of group chips; a chip opens that group in a bottom
// sheet (data-fleet-sheet), its rows' buttons 44px tall. Either way a row's
// name opens its drill-in (FleetDrill) under it, and the drill-in's "Ask the
// chief about this" opens the Fleet side chat with a prefilled draft, never a
// sent message.
// ---------------------------------------------------------------------------

// The digest's routes.
const (
	// FleetDigestPath is the card (GET). ?look=1 records a look: the panel
	// was opened on it (fleet.Store.Look).
	FleetDigestPath = "/ui/chief/fleet"
	// FleetDrillPath is one session's drill-in (GET ?id=<fleet id>).
	FleetDrillPath = "/ui/chief/fleet/session"
	// FleetChatPath opens the Fleet side chat (POST about=<fleet id>, or none
	// for the fleet as a whole), answered with the panel body on it.
	FleetChatPath = "/ui/chief/fleet/chat"
	// FleetActionPathPrefix is a proposal's card actions (fleet proposals):
	// POST <prefix><id>/approve, /edit (text=), /reject — each answered with
	// the card, its Proposed group open and a note.
	FleetActionPathPrefix = "/ui/chief/fleet/actions/"
	// FleetBulkPath approves or rejects every listed proposal (POST op=approve
	// or op=reject, id=… for each row the owner was shown).
	FleetBulkPath = "/ui/chief/fleet/bulk"
	// FleetStepUpPath is the "Confirm it's you" dialog an approval loads when
	// the sign-in is stale and the page has none (GET).
	FleetStepUpPath = "/ui/chief/fleet/stepup"
)

// FleetActionPath is proposal id's card action verb (approve, edit, reject).
func FleetActionPath(id int64, verb string) string {
	return FleetActionPathPrefix + strconv.FormatInt(id, 10) + "/" + verb
}

// ChiefOpenParam is the query parameter that opens the chief panel on load
// (?chief=open): the fleet's "N proposals waiting" push's tap.
const ChiefOpenParam = "chief"

// ChiefOpenURL is a link that opens the chief panel on the home page.
const ChiefOpenURL = "/?" + ChiefOpenParam + "=open"

// The card's ids and attributes: what both test tiers read.
const (
	// fleetOpenEvent loads the card: dispatched by chiefPanelScript on a real
	// open (detail.look true) and once per slot by restore (look false).
	// Spelled as a literal in fleetDigestSlot's trigger too (the hx-trigger
	// ledger folds literals); TestTheFleetCardLoadsOnItsOwnEvent pins the two.
	fleetOpenEvent = "muster:fleet-open"
	// fleetRequestedAttr marks a slot element whose fetch has started
	// (htmx:beforeRequest): set by the CLIENT, never rendered, so restore()
	// asks a fresh element (a boosted navigation's) until it fetches, and
	// never again after — a failing fetch is not retried on every settle.
	fleetRequestedAttr = "data-fleet-requested"
	fleetDigestSlotID  = "fleet-digest"
	// fleetDigestStateAttr is the card's state: loading (the skeleton), ready,
	// or unavailable (FleetDigestView.Unavailable).
	fleetDigestStateAttr = "data-fleet-state"
	fleetGroupAttr       = "data-fleet-group"
	fleetCountAttr       = "data-fleet-count"
	fleetChipAttr        = "data-fleet-chip"
	fleetSheetAttr       = "data-fleet-sheet"
	fleetSheetCloseAttr  = "data-fleet-sheet-close"
	fleetRowAttr         = "data-fleet-row"
	fleetAskAttr         = "data-fleet-ask"
	fleetDrillAttr       = "data-fleet-drill"
	fleetDrillSlotAttr   = "data-fleet-drill-slot"
	fleetOpenAttr        = "data-fleet-open-session"
	fleetAskChiefAttr    = "data-fleet-ask-chief"
	fleetSinceAttr       = "data-fleet-since"
	fleetProposalAttr    = "data-fleet-proposal"
	fleetNoteAttr        = "data-fleet-note"
	fleetBulkAttr        = "data-fleet-bulk"
	// fleetStepUpSlotID is where the step-up dialog is loaded (ChiefPanel).
	fleetStepUpSlotID = "fleet-stepup"
	// ChiefPrefillAttr carries a draft the panel script puts in the chat box
	// once the body it is in has landed (the Fleet chat's "Ask the chief
	// about this"): it fills the box and sends nothing.
	ChiefPrefillAttr = "data-chief-prefill"
)

// The card's states (fleetDigestStateAttr).
const (
	FleetStateLoading     = "loading"
	FleetStateReady       = "ready"
	FleetStateUnavailable = "unavailable"
)

// FleetDigestView is the card.
type FleetDigestView struct {
	// Unavailable, when set, is why there is no digest (session state off, no
	// database): the card says it and nothing else.
	Unavailable string
	Total       int
	Groups      []FleetGroupView
	// Since is the "since you last looked" line ("" for no baseline yet).
	Since string
	// Note is the line the last card action left ("Sent.", "2 sent, 1
	// skipped (moved on)"), "" for none.
	Note string
	// OpenGroup is the group drawn open (the one a card action came from).
	OpenGroup string
}

// FleetGroupView is one group.
type FleetGroupView struct {
	Key, Label string
	// Note is the line the group's open view starts with ("" for none).
	Note string
	Rows []FleetRowView
	Asks []FleetAskView
	// Proposals are the Proposed group's cards (fleet proposals).
	Proposals []FleetProposalView
}

// Count is the group's count.
func (g FleetGroupView) Count() int { return len(g.Rows) + len(g.Asks) + len(g.Proposals) }

// FleetGroupProposed is the Proposed group's key: the chief's open proposals.
const FleetGroupProposed = "proposed"

// FleetProposalView is one proposal's card.
type FleetProposalView struct {
	ID int64
	// Session is its session's label, Where the window or agent.
	Session, Where string
	// Verb is its class as a verb ("Continue", "Write handoff, then close").
	Verb string
	// Reason is the proposer's one line.
	Reason string
	// Text is EXACTLY what will be sent, in full: muster's attribution line,
	// then the action's text.
	Text string
	// EditText is the text an Edit opens with (the action's own, without the
	// attribution line: an edited text is sent as the owner's).
	EditText string
	// Href is Open session ("" when it is not listed now).
	Href string
	// Age is how long it has waited.
	Age string
	// Editable: the class sends one text (a reassign routes a task first,
	// and is approved or rejected only).
	Editable bool
}

// FleetRowView is one session's row.
type FleetRowView struct {
	// ID is its fleet id ("mirror:<key>", "agent:<id>").
	ID    string
	Name  string
	State string
	// Detail is the row's second half: where, how long, its latest PR.
	Detail string
}

// FleetAskView is one of the chief's open cards in Needs you.
type FleetAskView struct {
	ID       int64
	Question string
	Age      string
	Href     string
}

// fleetDigestSlot is the card's slot in the panel, holding the skeleton until
// the panel opens.
//
// 🔴 ITS OWN EVENT, NOT THE BODY'S (fleetOpenEvent, audit round 1 🟡2): the
// body's muster:chief-open is re-dispatched by restore() on EVERY settle while
// the body has not loaded, and the card's own settle is one — so a card on that
// event refetched itself (a marks write and a whole fleet read each time) for
// as long as the body stayed unloaded. fleetOpenEvent is dispatched on a real
// open (setOpen, with look) and at most ONCE per slot element by restore
// (without look).
//
// 🔴 SPELLED AS LITERALS (the trigger), like the panel body's: a trigger the
// hx-trigger ledger cannot fold is a subscription it cannot verify. The live
// refresh is the attention queue's event (an episode opened, pushed or ended;
// an ask raised or answered: internal/api attentionChanged), debounced, and a
// slow poll; both are skipped while the panel is shut or the owner is inside a
// group (musterFleetShown, chiefPanelScript), so a refresh never closes what
// they opened.
func fleetDigestSlot(propose bool) g.Node {
	return Div(
		ID(fleetDigestSlotID),
		g.Attr("aria-live", "polite"),
		Class("flex-none border-b border-line px-3 py-2"),
		hx("hx-get", FleetDigestPath),
		hx("hx-trigger", "muster:fleet-open from:document, sse:attention_queue.changed[!window.musterFleetShown||musterFleetShown()] from:body delay:2s, every 120s[!window.musterFleetShown||musterFleetShown()]"),
		hx("hx-swap", "innerHTML"),
		FleetDigestSkeleton(propose),
	)
}

// FleetDigestSkeleton is the card before its first fetch: the real card's
// shape and height, every count "…".
func FleetDigestSkeleton(propose bool) g.Node {
	return fleetCard(FleetDigestView{Groups: skeletonGroups(propose)}, FleetStateLoading)
}

// skeletonGroups is the card's groups before a read: the digest's, with the
// Proposed group second when fleet proposals are on.
func skeletonGroups(propose bool) []FleetGroupView {
	out := make([]FleetGroupView, 0, len(fleetGroupKeys)+1)
	for _, k := range fleetGroupKeys {
		out = append(out, FleetGroupView{Key: k, Label: fleetGroupLabels[k]})
		if propose && k == "needs_you" {
			out = append(out, FleetGroupView{Key: FleetGroupProposed, Label: FleetProposedLabel})
		}
	}
	return out
}

// FleetProposedLabel is the Proposed group's name on the card.
const FleetProposedLabel = "Proposed"

// fleetGroupKeys and fleetGroupLabels mirror internal/fleet's GroupOrder and
// GroupLabel for the skeleton, which the shell renders without a read.
// TestTheSkeletonNamesTheFleetPackagesGroups pins the two together.
var fleetGroupKeys = []string{"needs_you", "waiting_ci", "working", "done", "stalled"}

var fleetGroupLabels = map[string]string{
	"needs_you": "Needs you", "waiting_ci": "Waiting on CI", "working": "Working", "done": "Done, can wrap", "stalled": "Stalled",
}

// RenderFleetDigest writes the card (the slot's content).
func RenderFleetDigest(w io.Writer, v FleetDigestView) error {
	state := FleetStateReady
	if v.Unavailable != "" {
		state = FleetStateUnavailable
	}
	return fleetCard(v, state).Render(w)
}

// fleetCard is the card in state.
func fleetCard(v FleetDigestView, state string) g.Node {
	loading := state == FleetStateLoading
	unavailable := state == FleetStateUnavailable
	head := "Fleet"
	sub := "loading…"
	switch {
	case unavailable:
		sub = "unavailable"
	case !loading:
		sub = plural(v.Total, "session", "sessions")
	}
	// 🔴 EVERY STATE HAS THE SKELETON'S SHAPE (the header row, one line under
	// it, five group rows; on a phone one row): an unavailable card draws the
	// groups inert with "–" and its reason on the since line, so the chat
	// under the card does not move when it lands (cls.spec.ts measured
	// 0.032 when the unavailable card was a single sentence).
	inert := loading || unavailable
	groups := v.Groups
	if unavailable {
		propose := false
		for _, gr := range v.Groups {
			propose = propose || gr.Key == FleetGroupProposed
		}
		groups = skeletonGroups(propose)
	}
	since := v.Since
	if since == "" {
		since = "Since you last looked: nothing recorded yet."
	}
	sinceCls := "hidden truncate text-[11px] text-muted sm:block"
	switch {
	case loading:
		since = " "
	case unavailable:
		since = v.Unavailable
		sinceCls = "hidden truncate text-[11px] text-st-warning-fg sm:block"
	}
	countOf := func(gr FleetGroupView) string {
		switch {
		case loading:
			return "…"
		case unavailable:
			return "–"
		}
		return strconv.Itoa(gr.Count())
	}
	return Section(
		g.Attr("data-fleet-digest", ""),
		g.Attr(fleetDigestStateAttr, state),
		g.Attr("aria-label", "Fleet digest"),
		Class("flex flex-col gap-1"),
		// The desktop head: name, total, the since line. A fixed height, so
		// the Ask button the ready card adds does not grow it.
		Div(Class("hidden h-7 items-center gap-2 sm:flex"),
			H2(Class("text-xs font-semibold uppercase tracking-wide text-fg2"), g.Text(head)),
			Span(Class("text-[11px] text-muted"), g.Text(sub)),
			g.If(!inert, fleetAskChiefButton("", "Ask the chief", "ml-auto h-7"))),
		P(g.Attr(fleetSinceAttr, ""), g.If(unavailable, g.Attr("data-fleet-unavailable", "")), g.If(unavailable, g.Attr("title", v.Unavailable)),
			Class(sinceCls), g.Text(since)),
			// A card action's note ("Sent.", "2 sent, 1 skipped (moved on)"):
			// drawn only when an action left one, over the groups.
			g.If(v.Note != "", P(g.Attr(fleetNoteAttr, ""), g.Attr("role", "status"),
				Class("rounded-md bg-s2 px-2 py-1 text-[11px] text-fg"), g.Text(v.Note))),
		// The desktop groups: each opens inline.
		Div(Class("hidden flex-col gap-0.5 sm:flex"), g.Map(groups, func(gr FleetGroupView) g.Node {
			return fleetGroupDetails(gr, countOf(gr), inert, gr.Key == v.OpenGroup)
		})),
		// The phone: one row of chips, each opening its sheet — or, with no
		// digest, the reason in that row's place.
		g.If(!unavailable, Div(Class("flex items-center gap-1 overflow-x-auto sm:hidden"), g.Attr("role", "list"),
			Span(Class("shrink-0 pr-1 text-[11px] font-semibold uppercase tracking-wide text-fg2"), g.Text(head)),
			g.Map(groups, func(gr FleetGroupView) g.Node { return fleetChip(gr, countOf(gr), inert) }))),
		g.If(unavailable, P(g.Attr("data-fleet-unavailable", ""), g.Attr("title", v.Unavailable),
			Class("flex min-h-[36px] items-center truncate text-[11px] text-st-warning-fg sm:hidden"), g.Text(v.Unavailable))),
		g.If(!inert, g.Map(groups, func(gr FleetGroupView) g.Node { return fleetSheet(gr, v, since) })),
	)
}

// fleetGroupDetails is one group on a desktop: a closed <details> whose
// summary is one fixed-height line.
func fleetGroupDetails(gr FleetGroupView, count string, inert, open bool) g.Node {
	summary := Summary(
		Class("flex min-h-[28px] cursor-pointer list-none items-center gap-2 rounded-md px-2 text-xs text-fg2 hover:bg-s2"),
		Span(Class("mr-auto truncate"), g.Text(gr.Label)),
		Span(g.Attr(fleetCountAttr, ""), Class("shrink-0 tabular-nums text-fg"), g.Text(count)),
		Span(Class("shrink-0 text-muted"), g.Attr("aria-hidden", "true"), g.Text("▸")),
	)
	if inert {
		return Div(g.Attr(fleetGroupAttr, gr.Key), summary)
	}
	return Details(g.Attr(fleetGroupAttr, gr.Key), Class("group"), g.If(open, g.Attr("open", "")),
		summary,
		Div(Class("max-h-96 overflow-y-auto pb-1 pl-2"), fleetGroupBody(gr)))
}

// fleetChip is one group's chip on a phone.
func fleetChip(gr FleetGroupView, count string, loading bool) g.Node {
	return Button(Type("button"), g.Attr(fleetChipAttr, gr.Key), g.Attr("role", "listitem"),
		g.If(!loading, g.Attr("aria-controls", "fleet-sheet-"+gr.Key)),
		g.If(loading, g.Attr("disabled", "")),
		Class("press inline-flex min-h-[36px] shrink-0 items-center gap-1 rounded-full bg-s1 px-3 text-xs text-fg2 ring-1 ring-inset ring-edge"),
		Span(g.Text(gr.Label)),
		Span(g.Attr(fleetCountAttr, ""), Class("tabular-nums font-semibold text-fg"), g.Text(count)))
}

// fleetSheet is one group's bottom sheet on a phone (hidden until its chip is
// tapped). 🔴 FIXED INSIDE A TRANSFORMED PANEL: the panel's transform makes
// it the containing block, so the sheet sits at the PANEL's bottom, which on
// a phone (the panel full width and height) is the screen's.
func fleetSheet(gr FleetGroupView, v FleetDigestView, since string) g.Node {
	return Div(ID("fleet-sheet-"+gr.Key), g.Attr(fleetSheetAttr, gr.Key), g.Attr("role", "dialog"), g.Attr("aria-label", gr.Label),
		g.If(gr.Key != v.OpenGroup, g.Attr("hidden", "")),
		Class("fixed inset-x-0 bottom-0 z-50 flex max-h-[85%] flex-col rounded-t-2xl bg-bg shadow-2xl ring-1 ring-line sm:hidden"),
		Div(Class("flex flex-none items-center gap-2 border-b border-line px-3 py-2"),
			H3(Class("mr-auto text-sm font-semibold text-fg"), g.Text(gr.Label+" ("+strconv.Itoa(gr.Count())+")")),
			Button(Type("button"), g.Attr(fleetSheetCloseAttr, ""), g.Attr("aria-label", "Close"),
				Class("press inline-flex h-11 w-11 items-center justify-center rounded-md text-muted hover:bg-s2"), g.Text("✕"))),
		Div(Class("min-h-0 flex-1 overflow-y-auto p-3"),
			P(Class("mb-2 text-[11px] text-muted"), g.Text(since)),
			fleetGroupBody(gr)),
		Div(Class("flex flex-none flex-col gap-2 border-t border-line p-3"),
			g.If(gr.Key == FleetGroupProposed && len(gr.Proposals) > 0, fleetBulk(gr.Proposals)),
			fleetAskChiefButton("", "Ask the chief about the fleet", "w-full min-h-[44px] justify-center"),
		),
	)
}

// fleetGroupBody is a group's open view: its note, its asks, its rows.
func fleetGroupBody(gr FleetGroupView) g.Node {
	if gr.Count() == 0 {
		return P(Class("px-1 py-1 text-[11px] text-muted"), g.Text("None."))
	}
	return g.Group{
		g.If(gr.Note != "", P(Class("px-1 pb-1 text-[11px] text-muted"), g.Text(gr.Note))),
		Ul(Class("flex flex-col gap-1"),
			g.Map(gr.Proposals, fleetProposalRow),
			g.Map(gr.Asks, fleetAskRow),
			g.Map(gr.Rows, fleetRow)),
		// The desktop's bulk actions sit under the group's rows; the phone's
		// in its sheet's footer (fleetSheet).
		g.If(gr.Key == FleetGroupProposed && len(gr.Proposals) > 0, Div(Class("hidden pt-1 sm:block"), fleetBulk(gr.Proposals))),
	}
}

// fleetButton is a card action's button: full width and 44px tall on a phone
// (tap only — the text must be seen; there is no swipe), compact on desktop.
func fleetButton(label, cls string, attrs ...g.Node) g.Node {
	nodes := []g.Node{Type("button"),
		Class("press inline-flex min-h-[44px] w-full items-center justify-center rounded-md px-3 text-xs font-medium ring-1 ring-inset " +
			"sm:min-h-[28px] sm:w-auto " + cls)}
	nodes = append(nodes, attrs...)
	return Button(append(nodes, g.Text(label))...)
}

// fleetPost is an htmx post that answers with the card, swapped into the
// digest's slot.
func fleetPost(path string) g.Node {
	return g.Group{hx("hx-post", path), hx("hx-target", "#"+fleetDigestSlotID), hx("hx-swap", "innerHTML")}
}

// fleetProposalRow is one proposal's card: the session, the class as a verb,
// the reason, EXACTLY what will be sent (in full, never cut), and Approve ·
// Edit · Reject · Open session.
func fleetProposalRow(p FleetProposalView) g.Node {
	id := strconv.FormatInt(p.ID, 10)
	return Li(g.Attr(fleetProposalAttr, id),
		Class("flex flex-col gap-1.5 rounded-lg bg-s1 p-2 text-xs ring-1 ring-inset ring-accent/40"),
		Div(Class("flex items-baseline gap-2"),
			Span(Class("min-w-0 flex-1 truncate font-medium text-fg"), g.Text(p.Session)),
			Span(g.Attr("data-fleet-verb", ""), Class("shrink-0 font-semibold text-accent"), g.Text(p.Verb)),
			g.If(p.Age != "", Span(Class("shrink-0 text-muted"), g.Text(p.Age)))),
		g.If(p.Where != "", P(Class("truncate text-[11px] text-muted"), g.Text(p.Where))),
		g.If(p.Reason != "", P(Class("break-words text-[11px] text-fg2"), g.Text("Why: "+p.Reason))),
		Div(Class("flex flex-col gap-0.5"),
			Span(Class("text-[10px] font-semibold uppercase tracking-wide text-muted"), g.Text("Will send")),
			Pre(g.Attr("data-fleet-text", ""), Class("whitespace-pre-wrap break-words rounded-md bg-bg p-2 font-mono text-[11px] text-fg ring-1 ring-inset ring-line"),
				g.Text(p.Text))),
		Div(Class("flex flex-col gap-1.5 sm:flex-row sm:flex-wrap sm:items-center"),
			fleetButton("Approve", "bg-accent/20 text-fg ring-accent/60 hover:bg-accent/30", g.Attr("data-fleet-approve", id),
				fleetPost(FleetActionPath(p.ID, "approve"))),
			g.If(p.Editable, fleetButton("Edit", "bg-s2 text-fg2 ring-edge hover:bg-s3", g.Attr("data-fleet-edit", id),
				g.Attr("aria-expanded", "false"), g.Attr("aria-controls", "fleet-edit-"+id))),
			fleetButton("Reject", "bg-s2 text-fg2 ring-edge hover:bg-s3", g.Attr("data-fleet-reject", id),
				fleetPost(FleetActionPath(p.ID, "reject"))),
			g.If(p.Href != "", A(Href(p.Href), g.Attr(fleetOpenAttr, id),
				Class("press inline-flex min-h-[44px] w-full items-center justify-center rounded-md px-3 text-xs font-medium text-accent ring-1 ring-inset ring-edge hover:underline sm:min-h-[28px] sm:w-auto"),
				g.Text("Open session")))),
		// Edit: the text in a box, sent as the owner's own message (no chief
		// line) — a fresh sign-in is asked for if the current one is stale.
		g.If(p.Editable, Form(ID("fleet-edit-"+id), g.Attr("data-fleet-edit-form", id), g.Attr("hidden", ""),
			fleetPost(FleetActionPath(p.ID, "edit")),
			Class("flex flex-col gap-1.5"),
			Label(For("fleet-edit-text-"+id), Class("text-[10px] font-semibold uppercase tracking-wide text-muted"),
				g.Text("Your message (sent as yours, with no chief line)")),
			Textarea(ID("fleet-edit-text-"+id), Name("text"), Rows("5"), MaxLength("4096"),
				Class("w-full rounded-md bg-bg p-2 font-mono text-[11px] text-fg ring-1 ring-inset ring-edge"), g.Text(p.EditText)),
			Button(Type("submit"), Class("press inline-flex min-h-[44px] w-full items-center justify-center rounded-md bg-accent/20 px-3 text-xs font-medium text-fg ring-1 ring-inset ring-accent/60 sm:min-h-[28px] sm:w-auto"),
				g.Text("Send as mine")))),
	)
}

// fleetBulk is the Proposed group's Approve all and Reject all: each asks to
// confirm, listing every row's verb and session, and posts the ids the owner
// was shown — a proposal filed after the card was drawn is not swept in.
func fleetBulk(ps []FleetProposalView) g.Node {
	var list []string
	ids := g.Group{}
	for _, p := range ps {
		list = append(list, "• "+p.Verb+" — "+p.Session)
		ids = append(ids, Input(Type("hidden"), Name("id"), Value(strconv.FormatInt(p.ID, 10))))
	}
	n := strconv.Itoa(len(ps))
	form := func(op, label, confirm, cls string) g.Node {
		return Form(g.Attr(fleetBulkAttr, op), fleetPost(FleetBulkPath), hx("hx-confirm", confirm+"\n"+strings.Join(list, "\n")),
			Class("w-full sm:w-auto"),
			Input(Type("hidden"), Name("op"), Value(op)), ids,
			Button(Type("submit"), Class("press inline-flex min-h-[44px] w-full items-center justify-center rounded-md px-3 text-xs font-medium ring-1 ring-inset sm:min-h-[28px] sm:w-auto "+cls),
				g.Text(label)))
	}
	return Div(Class("flex flex-col gap-1.5 sm:flex-row"),
		form("approve", "Approve all ("+n+")", "Send all "+n+"? Each is re-checked first; a session that moved on is skipped.", "bg-accent/20 text-fg ring-accent/60"),
		form("reject", "Reject all", "Reject all "+n+"?", "bg-s2 text-fg2 ring-edge"))
}

// fleetAskRow is one of the chief's open cards.
func fleetAskRow(a FleetAskView) g.Node {
	return Li(g.Attr(fleetAskAttr, strconv.FormatInt(a.ID, 10)),
		Class("flex min-h-[44px] items-center gap-2 rounded-lg bg-s1 px-2 text-xs ring-1 ring-inset ring-line sm:min-h-[32px]"),
		Span(Class("shrink-0"), g.Attr("aria-hidden", "true"), g.Text("❓")),
		Span(Class("min-w-0 flex-1 truncate text-fg"), g.Text("Chief asks: "+a.Question)),
		g.If(a.Age != "", Span(Class("shrink-0 text-muted"), g.Text(a.Age))),
		A(Href(a.Href), Class("shrink-0 font-medium text-accent hover:underline"), g.Text("Answer")))
}

// fleetRow is one session's row: its name opens the drill-in under it.
func fleetRow(r FleetRowView) g.Node {
	return Li(g.Attr(fleetRowAttr, r.ID), g.Attr("data-fleet-row-state", r.State),
		Class("rounded-lg bg-s1 ring-1 ring-inset ring-line"),
		Button(Type("button"), g.Attr(fleetDrillAttr, ""), g.Attr("aria-expanded", "false"),
			hx("hx-get", FleetDrillPath+"?id="+url.QueryEscape(r.ID)),
			hx("hx-target", "next ["+fleetDrillSlotAttr+"]"),
			hx("hx-swap", "innerHTML"),
			Class("press flex min-h-[44px] w-full items-center gap-2 px-2 text-left text-xs sm:min-h-[32px]"),
			Span(Class("min-w-0 max-w-[55%] shrink truncate font-medium text-fg"), g.Text(r.Name)),
			Span(Class("min-w-0 flex-1 truncate text-muted"), g.Text(r.Detail))),
		Div(g.Attr(fleetDrillSlotAttr, "")))
}

// fleetAskChiefButton opens the Fleet side chat with a draft about subject
// ("" for the fleet as a whole). 🔴 IT OPENS AND FILLS; IT NEVER SENDS: the
// answer is the panel body on the Fleet chat, carrying the draft
// (ChiefPrefillAttr), which the panel script puts in the box.
func fleetAskChiefButton(subject, label, cls string) g.Node {
	vals := `{}`
	if subject != "" {
		vals = `{"about":` + strconv.Quote(subject) + `}`
	}
	return Button(Type("button"), g.Attr(fleetAskChiefAttr, subject),
		hx("hx-post", FleetChatPath),
		hx("hx-vals", vals),
		hx("hx-target", "#"+chiefPanelBodyID),
		hx("hx-swap", "innerHTML"),
		Class("press inline-flex items-center gap-1 rounded-md bg-s2 px-2 text-[11px] font-medium text-fg2 ring-1 ring-inset ring-edge hover:bg-s3 "+cls),
		g.Text(label))
}

// FleetDrillView is one session's drill-in.
type FleetDrillView struct {
	ID    string
	Name  string
	Where string
	State string
	// Href is where Open session goes.
	Href string
	// Timeline is its stops in the last day, newest first.
	Timeline []FleetStopView
	PRs      []FleetPRView
	Tasks    []FleetTaskView
	// LastMessage is its newest message, redacted and cut as session_context
	// shows it; LastRole whose it is. LastNote says why there is none.
	LastMessage, LastRole, LastNote string
	// Failed, when set, replaces everything: the session is not the owner's,
	// or no longer listed.
	Failed string
}

// FleetStopView is one stop on the drill-in's timeline.
type FleetStopView struct {
	Opened, State, Ended string
}

// FleetPRView is one pull request on the drill-in.
type FleetPRView struct {
	Label, State, Head string
}

// FleetTaskView is one linked task on the drill-in.
type FleetTaskView struct {
	Title, Status, Role string
}

// RenderFleetDrill writes a drill-in.
func RenderFleetDrill(w io.Writer, v FleetDrillView) error { return fleetDrill(v).Render(w) }

func fleetDrill(v FleetDrillView) g.Node {
	if v.Failed != "" {
		return P(g.Attr("data-fleet-drill-failed", ""), Class("px-2 pb-2 text-[11px] text-st-warning-fg"), g.Text(v.Failed))
	}
	section := func(title string, body g.Node) g.Node {
		return Div(Class("flex flex-col gap-0.5"),
			H4(Class("text-[10px] font-semibold uppercase tracking-wide text-muted"), g.Text(title)), body)
	}
	line := func(s string) g.Node { return Li(Class("break-words text-[11px] text-fg2"), g.Text(s)) }
	var stops []g.Node
	for _, st := range v.Timeline {
		s := st.Opened + " · " + st.State
		if st.Ended != "" {
			s += " · " + st.Ended
		} else {
			s += " · open"
		}
		stops = append(stops, line(s))
	}
	var prs []g.Node
	for _, p := range v.PRs {
		s := p.Label + " · " + p.State
		if p.Head != "" {
			s += " · " + p.Head
		}
		prs = append(prs, line(s))
	}
	var tasks []g.Node
	for _, t := range v.Tasks {
		// A task's title is author text: the title rule (textplain.Title).
		tasks = append(tasks, line(textplain.Title(t.Title)+" · "+t.Status+" · "+t.Role))
	}
	none := func(nodes []g.Node, empty string) g.Node {
		if len(nodes) == 0 {
			return P(Class("text-[11px] text-muted"), g.Text(empty))
		}
		return Ul(Class("flex flex-col gap-0.5"), g.Group(nodes))
	}
	last := g.Node(P(Class("text-[11px] text-muted"), g.Text(v.LastNote)))
	if v.LastMessage != "" {
		last = P(g.Attr("data-fleet-last-message", ""), Class("line-clamp-4 whitespace-pre-wrap break-words text-[11px] text-fg2"),
			Span(Class("font-semibold text-muted"), g.Text(v.LastRole+": ")), g.Text(v.LastMessage))
	}
	return Div(g.Attr("data-fleet-drill-in", v.ID), Class("flex flex-col gap-2 border-t border-line px-2 py-2"),
		g.If(v.Where != "", P(Class("truncate text-[11px] text-muted"), g.Text(v.Where+" · "+v.State))),
		section("Stops, last 24 h", none(stops, "None.")),
		section("Pull requests", none(prs, "None recorded.")),
		section("Linked tasks", none(tasks, "None.")),
		section("Last message", last),
		Div(Class("flex flex-wrap items-center gap-2"),
			A(Href(v.Href), g.Attr(fleetOpenAttr, v.ID),
				Class("press inline-flex min-h-[44px] items-center rounded-md bg-accent/15 px-3 text-xs font-medium text-fg ring-1 ring-inset ring-accent/50 sm:min-h-[32px]"),
				g.Text("Open session")),
			fleetAskChiefButton(v.ID, "Ask the chief about this", "min-h-[44px] sm:min-h-[32px]")))
}

// FleetDrillDetail is a row's second half: where it is, its state and for how
// long, its latest pull request.
func FleetDrillDetail(where, state, since, pr string) string {
	parts := []string{}
	if s := strings.TrimSpace(strings.ReplaceAll(state, "_", " ")); s != "" {
		if since != "" {
			s += " " + since
		}
		parts = append(parts, s)
	}
	if pr != "" {
		parts = append(parts, pr)
	}
	if where != "" {
		parts = append(parts, where)
	}
	return strings.Join(parts, " · ")
}

// chiefPrefill is the draft a panel body carries for the panel script.
func chiefPrefill(text string) g.Node {
	if text == "" {
		return nil
	}
	return Div(g.Attr(ChiefPrefillAttr, text), g.Attr("hidden", ""))
}
