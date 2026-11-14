package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agentoutcome"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/provision"
)

// A GATEWAY AGENT ASKED SOMETHING OVER THE MACHINE MESSAGE DOOR (POST
// /api/agents/{name}/messages, `chief ask`) ANSWERS WITH ITS MUSTER TOOLS, AND
// THEY ACT AS ITS OWNER. Real Postgres, the real handler, the real gateway
// transport to a fake runtime that reads the tools each /v1/responses request
// carries (reportingRuntime) and calls agent_report_done only when offered.
//
// 🔴 RED BEFORE THIS CHANGE: the door called Gateway.Chat, a toolless turn, so
// the request carried no tool and the agent's outcome never moved.

// machineMessage drives handleAPIAgentSendMessage for agent a as principal p,
// over the real gateway transport to runtime, and returns the HTTP status.
func (o *outcomeFixture) machineMessage(t *testing.T, runtime *httptest.Server, p authz.Principal, a agents.Agent, text string) int {
	t.Helper()
	defer o.useRuntime(t, runtime, a)()
	w := o.machineMessageCtx(context.Background(), p, a, text)
	if w.Code != http.StatusOK {
		t.Logf("machine message answered HTTP %d: %s", w.Code, w.Body.String())
	}
	return w.Code
}

// useRuntime points the server's gateway at runtime (the real transport) and
// gives each of list its hooks token; the returned func restores the gateway.
func (o *outcomeFixture) useRuntime(t *testing.T, runtime *httptest.Server, list ...agents.Agent) func() {
	t.Helper()
	u, _ := url.Parse(runtime.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	pn, _ := strconv.Atoi(port)
	gw, err := agentgateway.New(agentgateway.Config{Driver: outcomeEndpoint{provision.Endpoint{Scheme: "http", Host: host, Port: pn}},
		Runtime: agentgateway.HooksSHA256(), Model: "runtime-sentinel", Secrets: o.srv.ext.Agents})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range list {
		if err := agents.NewPG(o.pool).SetHooksToken(authz.System(context.Background()), a.ID, "machine-msg-token-"+a.Name); err != nil {
			t.Fatal(err)
		}
	}
	prev := o.srv.ext.Gateway
	o.srv.ext.Gateway = gw
	return func() { o.srv.ext.Gateway = prev }
}

// machineMessageCtx is the handler call itself, under parent (whose
// cancellation the request carries) acting as p.
func (o *outcomeFixture) machineMessageCtx(parent context.Context, p authz.Principal, a agents.Agent, text string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"message": text})
	r := httptest.NewRequest(http.MethodPost, "/api/agents/"+a.Name+"/messages", strings.NewReader(string(body)))
	r.SetPathValue("name", a.Name)
	r = r.WithContext(authz.WithPrincipal(parent, p))
	w := httptest.NewRecorder()
	o.srv.handleAPIAgentSendMessage(w, r)
	return w
}

// tokenPrincipal is the agent owner's personal token on the door, as
// requireArmedHookToken admits it (admitAPIToken): `chief ask` with the owner's
// own token. Row-level security lets a request reach only its own user's agents,
// so the shared hook token's operator addresses the operator's agents alone.
func (o *outcomeFixture) tokenPrincipal() authz.Principal {
	return o.srv.principalFor(context.Background(), o.owner, authz.ViaToken)
}

// TestAMachineMessageToAGatewayAgentOffersItsMusterTools: the turn the machine
// message door starts for a gateway agent offers agent_report_done with the
// worker's system prompt, and the call the agent makes there moves it to done
// with one "finished" push.
func TestAMachineMessageToAGatewayAgentOffersItsMusterTools(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	a := o.dispatchGateway(t, "machine message tools", "openrouter/acme/acme-v4.1-pro")
	if agents.ResolveKind(a.Kind) != agents.KindGateway || a.Outcome != agentoutcome.Running {
		t.Fatalf("instrument check FAILED: kind %q outcome %q; want a tracked gateway agent", a.Kind, a.Outcome)
	}
	rt := &reportingRuntime{}
	if code := o.machineMessage(t, rt.serve(t), o.tokenPrincipal(), a, "Finish the fix and report done."); code != http.StatusOK {
		t.Fatalf("machine message: HTTP %d", code)
	}
	tools, instr := rt.requests()
	if len(tools) == 0 {
		t.Fatal("instrument check FAILED: the runtime saw no request")
	}
	if !slices.Contains(tools[0], agentReportDoneTool) {
		t.Fatalf("the machine message turn's request offered %v; want %s among them", tools[0], agentReportDoneTool)
	}
	if !strings.Contains(instr[0], "describe ONLY these real capabilities") {
		t.Errorf("the turn's instructions %q; want the worker's system prompt (AgentInstructions)", instr[0])
	}
	o.assertReportedDone(t, a)
}

