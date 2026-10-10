package main

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// THE CONTRACT TEST: ccd's /v1/responses driven by muster's OWN client —
// agentgateway.Gateway.Chat, the path muster's chat box and agentkickoff use, with
// the real HooksSHA256 runtime deriving the bearer from the agent row's token.
// Nothing here re-implements muster's side of the wire, which is the reason ccd
// lives in this module.

type staticEndpoint provision.Endpoint

func (s staticEndpoint) Endpoint(context.Context, provision.Ref) (provision.Endpoint, error) {
	return provision.Endpoint(s), nil
}

func mustersGateway(t *testing.T, url string) *agentgateway.Gateway {
	t.Helper()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := strconv.Atoi(port)
	gw, err := agentgateway.New(agentgateway.Config{
		Driver:  staticEndpoint{Host: host, Port: p},
		Runtime: agentgateway.HooksSHA256(),
		Model:   "ccd-ignores-this",
	})
	if err != nil {
		t.Fatal(err)
	}
	return gw
}

var contractAgent = agents.Agent{ID: 7, Name: "cc-agent", HooksToken: knownHooksToken}

func TestMustersClientReceivesTheReplyText(t *testing.T) {
	srv, cli := newScripted(t, cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl"), stopEvent: "Stop"})
	gw := mustersGateway(t, gatewayServer(t, srv).URL)

	var deltas []string
	reply, err := gw.Chat(context.Background(), contractAgent, "session-1", "list the workspace",
		func(s string) { deltas = append(deltas, s) })
	if err != nil {
		t.Fatal(err)
	}
	want := "First segment: I will list the workspace.\n\nSecond segment: the workspace holds file-a and file-b."
	if reply != want {
		t.Fatalf("muster assembled %q,\nwant %q", reply, want)
	}
	// The live stream muster forwards to the UI renders the same text: muster
	// inserts its own paragraph break between output items.
	if got := strings.Join(deltas, ""); got != want {
		t.Fatalf("streamed deltas render %q, want %q", got, want)
	}
	if len(cli.pasted) != 1 || cli.pasted[0] != "list the workspace" {
		t.Fatalf("pasted %q", cli.pasted)
	}
}

// The negative half: an is_error turn must surface in muster's client as an
// ERROR — not as ("", nil), which every muster caller reads as "the agent
// answered nothing".
func TestAnIsErrorTurnIsAnErrorInMustersClient(t *testing.T) {
	cases := []struct {
		file   string
		status string
		typ    string
	}{
		{"turn_auth_failed_2.1.296.jsonl", "HTTP 502", failAuth},
		{"turn_rate_limited.jsonl", "HTTP 429", failRateLimited},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			srv, _ := newScripted(t, cliScript{transcript: fixture(t, c.file), stopEvent: "StopFailure"})
			reply, err := mustersGateway(t, gatewayServer(t, srv).URL).Chat(context.Background(),
				contractAgent, "s", "hi", nil)
			if err == nil {
				t.Fatalf("muster's client got success %q for a failed turn", reply)
			}
			if reply != "" || !strings.Contains(err.Error(), c.status) || !strings.Contains(err.Error(), `"type":"`+c.typ+`"`) {
				t.Fatalf("reply %q err %v", reply, err)
			}
		})
	}
}

// muster derives the bearer from the row's token; a row with a different token
// must be refused before anything reaches the session.
func TestMustersClientWithTheWrongTokenIsRefused(t *testing.T) {
	srv, cli := newScripted(t, cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl"), stopEvent: "Stop"})
	other := contractAgent
	other.HooksToken = "some-other-agents-token"
	_, err := mustersGateway(t, gatewayServer(t, srv).URL).Chat(context.Background(), other, "s", "hi", nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("err = %v", err)
	}
	if len(cli.pasted) != 0 {
		t.Fatal("a wrongly-authenticated turn reached the session")
	}
}
