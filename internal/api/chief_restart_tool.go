package api

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// chief_restart: THE OPERATOR RESTARTS THEIR OWN CHIEF FROM THEIR OWN MIRRORED
// CLAUDE CODE SESSION.
//
// Operator ask: "wire the mcp so this can be done more easily". After a
// release, the chief keeps running the image and tool set it was started on
// until it is restarted; until this tool that was a browser-only flow (the
// agent page's Stop, then Start). On agent_destroy's pattern:
//
//   - LISTED ONLY on the mirror door (mirror_mcp.go's mirrorTools) and
//     DISPATCHED only there (handleMirrorMCP's own switch, like chief_ask and
//     route_reply — never dispatchCallerTool, which a gateway agent reaches
//     naming any tool), so no dispatched agent, the chief included, can call
//     it; and the handler refuses every caller that is not a mirrored session
//     besides.
//   - 🔴 IT TAKES NO AGENT. It restarts the CALLING OWNER'S chief, resolved by
//     resolveChief (the rule chiefAgent uses: the lowest-id agent labelled
//     "chief") over the owner's OWN agents (ownedBy) — so even with row-level
//     security rolled back, another owner's chief, or any agent that is not the
//     chief, cannot be named or reached. An owner whose chief is turned off
//     (chief_toggle.go) or who has none is refused.
//   - 🔴 THE SAME CODE PATH AS THE BROWSER: agentStartRefusal is checked first
//     (so a start that would be refused never begins with a stop), then
//     stopAgent and startAgent — the very functions the agent page's Stop and
//     Start run (agents.go). The browser's Stop/Start are NOT step-up gated
//     (requireSession only; docs/sending-and-step-up.md gates mirrored typing,
//     not agent lifecycle), so this door is no bypass of a fresh sign-in.
//   - 🔴 REFUSED WHILE THE CHIEF IS MID-TURN unless force. The browser's Stop
//     cuts a turn off without asking; this tool asks the same two questions
//     replaceChief does (chief_replace.go): the chief's SESSION CLAIM (a plugin
//     delivery, a kickoff, a memo turn) and its TURN GATE (an operator's chat
//     turn). Both are HELD through the restart when taken, so no turn starts
//     on a pod that is about to stop. force skips the refusal — exactly the
//     browser Stop's behaviour — and holds whichever of the two it could take.
//   - It WAITS, bounded by chiefRestartWait, for the chief's instance to be
//     observed running and ready on a NEW pod (when there was one before), and
//     reports the pod and the image it runs against MUSTER_AGENT_CC_IMAGE for
//     a claude-code chief.
//
// ⚠ NO AUDIT ROW, LIKE agent_destroy AND THE BROWSER'S Stop/Start (none of
// which writes one): the restart's provenance — the host the mirror token
// proves and the session the caller named — and its outcome are in muster's
// server log.
// ---------------------------------------------------------------------------

const chiefRestartTool = "chief_restart"

// chiefRestartMirrorOnly is chief_restart's refusal of a caller that is not a
// mirrored session.
const chiefRestartMirrorOnly = "chief_restart is available only to your owner's own mirrored Claude Code sessions"

// chiefRestartWait bounds how long the call waits for the restarted chief to
// be observed running; chiefRestartPoll is how often it looks. Vars so a test
// can shorten them.
var (
	chiefRestartWait = 4 * time.Minute
	chiefRestartPoll = 2 * time.Second
)

// chiefRestartReadBound bounds each instance read the tool makes.
const chiefRestartReadBound = 15 * time.Second

// chiefRestartClaimTTL is the session claim the restart holds: past its own
// wait, with room for the stop and the start. The claim is released on every
// exit; the TTL is the backstop for a process that dies mid-restart.
const chiefRestartClaimTTL = 10 * time.Minute

// chiefRestartToolDef is the mirrored sessions' chief_restart.
func chiefRestartToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: chiefRestartTool,
		Description: "Restart your owner's chief agent ON YOUR OWNER'S BEHALF — use it only when your owner asked you to (typically " +
			"after a muster release, so the chief runs the new image and tool set). It is the same as Stop then Start on the chief's " +
			"agent page: the pod is scaled down and started again from its current spec. It restarts only your owner's own chief and " +
			"takes no agent. It waits (up to a few minutes) for the chief to be running again and returns the pod before and after " +
			"and, for a claude-code chief, the image now running against the configured one. It is refused while the chief is " +
			"mid-turn (a message being delivered, or a chat turn) unless force is true, which cuts that turn off as Stop does.",
		Parameters: objSchema(map[string]any{
			"force": map[string]any{"type": "boolean", "description": "Restart even when the chief is mid-turn, cutting that turn off. Default false."},
		})}
}