// TestAMachineMessageTurnsToolsActAsTheAgentsOwner: a tool the machine message
// turn calls runs as the agent's OWNER through authz.ViaAgentMessage:
// attention_ask (whose insert takes its owner from the principal) files the
// owner's ask, and the principal the turn's tools were built under is the
// owner's, via agent-message — not the request's own (ViaToken here, the
// operator's for the shared hook token).
func TestAMachineMessageTurnsToolsActAsTheAgentsOwner(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	a := o.dispatchGateway(t, "machine message acts as owner", "openrouter/acme/acme-v4.1-pro")
	rt := &reportingRuntime{pre: &[2]string{"attention_ask", `{"question":"Which branch should the fix go on?"}`}}
	if code := o.machineMessage(t, rt.serve(t), o.tokenPrincipal(), a, "Ask me which branch, then report done."); code != http.StatusOK {
		t.Fatalf("machine message: HTTP %d", code)
	}
	rt.mu.Lock()
	out := rt.preOutput
	rt.mu.Unlock()
	if !strings.Contains(out, `"id":`) || strings.Contains(out, `"error"`) {
		t.Fatalf("attention_ask in the machine message turn answered %q; want the filed ask", out)
	}
	var owner int64
	if err := o.pool.QueryRow(authz.System(context.Background()), `SELECT owner_id FROM attention_asks WHERE agent_id = $1`, a.ID).Scan(&owner); err != nil {
		t.Fatalf("the ask the turn filed: %v", err)
	}
	if owner != o.owner {
		t.Errorf("the ask is owner %d's; want the agent's owner %d", owner, o.owner)
	}
	o.assertReportedDone(t, a)

	// The principal the door's tools are built under.
	reqCtx := authz.WithPrincipal(context.Background(), o.tokenPrincipal())
	ctx, tt, ok := o.srv.gatewayTurnTools(reqCtx, o.agent(t, a.ID), authz.ViaAgentMessage)
	if !ok || !slices.ContainsFunc(tt.Tools, func(d agents.ToolDef) bool { return d.Name == agentReportDoneTool }) {
		t.Fatalf("gatewayTurnTools for the door: ok=%t, %d tool(s); want agent_report_done among them", ok, len(tt.Tools))
	}
	if p, _ := authz.From(ctx); p.UserID != o.owner || p.Via != authz.ViaAgentMessage {
		t.Errorf("the door's turn tools act as %+v; want the owner %d via %s", p, o.owner, authz.ViaAgentMessage)
	}
}

// TestTurnPrincipalMintsOnlyTheTurnDoors: turnPrincipal acts as the agent's
// owner for the two turn doors (kickoff, agent-message) and for nothing else —
// it is not a general "act as the owner" door — and a claude-code agent is
// given no in-process tools on either (its tools come over MCP).
func TestTurnPrincipalMintsOnlyTheTurnDoors(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	gw := agents.Agent{ID: 1 << 40, Name: "door-wren", Kind: agents.KindGateway, OwnerID: o.owner}
	for _, via := range []authz.Via{authz.ViaKickoff, authz.ViaAgentMessage} {
		ctx, ok := o.srv.turnPrincipal(context.Background(), gw, via)
		if p, _ := authz.From(ctx); !ok || p.UserID != o.owner || p.Via != via {
			t.Errorf("turnPrincipal(%s) = %+v ok=%t; want the owner %d via %s", via, p, ok, o.owner, via)
		}
	}
	for _, via := range []authz.Via{authz.ViaSession, authz.ViaHook, authz.ViaToken, authz.ViaOfficer} {
		if _, ok := o.srv.turnPrincipal(context.Background(), gw, via); ok {
			t.Errorf("turnPrincipal minted a principal via %s; want refused (not a turn door)", via)
		}
	}
	cc := gw
	cc.Kind = agents.KindClaudeCode
	for _, via := range []authz.Via{authz.ViaKickoff, authz.ViaAgentMessage} {
		if _, tt, ok := o.srv.gatewayTurnTools(context.Background(), cc, via); ok || tt.Tools != nil {
			t.Errorf("a claude-code agent was given %d in-process tool(s) via %s; its tools come over MCP", len(tt.Tools), via)
		}
	}
}

