package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/router"
)

// ---------------------------------------------------------------------------
// THE APPROVAL RELAY'S OWN GATE (MUSTER_APPROVAL_GATE_URL / _TOKEN), DRIVEN
// THROUGH THE BINARY'S WIRING.
//
// The operator's decision is that muster stays STANDALONE. These tests pin that
// the dedicated gate (a) boots beside MUSTER_STANDALONE, (b) is what the relay
// files through, (c) is reached ONLY on the gate door, never on a router route,
// and (d) when absent leaves the relay answering 503.
// ---------------------------------------------------------------------------

// fakeGateDoor is the permission router as the dedicated gate sees it: it
// records every request and answers the gate door like the real one.
type fakeGateDoor struct {
	mu    sync.Mutex
	paths []string
	token []string
	actor []string
}

func (f *fakeGateDoor) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		f.token = append(f.token, r.Header.Get("Authorization")+"|"+r.Header.Get(router.TokenHeader))
		f.actor = append(f.actor, r.Header.Get(router.ActorHeader))
		f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/gate":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"requestId":"req-gate-1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/gate/req-gate-1":
			_, _ = w.Write([]byte(`{"state":"decided","response":"approve"}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/gate/"):
			_, _ = w.Write([]byte(`{"deleted":true}`))
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	})
}

func (f *fakeGateDoor) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// seedRelayAgent inserts an agent row whose own token the relay will accept.
func seedRelayAgent(t *testing.T, dsn, name, token string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `DELETE FROM agents WHERE name=$1`, name); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agents (name, namespace, hooks_token) VALUES ($1, $2, $3)`,
		name, "ns-"+name, token); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
}

func relayCall(t *testing.T, method, url, body, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("X-Muster-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const gateTestAgentToken = "gate-test-agent-own-token-0123456789"

// Standalone + the dedicated gate: validates, boots ready, and the relay files
// through THAT gate with THAT credential and muster's actor.
func TestAStandaloneMusterWithAnApprovalGateBootsAndRelaysThroughIt(t *testing.T) {
	door := &fakeGateDoor{}
	gate := httptest.NewServer(door.handler())
	defer gate.Close()

	cfg := dbConfig(t)
	cfg.Standalone = true
	cfg.ApprovalGateURL = gate.URL
	cfg.ApprovalGateToken = "approval-gate-token-0123456789"
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate refused standalone + approval gate: %v", err)
	}
	seedRelayAgent(t, cfg.Database, "gate-test-agent", gateTestAgentToken)
	base := startServer(t, cfg)

	if code, body := readyz(t, base); code != http.StatusOK {
		t.Fatalf("/readyz = %d (%v): standalone + approval gate must be a ready deployment", code, body)
	}
	code, body := relayCall(t, "POST", base+"/agent/approvals", `{"tool":"Bash","command":"rm -rf build"}`, gateTestAgentToken)
	if code != http.StatusCreated || !strings.Contains(body, "req-gate-1") {
		t.Fatalf("POST /agent/approvals = %d %s, want 201 through the dedicated gate", code, body)
	}
	code, body = relayCall(t, "GET", base+"/agent/approvals/req-gate-1", "", gateTestAgentToken)
	if code != http.StatusOK || !strings.Contains(body, `"response":"approve"`) {
		t.Fatalf("GET /agent/approvals/{id} = %d %s", code, body)
	}
	door.mu.Lock()
	defer door.mu.Unlock()
	for i, tok := range door.token {
		if !strings.Contains(tok, cfg.ApprovalGateToken) || door.actor[i] != defaultRouterActor {
			t.Fatalf("request %d reached the gate with credential %q actor %q", i, tok, door.actor[i])
		}
	}
}

// 🔴 THE GATE CONFIG TURNS ON NO ROUTER BEHAVIOUR. With the dedicated gate set
// and a board read, an agent-list read and a relay round-trip performed, the
// fake router sees ONLY the gate door — no session-liveness lookup, no event
// publish, no notification.
func TestTheApprovalGateDoesNotTurnOnRouterBehaviour(t *testing.T) {
	door := &fakeGateDoor{}
	gate := httptest.NewServer(door.handler())
	defer gate.Close()

	cfg := dbConfig(t)
	cfg.Standalone = true
	cfg.ApprovalGateURL = gate.URL
	cfg.ApprovalGateToken = "approval-gate-token-0123456789"

	// The liveness decision itself: still the standalone probe.
	port := router.NewPort(router.Config{BaseURL: cfg.RouterURL, Token: cfg.RouterToken, Actor: cfg.RouterActor})
	if _, ok := buildSessionLiveness(cfg, port, quietLogger()).(standaloneSessionLiveness); !ok {
		t.Fatal("with the approval gate set, session liveness is not the standalone probe")
	}

	// The wiring as the binary itself reports it: buildApp's banner is printed
	// from the router.Port the server was actually given, so a gate config that
	// leaked into the router client shows here as "permission router: CONFIGURED"
	// even though no route the test can drive would call it. (Added after the
	// mutant that built the router client from the gate variables SURVIVED the
	// traffic check below.)
	var buf strings.Builder
	app, err := buildApp(context.Background(), cfg, log.New(&buf, "", 0))
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	app.Close()
	if !strings.Contains(buf.String(), "permission router: NONE, DECLARED") ||
		!strings.Contains(buf.String(), "approval relay gate: DEDICATED") {
		t.Fatalf("with the approval gate set the binary wired a router (or no dedicated gate):\n%s", buf.String())
	}

	seedRelayAgent(t, cfg.Database, "gate-test-agent", gateTestAgentToken)
	base := startServer(t, cfg)
	for _, p := range []string{"/readyz", "/health"} {
		if resp, err := http.Get(base + p); err == nil {
			resp.Body.Close()
		}
	}
	relayCall(t, "POST", base+"/agent/approvals", `{"tool":"Bash","command":"ls"}`, gateTestAgentToken)
	relayCall(t, "GET", base+"/agent/approvals/req-gate-1", "", gateTestAgentToken)
	seen := door.seen()
	if len(seen) == 0 {
		t.Fatal("positive control FAILED: the fake gate saw no request, so this test measures nothing")
	}
	for _, p := range seen {
		if !strings.Contains(p, " /api/gate") {
			t.Fatalf("the router was called on %q: the approval gate switched on router behaviour (all: %v)", p, seen)
		}
	}
}

// Standalone with no gate: the relay answers 503, so the pod's hook defers to
// the terminal prompt.
func TestAStandaloneMusterWithoutAnApprovalGateAnswers503(t *testing.T) {
	cfg := dbConfig(t)
	cfg.Standalone = true
	seedRelayAgent(t, cfg.Database, "gate-test-agent", gateTestAgentToken)
	base := startServer(t, cfg)
	code, body := relayCall(t, "POST", base+"/agent/approvals", `{"tool":"Bash","command":"ls"}`, gateTestAgentToken)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("POST /agent/approvals = %d %s, want 503", code, body)
	}
}

