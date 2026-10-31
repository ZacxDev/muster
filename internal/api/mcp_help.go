package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/agents"
)

// The MCP doors' answer to a stale tool definition.
//
// Claude Code defers MCP tools: it loads only their names at start and fetches
// a tool's definition on demand, then saves that definition in the
// conversation and replays it on every resume. A later fetch does not replace
// it. So a long-lived session keeps a muster tool's definition from the release
// it first fetched it in, and after a muster release it can believe a tool
// lacks a field that was added since (the client does not enforce the stale
// schema: the call itself would have worked).
//
// Two things, both in serveMCP, so they hold on every door and for every tool:
//
//   - muster_help (mcpHelpTool) answers the current definition of any tool on
//     the door, or the door's list of tools;
//   - a refused call carries the called tool's current definition
//     (withCurrentDefinition), so the refusal itself says what changed.
//
// Every other tool's description, and the initialize instructions, are served
// exactly as the surface gives them.

// mcpHelpTool is the name of the doors' own help tool. It is the transport's,
// not the registry's: it is not in agentTools and the gateway does not offer it
// (a gateway turn gets its tool definitions afresh every turn).
const mcpHelpTool = "muster_help"

func mcpHelpToolDef() agents.ToolDef {
	return agents.ToolDef{
		Type: "function",
		Name: mcpHelpTool,
		Description: "muster's current tool definitions on this connection. With no tool: every tool here, by name and one-line summary. " +
			"With tool: that tool's full current definition (description and input schema), as tools/list serves it now. " +
			"Call it when a muster call is refused or a tool seems to lack something: the definition you hold may be from an older muster.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tool": strProp("A tool name. Leave it out to list the tools."),
			},
			"additionalProperties": false,
		},
	}
}

// mcpCurrentDefinitionMax bounds the definition a refused call carries. A
// larger one is replaced by a pointer to muster_help, so a refusal never grows
// past it by more than that pointer. It is a size guard, not a working limit:
// when this was written the largest definition either door serves was under
// 2 KB (attention_answer, about 1.4 KB as served).
const mcpCurrentDefinitionMax = 8 << 10

// mcpHelpSummaryMax bounds one tool's summary in muster_help's list, in runes.
const mcpHelpSummaryMax = 160

// mcpCatalog is one surface's tool set as it is served at one moment: the
// surface's tools plus muster_help.
type mcpCatalog struct {
	base   []agents.ToolDef // muster_help last
	served []map[string]any // what tools/list answers, in base's order
}

// newMCPCatalog builds the catalog of a surface offering tools. A surface
// offering none gets an empty catalog: no muster_help.
func newMCPCatalog(tools []agents.ToolDef) mcpCatalog {
	if len(tools) == 0 {
		return mcpCatalog{}
	}
	base := append(append([]agents.ToolDef(nil), tools...), mcpHelpToolDef())
	c := mcpCatalog{base: base}
	for _, d := range base {
		c.served = append(c.served, map[string]any{"name": d.Name, "description": d.Description, "inputSchema": d.Parameters})
	}
	return c
}

// lookup is tool name's served definition, if the surface offers it.
func (c mcpCatalog) lookup(name string) (map[string]any, bool) {
	for _, d := range c.served {
		if d["name"] == name {
			return d, true
		}
	}
	return nil, false
}

func (c mcpCatalog) names() []string {
	out := make([]string, 0, len(c.base))
	for _, d := range c.base {
		out = append(out, d.Name)
	}
	return out
}

// help is muster_help's answer to args.
func (c mcpCatalog) help(args string) string {
	var p struct {
		Tool string `json:"tool"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(args)), &p); err != nil {
		return toolErr("invalid arguments: %v", err)
	}
	if name := strings.TrimSpace(p.Tool); name != "" {
		d, ok := c.lookup(name)
		if !ok {
			return toolErr("unknown tool %q; the tools on this connection are: %s", name, strings.Join(c.names(), ", "))
		}
		return toolJSON(d)
	}
	type row struct {
		Name    string `json:"name"`
		Summary string `json:"summary"`
	}
	rows := make([]row, 0, len(c.base))
	for _, d := range c.base {
		rows = append(rows, row{d.Name, mcpSummary(d.Description)})
	}
	return toolJSON(map[string]any{"tools": rows,
		"note": "call " + mcpHelpTool + "{tool} for one tool's full current definition"})
}

// mcpSummary is a description's first sentence, at most mcpHelpSummaryMax runes.
func mcpSummary(desc string) string {
	s := strings.TrimSpace(desc)
	if i := strings.IndexAny(s, "\n"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, ". "); i >= 0 {
		s = s[:i+1]
	}
	if utf8.RuneCountInString(s) > mcpHelpSummaryMax {
		r := []rune(s)
		s = strings.TrimSpace(string(r[:mcpHelpSummaryMax-1])) + "…"
	}
	return s
}

// withCurrentDefinition is a tool's result with the tool's current definition
// added under current_definition when the result is a tool error (isToolErr),
// and the result unchanged otherwise. A definition over
// mcpCurrentDefinitionMax is replaced by a pointer to muster_help.
//
// The original error's bytes are kept: the definition is spliced in before the
// object's closing brace, which isToolErr has already shown is there.
func withCurrentDefinition(text string, def map[string]any) string {
	if def == nil || !isToolErr(text) {
		return text
	}
	trimmed := strings.TrimRight(text, " \t\r\n")
	if !strings.HasSuffix(trimmed, "}") {
		return text
	}
	b, err := json.Marshal(def)
	if err != nil || len(b) > mcpCurrentDefinitionMax {
		b, _ = json.Marshal(map[string]any{"name": def["name"],
			"note": fmt.Sprintf("the definition is over %d bytes; call %s{tool:%q} for it", mcpCurrentDefinitionMax, mcpHelpTool, def["name"])})
	}
	return trimmed[:len(trimmed)-1] + `,"current_definition":` + string(b) + "}"
}
