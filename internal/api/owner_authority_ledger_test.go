package api

import (
	"maps"
	"slices"
	"testing"
)

// ownerAuthorityCallers is THE LEDGER of the owner-authority rule's call
// sites: every function in internal/api that calls ownersOwnSession, with the
// mirror-door tools it serves. TestEveryOwnerAuthorityToolIsStepUpGated reads
// the call sites from the Go source, so a new owner-authority tool fails until
// it is listed here AND classified for the step-up gate.
var ownerAuthorityCallers = map[string][]string{
	"agent_destroy_tool.go:toolAgentDestroy":               {agentDestroyTool},
	"agents_list_tool.go:toolAgentsList":                   {agentsListTool},
	"attention_tools.go:toolAttentionAnswer":               {attentionAnswerTool},
	"chief_restart_tool.go:toolChiefRestart":               {chiefRestartTool},
	"connector_decide_tools.go:toolConnectorRequestDecide": {connectorRequestDecideTool},
	"connector_decide_tools.go:toolConnectorRequestsList":  {connectorRequestsListTool},
	// The step-up gate itself: a call the tool refuses as a spawned agent's
	// files no card. It serves no tool of its own.
	"host_stepup.go:hostStepUpGate":                        nil,
	"dispatch_hold_tools.go:dispatchHoldCaller":            {dispatchRequestTool, dispatchApproveTool},
	"outreach_batch.go:toolOutboundBatchDecide":            {pluginOutboundBatchDecideTool},
	"outreach_batch.go:toolOutboundBatchGet":               {pluginOutboundBatchGetTool},
	"plugin_draft_tools.go:toolPluginDraftDecide":          {pluginDraftDecideTool},
	"plugin_draft_tools.go:toolPluginDraftsList":           {pluginDraftsListTool},
	"plugin_message_route_tools.go:toolPluginMessageRoute": {pluginMessageRouteTool},
	"plugin_message_route_tools.go:toolPluginMessagesList": {pluginMessagesListTool},
	"plugin_pairing.go:toolPluginRequestDecide":            {pluginRequestDecideTool},
	"plugin_pairing.go:toolPluginRequestsList":             {pluginRequestsListTool},
	"route_reply_tool.go:toolRouteReply":                   {routeReplyToolName},
}

// ownerAuthorityNotGated is the owner-authority tools the step-up gate does
// NOT gate, each a decision: the reads of the owner's estate, and
// dispatch_request, which files a hold that starts nothing (dispatch_approve,
// gated, is the decision).
var ownerAuthorityNotGated = []string{agentsListTool, connectorRequestsListTool, dispatchRequestTool, pluginDraftsListTool,
	pluginMessagesListTool, pluginOutboundBatchGetTool, pluginRequestsListTool}

// TestEveryOwnerAuthorityToolIsStepUpGated pins the RELATIONSHIP between the
// two rules, both ways: every function that calls ownersOwnSession is in
// ownerAuthorityCallers (the set read from the source must equal it: it fails
// when it grows or shrinks); every tool those functions serve is either in
// hostStepUpTools — so a call from a machine that starts agents for the chief
// is a phone card, whichever window it names — or in ownerAuthorityNotGated
// and exempt (hostStepUpExempt); and every gated tool is served by one of
// them. The gated set itself is pinned literally.
func TestEveryOwnerAuthorityToolIsStepUpGated(t *testing.T) {
	got := callersIn(t, ".", "ownersOwnSession")
	got = slices.Compact(got)
	want := slices.Sorted(maps.Keys(ownerAuthorityCallers))
	if len(got) < 10 {
		t.Fatalf("instrument check FAILED: %d callers of ownersOwnSession parsed", len(got))
	}
	if !slices.Equal(got, want) {
		t.Errorf("ownersOwnSession is called from %v\nthe ledger names %v\nlist the new site with its tools, and classify them for the step-up gate", got, want)
	}
	served := map[string]bool{}
	for site, tools := range ownerAuthorityCallers {
		for _, n := range tools {
			served[n] = true
			_, gated := hostStepUpTools[n]
			_, exempt := hostStepUpExempt[n]
			notGated := slices.Contains(ownerAuthorityNotGated, n)
			switch {
			case gated && (exempt || notGated):
				t.Errorf("%s (%s) is both gated and exempt", n, site)
			case !gated && !(exempt && notGated):
				t.Errorf("owner-authority tool %s (%s) is not step-up gated: add it to hostStepUpTools, or to ownerAuthorityNotGated and hostStepUpExempt with the reason", n, site)
			}
		}
	}
	for n := range hostStepUpTools {
		if !served[n] {
			t.Errorf("gated tool %s is served by no function that checks ownersOwnSession", n)
		}
	}
	gatedWant := []string{agentDestroyTool, attentionAnswerTool, chiefRestartTool, connectorRequestDecideTool, dispatchApproveTool,
		pluginDraftDecideTool, pluginMessageRouteTool, pluginOutboundBatchDecideTool, pluginRequestDecideTool, routeReplyToolName}
	slices.Sort(gatedWant)
	if g := hostStepUpToolNames(); !slices.Equal(g, gatedWant) {
		t.Errorf("the gated set = %v, want %v", g, gatedWant)
	}
}
