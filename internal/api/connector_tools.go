package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/connectors"
)

// The connector store's agent tools. They are entries in the ONE agent tool
// registry (agentTools, agent.go) with the MCP flag set, so the gateway kind gets
// them on its native tool path and claude-code agents get the same definitions
// over POST /mcp (mcp.go). They execute in dispatchAgentTool like every other
// tool.
//
// 🔴 THE TOOLS NEVER RETURN A SECRET. They are thin wrappers over the agent JSON
// API (AgentConnectorList / AgentRequestConnector / AgentConnectorStatus), whose
// view type has no field for one; using a connector is the proxy's job, and the
// tool output says how.

// connectorToolDefs declares the connector tools, in registry order:
// connectors_list, connector_request, connector_status and postgres_query.
func connectorToolDefs() []agents.ToolDef {
	fn := func(name, desc string, params map[string]any) agents.ToolDef {
		return agents.ToolDef{Type: "function", Name: name, Description: desc, Parameters: params}
	}
	return []agents.ToolDef{
		fn("connectors_list", "List the platform connectors muster holds (e.g. github) and this agent's access to each "+
			"(none, pending, approved, denied, revoked). Credentials are never shown: an approved connector is used "+
			"through muster's proxy, as each entry's `usage` explains.", objSchema(map[string]any{})),
		fn("connector_request", "Ask the operator for access to a connector by name (lowercase letters, digits, '-'). "+
			"If it does not exist yet, the operator is prompted to enter the credential. Returns the request's status; "+
			"poll connector_status until it is approved or denied. A denial is final for that request.",
			objSchema(map[string]any{
				"name":   strProp("The connector name, e.g. github."),
				"reason": strProp("Why access is needed — shown to the operator."),
			}, "name")),
		fn("connector_status", "This agent's access to one connector, and how to use it once approved. With "+
			"wait_seconds (max 50) it waits for a pending request to be decided.",
			objSchema(map[string]any{
				"name": strProp("The connector name."),
				"wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 50,
					"description": "Wait this long for a pending request to be decided."},
			}, "name")),
		fn(postgresQueryToolName, "Run ONE read-only SQL statement against a postgres connector this agent has an approved "+
			"grant for (ask with connector_request first). muster connects to the database itself — you never see the "+
			"credential — and runs the statement in its own READ ONLY transaction with a statement timeout and a row and "+
			"byte cap. Returns {columns:[{name,type}], rows:[[text|null,…]], row_count, truncated, truncated_by}. Pass "+
			"values as params ($1, $2, …), never spliced into the SQL. Multiple statements, writes and transaction "+
			"control are refused.",
			objSchema(map[string]any{
				"connector": strProp("The postgres connector's name."),
				"sql":       strProp("One SQL statement (SELECT, SHOW, EXPLAIN, …), with $1, $2 … placeholders."),
				"params": map[string]any{"type": "array", "maxItems": connectors.PGMaxParams,
					"description": "Positional parameter values for $1, $2, … (strings, numbers, booleans or null).",
					"items":       map[string]any{}},
			}, "connector", "sql")),
	}
}

// postgresQueryToolName is the postgres connector's query tool
// (officers.ToolPostgresQuery names the same tool for the officer kickoff).
const postgresQueryToolName = "postgres_query"

