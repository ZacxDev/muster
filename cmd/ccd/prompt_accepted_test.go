package main

import (
	"context"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// ccd's 103 "the CLI took the prompt" (headerCCPromptAccepted), read by
// muster's OWN client (agents.WithTurnAccepted, via agentgateway.Gateway.Chat):
// muster marks an owner's turn in flight on it, so it must fire exactly when
// the CLI accepted the pasted text — and never for text ccd refused.

// TestThePromptAcceptedHeaderIsMusters pins the wire name to muster's.
func TestThePromptAcceptedHeaderIsMusters(t *testing.T) {
	if headerCCPromptAccepted != agents.HeaderCCPromptAccepted {
		t.Fatalf("ccd sends %q, muster reads %q", headerCCPromptAccepted, agents.HeaderCCPromptAccepted)
	}
}

// acceptedCount runs one turn through muster's client and counts the
// accepted signals it saw (at most one, by WithTurnAccepted's contract).
func acceptedCount(t *testing.T, srv *server) (int32, error) {
	t.Helper()
	var n atomic.Int32
	ctx := agents.WithTurnAccepted(context.Background(), func(a agents.TurnAcceptance) {
		if a.Lane == "" { // the primary: Chat names no lane
			n.Add(1)
		}
	})
	_, err := mustersGateway(t, gatewayServer(t, srv).URL).Chat(ctx, contractAgent, "s", "dispatch it", nil)
	return n.Load(), err
}

// TestTheCLIAcceptingThePromptSignalsBeforeTheTurnEnds: a turn the CLI took and
// NEVER finishes (no Stop: ccd answers 504 turn_timeout) is still marked
// accepted — so the signal is the 103 at submit, not the final 200. The
// completed turn is marked too (control).
func TestTheCLIAcceptingThePromptSignalsBeforeTheTurnEnds(t *testing.T) {
	srv, _ := newScripted(t, cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl")}) // never stops
	srv.cfg.TurnTimeout = 300e6
	n, err := acceptedCount(t, srv)
	if err == nil || !strings.Contains(err.Error(), "HTTP 504") {
		t.Fatalf("a turn that never stops: err %v, want ccd's 504", err)
	}
	if n != 1 {
		t.Fatalf("a prompt the CLI accepted signalled %d time(s), want 1 (before the turn ended)", n)
	}

	done, _ := newScripted(t, cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl"), stopEvent: "Stop"})
	if n, err := acceptedCount(t, done); err != nil || n != 1 {
		t.Fatalf("a completed turn: %d signal(s), err %v; want 1, nil", n, err)
	}
}

// TestNo103UnlessAsked: a caller that does not ask (no
// headerCCPromptAccepted on the request) gets exactly one answer, the final
// one, as before; asking gets the 103 first (control).
func TestNo103UnlessAsked(t *testing.T) {
	for _, ask := range []bool{false, true} {
		srv, _ := newScripted(t, cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl"), stopEvent: "Stop"})
		bearer, err := resolveBearer(knownHooksToken, "")
		if err != nil {
			t.Fatal(err)
		}
		var early atomic.Int32
		ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
			Got1xxResponse: func(int, textproto.MIMEHeader) error { early.Add(1); return nil }})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, gatewayServer(t, srv).URL+"/v1/responses",
			strings.NewReader(`{"input":"hi","stream":false}`))
		req.Header.Set("Authorization", "Bearer "+bearer)
		if ask {
			req.Header.Set(headerCCPromptAccepted, "1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		want := int32(0)
		if ask {
			want = 1
		}
		if resp.StatusCode != http.StatusOK || early.Load() != want {
			t.Fatalf("asked=%v: final %d, %d informational answer(s); want 200 and %d", ask, resp.StatusCode, early.Load(), want)
		}
	}
}

// TestARefusedPromptNeverSignalsAccepted: the session busy with a turn ccd did
// not paste (409 busy — the scenario where a CLI keeps running a turn ccd
// already gave up on), and a paste the CLI never submitted (504
// not_submitted) — neither reached the model, and neither signals.
func TestARefusedPromptNeverSignalsAccepted(t *testing.T) {
	busy, cli := newScripted(t, cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl"), stopEvent: "Stop"})
	busy.onHook(hookEvent{Event: "UserPromptSubmit", SessionID: fixtureSessionID, PromptID: fixturePromptID, Prompt: "typed in the terminal"})
	n, err := acceptedCount(t, busy)
	if err == nil || !strings.Contains(err.Error(), "HTTP 409") {
		t.Fatalf("a busy session: err %v, want ccd's 409", err)
	}
	if n != 0 || len(cli.pasted) != 0 {
		t.Fatalf("a busy session signalled %d time(s) and pasted %q; want neither", n, cli.pasted)
	}

	unsent, _ := newScripted(t, cliScript{noSubmit: true})
	unsent.cfg.SubmitTimeout = 300e6
	if n, err := acceptedCount(t, unsent); err == nil || n != 0 {
		t.Fatalf("a paste never submitted: %d signal(s), err %v; want 0 and an error", n, err)
	}
}
