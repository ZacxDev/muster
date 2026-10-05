package api

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// ---------------------------------------------------------------------------
// THE SEAM BETWEEN chatTurn's TOOLS FALLBACK AND Gateway.Chat's TRANSPORT CHOICE.
//
// 🔴 THESE ARE TWO DIFFERENT FALLBACKS AND THE HAZARD IS READING THEM AS ONE.
// Gateway.Chat moved onto /v1/responses, falling back to /v1/chat/completions
// internally on a 404, because the streaming chat-completions transport drops every
// content delta for a reasoning model (the measurement is on
// agentgateway.Gateway.Chat). That makes chatTurn's own branch LOOK like a retry of
// the endpoint that just 404'd — and the obvious tidy-up is to delete it. What the
// branch actually does is drop the TOOLS, which api.Gateway's doc puts here because
// this package is the only place that knows a tool-less answer is acceptable for an
// interactive turn. Deleting it turns every chat turn against an older agent image
// from "answers without tools" into an error.
//
// ⚠ WHAT THESE TESTS CAN AND CANNOT SHOW. They drive chatTurn over a recording
// gateway, so they pin WHICH METHOD the branch reaches and on which trigger. They
// say nothing about what either method puts on the wire — that is
// internal/agentgateway's suite, and neither is sufficient alone: every assertion
// about the transport was scoped to one package until this file existed.
//
// 🔴 THEY ARE INVARIANT GUARDS, NOT REGRESSION COVERAGE, AND THE DIFFERENCE IS
// MEASURED RATHER THAN ASSERTED: all three were run against origin/main before the
// transport change and all three PASSED there. The empty-reply defect never violated
// this relationship — it was one layer down, in which transport Gateway.Chat drove.
// What these pin is the invariant the FIX could break and no existing test would
// have noticed, which is a different and weaker claim than "this test would have
// caught the bug". The test that fails on pre-change code is
// agentgateway.TestAToollessTurnDoesNotRunOverTheTransportThatLosesAReasoningModelsAnswer.
// ---------------------------------------------------------------------------

// recordingGateway counts both chat methods and answers whatever a test tells it to.
// The two replies are pairwise distinct so which method produced an answer is
// readable off the answer itself, not only off the counters.
type recordingGateway struct {
	toolErr    error
	toolReply  string
	plainReply string

	toolCalls  int
	plainCalls int
}

func (g *recordingGateway) Chat(context.Context, agents.Agent, string, string, func(string)) (string, error) {
	g.plainCalls++
	return g.plainReply, nil
}

func (g *recordingGateway) ChatWithTools(context.Context, agents.Agent, string, string, string, []agents.ToolDef, agents.ToolDispatch, agents.StreamEmit) (string, error) {
	g.toolCalls++
	return g.toolReply, g.toolErr
}

// turnServer is the smallest Server chatTurn needs: a logger and a wired gateway.
func turnServer(gw Gateway) *Server {
	return &Server{
		logger: log.New(io.Discard, "", 0),
		ext:    Extensions{Gateway: gw},
	}
}

// TestTheChatTurnStillDegradesToAToollessTurnWhenTheResponsesEndpointIsAbsent is
// the guard against deleting the branch as "redundant".
func TestTheChatTurnStillDegradesToAToollessTurnWhenTheResponsesEndpointIsAbsent(t *testing.T) {
	gw := &recordingGateway{
		toolErr:    agents.ErrResponsesUnsupported,
		plainReply: "answering without tools",
	}
	reply, err := turnServer(gw).chatTurn(context.Background(), agents.Agent{ID: 7, Name: "swift-otter"},
		"sess-41", "are you there?", nil)
	if err != nil {
		t.Fatalf("the turn was LOST rather than degraded: %v\n"+
			"  internal/agentspec takes the agent image tag from configuration and has no "+
			"default, so an image that answers 404 on /v1/responses is still reachable by a "+
			"deployment and this path is not dead.", err)
	}
	if reply != gw.plainReply {
		t.Errorf("reply = %q, want %q — the tool-less retry's answer", reply, gw.plainReply)
	}
	if gw.toolCalls != 1 {
		t.Errorf("ChatWithTools was called %d time(s), want 1: tools are tried FIRST", gw.toolCalls)
	}
	if gw.plainCalls != 1 {
		t.Errorf("Chat was called %d time(s), want exactly 1. This branch drops the TOOLS, not a "+
			"transport: Gateway.Chat picks its own endpoint and falls back to chat-completions "+
			"internally on a 404, so a reading of this branch as 'retrying the endpoint that just "+
			"404'd' is wrong and deleting it loses every turn against an older image",
			gw.plainCalls)
	}
}