// TestAMachineMessageToAGatewayChiefOffersItsChatDoorsTools: a GATEWAY-KIND
// chief (`muster chief replace --kind gateway`) asked over the machine message
// door is offered EXACTLY the tool set its own chat door (chatTurn) offers it,
// with the same system prompt: its chief-only tools (ask_owner among them), and
// no worker-only one (agent_report_done is notChief). A deliberate choice — the
// door is the chief's chat, reached from the CLI — pinned as a relationship
// between two doors observed through the real transport, plus literal names.
//
// 🔴 RED BEFORE THIS CHANGE: the door offered the chief nothing ([]).
func TestAMachineMessageToAGatewayChiefOffersItsChatDoorsTools(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	chief := o.agent(t, o.chief.ID)
	if agents.ResolveKind(chief.Kind) != agents.KindGateway || !o.srv.isChiefAgent(o.asOwner, chief) {
		t.Fatalf("instrument check FAILED: kind %q chief=%t; want a gateway-kind chief", chief.Kind, o.srv.isChiefAgent(o.asOwner, chief))
	}

	door := &reportingRuntime{}
	if code := o.machineMessage(t, door.serve(t), o.tokenPrincipal(), chief, "What is open on the board?"); code != http.StatusOK {
		t.Fatalf("machine message to the chief: HTTP %d", code)
	}
	doorTools, doorInstr := door.requests()

	chat := &reportingRuntime{}
	restore := o.useRuntime(t, chat.serve(t), chief)
	if _, err := o.srv.chatTurn(o.asOwner, chief, "chief-chat-door-compare", "What is open on the board?", nil); err != nil {
		restore()
		t.Fatalf("the chief's chat turn: %v", err)
	}
	restore()
	chatTools, chatInstr := chat.requests()

	if len(doorTools) == 0 || len(chatTools) == 0 {
		t.Fatalf("instrument check FAILED: door saw %d request(s), chat %d", len(doorTools), len(chatTools))
	}
	got, want := slices.Sorted(slices.Values(doorTools[0])), slices.Sorted(slices.Values(chatTools[0]))
	if !slices.Equal(got, want) {
		t.Errorf("the machine message door offered the chief %v;\nits chat door offers %v", got, want)
	}
	if doorInstr[0] != chatInstr[0] || doorInstr[0] == "" {
		t.Errorf("the door's system prompt differs from the chat door's (door %d bytes, chat %d bytes)", len(doorInstr[0]), len(chatInstr[0]))
	}
	if !slices.Contains(got, askOwnerTool) {
		t.Errorf("the door offered the chief %v; want the chief-only %s among them", got, askOwnerTool)
	}
	if slices.Contains(got, agentReportDoneTool) {
		t.Errorf("the door offered the chief the worker-only %s", agentReportDoneTool)
	}
	t.Logf("a gateway chief on the machine message door is offered %d tool(s): %v", len(got), got)
}

// TestAMachineMessageTurnOutlivesItsRequest: the caller of the machine message
// door gives up (its request context is cancelled — `chief ask`'s 30 s timeout)
// while the agent's tool turn is mid-loop. The turn still finishes: its tool
// call runs, the agent reaches done, and its reply is stored.
//
// 🔴 RED with the turn on the request's own context: the cancellation cut the
// loop between its requests, the reply was never stored and the outcome stayed
// `running`.
func TestAMachineMessageTurnOutlivesItsRequest(t *testing.T) {
	o := newOutcomeFixture(t, stubModelCatalog{})
	a := o.dispatchGateway(t, "machine message outlives its caller", "openrouter/acme/acme-v4.1-pro")
	inFirst, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	requests := 0
	rt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/responses") {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		var output []any
		if n == 1 {
			// The first request holds until the caller has gone away.
			once.Do(func() { close(inFirst) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			if !strings.Contains(string(raw), agentReportDoneTool) {
				output = []any{map[string]any{"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": "no tools offered"}}}}
			} else {
				args, _ := json.Marshal(map[string]string{"summary": reportDoneSummary})
				output = []any{map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1",
					"name": agentReportDoneTool, "arguments": string(args)}}
			}
		} else {
			output = []any{map[string]any{"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": outlivedReply}}}}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseEvent("response.created", map[string]any{"response": map[string]any{"status": "in_progress"}}) +
			sseEvent("response.completed", map[string]any{"response": map[string]any{"status": "completed", "output": output}})))
	}))
	t.Cleanup(rt.Close)
	defer o.useRuntime(t, rt, a)()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- o.machineMessageCtx(ctx, o.tokenPrincipal(), a, "Fix it and report done.") }()
	select {
	case <-inFirst:
	case <-time.After(20 * time.Second):
		t.Fatal("instrument check FAILED: the runtime never saw the turn's first request")
	}
	cancel() // the caller gives up mid-turn
	// Let a request-bound turn notice before the runtime answers.
	select {
	case <-done:
		close(release)
	case <-time.After(500 * time.Millisecond):
		close(release)
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatal("the machine message handler never returned")
		}
	}

	mu.Lock()
	n := requests
	mu.Unlock()
	if n < 2 {
		t.Errorf("the turn made %d request(s) after its caller left; want its tool loop to finish (2)", n)
	}
	got := o.agent(t, a.ID)
	if got.Outcome != agentoutcome.Done {
		t.Errorf("outcome %q (reason %q) after the caller left mid-turn; want done", got.Outcome, got.OutcomeReason)
	}
	var stored int
	if err := o.pool.QueryRow(authz.System(context.Background()),
		`SELECT count(*) FROM chat_messages WHERE agent_id = $1 AND role = 'assistant' AND content = $2`, a.ID, outlivedReply).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Errorf("%d stored assistant reply(ies) %q; want 1", stored, outlivedReply)
	}
}

const outlivedReply = "Reported done; the draft PR is up."
