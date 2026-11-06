package ui

import (
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
)

// DispatchCard is one agent_dispatch outcome as the card draws it, composed in
// internal/api (dispatchCardFor). Refused set: the start was refused, and it is
// the sentence why; nothing else is read.
type DispatchCard struct {
	Refused string

	AgentID     int64
	Name        string
	DisplayName string
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
	// Gone: the agent no longer exists (destroyed). The card says so and links
	// nowhere.
	Gone bool
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
	case DispatchWaiting:
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
	title := c.DisplayName
	if title == "" {
		title = c.Name
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
	detail := g.If(c.Detail != "", P(g.Attr("data-dispatch-detail", ""), Class("break-words text-xs text-st-error-fg"), g.Text(c.Detail)))

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
		Class("flex w-full max-w-[92%] min-w-0 flex-col self-start rounded-xl bg-bg/50 ring-1 ring-inset ring-line"),
	}
	if !c.Gone && c.Name != "" && c.AgentID > 0 {
		// Its own refresh (see the file's note). outerHTML: the card replaces
		// itself, so the next refresh is the new card's.
		attrs = append(attrs,
			hx("hx-get", DispatchCardSrc(c.AgentID)),
			hx("hx-trigger", dispatchCardTrigger(c.Name)),
			hx("hx-swap", "outerHTML"),
			hx("hx-target", "this"))
	}
	return Div(append(attrs, main, task)...)
}

// RenderDispatchCard writes one card (DispatchCardPath's answer).
func RenderDispatchCard(w io.Writer, c DispatchCard) error { return DispatchAgentCard(c).Render(w) }
