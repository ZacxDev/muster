package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// clearRecorder is the agents store as the gateway sees it: only
// ClearKickoffFailure is implemented; any other call is a nil-interface panic
// (stubStore's convention).
type clearRecorder struct {
	agents.Store
	mu  sync.Mutex
	ids []int64
	// tok is every agent's stored hooks token (Secrets).
	tok string
}

func (s *clearRecorder) Secrets(context.Context, int64) (agents.Secrets, error) {
	return agents.Secrets{HooksToken: s.tok}, nil
}

func (s *clearRecorder) ClearKickoffFailure(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids = append(s.ids, id)
	return nil
}

// endpointDriver is a provision.Provisioner whose only reachable method is
// Endpoint, pointing at a test server.
type endpointDriver struct {
	provision.Provisioner
	ep provision.Endpoint
}

func (d endpointDriver) Endpoint(context.Context, provision.Ref) (provision.Endpoint, error) {
	return d.ep, nil
}

// TestTheBuiltGatewayClearsAStaleKickoffFailureThroughTheStore pins the WIRING
// half of the stale "kickoff failed" fix: agentgateway clears the record only if
// it was handed a clearer, and buildGatewayWith is what hands it the agents
// store. Without this, the gateway's own test passes over a production gateway
// built with Kickoff nil — a fix that is inert where it ships.
func TestTheBuiltGatewayClearsAStaleKickoffFailureThroughTheStore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.completed\n")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"output":[{"type":"message",`+
			`"role":"assistant","content":[{"type":"output_text","text":"done"}]}]}}`+"\n\n")
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	p, _ := strconv.Atoi(port)

	store := &clearRecorder{tok: "0123456789abcdef"}
	gw, err := buildGatewayWith(gatewayTestConfig(provisionerNoop, gatewayHooksSHA256),
		endpointDriver{ep: provision.Endpoint{Scheme: "http", Host: host, Port: p}}, nil, store)
	if err != nil || gw == nil {
		t.Fatalf("instrument check FAILED: buildGatewayWith built no gateway (err %v)", err)
	}
	a := agents.Agent{ID: 5813, Name: "wiring-heron", Namespace: "muster-agent-wiring-heron",
		HooksToken: "0123456789abcdef", KickedOff: true, KickoffError: "first turn timed out"}
	if reply, err := gw.Chat(context.Background(), a, "k", "status?", nil); err != nil || reply != "done" {
		t.Fatalf("instrument check FAILED: the turn did not complete (reply %q, err %v)", reply, err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.ids) != 1 || store.ids[0] != a.ID {
		t.Fatalf("a completed turn on the built gateway cleared kickoff failures for %v, want [%d]: "+
			"buildGatewayWith did not wire the agents store as the gateway's Kickoff", store.ids, a.ID)
	}
}

// TestTheBuiltGatewayReadsTheAgentsSecretsThroughTheStore pins the WIRING half
// of the agent secrets split (team sharing phase 4, slice 4.2): an ordinary
// read of an agents row carries no credentials, so a turn is only possible if
// buildGatewayWith hands the gateway the store as its Secrets. The agent here
// is what such a read returns (no HooksToken); the turn must still go out, and
// with the bearer derived from the token the STORE holds.
func TestTheBuiltGatewayReadsTheAgentsSecretsThroughTheStore(t *testing.T) {
	const tok = "fixture-stored-hooks-token-4c1e"
	var mu sync.Mutex
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.completed\n")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"output":[{"type":"message",`+
			`"role":"assistant","content":[{"type":"output_text","text":"done"}]}]}}`+"\n\n")
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	p, _ := strconv.Atoi(port)
	driver := endpointDriver{ep: provision.Endpoint{Scheme: "http", Host: host, Port: p}}
	a := agents.Agent{ID: 7341, Name: "wiring-wren", Namespace: "muster-agent-wiring-wren", KickedOff: true}

	gw, err := buildGatewayWith(gatewayTestConfig(provisionerNoop, gatewayHooksSHA256), driver, nil, &clearRecorder{tok: tok})
	if err != nil || gw == nil {
		t.Fatalf("instrument check FAILED: buildGatewayWith built no gateway (err %v)", err)
	}
	if reply, err := gw.Chat(context.Background(), a, "k", "status?", nil); err != nil || reply != "done" {
		t.Fatalf("a turn on an agent read without credentials failed (reply %q, err %v): "+
			"buildGatewayWith did not wire the agents store as the gateway's Secrets", reply, err)
	}
	// The hooks-sha256 wire contract (agentgateway/runtime.go), spelled out.
	sum := sha256.Sum256([]byte("gw-" + tok))
	want := "Bearer " + hex.EncodeToString(sum[:])
	mu.Lock()
	defer mu.Unlock()
	if got != want {
		t.Fatalf("the turn's Authorization was %q, want the bearer derived from the STORED token %q", got, want)
	}

	// Control: the same gateway with no store has no way to the credentials, and
	// says so — the case above is not passing on a credential from elsewhere.
	bare, err := buildGatewayWith(gatewayTestConfig(provisionerNoop, gatewayHooksSHA256), driver, nil, nil)
	if err != nil || bare == nil {
		t.Fatalf("instrument check FAILED: buildGatewayWith built no gateway (err %v)", err)
	}
	if _, err := bare.Chat(context.Background(), a, "k", "status?", nil); err == nil ||
		!strings.Contains(err.Error(), "has no hooks token") {
		t.Fatalf("control: a gateway without the store sent a turn for a credential-less row (err %v)", err)
	}
}
