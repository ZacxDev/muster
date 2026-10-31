package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// muster_help and a refusal's current_definition (mcp_help.go), on both MCP
// doors — and nothing else about what the doors serve changes.

// servedTools decodes a tools/list result.
func servedTools(t *testing.T, r *mcpReply) []map[string]any {
	t.Helper()
	if r == nil || r.Error != nil {
		t.Fatalf("tools/list: %+v", r)
	}
	var res struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	return res.Tools
}

func instructionsOf(t *testing.T, r *mcpReply) string {
	t.Helper()
	var res struct {
		Instructions string `json:"instructions"`
	}
	if r == nil || r.Error != nil || json.Unmarshal(r.Result, &res) != nil {
		t.Fatalf("initialize: %+v", r)
	}
	return res.Instructions
}

func toolNamesOf(tools []map[string]any) []string {
	var out []string
	for _, tl := range tools {
		n, _ := tl["name"].(string)
		out = append(out, n)
	}
	return out
}

// checkHelp drives muster_help on one door (rpc posts one JSON-RPC body) and
// checks it against that door's tools/list.
func checkHelp(t *testing.T, rpc func(body string) *mcpReply) {
	t.Helper()
	tools := servedTools(t, rpc(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	names := toolNamesOf(tools)
	if len(names) < 2 || names[len(names)-1] != "muster_help" {
		t.Fatalf("muster_help is not offered last: %v", names)
	}

	// No tool: every tool on THIS door, by name and summary.
	text, isErr := toolText(t, rpc(mcpToolCall("muster_help", map[string]any{})))
	var list struct {
		Tools []struct{ Name, Summary string }
	}
	if isErr || json.Unmarshal([]byte(text), &list) != nil {
		t.Fatalf("muster_help{}: %s", text)
	}
	var listed []string
	for _, tl := range list.Tools {
		listed = append(listed, tl.Name)
		if tl.Summary == "" || len([]rune(tl.Summary)) > mcpHelpSummaryMax {
			t.Errorf("%s's summary %q", tl.Name, tl.Summary)
		}
	}
	if !reflect.DeepEqual(listed, names) {
		t.Fatalf("muster_help{} lists %v; tools/list serves %v", listed, names)
	}

	// A name: exactly what tools/list serves for it.
	for _, want := range tools {
		text, isErr := toolText(t, rpc(mcpToolCall("muster_help", map[string]any{"tool": want["name"]})))
		var got map[string]any
		if isErr || json.Unmarshal([]byte(text), &got) != nil {
			t.Fatalf("muster_help{%v}: %s", want["name"], text)
		}
		if a, b := normJSON(t, got), normJSON(t, want); a != b {
			t.Errorf("muster_help{%v} =\n  %s\ntools/list serves\n  %s", want["name"], a, b)
		}
	}

	// An unknown name: an error naming every valid one.
	text, isErr = toolText(t, rpc(mcpToolCall("muster_help", map[string]any{"tool": "route_to_nowhere"})))
	if !isErr || !strings.Contains(text, "unknown tool \\\"route_to_nowhere\\\"") {
		t.Fatalf("muster_help{unknown}: %v %s", isErr, text)
	}
	if !strings.Contains(text, strings.Join(names, ", ")) {
		t.Fatalf("the unknown-name error does not list the valid names %v: %s", names, text)
	}
}

// checkRefusalCarriesDefinition: a refused call's text carries the called
// tool's served definition under current_definition, and stays an error.
func checkRefusalCarriesDefinition(t *testing.T, tools []map[string]any, name string, r *mcpReply) {
	t.Helper()
	text, isErr := toolTextRaw(t, r)
	if !isErr {
		t.Fatalf("%s: positive control FAILED, the call was not refused: %s", name, text)
	}
	var got struct {
		Error             string         `json:"error"`
		CurrentDefinition map[string]any `json:"current_definition"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil || got.Error == "" {
		t.Fatalf("%s: refusal %s", name, text)
	}
	var want map[string]any
	for _, tl := range tools {
		if tl["name"] == name {
			want = tl
		}
	}
	if want == nil {
		t.Fatalf("%s is not served", name)
	}
	if a, b := normJSON(t, got.CurrentDefinition), normJSON(t, want); a != b {
		t.Fatalf("%s's refusal carries\n  %s\nbut tools/list serves\n  %s", name, a, b)
	}
}

// checkServedUnchanged: every tool a door serves except muster_help is, byte
// for byte (name, description, input schema) and in order, the surface's own
// definition, and initialize answers exactly wantInstructions. muster_help
// adds a tool; it changes no other tool and no instruction.
func checkServedUnchanged(t *testing.T, rpc func(body string) *mcpReply, defs []agents.ToolDef, wantInstructions string) {
	t.Helper()
	tools := servedTools(t, rpc(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	// Positive control: there is something to compare.
	if len(defs) == 0 || len(tools) != len(defs)+1 {
		t.Fatalf("positive control: %d tools served for %d surface definitions", len(tools), len(defs))
	}
	for i, d := range defs {
		tl := tools[i]
		if tl["name"] != d.Name || tl["description"] != d.Description {
			t.Errorf("tool %d: served %v %q, the surface's is %s %q", i, tl["name"], tl["description"], d.Name, d.Description)
		}
		if a, b := normJSON(t, tl), normJSON(t, map[string]any{"name": d.Name, "description": d.Description, "inputSchema": d.Parameters}); a != b {
			t.Errorf("%s served\n  %s\nthe surface's\n  %s", d.Name, a, b)
		}
	}
	if got := instructionsOf(t, rpc(`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)); got != wantInstructions {
		t.Errorf("initialize instructions:\n  %q\nwant exactly\n  %q", got, wantInstructions)
	}
}

// TestTheDoorsServeTheirToolsAndInstructionsUnchanged: on POST /mcp (the
// chief and a worker) and POST /mirror/mcp, descriptions and instructions are
// exactly what the surface defines — no stamp, no added sentence.
func TestTheDoorsServeTheirToolsAndInstructionsUnchanged(t *testing.T) {
	g := newGatedFixture(t)
	for _, c := range []struct {
		name  string
		agent agents.Agent
		token string
	}{{"chief", g.chief, g.chiefTok}, {"worker", g.worker, g.workerTok}} {
		t.Run("agent door "+c.name, func(t *testing.T) {
			rpc := func(body string) *mcpReply { r, _ := g.mcp(t, c.token, body, nil); return r }
			checkServedUnchanged(t, rpc, g.srv.MCPToolDefsFor(g.asOwner, c.agent), g.srv.mcpInstructionsFor(g.asOwner, c.agent))
		})
	}
	t.Run("mirror door", func(t *testing.T) {
		f, ws, _, _ := newMirrorMCPFixture(t)
		rpc := func(body string) *mcpReply {
			_, r, raw := f.mirrorRPC(t, mirrorAs(ws, focusSID), body)
			if r == nil {
				t.Fatalf("/mirror/mcp: %s", raw)
			}
			return r
		}
		checkServedUnchanged(t, rpc, f.srv.mirrorTools(), "muster's tools for a mirrored Claude Code session. "+mirrorAttentionInstruction)
	})
}

// TestTheAgentDoorOffersMusterHelp: POST /mcp.
func TestTheAgentDoorOffersMusterHelp(t *testing.T) {
	f := newConnFixture(t)
	tok := "help-agent-token-0123456789"
	f.seedAgent(t, "help-agent", tok)
	rpc := func(body string) *mcpReply { r, _ := f.mcp(t, tok, body, nil); return r }

	checkHelp(t, rpc)

	// A refused call carries the definition; a successful one does not.
	tools := servedTools(t, rpc(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	checkRefusalCarriesDefinition(t, tools, "connector_status",
		rpc(mcpToolCall("connector_status", map[string]any{"name": "Bad Name"})))
	text, isErr := toolTextRaw(t, rpc(mcpToolCall("connectors_list", map[string]any{})))
	if isErr || strings.Contains(text, "current_definition") {
		t.Fatalf("a successful call: %v %s", isErr, text)
	}
}

// TestMusterHelpAnswersOnlyTheCallingAgentsTools: on POST /mcp muster_help
// sees exactly what that agent is offered — the ledgered sets — and a tool the
// agent is not offered is an unknown name to it, for the chief (the
// not-chief tools) and for a worker (the chief-only tools).
func TestMusterHelpAnswersOnlyTheCallingAgentsTools(t *testing.T) {
	g := newGatedFixture(t)
	for _, c := range []struct {
		name     string
		token    string
		want     []string
		hiddenTo []string
	}{
		{"chief", g.chiefTok, mcpLedger, notChiefToolList},
		{"worker", g.workerTok, workerMCPLedger, chiefOnlyLedger},
	} {
		t.Run(c.name, func(t *testing.T) {
			rpc := func(body string) *mcpReply { r, _ := g.mcp(t, c.token, body, nil); return r }
			checkHelp(t, rpc)
			text, isErr := toolText(t, rpc(mcpToolCall("muster_help", map[string]any{})))
			var list struct{ Tools []struct{ Name string } }
			if isErr || json.Unmarshal([]byte(text), &list) != nil {
				t.Fatalf("muster_help{}: %s", text)
			}
			var got []string
			for _, tl := range list.Tools {
				got = append(got, tl.Name)
			}
			if want := append(append([]string(nil), c.want...), "muster_help"); !reflect.DeepEqual(got, want) {
				t.Fatalf("muster_help{} lists %v, want exactly %v", got, want)
			}
			// Positive control: there is a tool this agent must not see.
			if len(c.hiddenTo) == 0 {
				t.Fatal("positive control: no tool is hidden from this agent")
			}
			for _, n := range c.hiddenTo {
				text, isErr := toolText(t, rpc(mcpToolCall("muster_help", map[string]any{"tool": n})))
				if !isErr || !strings.Contains(text, "unknown tool \\\""+n+"\\\"") {
					t.Errorf("muster_help{tool:%s} for the %s: %v %s", n, c.name, isErr, text)
				}
			}
		})
	}
}

// TestTheMirrorDoorOffersMusterHelp: POST /mirror/mcp.
func TestTheMirrorDoorOffersMusterHelp(t *testing.T) {
	f, ws, _, _ := newMirrorMCPFixture(t)
	rpc := func(body string) *mcpReply {
		_, r, raw := f.mirrorRPC(t, mirrorAs(ws, focusSID), body)
		if r == nil {
			t.Fatalf("/mirror/mcp: %s", raw)
		}
		return r
	}
	checkHelp(t, rpc)

	tools := servedTools(t, rpc(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	checkRefusalCarriesDefinition(t, tools, attentionWaitTool,
		rpc(mcpToolCall(attentionWaitTool, map[string]any{"id": 987654, "wait_seconds": 0})))
	text, isErr := toolTextRaw(t, rpc(mcpToolCall("tasks_list", map[string]any{})))
	if isErr || strings.Contains(text, "current_definition") {
		t.Fatalf("a successful call: %v %s", isErr, text)
	}
	// muster_help answers before the session is bound: the host lists no
	// window for this session, and help still works.
	_, r, raw := f.mirrorRPC(t, mirrorAs(ws, "9f9f9f9f-0000-4000-8000-000000000000"), mcpToolCall("muster_help", map[string]any{}))
	if r == nil {
		t.Fatal(raw)
	}
	if text, isErr := toolText(t, r); isErr || !strings.Contains(text, `"name":"tasks_list"`) {
		t.Fatalf("muster_help from an unbound session: %s", text)
	}
}

// serveSurface posts one JSON-RPC body to serveMCP over surface.
func serveSurface(t *testing.T, surface mcpSurface, body string) *mcpReply {
	t.Helper()
	s := New(nil, AuthConfig{HookToken: connTestHookToken, UIPassword: connTestPassword}, log.New(io.Discard, "", 0))
	w := httptest.NewRecorder()
	s.serveMCP(w, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)), surface)
	var r mcpReply
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("not JSON-RPC: %s", w.Body)
	}
	return &r
}

func testSurface(defs []agents.ToolDef, call func(name, args string) string) mcpSurface {
	return mcpSurface{tools: func() []agents.ToolDef { return defs }, call: call, instructions: "base instructions."}
}

func echoDef(desc string, props map[string]any) agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: "echo", Description: desc,
		Parameters: map[string]any{"type": "object", "properties": props}}
}

