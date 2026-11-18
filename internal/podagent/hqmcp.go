package podagent

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// THE HQ MCP SERVER: an owner's HQ task system, reached through muster's
// connector proxy, for the claude-code CHIEF only.
//
// muster (internal/agentspec, set for the chief by internal/agentprovision)
// writes EnvHQMCP=1; the pod's `ccd seed` (cmd/ccd/hqmcp.go) then registers
// the stdio server `hq mcp` under the name HQMCPServerName — so its tools
// appear as mcp__hq__<tool> — and adds HQAllowEntry to permissions.allow.
//
// 🔴 NO HQ CREDENTIAL EVER REACHES THE POD. The server is started with
// HQ_URL = <MUSTER_CONNECT_URL>/connect/hq/<the connector's host> and
// HQ_TOKEN = the agent's OWN muster token (EnvAgentToken, already in the
// pod's environment, and already written into .claude.json as the muster
// server's X-Muster-Token). The hq client sends it as `Authorization: Bearer`;
// muster's proxy authenticates the agent by it, strips it, and injects the
// `hq` connector's stored credential on the way out.
//
// 🔴 THE ALLOW ENTRY IS EVERY HQ TOOL (operator decision): the HQ token the
// owner stores in the `hq` connector is the only bound on what the chief can
// do in HQ. It is emitted for the chief alone, never for a worker: a standing
// grant shares an approval across the owner's agents, so a deployment-wide
// allow would hand every worker the same unprompted reach.
// ---------------------------------------------------------------------------

const (
	// EnvHQMCP, set to "1", makes `ccd seed` offer the HQ MCP server.
	EnvHQMCP = "CCD_HQ_MCP"
	// HQConnector is the muster connector the server is reached through.
	HQConnector = "hq"
	// HQMCPServerName is the server's key in the CLI's mcpServers.
	HQMCPServerName = "hq"
	// HQCommand is the stdio server's program and arguments.
	HQCommand = "hq"
	// HQAllowEntry is the permissions.allow entry for every tool the server
	// serves (the CLI accepts a `*` in the tool position after a literal
	// mcp__<server>__ prefix in an allow rule).
	HQAllowEntry = "mcp__" + HQMCPServerName + "__*"
)

// HQArgs are HQCommand's arguments: `hq mcp`, every tool the token allows.
var HQArgs = []string{"mcp"}

// ParseHQMCP reads an EnvHQMCP value: "" off, exactly "1" on, anything else
// an error (a value muster did not write is refused, never guessed at).
func ParseHQMCP(v string) (bool, error) {
	switch v {
	case "":
		return false, nil
	case "1":
		return true, nil
	}
	return false, fmt.Errorf("%s=%q: the only value is \"1\" (offer the HQ MCP server); unset is off", EnvHQMCP, v)
}

// HQProxyURL is the base URL the hq client is given: the `hq` connector's
// prefix on muster's proxy for host. Every HQ API path is appended to it.
func HQProxyURL(base, host string) string {
	return strings.TrimRight(base, "/") + "/connect/" + HQConnector + "/" + strings.ToLower(host)
}

// HQServes reports whether the server should be registered for this answer of
// GET /agent/connectors/hq, and with which host.
//
// 🔴 A GRANT OF "none" OR "pending" REGISTERS IT; ONLY denied AND revoked DO
// NOT. The chief has no connector_request tool: its grant is filed by its FIRST
// proxied call (connectors.Proxy.CheckAccess), which an owner's standing grant
// approves at once and otherwise puts in front of the owner. A gate on
// "approved" would therefore never open for the chief — nothing would ever
// file the request. The proxy, not this registration, decides every call.
func HQServes(c AgentConnector) (host string, ok bool) {
	if !c.Exists || len(c.Hosts) == 0 || c.Hosts[0] == "" {
		return "", false
	}
	switch c.Grant {
	case "denied", "revoked":
		return "", false
	}
	return c.Hosts[0], true
}