// TestTheChatTurnDoesNotDropToolsOnAnyErrorBUTTheMissingEndpoint is the negative
// control, and without it the test above passes over a branch with no condition.
//
// 🔴 A 400 IS THE CASE THAT MATTERS, AND IT IS A REAL ONE. agents.ToolDef's shape is
// runtime-version-specific and the two forms are each other's HTTP 400, so a wrong
// image answers 400 rather than 404 — and ErrResponsesUnsupported is a 404 ONLY.
// Degrading there would silently deliver a tool-less answer for every turn an agent
// on the wrong image ever runs, which reads as a model that ignores its tools.
func TestTheChatTurnDoesNotDropToolsOnAnyErrorBUTTheMissingEndpoint(t *testing.T) {
	wireRefusal := errors.New("responses HTTP 400: tools.0.function: expected object")
	gw := &recordingGateway{toolErr: wireRefusal, plainReply: "answering without tools"}
	_, err := turnServer(gw).chatTurn(context.Background(), agents.Agent{ID: 7, Name: "swift-otter"},
		"sess-41", "are you there?", nil)
	if !errors.Is(err, wireRefusal) {
		t.Errorf("a 400 from the tool path did not reach the caller (got %v) — the operator needs "+
			"the runtime's own message to tell a version mismatch from a malformed request", err)
	}
	if gw.plainCalls != 0 {
		t.Errorf("Chat was called %d time(s) for a non-404 failure, want 0: the degrade keys on "+
			"ErrResponsesUnsupported, and widening it hides a wrong agent image behind answers "+
			"that merely look tool-less", gw.plainCalls)
	}
	// 🔴 POSITIVE CONTROL ON THAT ZERO: the same gateway must be ABLE to reach Chat,
	// or "0 calls" is indistinguishable from a stub nothing can call.
	gw.toolErr = agents.ErrResponsesUnsupported
	if _, err := turnServer(gw).chatTurn(context.Background(), agents.Agent{ID: 7, Name: "swift-otter"},
		"sess-41", "are you there?", nil); err != nil {
		t.Fatalf("positive control FAILED: %v", err)
	}
	if gw.plainCalls != 1 {
		t.Errorf("positive control FAILED: Chat recorded %d call(s) on the 404 trigger, so the "+
			"zero above was not evidence of anything", gw.plainCalls)
	}
}

// TestASuccessfulToolTurnNeverTouchesTheToollessPath pins the ordinary case, which
// is the one a widened condition would break without any test noticing.
func TestASuccessfulToolTurnNeverTouchesTheToollessPath(t *testing.T) {
	gw := &recordingGateway{toolReply: "on it, reading the task now", plainReply: "answering without tools"}
	reply, err := turnServer(gw).chatTurn(context.Background(), agents.Agent{ID: 7, Name: "swift-otter"},
		"sess-41", "read your task", nil)
	if err != nil {
		t.Fatalf("chatTurn: %v", err)
	}
	if reply != gw.toolReply {
		t.Errorf("reply = %q, want the TOOL turn's answer %q", reply, gw.toolReply)
	}
	if gw.plainCalls != 0 {
		t.Errorf("Chat was called %d time(s) on a successful tool turn, want 0 — a turn that "+
			"silently lost its tools produces an agent that cannot read the task it was "+
			"dispatched for", gw.plainCalls)
	}
}