func TestHalfAnApprovalGateIsRefusedAtBoot(t *testing.T) {
	for name, c := range map[string]config{
		"url only":   {Database: "postgres://x/y", Standalone: true, ApprovalGateURL: "https://gate.example.invalid"},
		"token only": {Database: "postgres://x/y", Standalone: true, ApprovalGateToken: "t"},
	} {
		t.Run(name, func(t *testing.T) {
			err := c.validate()
			if err == nil || !strings.Contains(err.Error(), envApprovalGateURL) || !strings.Contains(err.Error(), envApprovalGateToken) {
				t.Fatalf("validate = %v, want a refusal naming both variables", err)
			}
		})
	}
	// Control: neither, and both, are accepted beside standalone.
	for _, c := range []config{
		{Database: "postgres://x/y", Standalone: true},
		{Database: "postgres://x/y", Standalone: true, ApprovalGateURL: "https://g.example.invalid", ApprovalGateToken: "t"},
	} {
		if err := c.validate(); err != nil {
			t.Fatalf("validate(%+v) = %v, want accepted", c, err)
		}
	}
}

// Which gate the relay gets: dedicated wins, then the router, then none — and
// "none" is an untyped nil, never an unconfigured client in an interface.
func TestApprovalGateSelection(t *testing.T) {
	routerPort := router.NewPort(router.Config{BaseURL: "https://router.example.invalid", Token: "r", Actor: "muster"})
	unset := router.NewPort(router.Config{})
	dedicated := config{ApprovalGateURL: "https://gate.example.invalid", ApprovalGateToken: "g", RouterActor: "muster"}

	g, src := approvalGateFor(dedicated, routerPort)
	if src != approvalGateDedicated || g == nil {
		t.Fatalf("dedicated + router: %q %v, want dedicated", src, g)
	}
	if p, ok := g.(router.Port); !ok || p.C == routerPort.C {
		t.Fatal("the dedicated gate reused the router's client")
	}
	if g, src = approvalGateFor(config{RouterActor: "muster"}, routerPort); src != approvalGateRouter || g == nil {
		t.Fatalf("router only: %q %v, want router", src, g)
	}
	if g, src = approvalGateFor(config{RouterActor: "muster"}, unset); src != approvalGateNone || g != nil {
		t.Fatalf("neither: %q %#v, want none and an untyped nil", src, g)
	}
	var _ api.GatePort = g
}

// Each arm of the relay-gate banner line, rendered: the ledger test covers the
// dedicated and none arms; this pins all three by STATE word.
func TestTheApprovalGateBannerNamesWhichGateTheRelayUses(t *testing.T) {
	for want, cfg := range map[string]config{
		"approval relay gate: DEDICATED":               {Standalone: true, RouterActor: "muster", ApprovalGateURL: "https://g.example.invalid", ApprovalGateToken: "g"},
		"approval relay gate: THE PERMISSION ROUTER'S": {RouterURL: "https://r.example.invalid", RouterToken: "r", RouterActor: "muster"},
		"approval relay gate: NONE":                    {Standalone: true, RouterActor: "muster"},
	} {
		out := renderBanner(t, cfg, api.Extensions{})
		if n := strings.Count(out, "approval relay gate:"); n != 1 || !strings.Contains(out, want) {
			t.Errorf("want exactly one relay-gate line, %q; got %d in:\n%s", want, n, out)
		}
	}
}
