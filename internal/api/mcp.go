package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
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
// 🔴 THE TOOLS NEVER RETURN A SECRET. They are thin wrappers over the same
// agent JSON API (AgentConnectorList / AgentRequestConnector /
// AgentConnectorStatus) whose view type has no field for one; using a connector
// is the proxy's job, and the tool output says how.

// mcpProtocolVersions are the protocol revisions this server answers. A client
// asking for one gets it back; any other gets the newest.
var mcpProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

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

// MCPToolNames are the tools this server lists, in order.
var MCPToolNames = []string{"connectors_list", "connector_request", "connector_status"}

func mcpTools() []map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	return []map[string]any{
		{
			"name": "connectors_list",
			"description": "List the platform connectors muster holds (e.g. github) and this agent's access to each " +
				"(none, pending, approved, denied, revoked, expired). Credentials are never shown: an approved " +
				"connector is used through muster's proxy, as each entry's `usage` explains.",
			"inputSchema": objSchema(map[string]any{}),
		},
		{
			"name": "connector_request",
			"description": "Ask the operator for access to a connector by name (lowercase letters, digits, '-'). " +
				"If it does not exist yet, the operator is prompted to enter the credential. Returns the request's " +
				"status; poll connector_status until it is approved or denied. A denial is final for that request.",
			"inputSchema": objSchema(map[string]any{
				"name":   str("The connector name, e.g. github."),
				"reason": str("Why access is needed — shown to the operator."),
			}, "name"),
		},
		{
			"name": "connector_status",
			"description": "This agent's access to one connector, and how to use it once approved. With " +
				"wait_seconds (max 50) it waits for a pending request to be decided.",
			"inputSchema": objSchema(map[string]any{
				"name":         str("The connector name."),
				"wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 50, "description": "Wait this long for a pending request to be decided."},
			}, "name"),
		},
	}
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

// handleMCP serves POST /mcp.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	// A browser page must not be able to drive an agent's tools (DNS-rebinding
	// defence the transport asks servers to apply). Agent clients send no Origin.
	if r.Header.Get("Origin") != "" {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin MCP requests are refused"})
		return
	}
	a, _ := agentFromContext(r.Context())
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
		version := mcpProtocolVersions[0]
		for _, v := range mcpProtocolVersions {
			if v == p.ProtocolVersion {
				version = v
			}
		}
		s.writeMCP(w, req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "muster", "version": BuildVersion},
			"instructions": "muster's connector store. To reach an external platform (e.g. GitHub), call " +
				"connector_request for it, wait for the operator's approval with connector_status, then send HTTP " +
				"requests through $MUSTER_CONNECT_URL/connect/<connector>/<host>/<path> with header " +
				"`X-Muster-Token: $MUSTER_HOOK_TOKEN`. You never see the credential; muster adds it.",
		}, nil)
	case "ping":
		s.writeMCP(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		s.writeMCP(w, req.ID, map[string]any{"tools": mcpTools()}, nil)
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			s.writeMCP(w, req.ID, nil, &mcpError{Code: -32602, Message: "invalid tools/call params"})
			return
		}
		var args struct {
			Name        string `json:"name"`
			Reason      string `json:"reason"`
			WaitSeconds int    `json:"wait_seconds"`
		}
		_ = json.Unmarshal([]byte(orEmptyJSON(string(p.Arguments))), &args)
		var (
			out  any
			terr error
		)
		ctx := r.Context()
		switch p.Name {
		case "connectors_list":
			out, terr = s.AgentConnectorList(ctx, a)
		case "connector_request":
			out, _, terr = s.AgentRequestConnector(ctx, a, strings.TrimSpace(args.Name), args.Reason)
		case "connector_status":
			if err := connectors.ValidateName(strings.TrimSpace(args.Name)); err != nil {
				terr = err
				break
			}
			wait := args.WaitSeconds
			if wait < 0 {
				wait = 0
			}
			out, terr = s.AgentConnectorStatus(ctx, a, strings.TrimSpace(args.Name), time.Duration(wait)*time.Second)
		default:
			s.writeMCP(w, req.ID, nil, &mcpError{Code: -32602, Message: "unknown tool " + p.Name})
			return
		}
		if terr != nil {
			msg := "the tool failed"
			if errors.Is(terr, connectors.ErrInvalid) {
				msg = terr.Error()
			} else {
				s.logger.Printf("mcp: %s for agent %d: %v", p.Name, a.ID, terr)
			}
			s.writeMCP(w, req.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": toolErr("%s", msg)}}, "isError": true}, nil)
			return
		}
		s.writeMCP(w, req.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": toolJSON(out)}}}, nil)
	default:
		s.writeMCP(w, req.ID, nil, &mcpError{Code: -32601, Message: "method not found: " + req.Method})
	}
}
