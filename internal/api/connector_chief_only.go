package api

import (
	"context"

	"github.com/ZacxDev/muster/internal/agents"
)

// chiefOnlyConnectors are the connectors whose credential the proxy serves to
// the owner's CHIEF and no other agent.
//
// 🔴 `hq` IS ONE BECAUSE ITS GRANT IS SHARED AND ITS TOKEN IS BROAD. The chief's
// HQ MCP server reaches HQ through /connect/hq/ (internal/podagent hqmcp.go),
// and approving the chief's request records an owner-wide standing grant
// (shareApproval) keyed on the connector name. Without this rule any of the
// owner's other agents would then be auto-approved on its first /connect/hq/
// call and served the owner's HQ token — whose lowest useful scope also pages
// people. The refusal is at the PROXY, before any grant is filed, so it also
// holds for a grant the owner approves for another agent by hand.
var chiefOnlyConnectors = map[string]bool{"hq": true}

// chiefOnlyConnectorRefusal is the proxy's refusal of connector name for agent
// a, or "" when a may use it. An undecidable chief question refuses.
func (s *Server) chiefOnlyConnectorRefusal(ctx context.Context, a agents.Agent, name string) string {
	if !chiefOnlyConnectors[name] {
		return ""
	}
	_, chief, err := s.agentIsChief(ctx, a.ID)
	if err != nil {
		s.logger.Printf("connectors: agent %d: could not decide whether it is the chief for connector %q: %v", a.ID, name, err)
		return "could not decide whether this agent is its owner's chief, which connector " + name + " requires; retry shortly"
	}
	if chief {
		return ""
	}
	return "connector " + name + " serves its owner's chief only; " + a.Name + " is not the chief, so it was refused " +
		"(ask the chief to do it)"
}
