package mirror

import (
	"encoding/json"
	"slices"
	"strings"
)

// A CALLER OF muster-mirror's LOOPBACK MCP ENDPOINT, AS BOTH ENDS NAME IT
// (internal/mirrorhost's MCPProxy, internal/api's mirrored sessions' door).
//
// A Claude Code caller is named by its Claude session id. An opencode caller —
// whose one shared service cannot name a pane — is named by the session its
// tools/call carries (OpenCodeCallSession) and, once its host has bound that
// to a window, by that session's KEY (RuntimeSpec.AgentKey: "opencode:<id>"),
// so an opencode id can never be read as a Claude one.

// OpenCodeSessionMeta is the key opencode puts the calling session's id under
// in every tools/call's params._meta (packages/core/src/mcp/client.ts at
// 6ec5682): the one per-session signal it sends.
const OpenCodeSessionMeta = "ai.opencode/sessionID"

// OpenCodeCallSession is the session a tools/call's params name under
// OpenCodeSessionMeta, trimmed; "" when they name none, or name it as anything
// but a JSON string. Whether it is believed is the caller resolver's decision.
func OpenCodeCallSession(params json.RawMessage) string {
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if json.Unmarshal(params, &p) != nil {
		return ""
	}
	var sid string
	_ = json.Unmarshal(p.Meta[OpenCodeSessionMeta], &sid)
	return strings.TrimSpace(sid)
}

// OpenCodeCallerKey is the session key an opencode MCP caller is named by
// (opencode:<id>), "" for an id the wire would not carry.
func OpenCodeCallerKey(id string) string {
	if !ValidAgentSession(id) {
		return ""
	}
	return openCodeSpec().AgentKey(id)
}

// openCodeSpec is the opencode runtime's row: its KeyPrefix is what an
// opencode caller's key starts with.
func openCodeSpec() RuntimeSpec {
	r, _ := LookupRuntime(RuntimeOpenCode)
	return r
}

// RunsMCPCaller reports whether w runs the mirrored session an MCP caller is
// named by — THE ONE window rule of the mirrored sessions' MCP door:
//
//   - a Claude Code session id: in the window's active pane or another of its
//     panes (a split);
//   - an opencode caller's key (OpenCodeCallerKey): the session in the
//     window's active pane, and only while its host SENDS to it — bound by the
//     launcher (AgentSend) or by a verified title binding (AgentBind). A title
//     match the host does not send to binds no call either: it is the binding
//     muster-mirror resolved the call by.
func (w Window) RunsMCPCaller(key string) bool {
	if key == "" {
		return false
	}
	if id, ok := strings.CutPrefix(key, openCodeSpec().KeyPrefix); ok {
		return w.Runtime == RuntimeOpenCode && !w.Claude && w.AgentSession == id &&
			(w.AgentSend || w.AgentBind == AgentBindVerified)
	}
	return (w.Claude && w.ClaudeSession == key) || slices.Contains(w.OtherClaude, key)
}

// ValidMCPCaller reports whether key can name an MCP caller at all: a Claude
// session id, or an opencode caller's key.
func ValidMCPCaller(key string) bool {
	if id, ok := strings.CutPrefix(key, openCodeSpec().KeyPrefix); ok {
		return ValidAgentSession(id)
	}
	return ValidClaudeSession(key)
}

// OpenCodeMCPCaller reports whether key is an opencode caller's.
func OpenCodeMCPCaller(key string) bool {
	return strings.HasPrefix(key, openCodeSpec().KeyPrefix)
}
