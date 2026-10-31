package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// muster_help, the rev stamp and a refusal's current_definition (mcp_help.go),
// on both MCP doors.

var revStampRE = regexp.MustCompile(` \[muster tools rev ([0-9a-f]{8})\]$`)

// mcpRevInstructionWant is the instruction sentence, pinned whole: it is the
// one thing a model holding a stale definition is told.
func mcpRevInstructionWant(rev string) string {
	return "muster's tool definitions change between releases, and this connection's tools are at rev " + rev +
		" (each muster MCP endpoint has its own rev): if a muster tool definition you hold carries a different rev, " +
		"or a muster call is refused, call muster_help for the current one."
}

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

// toolsRev is the one rev every served description carries; the test fails if
// any description has no stamp or a different one.
func toolsRev(t *testing.T, tools []map[string]any) string {
	t.Helper()
	if len(tools) == 0 {
		t.Fatal("positive control FAILED: no tools served")
	}
	rev := ""
	for _, tl := range tools {
		desc, _ := tl["description"].(string)
		m := revStampRE.FindStringSubmatch(desc)
		if m == nil {
			t.Fatalf("%v's description carries no rev stamp: %q", tl["name"], desc)
		}
		if rev != "" && m[1] != rev {
			t.Fatalf("%v carries rev %s, another tool %s", tl["name"], m[1], rev)
		}
		rev = m[1]
	}
	return rev
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
	if names[len(names)-1] != "muster_help" {
		t.Fatalf("muster_help is not offered last: %v", names)
	}
	rev := toolsRev(t, tools)

	// No tool: every tool on THIS door, by name and summary.
	text, isErr := toolText(t, rpc(mcpToolCall("muster_help", map[string]any{})))
	var list struct {
		Rev   string `json:"rev"`
		Tools []struct{ Name, Summary string }
	}
	if isErr || json.Unmarshal([]byte(text), &list) != nil {
		t.Fatalf("muster_help{}: %s", text)
	}
	var listed []string
	for _, tl := range list.Tools {
		listed = append(listed, tl.Name)
		if tl.Summary == "" || len([]rune(tl.Summary)) > mcpHelpSummaryMax || strings.Contains(tl.Summary, "muster tools rev") {
			t.Errorf("%s's summary %q", tl.Name, tl.Summary)
		}
	}
	if !reflect.DeepEqual(listed, names) || list.Rev != rev {
		t.Fatalf("muster_help{} lists %v at rev %s; tools/list serves %v at rev %s", listed, list.Rev, names, rev)
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

// TestTheAgentDoorOffersMusterHelpAndStampsItsTools: POST /mcp.
func TestTheAgentDoorOffersMusterHelpAndStampsItsTools(t *testing.T) {
	f := newConnFixture(t)
	tok := "help-agent-token-0123456789"
	f.seedAgent(t, "help-agent", tok)
	rpc := func(body string) *mcpReply { r, _ := f.mcp(t, tok, body, nil); return r }

	checkHelp(t, rpc)

	tools := servedTools(t, rpc(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	rev := toolsRev(t, tools)
	// Stable across calls.
	if again := toolsRev(t, servedTools(t, rpc(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))); again != rev {
		t.Fatalf("rev moved between two tools/list calls: %s then %s", rev, again)
	}
	// The same rev in the instructions, in the instruction sentence.
	in := instructionsOf(t, rpc(`{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`))
	if !strings.HasSuffix(in, " "+mcpRevInstructionWant(rev)) {
		t.Fatalf("the agent door's instructions do not end with the rev sentence for %s:\n%s", rev, in)
	}

	// A refused call carries the definition; a successful one does not.
	checkRefusalCarriesDefinition(t, tools, "connector_status",
		rpc(mcpToolCall("connector_status", map[string]any{"name": "Bad Name"})))
	text, isErr := toolText(t, rpc(mcpToolCall("connectors_list", map[string]any{})))
	if isErr || strings.Contains(text, "current_definition") {
		t.Fatalf("a successful call: %v %s", isErr, text)
	}
}

// TestTheMirrorDoorOffersMusterHelpAndStampsItsTools: POST /mirror/mcp, with
// its own rev.
func TestTheMirrorDoorOffersMusterHelpAndStampsItsTools(t *testing.T) {
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
	rev := toolsRev(t, tools)
	if again := toolsRev(t, servedTools(t, rpc(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))); again != rev {
		t.Fatalf("rev moved between two tools/list calls: %s then %s", rev, again)
	}
	in := instructionsOf(t, rpc(`{"jsonrpc":"2.0","id":3,"method":"initialize","params":{}}`))
	if !strings.HasSuffix(in, " "+mcpRevInstructionWant(rev)) {
		t.Fatalf("the mirror door's instructions do not end with the rev sentence for %s:\n%s", rev, in)
	}

	checkRefusalCarriesDefinition(t, tools, attentionWaitTool,
		rpc(mcpToolCall(attentionWaitTool, map[string]any{"id": 987654, "wait_seconds": 0})))
	text, isErr := toolText(t, rpc(mcpToolCall("tasks_list", map[string]any{})))
	if isErr || strings.Contains(text, "current_definition") {
		t.Fatalf("a successful call: %v %s", isErr, text)
	}
	// muster_help answers before the session is bound: a host lists no
	// window for this session, and help still works.
	_, r, raw := f.mirrorRPC(t, mirrorAs(ws, "9f9f9f9f-0000-4000-8000-000000000000"), mcpToolCall("muster_help", map[string]any{}))
	if r == nil {
		t.Fatal(raw)
	}
	if text, isErr := toolText(t, r); isErr || !strings.Contains(text, `"rev":"`+rev+`"`) {
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

// TestTheRevFollowsTheToolSetAndNotItsOwnStamp: the rev changes when a schema
// or a description changes and not otherwise, two surfaces with different
// tools get different revs, and the rev is the hash of the UNSTAMPED
// definitions.
func TestTheRevFollowsTheToolSetAndNotItsOwnStamp(t *testing.T) {
	never := func(string, string) string { return `{}` }
	revOf := func(d agents.ToolDef) string {
		return toolsRev(t, servedTools(t, serveSurface(t, testSurface([]agents.ToolDef{d}, never), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	}
	v1 := echoDef("Echo a session.", map[string]any{"session": strProp("s")})
	r1 := revOf(v1)
	if again := revOf(echoDef("Echo a session.", map[string]any{"session": strProp("s")})); again != r1 {
		t.Fatalf("the same tool set hashed %s then %s", r1, again)
	}
	v2 := echoDef("Echo a session.", map[string]any{"session": strProp("s"), "task_id": map[string]any{"type": "integer"}})
	if r2 := revOf(v2); r2 == r1 {
		t.Fatalf("adding task_id to the schema left the rev at %s", r1)
	}
	if r3 := revOf(echoDef("Echo a session, or a task.", map[string]any{"session": strProp("s")})); r3 == r1 {
		t.Fatalf("changing the description left the rev at %s", r1)
	}
	renamed := v1
	renamed.Name = "echo2"
	if r4 := revOf(renamed); r4 == r1 {
		t.Fatalf("renaming the tool left the rev at %s", r1)
	}

	// Not fed by its own stamp: the rev is the hash of what is served with the
	// stamps taken off.
	tools := servedTools(t, serveSurface(t, testSurface([]agents.ToolDef{v1, v2}, never), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	rev := toolsRev(t, tools)
	var unstamped []agents.ToolDef
	for _, tl := range tools {
		desc := revStampRE.ReplaceAllString(tl["description"].(string), "")
		params, _ := tl["inputSchema"].(map[string]any)
		unstamped = append(unstamped, agents.ToolDef{Name: tl["name"].(string), Description: desc, Parameters: params})
	}
	if got := mcpToolsRev(unstamped); got != rev {
		t.Fatalf("the served rev %s is not the hash of the unstamped definitions (%s)", rev, got)
	}

	// Instructions carry the same rev; a surface with no tools gets no line
	// and no muster_help.
	in := instructionsOf(t, serveSurface(t, testSurface([]agents.ToolDef{v1}, never), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if in != "base instructions. "+mcpRevInstructionWant(r1) {
		t.Fatalf("instructions: %q", in)
	}
	empty := testSurface(nil, never)
	if got := servedTools(t, serveSurface(t, empty, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)); len(got) != 0 {
		t.Fatalf("a surface with no tools serves %v", got)
	}
	if in := instructionsOf(t, serveSurface(t, empty, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)); in != "base instructions." {
		t.Fatalf("a surface with no tools: instructions %q", in)
	}
	if r := serveSurface(t, empty, mcpToolCall("muster_help", map[string]any{})); r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("muster_help on a surface with no tools: %+v", r)
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

	big := echoDef(strings.Repeat("x", mcpCurrentDefinitionMax), map[string]any{})
	out := serveSurface(t, testSurface([]agents.ToolDef{big}, func(string, string) string { return toolErr("no") }), mcpToolCall("echo", map[string]any{}))
	text, isErr := toolTextRaw(t, out)
	if !isErr || len(text) > 512 || !strings.Contains(text, `call muster_help{tool:\"echo\"}`) {
		t.Fatalf("an oversize definition: %v %d bytes %.300s", isErr, len(text), text)
	}
}
