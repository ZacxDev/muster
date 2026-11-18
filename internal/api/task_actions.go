package api

import (
	"context"

	"strconv"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
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
// and agent_dispatch refuses through chiefDispatchDeploymentRefusal and
// chiefDispatchTaskRefusal — the same functions
// chiefTaskActions calls — so the offer and the gate cannot disagree.
//
//   - task_forward: a running agent other than the chief works the task
//     (forwardTarget; the tool refuses otherwise).
//   - agent_dispatch: the tool would start an agent on the task — the opt-in and
//     the deployment (chiefDispatchDeploymentRefusal) and the task itself: not
//     gated, open, no live agent on it (chiefDispatchTaskRefusal) — and the
//     chief's budget not Full (plugins.ChiefBudget, the predicates the
//     reservation refuses on). The tool's argument checks (kind, repo,
//     instructions) are the chief's to satisfy.
//   - on a task whose only gate is the one the chief filed (releasableGates):
//     agent_dispatch is judged on the task AS IF released, each per-target
//     option becomes "Release & dispatch on <target>", and "Release only" is
//     added — the owner's tap releases the gate (answer_release.go), and only
//     then does the chief's agent_dispatch run, on the ungated task.
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
	all, err := s.forwardTargets(ctx, chief, taskID)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	return &all[0], nil
}

// forwardTargets is every agent task_forward could pass task taskID's change
// to, newest first: THE one predicate (agentCanBeSent, never the chief), which
// forwardTarget takes the first of and the task triage (task_triage.go)
// counts.
func (s *Server) forwardTargets(ctx context.Context, chief agents.Agent, taskID int64) ([]agents.Agent, error) {
	on, err := s.liveOnTask(ctx, agentOwnerID(chief), taskID)
	if err != nil {
		return nil, err
	}
	var out []agents.Agent
	for i := range on { // newest first
		if on[i].ID != chief.ID && agentCanBeSent(on[i].Status) {
			out = append(out, on[i])
		}
	}
	return out, nil
}

// chiefMayDispatch reports whether owner's chief may start an agent with
// agent_dispatch: the tool is on and owner opted in.
func chiefMayDispatch(ext Extensions, owner int64) bool {
	return chiefDispatchOn(ext) && owner > 0 && ext.TaskRouting.DispatchOwners[owner]
}

// chiefDispatchDeploymentRefusal is agent_dispatch's refusal for owner that no
// task or argument changes, "" when there is none: the opt-in
// (chiefMayDispatch), then the deployment's ability to start an agent at all.
// toolAgentDispatch answers with it first; chiefTaskActions offers dispatch only
// when it is "".
func (s *Server) chiefDispatchDeploymentRefusal(owner int64) string {
	if !chiefMayDispatch(s.ext, owner) {
		return chiefDispatchOptOut
	}
	if !s.dispatchServed() {
		return "this deployment cannot start agents"
	}
	if reason := s.ext.Provisioner.KickoffUndeliverableReason(); reason != "" {
		return "this deployment cannot start an agent now: " + reason
	}
	return ""
}

// chiefDispatchTaskRefusal is agent_dispatch's refusal for task n itself, ""
// when there is none: gated, not open, or a live agent (pending, provisioning,
// running) already on it. toolAgentDispatch runs it under its reservation lock;
// chiefTaskActions offers dispatch only when it is "". The tool's ARGUMENT
// checks (kind, repo, instructions) come on top and are not part of the offer:
// the chief picks those.
func (s *Server) chiefDispatchTaskRefusal(ctx context.Context, owner int64, n notes.Note) (string, error) {
	r, err := s.chiefDispatchTaskBlock(ctx, owner, n)
	return r.refusal, err
}

// taskBlock is why agent_dispatch refuses a task itself: the chief's refusal
// sentence, and the same reason in the OWNER's words for the ask
// (chiefTaskActionsLine). Both zero when the task allows a start.
type taskBlock struct {
	refusal string
	owner   string
}

