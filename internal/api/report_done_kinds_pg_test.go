package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentoutcome"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
)

// EVERY DISPATCHED AGENT IS OFFERED agent_report_done, THROUGH THE TOOL PATH ITS
// KIND ACTUALLY USES, AND ITS CALL REACHES DONE. Real Postgres, the real dispatch
// doors (the chief's agent_dispatch, and the owner's session's
// dispatch_request + dispatch_approve over /mirror/mcp), and:
//
//   - gateway: the REAL kickoff deliverer (wired as cmd/muster-server wires it,
//     kickoffOnce) over the REAL gateway transport to a fake runtime that reads
//     the tools each /v1/responses request carries, and calls agent_report_done
//     only when it is offered — as the live model did. A gateway agent does its
//     whole job in that first turn.
//   - claude-code: the agent's own POST /mcp door (tools/list, tools/call) — the
//     only way its CLI reaches muster's tools.
//
// 🔴 THE INCIDENT (anonymised): a gateway agent started through
// dispatch_request/dispatch_approve with no task, outcome-tracked, was told in
// its kickoff to call agent_report_done and answered that the tool "isn't in my
// available toolset"; its outcome stayed `waiting`. The first turn was sent by
// agentgateway.Gateway.Send — RunToollessTurn — so it carried no muster tool at
// all; agent_report_done was in the registry (AgentToolDefsFor) and only chat
// turns ever read it.

const reportDoneSummary = "Opened draft PR #7 with the fix."

// reportingRuntime is an agent runtime that behaves as the incident's model did:
// offered agent_report_done, it calls it (then answers once the call's output
// comes back); not offered it, it says so in text. It records the tool names of
// every request.
type reportingRuntime struct {
	mu    sync.Mutex
	tools [][]string
	instr []string
	// pre, when set, is a call the runtime makes FIRST (before reporting done);
	// preOutput is the output muster fed back for it.
	pre       *[2]string
	preOutput string
	step      int
}

func (rt *reportingRuntime) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/responses") {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Instructions string `json:"instructions"`
			Input        []struct {
				Type   string `json:"type"`
				Output string `json:"output"`
			} `json:"input"`
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var names []string
		for _, tl := range req.Tools {
			names = append(names, tl.Name)
		}
		answered := false
		for _, in := range req.Input {
			if in.Type == "function_call_output" {
				answered = true
				if rt.pre != nil && rt.step == 1 {
					rt.preOutput = in.Output
				}
			}
		}
		rt.mu.Lock()
		rt.tools = append(rt.tools, names)
		rt.instr = append(rt.instr, req.Instructions)
		step := rt.step
		rt.step++
		pre := rt.pre
		rt.mu.Unlock()
		if pre != nil {
			// pre first, then agent_report_done, then text.
			switch step {
			case 0:
				answered = false
				names = []string{pre[0]}
			case 1:
				answered = false
			}
		}

		message := func(text string) []any {
			return []any{map[string]any{"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": text}}}}
		}
		var output []any
		switch {
		case answered:
			output = message("Reported; the draft PR is up.")
		case pre != nil && step == 0:
			output = []any{map[string]any{"type": "function_call", "id": "fc_0", "call_id": "call_0",
				"name": pre[0], "arguments": pre[1]}}
		case slices.Contains(names, agentReportDoneTool):
			args, _ := json.Marshal(map[string]string{"summary": reportDoneSummary})
			output = []any{map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1",
				"name": agentReportDoneTool, "arguments": string(args)}}
		default:
			output = message("**Blockers:** `agent_report_done` isn't in my available toolset, so I'm reporting here instead.")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseEvent("response.created", map[string]any{"response": map[string]any{"status": "in_progress"}}) +
			sseEvent("response.completed", map[string]any{"response": map[string]any{"status": "completed", "output": output}})))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (rt *reportingRuntime) requests() ([][]string, []string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([][]string(nil), rt.tools...), append([]string(nil), rt.instr...)
}

