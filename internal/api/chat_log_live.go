package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agentoutcome"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE LIVE HALF OF A DRAWN CONVERSATION: what #chat-log shows besides the stored
// rows — whether a turn is running in it now, and the state of every agent the
// chief started in it. 🔴 ONE COMPOSER FOR THE THREE RENDERERS of #chat-log (the
// agent page, the chief panel, the chat-log refetch): a second copy is how a
// reload would draw "working…" while the refetch did not.

// chatLinesOf is the stored rows as the transcript draws them — the one
// conversion, for the same three renderers.
func chatLinesOf(msgs []agents.ChatMessage) []ui.ChatLine {
	lines := make([]ui.ChatLine, 0, len(msgs))
	for _, m := range msgs {
		lines = append(lines, ui.ChatLine{Role: m.Role, Content: m.Content, Kind: m.Kind, ToolID: m.ToolID, ToolName: m.ToolName,
			ToolOK: m.ToolOK, ID: m.ID, Typed: m.TypedBy > 0})
	}
	return lines
}

// withChatLogLive fills v's live fields for session: TurnInFlight from the
// session's turn count, and DispatchCards for the agent_dispatch calls in
// v.Messages.
func (s *Server) withChatLogLive(ctx context.Context, v *ui.AgentDetailView, session agents.ChatSession) {
	v.TurnInFlight = s.sessionTurns.inFlight(session.SessionKey)
	v.DispatchCards = s.dispatchCardsFor(ctx, v.Messages)
}

// dispatchCardsFor composes a card for every agent_dispatch call in lines that
// has a result naming an agent or a refusal, by the call's tool id; nil when
// there is none. The instance list is read once, and only when a card needs it.
func (s *Server) dispatchCardsFor(ctx context.Context, lines []ui.ChatLine) map[string]ui.DispatchCard {
	var out map[string]ui.DispatchCard
	var idx map[string]*provision.Instance
	read := false
	for _, l := range lines {
		if l.Kind != "tool_result" || !isAgentDispatchCall(l.ToolName) || l.ToolID == "" {
			continue
		}
		r, ok := ui.ReadDispatchResult(l.Content)
		if !ok {
			continue
		}
		if out == nil {
			out = map[string]ui.DispatchCard{}
		}
		if r.Refused != "" {
			out[l.ToolID] = ui.DispatchCard{Refused: r.Refused}
			continue
		}
		if !read {
			idx, read = s.instanceIndex(ctx), true
		}
		out[l.ToolID] = s.dispatchCardFor(ctx, r.AgentID, idx)
	}
	return out
}

// isAgentDispatchCall reports whether a stored call is agent_dispatch, as a
// gateway chief names it or a claude-code chief does over MCP.
func isAgentDispatchCall(name string) bool {
	return name == agentDispatchTool || name == "mcp__muster__"+agentDispatchTool
}

// dispatchCardFor is agent id's card. The read is the caller's principal's: an
// agent that is gone — or is not the viewer's — draws as removed, and names
// nothing of it.
func (s *Server) dispatchCardFor(ctx context.Context, id int64, idx map[string]*provision.Instance) (c ui.DispatchCard) {
	// 🔴 THE VERSION IS TAKEN BEFORE ANY STATE IS READ (ui.dispatchCardGuardJS):
	// a render that read later always carries a higher version. Wall clock, not
	// s.now(): it orders real renders, and a test's frozen clock would tie them.
	// Stamped on every card this returns, whichever branch returns it.
	version := time.Now().UnixMicro()
	defer func() { c.Version = version }()
	a, err := s.ext.Agents.Get(ctx, id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Gone (destroyed), or not the viewer's: removed, naming nothing of it.
		return ui.DispatchCard{AgentID: id, Gone: true, Name: "Agent #" + strconv.FormatInt(id, 10)}
	case err != nil:
		// A read that FAILED is not a removed agent: the card says so and stays
		// refreshable, so the next event draws the agent again.
		s.logger.Printf("agents: dispatch card for agent %d: %v", id, err)
		return ui.DispatchCard{AgentID: id, Unreadable: true, Name: "Agent #" + strconv.FormatInt(id, 10)}
	}
	c = ui.DispatchCard{
		AgentID: a.ID, Name: a.Name, DisplayName: a.DisplayName, Repo: a.Repo,
		Href:    ui.AgentCardView{Name: a.Name, SessionID: s.latestSessionIDs(ctx, []int64{a.ID})[a.ID]}.PageHref(),
		Runtime: dispatchRuntimeLabel(a.Kind, a.Model),
	}
	if a.NoteID != nil {
		c.TaskID = *a.NoteID
	}
	c.State, c.Detail = s.dispatchCardState(ctx, a, liveStatusIndexed(a, idx))
	return c
}

