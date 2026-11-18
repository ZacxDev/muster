package api

import (
	"slices"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// TestEveryMirrorToolIsClassifiedForATeamHost is the teamHostTools ledger
// (team sharing 2b.2): EVERY tool the mirror door can list — read from
// mirrorTools' own definition functions (mirrorToolDefFuncs, which
// TestEveryMirrorToolIsClassifiedForTheStepUp pins to mirrorTools' source) —
// is classified kept or refused for a team token, and teamHostTools names no
// tool the door does not have. It fails when the mirror tool set grows OR
// shrinks. The kept set is pinned exactly (a change is a decision), and every
// owner-authority tool (hostStepUpTools) is refused, so a team host never
// reaches the step-up gate.
func TestEveryMirrorToolIsClassifiedForATeamHost(t *testing.T) {
	door := map[string]bool{}
	for _, defs := range mirrorToolDefFuncs {
		for _, d := range defs() {
			door[d.Name] = true
		}
	}
	if len(door) < 40 {
		t.Fatalf("instrument check FAILED: %d mirror tools read", len(door))
	}
	for n := range door {
		r, ok := teamHostTools[n]
		if !ok {
			t.Errorf("mirror tool %s is not classified in teamHostTools: decide whether a team host's sessions may run it", n)
			continue
		}
		if r.why == "" {
			t.Errorf("teamHostTools[%s] gives no reason", n)
		}
	}
	for n := range teamHostTools {
		if !door[n] {
			t.Errorf("teamHostTools classifies %s, which the mirror door does not have: remove it", n)
		}
	}
	var kept []string
	for n, r := range teamHostTools {
		if r.keep {
			kept = append(kept, n)
		}
	}
	slices.Sort(kept)
	want := []string{agentReportDoneTool, attentionAskTool, attentionCancelTool, attentionWaitTool,
		officers.ToolAskPerson, officers.ToolBrief, officers.ToolClaim, officers.ToolComplete, officers.ToolContactGet,
		officers.ToolContactNote, officers.ToolContactsList, officers.ToolHeartbeat, officers.ToolItems,
		officers.ToolPostDraft, officers.ToolRelease, taskCommentTool, taskGetTool, taskSetStatusTool, tasksListTool,
		trackTaskTool}
	slices.Sort(want)
	// Errorf, not Fatalf: the per-decision checks below must still run, so a
	// wrong kept tool fails with ITS reason too (mutation-checked).
	if !slices.Equal(kept, want) {
		t.Errorf("the kept set = %v\nwant %v", kept, want)
	}
	for n := range hostStepUpTools {
		if teamHostKeeps(n) {
			t.Errorf("owner-authority tool %s is kept for a team host: a team has no phone, so it must be refused before the step-up gate", n)
		}
	}
	if teamHostKeeps(routeReplyToolName) {
		t.Error("route_reply is kept for a team host; operator decision G2 refuses it")
	}
	for _, n := range []string{"connectors_list", "connector_request", "connector_status", postgresQueryToolName, internalHTTPToolName} {
		if teamHostKeeps(n) {
			t.Errorf("connector instance tool %s is kept for a team host; operator decision G3 refuses it", n)
		}
	}
	if teamHostKeeps(pluginOutboundBatchGetTool) {
		t.Error("plugin_outbound_batch_get (an estate read) is kept for a team host")
	}
	if teamHostKeeps("a_tool_nobody_classified") {
		t.Error("an unclassified tool is kept: teamHostKeeps must fail closed")
	}
}

// mirrorDoorAgentReaders is every entry of the agent-read ledgers
// (agentReadCallers, and agentsListCallers under "list <fn>") whose function
// SERVES A MIRROR DOOR TOOL, with that tool. Those ledgers classed such a
// caller "machine: … (no team memberships)" when the mirror door never carried
// a team; since 2b.2 a team token's principal IS the team, and a team agent is
// its own row. So each entry must name the teamHostTools refusal that keeps a
// team token from it (a refused tool) or say what it reads on a team host (a
// kept one).
//
// ⚠ ITS GROWTH CHECK IS A CONVENTION, NOT A CALL GRAPH: an entry that names
// the mirror door ("mirror door", "mirrored session", "team host") must be in
// this map. A new mirror tool's handler that reads agents and whose ledger
// line names none of those is not caught here; its tool still fails
// TestEveryMirrorToolIsClassifiedForATeamHost until it is classified.
var mirrorDoorAgentReaders = map[string]string{
	"Get Server.toolConnectorRequestDecide":  connectorRequestDecideTool,
	"Get Server.toolConnectorRequestsList":   connectorRequestsListTool,
	"Get Server.toolAgentDestroy":            agentDestroyTool,
	"GetByName Server.toolAgentDestroy":      agentDestroyTool,
	"Get Server.awaitChiefRunning":           chiefRestartTool,
	"GetByName Server.officerHostCaller":     officers.ToolBrief,
	"GetByName Server.ensureHostInstanceRow": officers.ToolBrief,
	"list Server.toolChiefRestart":           chiefRestartTool,
	"list Server.toolAgentsList":             agentsListTool,
}

// TestNoMirrorDoorAgentReaderIsReachableByATeamToken is the seam between the
// agent-read ledgers and teamHostTools (team sharing 2b.2).
func TestNoMirrorDoorAgentReaderIsReachableByATeamToken(t *testing.T) {
	ledger := map[string]string{}
	for k, v := range agentReadCallers {
		ledger[k] = v
	}
	for k, v := range agentsListCallers {
		ledger["list "+k] = v
	}
	for k, tool := range mirrorDoorAgentReaders {
		why, ok := ledger[k]
		if !ok {
			t.Errorf("mirrorDoorAgentReaders names %s, which neither agent-read ledger lists: remove it", k)
			continue
		}
		r, classified := teamHostTools[tool]
		switch {
		case !classified:
			t.Errorf("%s serves %s, which teamHostTools does not classify", k, tool)
		case !r.keep && !strings.Contains(why, "teamHostTools"):
			t.Errorf("%s serves %s, refused to a team token, but its ledger line %q does not name the teamHostTools refusal", k, tool, why)
		case r.keep && !strings.Contains(why, "team host"):
			t.Errorf("%s serves %s, KEPT for a team token, but its ledger line %q does not say what it reads on a team host", k, tool, why)
		}
	}
	for k, why := range ledger {
		if _, ok := mirrorDoorAgentReaders[k]; ok {
			continue
		}
		for _, mark := range []string{"mirror door", "mirrored session", "team host"} {
			if strings.Contains(why, mark) {
				t.Errorf("ledger entry %s (%q) names the mirror door but is not in mirrorDoorAgentReaders: say which mirror tool it serves", k, why)
			}
		}
	}
}
