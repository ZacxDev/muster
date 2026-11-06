package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/mirror"
)

// muster's MCP server: the connector store's tools for agents, over MCP's
// streamable-HTTP transport (one JSON-RPC message per POST, one JSON response).
//
// It is served at POST /mcp behind requireConnectorAgent — the same door as the
// proxy — so a tool call acts as the CALLING AGENT and the shared hook token is
// refused exactly as it is there. It speaks the subset of MCP a tool server
// needs: initialize, notifications/initialized, ping, tools/list, tools/call.
// There is no server-initiated stream (GET /mcp answers 405, which the
// transport defines as "this server offers none") and no session id: every
// call carries the agent's own token, so there is nothing to resume.
//
// 🔴 THIS FILE IS A TRANSPORT, NOT A TOOL SET. It declares no tool and executes
// none: tools/list is the agent tool registry's MCP subset ([Server.MCPToolDefsFor],
// filtered to what the calling agent may see)
// and tools/call runs the registry's one dispatcher (dispatchAgentTool). Which
// tools are on /mcp is a per-tool flag in agentTools (agent.go), whose comment
// says which tools carry it and why. The one tool it adds itself is muster_help,
// with a refusal's current_definition (mcp_help.go): those are about the
// served definitions, so they live with the transport.

// The protocol revisions this server answers are mirror.MCPProtocolVersions,
// shared with a host's local MCP endpoint, which answers `initialize` itself
// for a caller outside its tmux server.

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcpInstructions is the initialize result's instructions. The proxy recipe in
// it is ProxyRecipe's — the same sentence a connector's usage line carries — so
// the two cannot teach different URL shapes.
//
// The git sentence is true because ccd keeps git's routing in step with the
// grants by itself (cmd/ccd/connect.go, syncGitRouting): the MCP server is
// served only to claude-code agents, and ccd runs in every one of them.
func mcpInstructions() string {
	return "muster's connector store. To reach an external platform (e.g. GitHub), call connector_request " +
		"for it, wait for the operator's approval with connector_status, then call it through muster's proxy: " +
		ProxyRecipe("<connector>", "<host>") + " You never see the credential. " + mcpGitNote
}

// mcpChiefInstructions is mcpInstructions for the chief, which is not offered
// connector_request / connector_status (agentTool's notChief): its connectors
// are the ones its owner granted it.
func mcpChiefInstructions() string {
	return "muster's connector store. connectors_list shows the platform connectors your owner has granted you; " +
		"call one through muster's proxy: " + ProxyRecipe("<connector>", "<host>") + " You never see the credential. " + mcpGitNote
}

// mcpComposeChiefInstructions is the initialize instructions for the chief on
// the Docker Compose install (composeChiefRefuses).
const mcpComposeChiefInstructions = "muster's tools for the chief on this install (Docker Compose): the tools this server " +
	"lists are exactly the ones you can run. " + agentspec.ComposeChiefUnavailable

// mcpInstructionsFor is the initialize instructions this server gives agent a:
// the connector store's (the chief's own when a is the chief), and the
// attention queue's paragraph when its tools are offered (attention_tools.go's
// attentionInstructionFor: an officer instance's differs).
func (s *Server) mcpInstructionsFor(ctx context.Context, a agents.Agent) string {
	// The Compose chief: none of the connector store's or the attention
	// queue's tools is among the ones it can run, so neither paragraph is true
	// for it; it is told what this install lacks (agentspec.ComposeChiefUnavailable).
	if s.isComposeChief(ctx, a) {
		return mcpComposeChiefInstructions
	}
	in := mcpInstructions()
	if s.isChiefAgent(ctx, a) {
		in = mcpChiefInstructions()
	}
	if s.attentionToolsOn() {
		return in + " " + s.attentionInstructionFor(a)
	}
	return in
}

// mcpGitNote tells a claude-code agent how git follows a grant.
const mcpGitNote = "For git, plain `git clone/fetch/push https://<host>/...` goes through the proxy on its own: " +
	"once a connector is approved, git is routed through muster for its hosts within " +
	"about 15 seconds (run `ccd connect-git <connector>` to apply it at once), and the routing is removed " +
	"the same way when access is revoked."