// dispatchableStatus is whether task n's STATUS allows a dispatch: open — or,
// for a plugin task reference, open or in_progress. A reference's status is
// the upstream's, and an upstream task is often in progress there (assigned,
// being worked) before muster starts an agent on it; for a reference the
// "already being worked here" check is the live-agent one that follows, not
// the status.
func dispatchableStatus(n notes.Note) bool {
	if n.IsReference() && n.Status == notes.StatusInProgress {
		return true
	}
	return n.Status == notes.StatusOpen
}

// chiefDispatchClaimed is agent_dispatch's refusal of a claimed task.
const chiefDispatchClaimed = "is already claimed: a session is working it, so no agent is started on it (its claim must be released first)"

// chiefDispatchTaskBlock is THE task predicate behind chiefDispatchTaskRefusal
// (the tool's refusal) and chiefTaskActions (the ask's reason): one check,
// two wordings.
func (s *Server) chiefDispatchTaskBlock(ctx context.Context, owner int64, n notes.Note) (taskBlock, error) {
	id := strconv.FormatInt(n.ID, 10)
	if reason, gated := notes.GateReason(n.Tags); gated {
		return taskBlock{"task " + id + " (" + reason + ") " + chiefDispatchGatedText,
			"this task is gated (" + reason + "), so only you can release it"}, nil
	}
	if !dispatchableStatus(n) {
		return taskBlock{"task " + id + " " + chiefDispatchNotOpen + " (" + n.Status + ")",
			"this task is not open (" + n.Status + "), so it cannot start one on it"}, nil
	}
	// 🔴 A CLAIMED TASK IS BEING WORKED (notes.Claims): no new agent on it — not
	// offered on a card (a question about a claimed task offers no dispatch
	// button) and refused by agent_dispatch. Releasing the claim lifts it.
	if claims := notes.Claims(n); len(claims) > 0 {
		return taskBlock{"task " + id + " " + chiefDispatchClaimed + " (" + claimNames(claims) + ")",
			claimNames(claims) + " is already working this task, so it cannot start another"}, nil
	}
	on, err := s.liveOnTask(ctx, owner, n.ID)
	if err != nil {
		return taskBlock{}, err
	}
	for _, x := range on {
		if agentLive(x.Status) {
			return taskBlock{"task " + id + " " + chiefDispatchHasAgent,
				"an agent on this task is still starting, so it cannot start another"}, nil
		}
	}
	return taskBlock{}, nil
}

