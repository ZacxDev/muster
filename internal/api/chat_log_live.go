package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/notes"
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
func (s *Server) dispatchCardFor(ctx context.Context, id int64, idx map[string]*provision.Instance) ui.DispatchCard {
	a, err := s.ext.Agents.Get(ctx, id)
	if err != nil {
		return ui.DispatchCard{AgentID: id, Gone: true, Name: "Agent #" + strconv.FormatInt(id, 10)}
	}
	c := ui.DispatchCard{
		AgentID: a.ID, Name: a.Name, DisplayName: a.DisplayName, Repo: a.Repo,
		Href:    ui.AgentCardView{Name: a.Name, SessionID: s.latestSessionIDs(ctx, []int64{a.ID})[a.ID]}.PageHref(),
		Runtime: dispatchRuntimeLabel(a.Kind, a.Model),
	}
	if a.NoteID != nil {
		c.TaskID = *a.NoteID
	}
	c.State, c.Detail = s.dispatchState(ctx, a, liveStatusIndexed(a, idx))
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

// dispatchState is a started agent's state on its card, with the detail an
// error carries. 🔴 THE ONE RULE (dispatch cards read nothing else):
//
//   - error: the agent's status is error, or its first turn failed;
//   - done: it is stopped, or its task is ready for review or complete;
//   - starting: it is pending or provisioning, or its first turn is still owed;
//   - working: a turn of its is running — a chat turn here (turnGate) or its
//     first turn (the kickoff claim is held);
//   - waiting: it runs and no turn is running: its turn ended, and it waits for
//     its owner.
func (s *Server) dispatchState(ctx context.Context, a agents.Agent, status string) (string, string) {
	switch {
	case status == agents.StatusError:
		return ui.DispatchError, firstNonEmptyLine(a.KickoffError)
	case agents.KickoffFailed(a):
		return ui.DispatchError, "Its first turn failed."
	case status == agents.StatusStopped:
		return ui.DispatchDone, ""
	}
	if a.NoteID != nil && s.ext.Notes != nil {
		if n, err := s.ext.Notes.Get(ctx, *a.NoteID); err == nil && (n.Status == notes.StatusReadyForReview || n.Status == notes.StatusComplete) {
			return ui.DispatchDone, ""
		}
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