// isToolErr reports whether a dispatched tool's text is toolErr's encoding —
// the registry's one spelling of a failed tool call — so MCP can flag it isError.
// serveMCP adds current_definition to such a text (withCurrentDefinition), and
// the result must still read as an error, so that key is allowed too.
func isToolErr(text string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &m) != nil {
		return false
	}
	if _, ok := m["error"]; !ok {
		return false
	}
	// toolErr's {error}, plus only the keys a structured refusal adds:
	// `refused` (session_routes.go's routeRefusal), `budget`
	// (agent_dispatch's dispatchRefusal) and `candidates`
	// (plugin_message_route's subject refusals), `retryable` (an officer
	// tool's HQ failure HQ says to retry, officerToolResult), and
	// `current_definition` (serveMCP's, on any of them) — nothing else.
	for k := range m {
		switch k {
		case "error", "refused", "budget", "candidates", "retryable", "current_definition":
		default:
			return false
		}
	}
	return true
}

func (s *Server) writeMCP(w http.ResponseWriter, id json.RawMessage, result any, rpcErr *mcpError) {
	body := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		body["error"] = rpcErr
	} else {
		body["result"] = result
	}
	if len(id) == 0 {
		body["id"] = nil
	}
	s.writeJSON(w, http.StatusOK, body)
}

// handleMCP serves POST /mcp: the calling agent's tools.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	disabled, off := s.mcpFeatureHooks(agentDoor)
	compose := s.isComposeChief(r.Context(), a)
	s.serveMCP(w, r, mcpSurface{
		tools:        func() []agents.ToolDef { return s.MCPToolDefsFor(r.Context(), a) },
		call:         func(name, args string) string { return s.dispatchAgentTool(r.Context(), a, name, args) },
		instructions: s.mcpInstructionsFor(r.Context(), a),
		disabled:     disabled,
		offFeatures:  off,
		// The Compose chief is offered exactly its pre-allowed tools, and
		// muster_help is not one of them (composeChiefRefuses).
		noHelp: compose && !agentspec.ComposeChiefAllowed(mcpHelpTool),
	})
}

// mcpSurface is one MCP door's tool set: what tools/list answers, how a
// tools/call runs, and the initialize instructions. POST /mcp (an agent's
// token) and POST /mirror/mcp (a mirrored session, through its host's token)
// are two surfaces over the ONE protocol implementation below.
type mcpSurface struct {
	tools        func() []agents.ToolDef
	call         func(name, args string) string
	instructions string
	// disabled explains a name the door does not list because its feature is
	// off ("disabled: <feature> — set … (docs/…)"), or "" (features.go's
	// mcpDisabledTool). nil: every unlisted name is unknown.
	disabled func(name string) string
	// offFeatures is the features that gate this door's tools and are off (not
	// offered) and misconfigured (offered, degraded), which muster_help lists
	// (features.go's mcpOffFeatures). nil: none listed.
	offFeatures func() (off, degraded []mcpOffFeature)
	// noHelp: the door does not add muster_help (the Compose chief, which
	// could not run it).
	noHelp bool
}

// explainUnlisted is surface.disabled's answer for name, or "".
func (m mcpSurface) explainUnlisted(name string) string {
	if m.disabled == nil {
		return ""
	}
	return m.disabled(name)
}