// toolPostgresQuery runs one read-only statement for agent a.
//
// 🔴 THE GRANT CHECK IS THE PROXY'S OWN (Proxy.CheckAccess): the same grant
// row, the same reconciliation, and the same implicit pending request on first
// use as /connect/ — so an agent reaches a database only after its owner
// approved THAT agent for THAT connector. An officer instance is refused first
// for any connector its officer does not declare (officerConnectorRefusal).
// TestAnUngrantedAgentsPostgresQueryIsRefused pins it.
func (s *Server) toolPostgresQuery(ctx context.Context, a agents.Agent, arguments string) string {
	if s.ext.Connectors == nil || s.connectProxy == nil || s.pgExec == nil {
		return toolErr("%s", ConnectorsUnconfigured)
	}
	var in struct {
		Connector string            `json:"connector"`
		SQL       string            `json:"sql"`
		Params    []json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil {
		return toolErr("arguments must be {connector, sql, params?}: %v", err)
	}
	name := strings.TrimSpace(in.Connector)
	if err := connectors.ValidateName(name); err != nil {
		return s.connectorToolResult(postgresQueryToolName, a, nil, err)
	}
	if err := s.officerConnectorRefusal(a, name); err != nil {
		return toolErr("%s", err.Error())
	}
	caller := s.callerOf(a)
	acc, err := s.connectProxy.CheckAccess(ctx, caller, name, "")
	switch {
	case errors.Is(err, connectors.ErrRoleUncertain):
		return toolErr("%s", err.Error())
	case err != nil:
		return s.connectorToolResult(postgresQueryToolName, a, nil, err)
	case !acc.Usable:
		return toolErr("this agent has no approved grant for connector %s (grant: %s): ask with connector_request{name: %q, "+
			"reason}, then wait with connector_status until it is approved", name, acc.Grant.Status, name)
	}
	res, err := s.pgExec.Query(ctx, caller, name, connectors.PGQuery{SQL: in.SQL, Params: in.Params})
	var pgErr *connectors.PGError
	switch {
	case err == nil:
		return toolJSON(res)
	case errors.As(err, &pgErr):
		// toolErr's {error} shape, so MCP flags it isError; the class is in the text.
		return toolErr("%s [%s]", pgErr.Message, pgErr.Class)
	case errors.Is(err, connectors.ErrInvalid), errors.Is(err, connectors.ErrRateLimited):
		return toolErr("%s", err.Error())
	case errors.Is(err, connectors.ErrNotFound):
		return toolErr("connector %s does not exist", name)
	case errors.Is(err, connectors.ErrDecrypt):
		return toolErr("the connector's credential could not be decrypted; the operator must delete and re-create it")
	}
	s.logger.Printf("connectors: tool %s for agent %d: %s", postgresQueryToolName, a.ID, connectors.RedactPG(err.Error()))
	return toolErr("the tool failed")
}

// connectorToolResult encodes a connector tool's outcome. A validation refusal
// or the per-agent pending cap is shown to the agent verbatim (it says what to
// do next); anything else is logged and summarised, so a
// database error never reaches a model.
func (s *Server) connectorToolResult(tool string, a agents.Agent, out any, err error) string {
	if err == nil {
		return toolJSON(out)
	}
	if errors.Is(err, connectors.ErrInvalid) || errors.Is(err, connectors.ErrTooManyPending) {
		return toolErr("%s", err.Error())
	}
	s.logger.Printf("connectors: tool %s for agent %d: %v", tool, a.ID, err)
	return toolErr("the tool failed")
}

func (s *Server) toolConnectorsList(ctx context.Context, a agents.Agent) string {
	if s.ext.Connectors == nil {
		return toolErr("%s", ConnectorsUnconfigured)
	}
	out, err := s.AgentConnectorList(ctx, a)
	return s.connectorToolResult("connectors_list", a, out, err)
}

func (s *Server) toolConnectorRequest(ctx context.Context, a agents.Agent, name, reason string) string {
	if s.ext.Connectors == nil {
		return toolErr("%s", ConnectorsUnconfigured)
	}
	out, _, err := s.AgentRequestConnector(ctx, a, strings.TrimSpace(name), reason)
	return s.connectorToolResult("connector_request", a, out, err)
}

func (s *Server) toolConnectorStatus(ctx context.Context, a agents.Agent, name string, waitSeconds int) string {
	if s.ext.Connectors == nil {
		return toolErr("%s", ConnectorsUnconfigured)
	}
	name = strings.TrimSpace(name)
	if err := connectors.ValidateName(name); err != nil {
		return s.connectorToolResult("connector_status", a, nil, err)
	}
	if waitSeconds < 0 {
		waitSeconds = 0
	}
	out, err := s.AgentConnectorStatus(ctx, a, name, time.Duration(waitSeconds)*time.Second)
	return s.connectorToolResult("connector_status", a, out, err)
}
