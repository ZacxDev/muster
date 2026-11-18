package api

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/ZacxDev/muster/internal/agentoutcome"
	"github.com/ZacxDev/muster/internal/agents"
)

// agent_report_done: A STARTED AGENT SAYS IT FINISHED (agent_outcome.go).
//
// 🔴 DONE IS AN EXPLICIT SIGNAL, NEVER AN INFERENCE. A turn that ends is as often
// a question ("Which repository should I work in?") as a result, and reading it
// as done stopped watching an agent that was waiting on its owner. So an agent's
// work is done only when it says so: by moving its task to ready_for_review or
// complete (setNoteStatus), or — for work with no task — by calling this. Every
// other idle end is `waiting` on the owner.

const agentReportDoneTool = "agent_report_done"

func agentReportDoneToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: agentReportDoneTool,
		Description: "Say you have FINISHED the work you were started on, with a one-line summary of the result (a pull " +
			"request URL, what changed). Call it once, when you are done — not to ask a question (ask with attention_ask, " +
			"or end your turn with the question) and not for progress. On a task, setting the task ready_for_review says " +
			"the same. Until you call one of them, muster tells your owner you are waiting on them, not done.",
		Parameters: objSchema(map[string]any{
			"summary": strProp("One line: what you finished, e.g. the draft pull request's URL."),
		}, "summary")}
}

// toolAgentReportDone records a's work as done (one push "<agent> finished:
// <summary>", a note to the chief that started it). It answers whether that
// changed anything: an agent nobody is tracking, or one already done, is told so.
func (s *Server) toolAgentReportDone(ctx context.Context, a agents.Agent, arguments string) string {
	var in struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil || strings.TrimSpace(in.Summary) == "" {
		return toolErr("arguments are {summary}: one line saying what you finished")
	}
	if !s.outcomesOn() {
		return toolErr("this deployment records no outcomes")
	}
	octx, cancel := outcomeCtx(ctx)
	defer cancel()
	fresh, err := s.ext.Agents.Get(octx, a.ID)
	if err != nil {
		return toolErr("your record could not be read; try again")
	}
	switch fresh.Outcome {
	case agentoutcome.Running, agentoutcome.Waiting, agentoutcome.Stuck:
	case agentoutcome.Done:
		return toolJSON(map[string]any{"ok": true, "outcome": agentoutcome.Done, "next": "Already recorded as done."})
	default:
		return toolJSON(map[string]any{"ok": false, "next": "You were not started as a dispatched agent, so there is nothing to " +
			"report done; tell your owner in your reply."})
	}
	summary := agentoutcome.FirstLine(agents.ScrubNote(in.Summary, fresh.PendingNote), outcomePushTitleMax)
	s.outcomeDone(octx, fresh, "finished", summary, true)
	return toolJSON(map[string]any{"ok": true, "outcome": agentoutcome.Done, "next": "Recorded: your owner is told you finished."})
}