// serveMCP speaks MCP's streamable-HTTP transport for one surface.
func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request, surface mcpSurface) {
	// A browser page must not be able to drive an agent's tools (DNS-rebinding
	// defence the transport asks servers to apply). Agent clients send no Origin.
	if r.Header.Get("Origin") != "" {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin MCP requests are refused"})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		s.writeMCP(w, nil, nil, &mcpError{Code: -32700, Message: "could not read the request"})
		return
	}
	var req mcpRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		s.writeMCP(w, nil, nil, &mcpError{Code: -32600, Message: "not a JSON-RPC 2.0 request (batches are not supported)"})
		return
	}
	// A notification (no id) gets 202 and no body.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := mirror.NegotiateMCPVersion(p.ProtocolVersion)
		s.writeMCP(w, req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "muster", "version": BuildVersion},
			"instructions":    surface.instructions,
		}, nil)
	case "ping":
		s.writeMCP(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		tools := newMCPCatalog(surface.tools(), !surface.noHelp).served
		if tools == nil {
			tools = []map[string]any{}
		}
		s.writeMCP(w, req.ID, map[string]any{"tools": tools}, nil)
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			s.writeMCP(w, req.ID, nil, &mcpError{Code: -32602, Message: "invalid tools/call params"})
			return
		}
		// Only a tool this surface LISTS may be called through it: a gateway-only
		// tool's name is as unknown here as a made-up one.
		catalog := newMCPCatalog(surface.tools(), !surface.noHelp)
		def, listed := catalog.lookup(p.Name)
		if !listed {
			// 🔴 HIDDEN, BUT EXPLAINED: a tool left out because its FEATURE is off
			// answers which feature and how to turn it on, as a tool error the
			// model reads — not "unknown tool", which reads as a typo and names
			// nothing to fix. Any other unlisted name is still unknown.
			if why := surface.explainUnlisted(p.Name); why != "" {
				s.writeMCP(w, req.ID, toolResult(toolErr("%s", why)), nil)
				return
			}
			s.writeMCP(w, req.ID, nil, &mcpError{Code: -32602, Message: "unknown tool " + p.Name})
			return
		}
		s.writeMCPToolResult(w, req.ID, def, func() string {
			if p.Name == mcpHelpTool {
				return catalog.help(string(p.Arguments), surface)
			}
			return surface.call(p.Name, string(p.Arguments))
		})
	default:
		s.writeMCP(w, req.ID, nil, &mcpError{Code: -32601, Message: "method not found: " + req.Method})
	}
}

// mcpHeartbeatEvery is how often a tool call still running writes a keep-alive
// byte. A var so a test can shorten it.
var mcpHeartbeatEvery = 15 * time.Second

// toolResult is a dispatched tool's text as an MCP tools/call result.
func toolResult(text string) map[string]any {
	result := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
	if isToolErr(text) {
		result["isError"] = true
	}
	return result
}

// writeMCPToolResult runs one tool call and writes its JSON-RPC answer.
//
// 🔴 A LONG CALL KEEPS THE CONNECTION ALIVE WITH WHITESPACE. attention_wait
// blocks for up to ten minutes, longer than the idle timeout of any proxy
// between an agent and muster (and connector_status for fifty seconds). So once
// a call has run for mcpHeartbeatEvery, the response's status and
// Content-Type are sent and a newline follows every mcpHeartbeatEvery until the
// answer, which is then written after them. Leading whitespace is legal JSON
// (RFC 8259 §2), so the body still parses as the one JSON-RPC response the
// transport promises; a call that finishes sooner is written exactly as before.
//
// def is the called tool's served definition. Whatever the call answers — the
// tool's own text, or "the tool failed" when it panicked — a refusal carries
// def under current_definition (withCurrentDefinition, mcp_help.go): the caller
// may be holding an older one.
func (s *Server) writeMCPToolResult(w http.ResponseWriter, id json.RawMessage, def map[string]any, run func() string) {
	done := make(chan string, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Printf("PANIC in MCP tool call: %v", rec)
				done <- withCurrentDefinition(toolErr("the tool failed"), def)
			}
		}()
		done <- withCurrentDefinition(run(), def)
	}()
	t := time.NewTicker(mcpHeartbeatEvery)
	defer t.Stop()
	rc := http.NewResponseController(w)
	started := false
	for {
		select {
		case text := <-done:
			if !started {
				s.writeMCP(w, id, toolResult(text), nil)
				return
			}
			if err := json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": toolResult(text)}); err != nil {
				s.logger.Printf("mcp: write a long call's answer: %v", err)
			}
			return
		case <-t.C:
			if !started {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				started = true
			}
			_, _ = w.Write([]byte("\n"))
			_ = rc.Flush()
		}
	}
}