// TestEveryDispatchedGatewayAgentReportsDoneInItsFirstTurn: through each door —
// the chief's agent_dispatch with a task and without, the owner's session's
// dispatch_request + dispatch_approve without a task (the incident) and with
// one — a gateway agent's first turn offers agent_report_done, its system prompt
// is the worker's, and the call it makes there moves the agent to done: one
// "finished" push with the summary.
//
// 🔴 RED BEFORE THIS CHANGE (and with KickoffTurnTools forced to "not ok", the
// base's exact send): request 1 carried NO tools, the runtime answered the
// incident's sentence, and every case ended `waiting`.
func TestEveryDispatchedGatewayAgentReportsDoneInItsFirstTurn(t *testing.T) {
	type started struct {
		door  string
		agent agents.Agent
		check func(*testing.T, agents.Agent)
		srv   *Server
		run   func(*testing.T, *httptest.Server, agents.Agent)
	}
	var cases []started

	// The chief's agent_dispatch.
	o := newOutcomeFixture(t, stubModelCatalog{})
	withTask := o.dispatchGateway(t, "report done, task", "openrouter/acme/acme-v4.1-pro")
	noTask := o.dispatchNoTask(t, "pod/gateway", map[string]any{"model": "openrouter/acme/acme-v4.1-pro"})
	for door, a := range map[string]agents.Agent{"agent_dispatch with a task": withTask, "agent_dispatch, no task": noTask} {
		cases = append(cases, started{door: door, agent: a, srv: o.srv,
			run:   func(t *testing.T, rt *httptest.Server, a agents.Agent) { o.kickoff(t, rt, a) },
			check: func(t *testing.T, a agents.Agent) { o.assertReportedDone(t, a) }})
	}

	// The owner's session: dispatch_request + dispatch_approve over /mirror/mcp.
	h := newHoldFixture(t)
	hp := &outcomeFixture{taskRouteFixture: h.taskRouteFixture, pusher: h.pusher,
		prefix: "rdh" + strconv.FormatInt(time.Now().UnixNano()%1e9, 36) + "-", clock: newOutcomeClock()}
	h.srv.ext.Outcomes = agentoutcome.NewPG(h.pool)
	h.srv.ext.AgentNamespacePrefix = hp.prefix
	approve := func(t *testing.T, args map[string]any) agents.Agent {
		t.Helper()
		before := map[int64]bool{}
		for _, a := range h.ownersAgents(t) {
			before[a.ID] = true
		}
		held := h.request(t, args)
		if text, isErr, rpc := h.tool(t, h.ws, focusSID, dispatchApproveTool, map[string]any{"hold_id": held.HoldID}); rpc != nil || isErr {
			t.Fatalf("dispatch_approve: %v %s", rpc, text)
		}
		for _, a := range h.ownersAgents(t) {
			if !before[a.ID] {
				return a
			}
		}
		t.Fatal("dispatch_approve started no agent")
		return agents.Agent{}
	}
	n := h.task(t, "session dispatch, task")
	onTask := gatewayRequest()
	onTask["task_id"] = n.ID
	for door, a := range map[string]agents.Agent{
		"dispatch_request/approve, no task (the incident)": approve(t, gatewayRequest()),
		"dispatch_request/approve with a task":             approve(t, onTask),
	} {
		cases = append(cases, started{door: door, agent: a, srv: h.srv,
			run: func(t *testing.T, rt *httptest.Server, a agents.Agent) {
				kickoffOnce(t, h.srv, h.pool, hp.prefix, rt, 0, a)
			},
			check: func(t *testing.T, a agents.Agent) { hp.assertReportedDone(t, a) }})
	}

	for _, c := range cases {
		t.Run(c.door, func(t *testing.T) {
			if agents.ResolveKind(c.agent.Kind) != agents.KindGateway {
				t.Fatalf("instrument check FAILED: %s started a %q agent, want gateway", c.door, c.agent.Kind)
			}
			if c.agent.Outcome != agentoutcome.Running || c.agent.DispatchedBy == nil {
				t.Fatalf("instrument check FAILED: %s: outcome %q dispatched_by %v; want a tracked, running agent",
					c.door, c.agent.Outcome, c.agent.DispatchedBy)
			}
			rt := &reportingRuntime{}
			c.run(t, rt.serve(t), c.agent)
			tools, instr := rt.requests()
			if len(tools) == 0 {
				t.Fatal("instrument check FAILED: the runtime saw no request")
			}
			if !slices.Contains(tools[0], agentReportDoneTool) {
				t.Fatalf("the first turn's request offered %v; want %s among them", tools[0], agentReportDoneTool)
			}
			if !strings.Contains(instr[0], "describe ONLY these real capabilities") {
				t.Errorf("the first turn's instructions %q; want the worker's system prompt (AgentInstructions)", instr[0])
			}
			c.check(t, c.agent)
		})
	}
}