// chiefRestartOn reports whether chief_restart is offered: it needs the agent
// store and a provisioner.
func (s *Server) chiefRestartOn() bool { return s.ext.Agents != nil && s.ext.Provisioner != nil }

// chiefRestartSide is one side (before or after) of a restart as the result
// reports it.
type chiefRestartSide struct {
	Status string `json:"status"`
	Pod    string `json:"pod,omitempty"`
	Ready  bool   `json:"ready"`
	Image  string `json:"image,omitempty"`
}

// sideOf is agent a's side as idx shows it; observed is false when live state
// could not be read (a nil index), and the side is then the stored status.
func sideOf(a agents.Agent, idx map[string]*provision.Instance) (side chiefRestartSide, observed bool) {
	side.Status = liveStatusIndexed(a, idx)
	if idx == nil {
		return side, false
	}
	if inst := idx[a.Name]; inst != nil {
		side.Pod, side.Ready, side.Image = inst.InstanceID, inst.Ready, inst.Image
	}
	return side, true
}

// toolChiefRestart is chief_restart.
func (s *Server) toolChiefRestart(ctx context.Context, c toolCaller, arguments string) string {
	if !c.mirroredSession() {
		return toolErr("%s", chiefRestartMirrorOnly)
	}
	var in struct {
		Force bool `json:"force"`
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(orEmptyJSON(arguments))))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return toolErr("arguments are {force?}; chief_restart takes no agent — it restarts your owner's own chief")
	}
	if !s.chiefRestartOn() {
		return toolErr("this deployment has no agent provisioner wired, so it cannot restart the chief")
	}
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return toolErr("the chief could not be read")
	}
	owner := p.UserID
	list, err := s.ext.Agents.List(ctx)
	if err != nil {
		s.logger.Printf("chief_restart: list agents: %v", err)
		return toolErr("the chief could not be read; nothing was restarted")
	}
	// 🔴 THE OWNER GUARD: the chief is resolved among the CALLER'S OWN agents
	// only. Row-level security already scopes the list; this is the same rule
	// stated again, so a list that is not scoped (RLS rolled back) still cannot
	// hand this owner another owner's chief.
	chief, found := resolveChief(ownedBy(list, owner))
	if !found {
		return toolErr("your owner has no chief (no agent labelled %q); nothing was restarted", ChiefDisplayName)
	}
	if on, err := s.chiefOn(ctx, owner, list); err != nil || !on {
		return toolErr("your owner's chief %s is turned off; nothing was restarted — turn it on from muster first", chief.Name)
	}
	if why := s.agentStartRefusal(ctx, chief); why != "" {
		return toolErr("the chief %s could not be started again (%s), so it was not stopped; nothing was restarted", chief.Name, why)
	}

	// MID-TURN: the session claim, then the turn gate, as replaceChief takes them.
	claimOwner := "chief-restart/" + randomHex(6)
	claimed, err := s.ext.Agents.ClaimKickoff(ctx, chief.ID, claimOwner, chiefRestartClaimTTL)
	if err != nil {
		s.logger.Printf("chief_restart: claim %s: %v", chief.Name, err)
		return toolErr("could not check whether the chief is mid-turn; nothing was restarted")
	}
	if claimed {
		defer func() {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			_ = s.ext.Agents.ReleaseKickoffClaim(rctx, chief.ID, claimOwner)
		}()
	} else if !in.Force {
		return toolErr("the chief %s is mid-turn (a plugin message, its kickoff or a memo is being delivered to it); nothing was "+
			"restarted — retry when it finishes, or pass force:true to cut the turn off as Stop does", chief.Name)
	}
	gated := s.turns.close(chief.ID)
	if gated {
		defer s.turns.reopen(chief.ID)
	} else if !in.Force {
		return toolErr("a chat turn to the chief %s is running; nothing was restarted — retry when it finishes, or pass "+
			"force:true to cut the turn off as Stop does", chief.Name)
	}
	forced := in.Force && (!claimed || !gated)

	before, _ := sideOf(chief, s.instanceIndexWithin(ctx, chiefRestartReadBound))
	from := c.asker.MirrorHost + "/" + c.asker.MirrorSession
	s.logger.Printf("agents: restart of the chief %d (%s) started by the operator via MCP from %s (mid-turn force: %v)", chief.ID, chief.Name, from, forced)

	// The stop and the start outlive the request (a caller that goes away must
	// not leave the chief stopped half-way) but keep its principal.
	opCtx := context.WithoutCancel(ctx)
	if err := s.stopAgent(opCtx, chief.ID); err != nil {
		s.logger.Printf("agents: restart of the chief %d (%s) via MCP from %s: stop: %v", chief.ID, chief.Name, from, err)
		return toolErr("the chief %s could not be stopped (%v); it was not restarted", chief.Name, err)
	}
	if err := s.startAgent(opCtx, chief.ID); err != nil {
		s.logger.Printf("agents: restart of the chief %d (%s) via MCP from %s: start: %v", chief.ID, chief.Name, from, err)
		return toolErr("the chief %s was STOPPED but could not be started again (%v); start it from its agent page", chief.Name, err)
	}

	after, observed := s.awaitChiefRunning(ctx, chief, before.Pod)
	out := map[string]any{"agent_id": chief.ID, "agent": chief.Name, "before": before, "after": after}
	if forced {
		out["forced"] = true
	}
	expected := ""
	if agents.ResolveKind(chief.Kind) == agents.KindClaudeCode {
		expected = s.ext.AgentCCImage
	}
	if expected != "" {
		out["expected_image"] = expected
	}
	if !observed || after.Status != agents.StatusRunning || !after.Ready || (before.Pod != "" && after.Pod == before.Pod) {
		s.logger.Printf("agents: restart of the chief %d (%s) via MCP from %s: not confirmed running within %s (status %s, pod %q, was %q)",
			chief.ID, chief.Name, from, chiefRestartWait, after.Status, after.Pod, before.Pod)
		return toolErr("the chief %s was stopped and started, but was not seen running on a new pod within %s: status %s, pod %q "+
			"(before: %s, pod %q); check its agent page", chief.Name, chiefRestartWait, after.Status, after.Pod, before.Status, before.Pod)
	}
	if expected != "" && after.Image != "" && after.Image != expected {
		s.logger.Printf("agents: restart of the chief %d (%s) via MCP from %s: running image %s, configured %s", chief.ID, chief.Name, from, after.Image, expected)
		return toolErr("the chief %s restarted on pod %s, but it runs image %s, not the configured %s", chief.Name, after.Pod, after.Image, expected)
	}
	out["restarted"] = true
	switch {
	case expected != "" && after.Image != "":
		out["image_matches"] = true
	case expected != "":
		out["image_note"] = "the provisioner does not report the running image, so it was not compared with expected_image"
	}
	s.logger.Printf("agents: restart of the chief %d (%s) via MCP from %s: running on pod %s (was %q)", chief.ID, chief.Name, from, after.Pod, before.Pod)
	return toolJSON(out)
}

// awaitChiefRunning reads the chief's instance until it is running and ready
// on a pod other than prevPod (any pod when prevPod is ""), chiefRestartWait
// passes, or ctx ends. It returns the last side read and whether live state
// was observed at all on that read. A status of error (an image that will not
// pull, a crash loop) ends the wait at once.
func (s *Server) awaitChiefRunning(ctx context.Context, chief agents.Agent, prevPod string) (chiefRestartSide, bool) {
	deadline := time.Now().Add(chiefRestartWait)
	for {
		side, observed := sideOf(chief, s.instanceIndexWithin(ctx, chiefRestartReadBound))
		if observed && side.Status == agents.StatusRunning && side.Ready && side.Pod != "" && side.Pod != prevPod {
			return side, true
		}
		if observed && side.Status == agents.StatusError {
			return side, true
		}
		if !time.Now().Before(deadline) {
			return side, observed
		}
		select {
		case <-ctx.Done():
			return side, observed
		case <-time.After(chiefRestartPoll):
		}
	}
}
