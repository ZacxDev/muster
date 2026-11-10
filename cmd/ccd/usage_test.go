package main

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/podagent"
)

// The fixture turn (turn_success_tools_2.1.289.jsonl) made TWO API calls: the
// first's usage is repeated on its four block records (thinking, text, text,
// tool_use) — input 4, cache creation 3550, cache read 245219, output 829 —
// and the second's on its two (thinking, text) — input 2, cache creation
// 1201, cache read 248769, output 117. The earlier turn in the file is not
// this one's. Counted once per call:
var fixtureTurnUsage = podagent.Usage{InputTokens: 6, OutputTokens: 946, CacheReadInputTokens: 493988, CacheCreationInputTokens: 4751}

// TestExtractTurnSumsTheTurnsUsageOncePerAPICall pins the per-turn tokens read
// from a real (content-replaced) transcript.
func TestExtractTurnSumsTheTurnsUsageOncePerAPICall(t *testing.T) {
	res := extract(t, fixture(t, "turn_success_tools_2.1.289.jsonl"), fixturePromptID)
	if res.Usage == nil || *res.Usage != fixtureTurnUsage {
		t.Fatalf("the turn's usage = %+v, want %+v", res.Usage, fixtureTurnUsage)
	}
	// A failed turn's API-error record carries no usage worth counting.
	if bad := extract(t, fixture(t, "turn_auth_failed_2.1.296.jsonl"), fixturePromptID); bad.Usage != nil {
		t.Fatalf("an auth-failed turn's usage = %+v, want none", bad.Usage)
	}
}

// TestMustersClientReceivesTheTurnsUsage is the contract: ccd's response for
// that turn, read by muster's own client, fills the turn's usage sink with
// exactly those numbers — the Anthropic split, taken as it is.
func TestMustersClientReceivesTheTurnsUsage(t *testing.T) {
	srv, _ := newScripted(t, cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl"), stopEvent: "Stop"})
	gw := mustersGateway(t, gatewayServer(t, srv).URL)
	ctx, sink := agents.WithTurnUsage(context.Background())
	if _, err := gw.Chat(ctx, contractAgent, "session-1", "list the workspace", nil); err != nil {
		t.Fatal(err)
	}
	in, out, read, create, known := sink.Snapshot()
	if !known || in != 6 || out != 946 || read != 493988 || create != 4751 {
		t.Fatalf("muster's sink = in %d out %d read %d create %d known %v; want 6 946 493988 4751 true", in, out, read, create, known)
	}
}