// dispatchRuntimeLabel is where an agent runs, in the owner's words — the words
// agent_dispatch's refusal offers its targets in (dispatchTargetLabel).
func dispatchRuntimeLabel(kind, model string) string {
	if agents.ResolveKind(kind) == agents.KindClaudeCode {
		return "Claude Code"
	}
	if model == "" {
		return "Gateway · default model"
	}
	return "Gateway · " + model
}

// dispatchCardState is a started agent's state on its card, with the detail an
// error carries — THE ONE FUNCTION a card's state comes from (dispatchCardFor is
// its only caller).
//
// 🔴 IT IS NOT AN END-STATE RULE OF ITS OWN. A recorded outcome, when the agent
// has one (outcomeCardState), is read FIRST and wins: the dispatched-agent
// outcome work (feat/dispatch-outcomes: agents.Agent.Outcome, with failed and
// stuck) extends THIS function by filling outcomeCardState in, never by adding a
// second rule beside it. Only an agent with no recorded outcome is derived
// (derivedCardState).
func (s *Server) dispatchCardState(ctx context.Context, a agents.Agent, status string) (string, string) {
	if state, detail, ok := s.outcomeCardState(a); ok {
		return state, detail
	}
	return s.derivedCardState(ctx, a, status)
}

// outcomeCardState is the card state of agent a's RECORDED outcome; ok false
// when it has none. It reads the outcome API's own projection (AgentOutcomeOf,
// agent_outcome.go) — what the agent page's badge and notice draw from the same
// row — so the card and the page cannot disagree:
//
//	failed  → failed, its reason      stuck → stuck, its reason
//	waiting → waiting for you, the reply's last line
//	done    → done                    running / untracked → derived below
//
// A running agent is left to derivedCardState on purpose: "working" and
// "waiting for you" are finer than "running", and nothing is recorded to say
// which.
func (s *Server) outcomeCardState(a agents.Agent) (state, detail string, ok bool) {
	o := s.AgentOutcomeOf(a, time.Time{})
	switch o.Outcome {
	case agentoutcome.Failed:
		return ui.DispatchFailed, o.Reason, true
	case agentoutcome.Stuck:
		return ui.DispatchStuck, o.Reason, true
	case agentoutcome.Waiting:
		return ui.DispatchWaiting, o.Reason, true
	case agentoutcome.Done:
		return ui.DispatchDone, "", true
	}
	return "", "", false
}

// derivedCardState derives a card state from what the agent's row and this
// process can see, for an agent with no recorded END (outcomeCardState answered
// nothing):
//
//   - error: the agent's status is error;
//   - done: it is stopped;
//   - starting: it is pending or provisioning, or its first turn is still owed;
//   - working: a turn of its is running — a chat turn here (turnGate) or its
//     first turn (the kickoff claim is held);
//   - waiting: it runs and no turn is running: its turn ended, and it waits for
//     its owner.
//
// 🔴 A FAILED FIRST TURN AND A TASK THE AGENT MOVED TO REVIEW ARE NOT DERIVED
// HERE. They were (#496), as rules of this function's own; they are now the
// recorded outcome's (failed, done — internal/api agent_outcome.go), which the
// agent page reads too, so there is ONE rule and the card cannot say "done"
// while the page says "running". TestTheCardAndThePageAgreeOnEveryOutcome pins it.
func (s *Server) derivedCardState(ctx context.Context, a agents.Agent, status string) (string, string) {
	switch {
	case status == agents.StatusError:
		return ui.DispatchError, firstNonEmptyLine(a.KickoffError)
	case status == agents.StatusStopped:
		return ui.DispatchDone, ""
	}
	switch {
	case status == agents.StatusPending || status == agents.StatusProvisioning || agents.KickoffOwed(a):
		return ui.DispatchStarting, ""
	case s.turns.busy(a.ID):
		return ui.DispatchWorking, ""
	}
	if holder, err := s.ext.Agents.KickoffClaimHolder(ctx, a.ID); err == nil && holder != "" {
		return ui.DispatchWorking, ""
	}
	return ui.DispatchWaiting, ""
}

// handleDispatchCard serves GET /ui/dispatch-card: one card, for ?agent=<id>
// (a started agent: the card's own refresh, and the live stream's) or
// ?refused=<sentence> (the live stream's refusal). See ui/dispatch_card.go.
func (s *Server) handleDispatchCard(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var c ui.DispatchCard
	if refused := strings.TrimSpace(q.Get("refused")); refused != "" {
		const max = 2000
		if len(refused) > max {
			refused = refused[:max]
		}
		c = ui.DispatchCard{Refused: refused}
	} else {
		id, err := strconv.ParseInt(q.Get("agent"), 10, 64)
		if err != nil || id <= 0 || s.ext.Agents == nil {
			http.Error(w, "agent is required", http.StatusBadRequest)
			return
		}
		c = s.dispatchCardFor(r.Context(), id, s.instanceIndex(r.Context()))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := ui.RenderDispatchCard(w, c); err != nil {
		s.logger.Printf("agents: render dispatch card: %v", err)
	}
}