// dispatchNoTask starts a target agent with NO task through the chief's
// agent_dispatch, in a turn the owner typed (owner_asked), as an owner asking
// their chief for research does; extra adds arguments.
func (o *outcomeFixture) dispatchNoTask(t *testing.T, target string, extra map[string]any) agents.Agent {
	t.Helper()
	// owner_asked is verified against the chief's pod's CLIs (the owner's lane
	// working): the release fixture's pod, kept for the dispatch call alone.
	pod := &podLanes{Gateway: o.srv.ext.Gateway}
	pod.only()
	prev := o.srv.ext.Gateway
	o.srv.ext.Gateway = pod
	defer func() { o.srv.ext.Gateway = prev }()
	o.ownerTurn(t, o.chief, o.owner, "Start an agent to research the scheduler.")
	args := map[string]any{"target": target, "prompt": "Research the scheduler and report what you find.",
		"request_key": "rd-" + strconv.FormatInt(time.Now().UnixNano(), 36), "owner_asked": true}
	for k, v := range extra {
		args[k] = v
	}
	text := o.callRaw(o.chief, agentDispatchTool, args)
	var out struct {
		AgentID int64 `json:"agent_id"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.AgentID <= 0 || strings.Contains(text, `"error"`) {
		t.Fatalf("agent_dispatch with no task answered %s", text)
	}
	a := o.agent(t, out.AgentID)
	if a.NoteID != nil {
		t.Fatalf("instrument check FAILED: the no-task start has task %d", *a.NoteID)
	}
	return a
}

// assertReportedDone: a is done, and its owner got exactly one "finished" push
// carrying the summary the runtime reported.
func (o *outcomeFixture) assertReportedDone(t *testing.T, a agents.Agent) {
	t.Helper()
	got := o.agent(t, a.ID)
	if got.Outcome != agentoutcome.Done {
		t.Fatalf("outcome %q (reason %q); want done", got.Outcome, got.OutcomeReason)
	}
	var finished int
	o.srv.WaitForPushes()
	for _, p := range o.pusher.to(o.ownerPushDevice()) {
		if p.Tag == "agent-"+strconv.FormatInt(a.ID, 10)+"-outcome" && strings.HasSuffix(p.Title, " finished: "+reportDoneSummary) {
			finished++
		}
	}
	if finished != 1 {
		t.Errorf("%d \"finished: %s\" push(es); want 1", finished, reportDoneSummary)
	}
}

// ownerPushDevice is the device the fixture's owner subscribed: the outcome
// fixture's own, or the hold fixture's.
func (o *outcomeFixture) ownerPushDevice() string {
	if o.pushes == nil {
		return ep("hold-owner")
	}
	return o.ownerDevice()
}

// TestEveryDispatchedClaudeCodeAgentIsOfferedReportDoneOverMCP: a claude-code
// agent, with a task and without, started by the chief's agent_dispatch, finds
// agent_report_done in its own /mcp door's tools/list, and calling it there
// moves it to done.
//
// ⚠ AN INVARIANT GUARD, NOT A REGRESSION TEST: this was already true on the
// base (the MCP door reads MCPToolDefsFor, and agent_report_done is mcp:true) —
// it pins that the claude-code kind was NOT affected, and stays so. Its first
// turn is deliberately still toolless (KickoffTurnTools answers not ok): ccd
// ignores a request's tools.
func TestEveryDispatchedClaudeCodeAgentIsOfferedReportDoneOverMCP(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	n := o.task(t, "cc report done, task")
	withTask := o.startedOne(t, o.callRaw(o.chief, agentDispatchTool, map[string]any{"target": "pod/claude-code", "task_id": n.ID}), n.ID)
	noTask := o.dispatchNoTask(t, "pod/claude-code", nil)
	for name, a := range map[string]agents.Agent{"with a task": withTask, "no task": noTask} {
		t.Run(name, func(t *testing.T) {
			if a.Kind != agents.KindClaudeCode || a.DispatchedBy == nil {
				t.Fatalf("instrument check FAILED: kind %q dispatched_by %v", a.Kind, a.DispatchedBy)
			}
			if _, tt, ok := o.srv.KickoffTurnTools(context.Background(), a); ok || tt.Tools != nil {
				t.Errorf("a claude-code first turn was given %d in-process tools; its tools come over MCP", len(tt.Tools))
			}
			tok := "rd-cc-token-" + a.Name
			if err := agents.NewPG(o.pool).SetHooksToken(authz.System(context.Background()), a.ID, tok); err != nil {
				t.Fatal(err)
			}
			r, code := o.mcp(t, tok, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
			var res struct {
				Tools []struct{ Name string } `json:"tools"`
			}
			if r == nil || json.Unmarshal(r.Result, &res) != nil {
				t.Fatalf("tools/list: HTTP %d %+v", code, r)
			}
			var names []string
			for _, tl := range res.Tools {
				names = append(names, tl.Name)
			}
			if !slices.Contains(names, agentReportDoneTool) {
				t.Fatalf("tools/list over /mcp = %v; want %s", names, agentReportDoneTool)
			}
			raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
				"params": map[string]any{"name": agentReportDoneTool, "arguments": map[string]string{"summary": reportDoneSummary}}})
			if r, code := o.mcp(t, tok, string(raw), nil); r == nil || r.Error != nil {
				t.Fatalf("tools/call: HTTP %d %+v", code, r)
			}
			o.assertReportedDone(t, a)
		})
	}
}

// TestAFirstTurnsToolsActAsTheAgentsOwner: a tool the first turn calls runs as
// the agent's OWNER (authz.ViaKickoff), not as the deliverer's System
// principal — attention_ask files the owner's question (under System it answers
// "the question could not be filed": its insert takes the owner from the
// principal), and the ask is the owner's. Then the turn reports done.
//
// 🔴 RED with KickoffTurnTools passing the deliverer's context through (System).
func TestAFirstTurnsToolsActAsTheAgentsOwner(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	a := o.dispatchGateway(t, "acts as owner", "openrouter/acme/acme-v4.1-pro")
	rt := &reportingRuntime{pre: &[2]string{"attention_ask", `{"question":"Which branch should the fix go on?"}`}}
	o.kickoff(t, rt.serve(t), a)
	rt.mu.Lock()
	out := rt.preOutput
	rt.mu.Unlock()
	if !strings.Contains(out, `"id":`) || strings.Contains(out, `"error"`) {
		t.Fatalf("attention_ask in the first turn answered %q; want the filed ask", out)
	}
	var owner int64
	if err := o.pool.QueryRow(o.asOwner, `SELECT owner_id FROM attention_asks WHERE agent_id = $1`, a.ID).Scan(&owner); err != nil {
		t.Fatalf("the ask the first turn filed: %v", err)
	}
	if owner != o.owner {
		t.Errorf("the ask is owner %d's; want the agent's owner %d", owner, o.owner)
	}
	o.assertReportedDone(t, a)
}
