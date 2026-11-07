package ui

import (
	"github.com/ZacxDev/muster/internal/sessionname"

	"encoding/json"
	"io"
	"net/url"
	"strconv"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE DISPATCHED-AGENT CARD — agent_dispatch's result in the chief's chat.
//
// When the chief starts an agent, its chat draws the start as a card the owner can
// TAP to open that agent: its name, where it runs (Claude Code, or the gateway and
// its model), its repository, the task it works (a link), and its state — starting,
// working, waiting, done or error — which follows the agent while the chat is open.
// A refused start draws as one compact line saying why, never as a broken card.
//
// 🔴 ONE RENDERER FOR EVERY PATH. The stored transcript (chatLogInner, from
// AgentDetailView.DispatchCards), the live stream (agentChatScript fetches
// DispatchCardPath for the result it just streamed) and the card's own refresh
// all draw DispatchAgentCard, so the three cannot disagree about what a start
// looks like.
//
// 🔴 ITS STATE FOLLOWS THE APP'S EXISTING EVENTS; IT ADDS NO POLL. The card
// re-fetches itself on the server's agent.changed and chat.reply events for ITS
// agent — the events the sidebar's agent list refreshes on — and #chat-log's own
// refetch redraws it too.

// AgentDispatchTool is the chief's tool that starts an agent.
const AgentDispatchTool = "agent_dispatch"

// DispatchCardPath serves one card (GET ?agent=<id>, or ?refused=<sentence>).
const DispatchCardPath = "/ui/dispatch-card"

// The states a started agent's card shows (data-dispatch-state).
const (
	DispatchStarting = "starting"
	DispatchWorking  = "working"
	DispatchWaiting  = "waiting"
	DispatchDone     = "done"
	DispatchError    = "error"
	// DispatchFailed and DispatchStuck are a RECORDED outcome
	// (internal/agentoutcome, the agent page's own "failed" / "stuck"): a turn of
	// the agent failed, or the silence watchdog saw nothing of it. The detail
	// says why.
	DispatchFailed = "failed"
	DispatchStuck  = "stuck"
)

// DispatchCard is one agent_dispatch outcome as the card draws it, composed in
// internal/api (dispatchCardFor). Refused set: the start was refused, and it is
// the sentence why; nothing else is read.
type DispatchCard struct {
	Refused string

	AgentID int64
	Name    string
	// SessionName is the agent's stable name (internal/sessionname), the
	// card's title; the zero value draws the slug. (No DisplayName: the name
	// rule is the card's only naming input.)
	SessionName sessionname.Name
	// Href opens the agent's page (AgentCardView.PageHref).
	Href string
	// Runtime is where it runs, in the owner's words: "Claude Code" or
	// "Gateway · <model>".
	Runtime string
	Repo    string
	TaskID  int64
	// State is one of the Dispatch* states; Detail says more when there is more
	// to say (an error's reason).
	State  string
	Detail string
	// Version orders this card against every other render of the same agent's
	// card (data-dispatch-version): the server's clock, in microseconds, taken
	// BEFORE it read the agent's state — so a higher version always shows state
	// read later. 0 draws no version (and the guard leaves the card alone).
	Version int64
	// Gone: the agent no longer exists (destroyed). The card says so and links
	// nowhere.
	Gone bool // Unreadable: the agent could not be read just now (a failed read, not a
	// missing row). The card says so, links nowhere, and stays refreshable.
	Unreadable bool
}

// DispatchResult is what the card needs from an agent_dispatch result: the
// agent it started, or the refusal's sentence. ok is false for a result that
// is neither (a replay still starting, an already-dispatched answer with no
// agent): those draw as the plain tool row.
type DispatchResult struct {
	AgentID int64
	Refused string
}

// ReadDispatchResult reads an agent_dispatch tool result. THE one reading: the
// api composes cards from it, and the page asks for a card with what it found.
func ReadDispatchResult(output string) (DispatchResult, bool) {
	var r struct {
		Error   string `json:"error"`
		AgentID int64  `json:"agent_id"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(output)), &r) != nil {
		return DispatchResult{}, false
	}
	switch {
	case r.AgentID > 0:
		// An agent that exists is a card even when a ledger write failed after it
		// started (the result then carries both): the owner must be able to reach it.
		return DispatchResult{AgentID: r.AgentID}, true
	case strings.TrimSpace(r.Error) != "":
		return DispatchResult{Refused: strings.TrimSpace(r.Error)}, true
	}
	return DispatchResult{}, false
}

// DispatchCardSrc is the card's own URL: for a started agent, by its id.
func DispatchCardSrc(agentID int64) string {
	return DispatchCardPath + "?agent=" + strconv.FormatInt(agentID, 10)
}

// DispatchCardRefusedSrc is a refusal's card URL (the live stream asks for it).
func DispatchCardRefusedSrc(sentence string) string {
	return DispatchCardPath + "?refused=" + url.QueryEscape(sentence)
}

// dispatchCardTrigger is a started agent's card refresh: the server's events for
// THAT agent (agent.changed: its status, its first turn starting or ending;
// chat.reply: a turn of its stored).
func dispatchCardTrigger(name string) string {
	q, _ := json.Marshal(name) // a string always marshals
	return "sse:agent.changed[JSON.parse(detail.data).id===" + string(q) + "] from:body, " +
		"sse:chat.reply[JSON.parse(detail.data).id===" + string(q) + "] from:body"
}

// dispatchStateLabel is a state in words.
func dispatchStateLabel(state string) string {
	switch state {
	case DispatchStarting:
		return "Starting"
	case DispatchWorking:
		return "Working"
	case DispatchWaiting:
		return "Waiting for you"
	case DispatchDone:
		return "Done"
	case DispatchError:
		return "Error"
	case DispatchFailed:
		return "Failed"
	case DispatchStuck:
		return "Stuck"
	}
	return state
}

// dispatchStateTone is a state's pill colours.
func dispatchStateTone(state string) string {
	switch state {
	case DispatchStarting:
		return "bg-st-open-bg text-st-open-fg"
	case DispatchWorking:
		return "bg-st-progress-bg text-st-progress-fg"
	case DispatchWaiting, DispatchStuck:
		return "bg-st-warning-bg text-st-warning-fg"
	case DispatchDone:
		return "bg-st-complete-bg text-st-complete-fg"
	}
	return "bg-st-error-bg text-st-error-fg"
}

// DispatchAgentCard draws one card: a refusal's compact line, or a started
// agent's card.
func DispatchAgentCard(c DispatchCard) g.Node {
	if c.Refused != "" {
		return Div(
			g.Attr("data-dispatch-card", ""),
			g.Attr("data-dispatch-state", "refused"),
			g.Attr("role", "note"),
			Class("flex w-full max-w-[92%] min-w-0 items-start gap-2 self-start rounded-xl bg-st-error-bg px-3 py-2 text-xs text-st-error-fg ring-1 ring-inset ring-st-error-fg/40"),
			Span(Class("shrink-0"), g.Attr("aria-hidden", "true"), g.Text("✗")),
			Span(Class("min-w-0 break-words"),
				Span(Class("font-semibold"), g.Text("No agent was started: ")),
				Span(g.Attr("data-dispatch-refused", ""), g.Text(c.Refused))),
		)
	}
	// The display name is a title (derived from a task's text): plained once.
	// The agent's stable name (internal/sessionname), as every surface
	// names it.
	title := sessionTitle(c.SessionName, "", c.Name)
	if c.Unreadable {
		card := Div(
			g.Attr("data-dispatch-card", ""),
			g.Attr("data-dispatch-state", "unreadable"),
			g.Attr("data-dispatch-agent", strconv.FormatInt(c.AgentID, 10)),
			g.Attr("role", "status"),
			Class("flex w-full max-w-[92%] min-w-0 items-center gap-2 self-start rounded-xl bg-bg/50 px-3 py-2 text-xs text-muted ring-1 ring-inset ring-line"),
			Span(Class("shrink-0"), g.Attr("aria-hidden", "true"), g.Text("🤖")),
			Span(Class("min-w-0 break-words"), g.Text(c.Name+": could not be read just now; this card updates when it can.")),
		)
		// Refreshed on the app's own events (any agent's: its name is unknown
		// until it reads), never on a timer.
		return liveCard(c.AgentID, "sse:agent.changed from:body, sse:chat.reply from:body", card)
	}
	state := c.State
	if c.Gone {
		state = DispatchDone
	}
	pill := Span(g.Attr("data-dispatch-status", state), Class("ml-auto shrink-0 rounded-full px-2 py-0.5 text-[11px] font-semibold "+dispatchStateTone(state)),
		g.Text(map[bool]string{true: "Removed", false: dispatchStateLabel(state)}[c.Gone]))
	lines := []g.Node{}
	if c.Runtime != "" {
		lines = append(lines, Span(g.Attr("data-dispatch-runtime", ""), Class("min-w-0 truncate"), g.Text(c.Runtime)))
	}
	if c.Repo != "" {
		lines = append(lines, Span(g.Attr("data-dispatch-repo", ""), Class("min-w-0 truncate font-mono"), g.Text(c.Repo)))
	}
	head := Div(Class("flex min-w-0 items-center gap-2"),
		Span(Class("shrink-0"), g.Attr("aria-hidden", "true"), g.Text("🤖")),
		Span(g.Attr("data-dispatch-name", ""), Class("min-w-0 truncate text-sm font-semibold text-fg"), g.Text(title)),
		pill)
	meta := Div(Class("flex min-w-0 flex-wrap items-center gap-x-3 gap-y-0.5 text-xs text-muted"), g.Group(lines))
	detailTone := "text-st-error-fg"
	if state == DispatchStuck || state == DispatchWaiting {
		detailTone = "text-st-warning-fg"
	}
	detail := g.If(c.Detail != "", P(g.Attr("data-dispatch-detail", ""), Class("break-words text-xs "+detailTone), g.Text(c.Detail)))

	// The card links to the agent; its task link sits BESIDE that link, never
	// inside it (an <a> in an <a> is invalid and the browser splits it).
	var main g.Node
	label := "Open agent " + title + " (" + map[bool]string{true: "removed", false: dispatchStateLabel(state)}[c.Gone] + ")"
	if c.Gone || c.Href == "" {
		main = Div(Class("flex min-w-0 flex-col gap-1 px-3 py-2"), head, meta, detail)
	} else {
		main = A(Href(c.Href), hx("hx-boost", "false"),
			g.Attr("data-dispatch-open", ""),
			g.Attr("aria-label", label),
			Class("press flex min-h-[44px] min-w-0 flex-col gap-1 rounded-xl px-3 py-2 transition hover:bg-s2"+focusRing),
			head, meta, detail)
	}
	var task g.Node
	if c.TaskID > 0 {
		id := strconv.FormatInt(c.TaskID, 10)
		task = A(Href("/tasks/"+id), hx("hx-boost", "false"), g.Attr("data-dispatch-task", id),
			Class("flex min-h-[44px] items-center border-t border-line px-3 text-xs font-medium text-accent underline"+focusRing),
			g.Text("Task #"+id))
	}
	attrs := []g.Node{
		g.Attr("data-dispatch-card", ""),
		g.Attr("data-dispatch-state", state),
		g.Attr("data-dispatch-agent", strconv.FormatInt(c.AgentID, 10)),
		g.If(c.Version > 0, g.Attr(DispatchVersionAttr, strconv.FormatInt(c.Version, 10))),
		Class("flex w-full max-w-[92%] min-w-0 flex-col self-start rounded-xl bg-bg/50 ring-1 ring-inset ring-line"),
	}
	card := Div(append(attrs, main, task)...)
	if c.Gone || c.Name == "" || c.AgentID <= 0 {
		return card
	}
	return liveCard(c.AgentID, dispatchCardTrigger(c.Name), card)
}

// DispatchLiveAttr marks a card's refreshing wrapper (liveCard).
const DispatchLiveAttr = "data-dispatch-live"

// liveCard wraps a card in the element that REFRESHES it: on trigger it asks
// for the card again and swaps the answer's card into itself (hx-select the
// card, innerHTML).
//
// 🔴 THE REFRESHING ELEMENT NEVER REPLACES ITSELF, AND THAT IS THE FIX FOR A
// CARD STUCK ON A STALE STATE. The card used to refresh itself with outerHTML.
// Two of its agent's events in one moment (the status promoter's, then the
// first turn's stamp) put the second request in htmx's queue behind the first
// — and the first's outerHTML swap REMOVED the element that queued it, so the
// newer state was never drawn: CI caught a card on "Starting" while its agent
// worked. The wrapper stays in the document, so a queued refresh runs from it
// and the LAST event's answer is the one shown (queue last). The browser case
// "a dispatched agent" plays that race deterministically.
//
// display: contents, so the card still lays out in #chat-log's column as if
// the wrapper were not there.
func liveCard(agentID int64, trigger string, card g.Node) g.Node {
	return Div(
		g.Attr(DispatchLiveAttr, strconv.FormatInt(agentID, 10)),
		Class("contents"),
		hx("hx-get", DispatchCardSrc(agentID)),
		hx("hx-trigger", trigger),
		hx("hx-select", "[data-dispatch-card]"),
		hx("hx-swap", "innerHTML"),
		hx("hx-target", "this"),
		hx("hx-sync", "this:queue last"),
		// The version guard, on the wrapper itself (dispatchCardGuardJS).
		hx("hx-on::before-swap", "window.musterDispatchCardGuard&&window.musterDispatchCardGuard(event)"),
		card,
	)
}

// RenderDispatchCard writes one card (DispatchCardPath's answer).
func RenderDispatchCard(w io.Writer, c DispatchCard) error { return DispatchAgentCard(c).Render(w) }

// DispatchVersionAttr is a card's version (DispatchCard.Version).
const DispatchVersionAttr = "data-dispatch-version"

// dispatchCardGuardJS is THE ordering rule for a live card: a card response is
// swapped in only when it is not OLDER than the newest render of the same
// agent's card this page has seen — the cards in the DOM (a #chat-log re-render
// draws them fresh) and every card response already swapped.
//
// 🔴 ORDER OF ARRIVAL IS NOT ORDER OF STATE, AND THIS IS WHY A QUEUE COULD NOT
// FIX IT. liveCard queues its own refreshes, but a card is also re-drawn by
// anything that re-renders the log — the chief's own reply, a note muster writes
// in its chat — and by requests from outside its queue, so a response rendered
// earlier can land after one rendered later. Each render carries the server's
// clock taken before it read the state (DispatchCard.Version), so "older" is a
// fact about the state, whatever re-rendered what and in which order. Pinned
// end to end by chief-ux.spec ("A STALE RESPONSE THAT ARRIVES AFTER A NEWER
// RENDER"): red without this guard, 3 of 3.
//
// It runs on each wrapper (liveCard's hx-on::before-swap) rather than as one
// delegated listener so that a swap into a wrapper a re-render has since
// detached is still recorded in seen: a detached node's events do not reach
// document. (The live-card flake once blamed on ordering was a different gap —
// a re-rendered card not yet listening — closed by #chat-log's settle:0ms.)
//
// Installed once per page (the flag): every page that draws cards includes it.
const dispatchCardGuardJS = `(function () {
  if (window.musterDispatchCardGuard) return;
  var seen = {};
  function ver(el) { var v = el && el.getAttribute && el.getAttribute('` + DispatchVersionAttr + `'); return v ? Number(v) : 0; }
  function newest(agent) {
    var m = seen[agent] || 0;
    var cards = document.querySelectorAll('[data-dispatch-card][data-dispatch-agent="' + agent + '"]');
    for (var i = 0; i < cards.length; i++) { var v = ver(cards[i]); if (v > m) m = v; }
    return m;
  }
  window.musterDispatchCardGuard = function (e) {
    var t = e.detail && e.detail.target;
    if (!t || !t.hasAttribute || !t.hasAttribute('` + DispatchLiveAttr + `')) return;
    var tpl = document.createElement('template');
    tpl.innerHTML = e.detail.serverResponse || '';
    var card = tpl.content.querySelector('[data-dispatch-card]');
    if (!card) return;
    var agent = card.getAttribute('data-dispatch-agent'), v = ver(card);
    if (!agent || !v) return;
    if (v < newest(agent)) { e.detail.shouldSwap = false; return; }
    seen[agent] = Math.max(seen[agent] || 0, v);
  };
})();`

// DispatchCardGuard is the guard's script element (dispatchCardGuardJS).
func DispatchCardGuard() g.Node { return Script(g.Raw(dispatchCardGuardJS)) }
