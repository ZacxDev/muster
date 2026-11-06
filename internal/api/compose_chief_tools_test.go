package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
)

// composeChiefToolLedger is the Compose chief's whole /mcp tool list with every
// gate open (everyExtensionOn: session routing on), sorted. A literal on
// purpose, typed independently of agentspec.ComposeChiefAllowedMCPTools: the
// test below holds the served list, the spec's allow list and this literal
// equal, so a tool joining or leaving any one of them is red.
var composeChiefToolLedger = []string{"attention_list", "fleet_list", "fleet_report", "focus_search",
	"route_to_session", "sessions_overview", "task_get", "tasks_list"}

// mcpAs drives POST /mcp as agent a on s, returning the JSON-RPC reply.
func mcpAs(t *testing.T, s *Server, a agents.Agent, body string) mcpReply {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), agentCtxKey{}, a))
	w := httptest.NewRecorder()
	s.handleMCP(w, r)
	var out mcpReply
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("not JSON-RPC (%d): %s", w.Code, w.Body)
	}
	return out
}

// TestTheComposeChiefIsOfferedExactlyItsAllowedTools: on the Docker Compose
// install every permission prompt in the chief's container is denied, so the
// chief must be offered exactly the tools its spec pre-allows — no more (a
// tool it would be told of and could never run: the bug, where it offered to
// comment on tasks and change their status) and no fewer. Pinned at every
// surface the chief reads: the registry listing, POST /mcp's tools/list (no
// muster_help, which is not pre-allowed), the initialize instructions, and a
// call by name. Controls: a worker on the same server keeps its tools, and the
// Kubernetes chief (no ComposeChief) keeps its whole list (chiefMCPToolList).
func TestTheComposeChiefIsOfferedExactlyItsAllowedTools(t *testing.T) {
	s, chief, worker := chiefAndWorkerServer(t)
	ctx := context.Background()

	// Control: the Kubernetes chief's list is unchanged.
	if got := toolNames(s.MCPToolDefsFor(ctx, chief)); !reflect.DeepEqual(got, chiefMCPToolList) {
		t.Fatalf("control: the Kubernetes chief's /mcp tools = %v, want %v", got, chiefMCPToolList)
	}
	k8sWorker := toolNames(s.MCPToolDefsFor(ctx, worker))
	// Control, every other surface: the Kubernetes chief still gets the
	// connector store's instructions and muster_help, and a call to
	// task_comment reaches the tool (whatever it answers, not the Compose refusal).
	if in := s.mcpInstructionsFor(ctx, chief); in == mcpComposeChiefInstructions || !strings.Contains(in, "connectors_list") {
		t.Errorf("control: the Kubernetes chief's initialize instructions = %q", in)
	}
	if r := mcpAs(t, s, chief, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); !strings.Contains(string(r.Result), `"name":"`+mcpHelpTool+`"`) {
		t.Errorf("control: the Kubernetes chief's tools/list has no %s: %s", mcpHelpTool, r.Result)
	}
	if got := s.dispatchAgentTool(ctx, chief, "task_comment", "{}"); strings.Contains(got, "Docker Compose") {
		t.Errorf("control: the Kubernetes chief's task_comment was refused as on Compose: %s", got)
	}

	s.ext.ComposeChief = &ComposeChief{}
	if got := slices.Sorted(slices.Values(agentspec.ComposeChiefAllowedMCPTools)); !slices.Equal(got, composeChiefToolLedger) {
		t.Fatalf("the spec's pre-allowed tools are %v, want the ledger %v", got, composeChiefToolLedger)
	}
	if got := slices.Sorted(slices.Values(toolNames(s.MCPToolDefsFor(ctx, chief)))); !slices.Equal(got, composeChiefToolLedger) {
		t.Errorf("the Compose chief's /mcp tools = %v, want exactly %v", got, composeChiefToolLedger)
	}
	if got := slices.Sorted(slices.Values(toolNames(s.AgentToolDefsFor(ctx, chief)))); !slices.Equal(got, composeChiefToolLedger) {
		t.Errorf("the Compose chief's gateway tools = %v, want exactly %v", got, composeChiefToolLedger)
	}
	if got := toolNames(s.MCPToolDefsFor(ctx, worker)); !reflect.DeepEqual(got, k8sWorker) {
		t.Errorf("control: a worker's tools changed with ComposeChief set: %v, want %v", got, k8sWorker)
	}

	// tools/list as served: the ledger, and no muster_help.
	var listed struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	if r := mcpAs(t, s, chief, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); r.Error != nil || json.Unmarshal(r.Result, &listed) != nil {
		t.Fatalf("tools/list: %+v", r)
	}
	var served []string
	for _, tl := range listed.Tools {
		served = append(served, tl.Name)
	}
	if slices.Sort(served); !slices.Equal(served, composeChiefToolLedger) {
		t.Errorf("tools/list serves the Compose chief %v, want exactly %v", served, composeChiefToolLedger)
	}

	// The initialize instructions: this install's statement, and nothing about
	// the connector store or the attention queue's ask.
	var ini struct{ Instructions string }
	if r := mcpAs(t, s, chief, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`); r.Error != nil || json.Unmarshal(r.Result, &ini) != nil {
		t.Fatalf("initialize: %+v", r)
	}
	if ini.Instructions != mcpComposeChiefInstructions || !strings.HasSuffix(ini.Instructions, agentspec.ComposeChiefUnavailable) {
		t.Errorf("the Compose chief's initialize instructions = %q", ini.Instructions)
	}
	for _, tl := range s.agentTools() {
		if n := tl.def.Name; !slices.Contains(composeChiefToolLedger, n) && strings.Contains(ini.Instructions, n) {
			t.Errorf("the Compose chief's initialize instructions name %s, which it cannot run", n)
		}
	}

	// A call by name to a tool it is not offered is refused: in the
	// dispatcher with the reason, and on the MCP door by the catalog (only a
	// listed tool may be called there), so the tool never runs.
	want := toolErr("%s", composeChiefRefusal("task_comment"))
	if got := s.dispatchAgentTool(ctx, chief, "task_comment", "{}"); got != want {
		t.Errorf("dispatching task_comment for the Compose chief: %s, want %s", got, want)
	}
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"task_set_status","arguments":{}}}`
	if r := mcpAs(t, s, chief, call); r.Error == nil || r.Error.Message != "unknown tool task_set_status" || len(r.Result) != 0 {
		t.Errorf("tools/call task_set_status for the Compose chief: error=%+v result=%s, want refused unrun", r.Error, r.Result)
	}
	// Control: the same call from the Kubernetes chief reaches the tool (its
	// own refusal of the empty arguments), so the refusal above is the
	// Compose axis and not the call's shape.
	s.ext.ComposeChief = nil
	r := mcpAs(t, s, chief, call)
	if r.Error != nil {
		t.Fatalf("control: the Kubernetes chief's tools/call task_set_status: %+v", r.Error)
	}
	if text, isErr := toolText(t, &r); !isErr || !strings.Contains(text, "id is required") {
		t.Errorf("control: the Kubernetes chief's tools/call task_set_status did not reach the tool: %s", text)
	}
}

// failingAgents is an agent store whose list cannot be read.
type failingAgents struct{ agents.Store }

func (failingAgents) List(context.Context) ([]agents.Agent, error) {
	return nil, errors.New("fixture: the agents table is unreachable")
}

// TestAnUnreadableAgentListFailsTheMCPDoorLoudly: whether the caller is the
// chief decides its whole tool list, so when the agent list cannot be read
// POST /mcp answers 503 for every method — never a tool list built as if the
// caller were not the chief (which, for the Compose chief, skipped its
// restriction and offered the not-chief tools). Control: the same request with
// a readable list is served.
func TestAnUnreadableAgentListFailsTheMCPDoorLoudly(t *testing.T) {
	s, chief, _ := chiefAndWorkerServer(t)
	s.ext.ComposeChief = &ComposeChief{}
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	call := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(list))
		r = r.WithContext(context.WithValue(r.Context(), agentCtxKey{}, chief))
		w := httptest.NewRecorder()
		s.handleMCP(w, r)
		return w
	}
	if w := call(); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"tasks_list"`) {
		t.Fatalf("control: tools/list with a readable agent list = %d %s", w.Code, w.Body)
	}
	readable := s.ext.Agents
	s.ext.Agents = failingAgents{}
	defer func() { s.ext.Agents = readable }()
	w := call()
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), `"tools"`) ||
		!strings.Contains(w.Body.String(), "could not read its agents") {
		t.Errorf("tools/list with an unreadable agent list = %d %s, want 503 and no tool list", w.Code, w.Body)
	}
}
