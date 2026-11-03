package mirror

// MCPProtocolVersions are the MCP protocol revisions muster answers, newest
// first: a client asking for one gets it back; any other gets the newest.
//
// ONE LIST, TWO ANSWERERS: muster's own MCP server (internal/api, POST /mcp
// and POST /mirror/mcp) and a host's local MCP endpoint (internal/mirrorhost's
// MCPProxy) when it answers `initialize` itself, for a caller outside the tmux
// server it mirrors. Both must negotiate alike.
var MCPProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// NegotiateMCPVersion is the protocol revision to answer an `initialize` that
// asked for requested.
func NegotiateMCPVersion(requested string) string {
	for _, v := range MCPProtocolVersions {
		if v == requested {
			return v
		}
	}
	return MCPProtocolVersions[0]
}
