package agentkickoff

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// A FIRST TURN CARRIES THE AGENT'S MUSTER TOOLS WHEN Config.TurnTools GIVES THEM
// (agentkickoff.go firstTurnSender). The integration test that the incident's
// shape reaches Done is internal/api's TestEveryDispatchedGatewayAgentReportsDoneInItsFirstTurn;
// these pin the deliverer's own branches: which send it takes, that a refusal
// after the tool turn's first request is never re-sent, and the 404 fallback.

// toolGateway is fakeGateway plus SendWithTools, which answers the scripted
// errors in order (then reply, nil) and records what each call carried.
type toolGateway struct {
	*fakeGateway
	mu        sync.Mutex
	errs      []error
	toolCalls int
	instr     []string
	toolNames [][]string
	dispatch  []agents.ToolDispatch
}

func (g *toolGateway) SendWithTools(_ context.Context, _ agentgateway.Target, sessionKey, instructions, message string,
	tools []agents.ToolDef, dispatch agents.ToolDispatch, _ agents.StreamEmit) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.toolCalls++
	g.log.add(fmt.Sprintf("ChatWithTools(%s,%s)", sessionKey, message))
	g.instr = append(g.instr, instructions)
	var names []string
	for _, d := range tools {
		names = append(names, d.Name)
	}
	g.toolNames = append(g.toolNames, names)
	g.dispatch = append(g.dispatch, dispatch)
	if len(g.errs) > 0 {
		err := g.errs[0]
		g.errs = g.errs[1:]
		return "", err
	}
	return "done — draft PR opened", nil
}

func (g *toolGateway) toolCallCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.toolCalls
}

// toolsFor is a Config.TurnTools answering ok for gateway-kind rows only (as
// api.Server.KickoffTurnTools does), and recording each agent it was asked for.
type toolsFor struct {
	mu    sync.Mutex
	asked []string
}

const testInstructions = "You are a worker. Call agent_report_done when finished."

func (f *toolsFor) turnTools(ctx context.Context, a agents.Agent) (context.Context, TurnTools, bool) {
	f.mu.Lock()
	f.asked = append(f.asked, a.Name)
	f.mu.Unlock()
	if agents.ResolveKind(a.Kind) != agents.KindGateway {
		return ctx, TurnTools{}, false
	}
	return ctx, TurnTools{Instructions: testInstructions,
		Tools:    []agents.ToolDef{{Type: "function", Name: "agent_report_done"}, {Type: "function", Name: "agent_get_task"}},
		Dispatch: func(string, string) string { return `{"ok":true}` }}, true
}

func toolHarness(t *testing.T, row agents.Agent, errs ...error) (*harness, *toolGateway, *toolsFor) {
	t.Helper()
	h := newHarness(t, []provision.Instance{readyInstance(row.Name, row.Name+"-7f9c-x2", 0)}, row)
	gw := &toolGateway{fakeGateway: h.gw, errs: errs}
	tf := &toolsFor{}
	d, err := New(Config{Store: h.store, Instances: h.insts, Gateway: gw, NamespacePrefix: testPrefix, Owner: ownerID,
		Now: func() time.Time { return fixedNow }, TurnTools: tf.turnTools})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.notReadyWait, d.notReadyPoll = 50*time.Millisecond, 5*time.Millisecond
	h.d = d
	return h, gw, tf
}

// TestAGatewayFirstTurnCarriesItsToolsAndAClaudeCodeOneDoesNot: a gateway
// agent's first turn goes out through SendWithTools with exactly the system
// prompt and tools TurnTools gave — agent_report_done among them — and never as
// the toolless Send; a claude-code agent's (TurnTools answers not ok) stays the
// toolless Send.
//
// 🔴 RED BEFORE THIS CHANGE: the deliverer had one send, Gateway.Send, which is
// RunToollessTurn — the incident's agent was told to call agent_report_done in
// the one turn it was offered no tool in.
func TestAGatewayFirstTurnCarriesItsToolsAndAClaudeCodeOneDoesNot(t *testing.T) {
	h, gw, tf := toolHarness(t, ownedRow())
	log := h.tick(t)
	if gw.toolCallCount() != 1 || h.gw.callCount() != 0 {
		t.Fatalf("gateway agent: %d tool turn(s), %d toolless; want 1 and 0.%s", gw.toolCallCount(), h.gw.callCount(), transcript(log))
	}
	if gw.instr[0] != testInstructions || strings.Join(gw.toolNames[0], ",") != "agent_report_done,agent_get_task" || gw.dispatch[0] == nil {
		t.Fatalf("the tool turn carried instructions %q tools %v dispatch-set=%t", gw.instr[0], gw.toolNames[0], gw.dispatch[0] != nil)
	}
	if want := "ChatWithTools(sk-lively-newt,read the repo and report the test count)"; indexOf(log, want) < 0 {
		t.Fatalf("no %q.%s", want, transcript(log))
	}
	if i, j := indexOf(log, "SetKickedOff(7301,true)"), indexOf(log, "ChatWithTools("); i < 0 || j < i {
		t.Fatalf("the stamp must precede the turn (stamp %d, turn %d).%s", i, j, transcript(log))
	}
	if r := h.store.row(7301); !r.KickedOff || agents.KickoffFailed(r) {
		t.Fatalf("kicked_off=%t failed=%t (%q)", r.KickedOff, agents.KickoffFailed(r), r.KickoffError)
	}
	if strings.Join(tf.asked, ",") != "lively-newt" {
		t.Errorf("TurnTools asked for %v; want the one agent, once", tf.asked)
	}

	cc := ownedRow()
	cc.Kind, cc.CCAccount = agents.KindClaudeCode, "acct-a"
	h, gw, _ = toolHarness(t, cc)
	log = h.tick(t)
	if gw.toolCallCount() != 0 || h.gw.callCount() != 1 {
		t.Fatalf("claude-code agent: %d tool turn(s), %d toolless; want 0 and 1.%s", gw.toolCallCount(), h.gw.callCount(), transcript(log))
	}
}