// TestASurfaceWithNoToolsGetsNoMusterHelp: muster_help is offered only where
// some tool is.
func TestASurfaceWithNoToolsGetsNoMusterHelp(t *testing.T) {
	empty := testSurface(nil, func(string, string) string { return `{}` })
	if got := servedTools(t, serveSurface(t, empty, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)); len(got) != 0 {
		t.Fatalf("a surface with no tools serves %v", got)
	}
	if r := serveSurface(t, empty, mcpToolCall("muster_help", map[string]any{})); r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("muster_help on a surface with no tools: %+v", r)
	}
	// Control: with one tool, it is offered.
	one := testSurface([]agents.ToolDef{echoDef("Echo.", map[string]any{})}, func(string, string) string { return `{}` })
	if got := toolNamesOf(servedTools(t, serveSurface(t, one, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))); !reflect.DeepEqual(got, []string{"echo", "muster_help"}) {
		t.Fatalf("a surface with one tool serves %v", got)
	}
}

// TestARefusalCarriesABoundedDefinitionAndStaysAnError: every structured
// refusal shape isToolErr allows still reads as an error with
// current_definition added, a success is untouched, and an oversize
// definition is replaced by a pointer to muster_help.
func TestARefusalCarriesABoundedDefinitionAndStaysAnError(t *testing.T) {
	for _, refusal := range []string{
		toolErr("no"),
		`{"error":"no","refused":"x"}`,
		`{"error":"no","budget":{"left":0}}`,
		`{"error":"no","candidates":["a"]}`,
	} {
		out := serveSurface(t, testSurface([]agents.ToolDef{echoDef("Echo.", map[string]any{})}, func(string, string) string { return refusal }),
			mcpToolCall("echo", map[string]any{}))
		text, isErr := toolTextRaw(t, out)
		if !isErr || !strings.HasPrefix(text, strings.TrimSuffix(refusal, "}")+`,"current_definition":{`) {
			t.Errorf("refusal %s became %v %s", refusal, isErr, text)
		}
	}
	// isToolErr's allowed keys: current_definition is one, nothing else new.
	if !isToolErr(`{"error":"x","current_definition":{"name":"y"}}`) {
		t.Error("isToolErr refuses current_definition")
	}
	if isToolErr(`{"error":"x","current_definitions":{}}`) || isToolErr(`{"current_definition":{}}`) {
		t.Error("isToolErr admits a key it should not")
	}

	ok := serveSurface(t, testSurface([]agents.ToolDef{echoDef("Echo.", map[string]any{})}, func(string, string) string { return `{"ok":true}` }),
		mcpToolCall("echo", map[string]any{}))
	if text, isErr := toolText(t, ok); isErr || text != `{"ok":true}` {
		t.Fatalf("a success was changed: %v %s", isErr, text)
	}

	// A tool that panics answers "the tool failed", and that refusal carries
	// the definition too.
	crash := serveSurface(t, testSurface([]agents.ToolDef{echoDef("Echo.", map[string]any{})}, func(string, string) string { panic("boom") }),
		mcpToolCall("echo", map[string]any{}))
	if text, isErr := toolTextRaw(t, crash); !isErr || !strings.HasPrefix(text, `{"error":"the tool failed","current_definition":{"description":"Echo.",`) {
		t.Errorf("a panicking tool: %v %s", isErr, text)
	}

	big := echoDef(strings.Repeat("x", mcpCurrentDefinitionMax), map[string]any{})
	out := serveSurface(t, testSurface([]agents.ToolDef{big}, func(string, string) string { return toolErr("no") }), mcpToolCall("echo", map[string]any{}))
	text, isErr := toolTextRaw(t, out)
	if !isErr || len(text) > 512 || !strings.Contains(text, `call muster_help{tool:\"echo\"}`) {
		t.Fatalf("an oversize definition: %v %d bytes %.300s", isErr, len(text), text)
	}
}
