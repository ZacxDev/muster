package api

import (
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// A TEAM HOST'S MCP DOOR (team sharing phase 2b.2; docs/design/team-sharing.md,
// "The team host's MCP door").
//
// A team host is a muster-mirror connected with a TEAM-owned mirror token. Its
// POST /mirror/mcp acts as the TEAM ACTING PRINCIPAL (authz.ViaMirrorHost is
// team-acting, and admit lets a team through it), and row-level security then
// shows it EVERY team row. So the tool set below is the WHOLE guard (D-1113
// #5): a team token is listed only the kept tools, and every other name is
// refused by name at the call (serveMCP runs a listed name only, and the
// door's disabled hook answers teamHostToolRefusal). A tool that is not
// classified here is refused too (fail closed), and
// TestEveryMirrorToolIsClassifiedForATeamHost fails until it is classified.
//
// 🔴 THIS LEDGER, admit's mirror arm and ViaMirrorHost's place in
// teamActingVias LAND TOGETHER, and none is opened without the others:
// opening the doors without the ledger would hand every session on a team
// host the team's whole power.
//
// Because every tool in hostStepUpTools is refused here, a team host never
// reaches the step-up gate (hostStepUpGate) and files no confirmation card: a
// team has no phone.
// ---------------------------------------------------------------------------

// teamHostRule is one mirror tool's classification for a team token.
type teamHostRule struct {
	keep bool
	why  string
}

func teamKept(why string) teamHostRule    { return teamHostRule{keep: true, why: why} }
func teamRefused(why string) teamHostRule { return teamHostRule{why: why} }

const (
	whyOwnerAuthority = "an owner-authority decision (hostStepUpTools): a team decides through a member in muster's web UI, never through a host"
	whyEstateRead     = "a read of the owner's whole estate: as the team it would show every team row to every session on the host"
	whyPage           = "pages are a personal table: a team-owned page would be unreadable by every member"
	whyConnectorG3    = "a host instance's connector tool: a team host officer that declares connectors is refused at boot (operator decision G3)"
	whyOfficer        = "a team host officer instance's own (hostOfficerMirrorTool binds it to a recorded spawn)"
)

// teamHostTools classifies EVERY mirror door tool as kept or refused for a
// team token.
var teamHostTools = map[string]teamHostRule{
	// The session's own asks.
	attentionAskTool:    teamKept("the session's own question; the team's members see it on Attention at viewer"),
	attentionWaitTool:   teamKept("waits on the session's own ask (ownAsk)"),
	attentionCancelTool: teamKept("withdraws the session's own ask (ownAsk)"),
	// The tasks the session created (taskScopeOf).
	tasksListTool:     teamKept("the tasks this session created"),
	taskGetTool:       teamKept("the tasks this session created"),
	taskCommentTool:   teamKept("the tasks this session created"),
	taskSetStatusTool: teamKept("the tasks this session created"),
	// The session's own host dispatch.
	agentReportDoneTool: teamKept("the session's own dispatch"),
	// A team host officer instance's tools.
	officers.ToolBrief:        teamKept(whyOfficer),
	officers.ToolItems:        teamKept(whyOfficer),
	officers.ToolClaim:        teamKept(whyOfficer),
	officers.ToolHeartbeat:    teamKept(whyOfficer),
	officers.ToolComplete:     teamKept(whyOfficer),
	officers.ToolRelease:      teamKept(whyOfficer),
	officers.ToolAskPerson:    teamKept(whyOfficer),
	officers.ToolPostDraft:    teamKept(whyOfficer + "; its draft waits for a manager"),
	officers.ToolContactGet:   teamKept(whyOfficer),
	officers.ToolContactsList: teamKept(whyOfficer),
	officers.ToolContactNote:  teamKept(whyOfficer),

	// Every owner-authority tool (all of hostStepUpTools).
	attentionAnswerTool:           teamRefused(whyOwnerAuthority),
	connectorRequestDecideTool:    teamRefused(whyOwnerAuthority),
	pluginDraftDecideTool:         teamRefused(whyOwnerAuthority),
	agentDestroyTool:              teamRefused(whyOwnerAuthority),
	chiefRestartTool:              teamRefused(whyOwnerAuthority),
	dispatchApproveTool:           teamRefused(whyOwnerAuthority),
	routeReplyToolName:            teamRefused(whyOwnerAuthority + "; and it relays text to an outside party as the team with no manager's approval (operator decision G2)"),
	pluginMessageRouteTool:        teamRefused(whyOwnerAuthority),
	pluginRequestDecideTool:       teamRefused(whyOwnerAuthority),
	pluginOutboundBatchDecideTool: teamRefused(whyOwnerAuthority),
	// Every read of the owner's whole estate.
	agentsListTool:             teamRefused(whyEstateRead),
	connectorRequestsListTool:  teamRefused(whyEstateRead),
	pluginDraftsListTool:       teamRefused(whyEstateRead),
	pluginMessagesListTool:     teamRefused(whyEstateRead),
	pluginRequestsListTool:     teamRefused(whyEstateRead),
	pluginOutboundBatchGetTool: teamRefused(whyEstateRead),
	// Starting work as the owner, and the owner's chief.
	dispatchRequestTool: teamRefused("starts an agent as the owner: a team agent is started by a manager"),
	chiefAskTool:        teamRefused("a question to the owner's chief: a team chief does not bind to team hosts in 2b (G4)"),
	// The page tools.
	pageCreateTool:       teamRefused(whyPage),
	pageUpdateTool:       teamRefused(whyPage),
	pageGetTool:          teamRefused(whyPage),
	pageListTool:         teamRefused(whyPage),
	pageShareRequestTool: teamRefused(whyPage),
	pageResponsesTool:    teamRefused(whyPage),
	pageLinkRevokeTool:   teamRefused(whyPage),
	// A host instance's connector tools (G3).
	"connectors_list":     teamRefused(whyConnectorG3),
	"connector_request":   teamRefused(whyConnectorG3),
	"connector_status":    teamRefused(whyConnectorG3),
	postgresQueryToolName: teamRefused(whyConnectorG3),
}

// teamHostKeeps reports whether a team token may run tool name: classified
// kept. An unclassified name is refused (fail closed).
func teamHostKeeps(name string) bool {
	r, ok := teamHostTools[name]
	return ok && r.keep
}

// teamHostToolRefusal is the door's answer — a sentence, for a tool error —
// to a team host session naming a tool it is not offered.
func teamHostToolRefusal(name string) string {
	return name + " is not offered on a team host: its sessions get attention_ask, attention_wait, attention_cancel, " +
		"the task tools over the tasks they created, agent_report_done and a team host officer's own tools; " +
		"a decision, a read of the team's whole estate or a page is a member's, from muster's web UI"
}

// TeamHostRemoteRefusal is the 401 a team token's remote-marked call gets
// (X-Muster-Client: remote).
//
// 🔴 IT IS NOT REDUNDANT WITH teamHostTools. remoteMirrorTools serves the
// four task tools, which teamHostTools KEEPS, and a remote call is served
// unbound: the session it names (X-Muster-Remote-Session) is recorded, never
// checked against a listing. Without this refusal a remote caller holding a
// team token could name any session and work every team task that session
// created, as the team. TestATeamTokenIsNotServedRemotely.
const TeamHostRemoteRefusal = "a team host token is not served remotely"