// chiefTaskActions is the actions chief can take on task n now, in a fixed
// order (forward, then dispatch), and — when the owner opted in to dispatch —
// the chief's dispatch budget (nil otherwise, or when it could not be read).
//
// agent_dispatch is offered only when the budget is NOT Full: the reservation
// refuses on the same predicates (plugins.ChiefBudget.LiveFull / DailyFull),
// so a full budget is an action the chief cannot take. A budget that could not
// be read withholds the offer (the ask is still filed; the tool re-checks).
//
// withheld is why agent_dispatch was NOT offered, in the owner's words, for
// the ask (chiefTaskActionsLine); "" exactly when it was offered. Each reason
// is named only when it is the one that applies, checked in the tool's own
// order: the opt-in (only then "not enabled for you"), the deployment, the
// task itself, then the budget (could not be read, or full).
func (s *Server) chiefTaskActions(ctx context.Context, chief agents.Agent, n notes.Note) (acts []chiefTaskAction, budget *plugins.ChiefBudget, withheld string, err error) {
	var out []chiefTaskAction
	t, err := s.forwardTarget(ctx, chief, n.ID)
	if err != nil {
		return nil, nil, "", err
	}
	if t != nil {
		out = append(out, chiefTaskAction{Tool: taskForwardTool,
			Label: "task_forward: pass it to " + mirror.OneLine(s.agentName(ctx, *t).Text, 80) + ", already working it"})
	}
	owner := agentOwnerID(chief)
	if chiefMayDispatch(s.ext, owner) {
		budget = s.chiefBudgetNow(ctx)
	}
	// 🔴 A GATE THE CHIEF FILED IS THE OWNER'S TO RELEASE FROM THE ASK
	// (releaseFromAnswer): the dispatch offer is then judged on the task AS IF
	// released — exactly what agent_dispatch will see once the owner's tap has
	// released it — and each per-target option becomes "Release & dispatch".
	judged, releasable := n, false
	if gates, ok := s.releasableGates(ctx, n); ok {
		judged.Tags, releasable = withoutTags(n.Tags, gates), true
	}
	switch {
	case !chiefMayDispatch(s.ext, owner):
		withheld = withheldNotEnabled
	case s.chiefDispatchDeploymentRefusal(owner) != "":
		withheld = withheldDeployment
	default:
		blk, err := s.chiefDispatchTaskBlock(ctx, owner, judged)
		if err != nil {
			return nil, nil, "", err
		}
		switch {
		case blk.refusal != "": // the task itself refuses: not the budget's doing
			withheld = blk.owner
		case budget == nil:
			withheld = withheldBudgetUnknown
		case budget.Full():
			withheld = withheldBudgetFull
		default:
			// 🔴 ONE OPTION PER TARGET. agent_dispatch has no default target,
			// and the owner's click reaches the chief as a turn the owner did
			// not type — so a target-less "start one" left the chief unable to
			// act on it. Each option names its target; agent_dispatch then
			// holds the chief to the one picked (ownersChosenTarget).
			targets := s.dispatchTargetsNow(ctx).Pods
			if len(targets) == 0 {
				withheld = withheldDeployment
			}
			for _, target := range targets {
				label := dispatchOptionLabel(target)
				if releasable {
					label = releaseDispatchLabel(target, n.Repo)
				}
				out = append(out, chiefTaskAction{Tool: agentDispatchTool, Label: label})
			}
		}
	}
	if releasable {
		// The release options FIRST — Release & dispatch on the first target,
		// then Release only, then the other targets, then task_forward: a phone
		// notification draws at most two options beside Dismiss
		// (muster-android AskPush.MAX_BUTTONS), so Release only must be one.
		var rel, rest []chiefTaskAction
		for _, x := range out {
			if x.Tool == agentDispatchTool {
				rel = append(rel, x)
			} else {
				rest = append(rest, x)
			}
		}
		only := chiefTaskAction{Tool: releaseOnlyAction, Label: releaseOnlyLabel}
		if len(rel) > 0 {
			rel = append(rel[:1], append([]chiefTaskAction{only}, rel[1:]...)...)
		} else {
			rel = []chiefTaskAction{only}
		}
		out = append(rel, rest...)
	}
	return out, budget, withheld, nil
}

// releasableGates is task n's gates when the owner may release them from a
// routed-attention ask: n is open and its only gate is the one the chief filed
// with task_create_gated (chiefsGate — an owner's own hold is never offered).
// ok false otherwise, including when the ledger could not be read.
func (s *Server) releasableGates(ctx context.Context, n notes.Note) ([]string, bool) {
	if !notes.IsGated(n.Tags) || n.Status != notes.StatusOpen || s.ext.Plugins == nil {
		return nil, false
	}
	gates, why := s.chiefsGate(ctx, n)
	return gates, why == ""
}

// Why chiefTaskActions withheld agent_dispatch, in the owner's words (the
// task's own reasons are chiefDispatchTaskBlock's).
const (
	withheldNotEnabled    = "starting agents is not enabled for you"
	withheldDeployment    = "this deployment cannot start an agent now"
	withheldBudgetUnknown = "its allowance for starting agents could not be read right now"
	withheldBudgetFull    = "it has used up its allowance for starting agents for now"
)

// chiefTaskActionsLine is the ask's line saying what the chief can do — muster's
// words, after the chief's note. withheld is chiefTaskActions's reason
// agent_dispatch was not offered.
func chiefTaskActionsLine(acts []chiefTaskAction, withheld string) string {
	for _, x := range acts {
		if x.Tool != releaseOnlyAction {
			continue
		}
		if withheld == "" {
			return "Your chief filed this task gated, so only you can release it: Release & dispatch releases the gate and your " +
				"chief starts a new agent on it where you pick; Release only releases it and starts nothing."
		}
		return "Your chief filed this task gated, so only you can release it: Release only releases the gate and starts nothing " +
			"(your chief cannot start an agent on it now: " + withheld + ")."
	}
	if len(acts) == 0 {
		return "Your chief cannot forward or dispatch this task now (no agent is working it, and " + withheld + "): it is yours to handle."
	}
	return "Your chief can act on your answer: pick one of the actions below, or write your own answer."
}
