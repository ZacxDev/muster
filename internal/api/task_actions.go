package api

import (
	"context"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
)

// ---------------------------------------------------------------------------
// WHAT THE CHIEF CAN DO WITH A TASK, NOW.
//
// When the chief puts a task in front of its owner (task_route_attention), the
// ask offers the owner — as its buttons — exactly the actions the chief can take
// on that task at that moment, so the owner never answers with an action the
// chief cannot carry out (an owner once answered "dispatch it to an agent" to a
// chief that had no dispatch tool).
//
// 🔴 ONE PREDICATE PER ACTION, AND THE TOOL ITSELF CALLS IT. The offer is not a
// second reading of the gates: task_forward finds its target with forwardTarget
// and task_dispatch checks the owner with chiefMayDispatch — the same functions
// chiefTaskActions calls — so the offer and the gate cannot disagree.
//
//   - task_forward: a running agent other than the chief works the task
//     (forwardTarget; the tool refuses otherwise).
//   - task_dispatch: task_dispatch is on and the owner opted in
//     (chiefMayDispatch: MUSTER_CHIEF_DISPATCH_OWNERS).
//   - route_to_session is NEVER offered for a task: it routes a plugin message
//     to a session for a REPLY, and a task event is no_reply, so the tool refuses
//     it (RefusedNoReply). Offering it would be exactly the action the chief
//     cannot take.
//
// ⚠ "NOW" IS WHEN THE ASK IS FILED. The buttons are the ask's options, stored
// with it; an agent that starts or stops working the task afterwards does not
// change them. The tool still re-checks at the call.
// ---------------------------------------------------------------------------

// chiefTaskAction is one action offered on a task's ask: the tool, and the
// button text the owner taps (which is also the answer the chief receives).
type chiefTaskAction struct {
	Tool  string
	Label string
}

// forwardTarget is the agent task_forward would pass task taskID's change to:
// the newest running agent on it that is not the chief itself, or nil.
func (s *Server) forwardTarget(ctx context.Context, chief agents.Agent, taskID int64) (*agents.Agent, error) {
	on, err := s.agentsOnTask(ctx, agentOwnerID(chief), taskID)
	if err != nil {
		return nil, err
	}
	for i := range on { // newest first
		if on[i].ID != chief.ID && agentCanBeSent(on[i].Status) {
			return &on[i], nil
		}
	}
	return nil, nil
}

// chiefMayDispatch reports whether owner's chief may start an agent with
// task_dispatch: the tool is on and owner opted in.
func chiefMayDispatch(ext Extensions, owner int64) bool {
	return chiefDispatchOn(ext) && owner > 0 && ext.TaskRouting.DispatchOwners[owner]
}

// chiefTaskActions is the actions chief can take on task n now, in a fixed
// order (forward, then dispatch).
func (s *Server) chiefTaskActions(ctx context.Context, chief agents.Agent, n notes.Note) ([]chiefTaskAction, error) {
	var out []chiefTaskAction
	t, err := s.forwardTarget(ctx, chief, n.ID)
	if err != nil {
		return nil, err
	}
	if t != nil {
		out = append(out, chiefTaskAction{Tool: taskForwardTool,
			Label: "task_forward: pass it to " + mirror.OneLine(firstNonBlank(t.DisplayName, t.Name), 80) + ", already working it"})
	}
	if chiefMayDispatch(s.ext, agentOwnerID(chief)) {
		out = append(out, chiefTaskAction{Tool: taskDispatchTool, Label: "task_dispatch: start a new agent on it"})
	}
	return out, nil
}

// chiefTaskActionsLine is the ask's line saying what the chief can do — muster's
// words, after the chief's note.
func chiefTaskActionsLine(acts []chiefTaskAction) string {
	if len(acts) == 0 {
		return "Your chief cannot forward or dispatch this task now (no agent is working it, and starting agents is not enabled for you): it is yours to handle."
	}
	return "Your chief can act on your answer: pick one of the actions below, or write your own answer."
}