// TestAToolTurnRefusedAfterItsFirstRequestIsNeverResent: a typed refusal that a
// toolless turn would re-send (409 busy) arriving on request 2 of a tool turn
// came AFTER the runtime took the message and ran its tools, so it is a failed
// turn, sent once — never re-sent (which would paste the kickoff twice) and never
// recorded as "not accepted" (resend-safe). The control: the same refusal on the
// FIRST request is re-sent, as before.
func TestAToolTurnRefusedAfterItsFirstRequestIsNeverResent(t *testing.T) {
	busy := &agents.RuntimeError{Status: 409, Type: "busy", Message: "another turn is already being driven"}
	h, gw, _ := toolHarness(t, ownedRow(), &agents.AfterFirstRequestError{Request: 2, Err: busy})
	log := h.tick(t)
	if gw.toolCallCount() != 1 {
		t.Fatalf("a refusal after request 1 was re-sent: %d tool turns.%s", gw.toolCallCount(), transcript(log))
	}
	r := h.store.row(7301)
	if !agents.KickoffFailed(r) || strings.HasPrefix(r.KickoffError, agents.KickoffBusyReason) ||
		strings.HasPrefix(r.KickoffError, agents.KickoffNotAcceptedReason) {
		t.Fatalf("kickoff_error %q; want a failed turn, not a resend-safe refusal", r.KickoffError)
	}

	h, gw, _ = toolHarness(t, ownedRow(), busy)
	log = h.tick(t)
	if gw.toolCallCount() != 2 {
		t.Fatalf("control: a first-request refusal was not re-sent: %d tool turns.%s", gw.toolCallCount(), transcript(log))
	}
	if r := h.store.row(7301); agents.KickoffFailed(r) {
		t.Fatalf("control: kickoff_error %q after the re-send landed", r.KickoffError)
	}
}

// TestAToolTurnFallsBackToolessOnlyOnAFirstRequest404: a runtime that lacks
// /v1/responses (a 404 on the first request) gets the toolless turn — the only
// one it can take — once; a 404 on a later request (the message was delivered
// and tools ran) is a failure, with no second, toolless copy of the kickoff.
func TestAToolTurnFallsBackToolessOnlyOnAFirstRequest404(t *testing.T) {
	h, gw, _ := toolHarness(t, ownedRow(), agents.ErrResponsesUnsupported)
	log := h.tick(t)
	if gw.toolCallCount() != 1 || h.gw.callCount() != 1 {
		t.Fatalf("first-request 404: %d tool turn(s), %d toolless; want 1 and 1.%s", gw.toolCallCount(), h.gw.callCount(), transcript(log))
	}
	if r := h.store.row(7301); agents.KickoffFailed(r) {
		t.Fatalf("first-request 404: kickoff_error %q; the toolless turn answered", r.KickoffError)
	}

	h, gw, _ = toolHarness(t, ownedRow(), &agents.AfterFirstRequestError{Request: 2, Err: agents.ErrResponsesUnsupported})
	log = h.tick(t)
	if gw.toolCallCount() != 1 || h.gw.callCount() != 0 {
		t.Fatalf("later-request 404: %d tool turn(s), %d toolless; want 1 and 0.%s", gw.toolCallCount(), h.gw.callCount(), transcript(log))
	}
	if r := h.store.row(7301); !agents.KickoffFailed(r) {
		t.Fatal("later-request 404: not recorded as a failed kickoff")
	}
}

// TestTurnToolsNeedAGatewayThatCanSendThem: New refuses TurnTools over a gateway
// with no SendWithTools — the tools would otherwise be configured and silently
// never sent, the incident's observable.
func TestTurnToolsNeedAGatewayThatCanSendThem(t *testing.T) {
	tf := &toolsFor{}
	_, err := New(Config{Store: newFakeStore(&recorder{}), Instances: &fakeInstances{}, Gateway: &fakeGateway{log: &recorder{}},
		TurnTools: tf.turnTools})
	if err == nil || !strings.Contains(err.Error(), "SendWithTools") {
		t.Fatalf("New = %v; want a refusal naming SendWithTools", err)
	}
	if _, err := New(Config{Store: newFakeStore(&recorder{}), Instances: &fakeInstances{}, Gateway: &fakeGateway{log: &recorder{}}}); err != nil {
		t.Fatalf("control: no TurnTools: %v", err)
	}
}
