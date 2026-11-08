package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

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

// machineMessage drives handleAPIAgentSendMessage for agent name as principal p,
// over the real gateway transport to runtime, and returns the HTTP status.
func (o *outcomeFixture) machineMessage(t *testing.T, runtime *httptest.Server, p authz.Principal, a agents.Agent, text string) int {
	t.Helper()
	u, _ := url.Parse(runtime.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	pn, _ := strconv.Atoi(port)
	gw, err := agentgateway.New(agentgateway.Config{Driver: outcomeEndpoint{provision.Endpoint{Scheme: "http", Host: host, Port: pn}},
		Runtime: agentgateway.HooksSHA256(), Model: "runtime-sentinel"})
	if err != nil {
		t.Fatal(err)
	}
	if err := agents.NewPG(o.pool).SetHooksToken(authz.System(context.Background()), a.ID, "machine-msg-token-"+a.Name); err != nil {
		t.Fatal(err)
	}
	prev := o.srv.ext.Gateway
	o.srv.ext.Gateway = gw
	defer func() { o.srv.ext.Gateway = prev }()
	body, _ := json.Marshal(map[string]string{"message": text})
	r := httptest.NewRequest(http.MethodPost, "/api/agents/"+a.Name+"/messages", strings.NewReader(string(body)))
	r.SetPathValue("name", a.Name)
	r = r.WithContext(authz.WithPrincipal(context.Background(), p))
	w := httptest.NewRecorder()
	o.srv.handleAPIAgentSendMessage(w, r)
	if w.Code != http.StatusOK {
		t.Logf("machine message answered HTTP %d: %s", w.Code, w.Body.String())
	}
	return w.Code
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
