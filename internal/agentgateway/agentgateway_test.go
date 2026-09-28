package agentgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// WHAT THESE TESTS CAN AND CANNOT SHOW.
//
// 🔴 THEY DRIVE AN httptest SERVER, WHICH ANSWERS WHATEVER IT IS ASKED. That makes
// them the right instrument for one question — what does this package put on the
// wire — and the wrong instrument for "does a real agent answer". The OWED record
// in internal/agents/responses.go named the stricter condition deliberately: one
// real turn against a LIVE runtime. liveruntime_test.go is that, behind
// `-tags liveenv`, and neither file is sufficient alone.
//
// 🔴 SO WHAT IS ASSERTED HERE IS THE THREE INPUTS THIS PACKAGE EXISTS TO SUPPLY —
// the URL it derived, the bearer it derived, and the model sentinel — read off the
// REQUEST rather than off the reply. A test that asserted only the assistant text
// would pass with every one of them wrong, because the fake answers regardless.
// ---------------------------------------------------------------------------

// fixedResolver answers with one endpoint and records the ref it was asked about.
type fixedResolver struct {
	ep   provision.Endpoint
	err  error
	refs []provision.Ref
}

func (r *fixedResolver) Endpoint(_ context.Context, ref provision.Ref) (provision.Endpoint, error) {
	r.refs = append(r.refs, ref)
	if r.err != nil {
		return provision.Endpoint{}, r.err
	}
	return r.ep, nil
}

// endpointOf turns an httptest server's URL into a provision.Endpoint, so the
// gateway derives its own URL from the endpoint exactly as it does in production
// rather than being handed one.
func endpointOf(t *testing.T, raw string) provision.Endpoint {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing the test server URL %q: %v", raw, err)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("splitting %q: %v", u.Host, err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("port %q: %v", port, err)
	}
	return provision.Endpoint{Scheme: u.Scheme, Host: host, Port: p}
}

// capturedRequest is what the fake runtime saw.
type capturedRequest struct {
	path   string
	auth   string
	sessKe string
	body   map[string]any
}

// fixtureAgent is one agent row. Its fields are pairwise distinct AND distinct
// from every constant the assertions name, so a mutant that returns the wrong
// field — or a hardcoded one — cannot land on the expected value by coincidence.
func fixtureAgent() agents.Agent {
	return agents.Agent{
		ID:         4291,
		Name:       "swift-otter",
		Namespace:  "devpod-swift-otter",
		HooksToken: "4f8c1e2b9d7a5063c1f48e2a6b90d3571e8c4a2f6b09d7e5314c8a2f6b0d9e73",
	}
}

// testSentinel is the passthrough sentinel these tests configure.
//
// 🔴 IT IS DELIBERATELY NOT A REAL RUNTIME'S VALUE. The sentinel is configuration,
// so a test that hardcoded the value one image happens to want would be asserting
// a deployment's choice rather than that the CONFIGURED value reaches the wire — and
// it would pass over a gateway that ignored Config.Model and sent a constant.
const testSentinel = "runtime-sentinel"

// wantBearer is the fixture's derived credential, from the shell — see
// runtime_test.go on why it is not computed here.
const wantBearer = "16d1747972e45bcdc46a3dd892d363a4416169f0f783c1dea0668ace82c9941c"

// TestAToollessTurnCarriesTheDerivedBearerTheSentinelAndTheSessionKey pins the
// chat-completions half of what reaches the runtime.
func TestAToollessTurnCarriesTheDerivedBearerTheSentinelAndTheSessionKey(t *testing.T) {
	var got capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.auth = r.Header.Get("Authorization")
		got.sessKe = r.Header.Get("X-Openclaw-Session-Key")
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"there\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	res := &fixedResolver{ep: endpointOf(t, srv.URL)}
	gw, err := New(Config{Driver: res, Runtime: HooksSHA256(), Model: testSentinel, Client: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var deltas []string
	reply, err := gw.Chat(context.Background(), fixtureAgent(), "sess-77", "are you there?",
		func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if reply != "hello there" {
		t.Errorf("assembled reply = %q, want %q", reply, "hello there")
	}
	// The deltas are asserted separately from the assembly: a mutant that returned
	// the full text without streaming would satisfy the line above.
	if len(deltas) != 2 || deltas[0] != "hello " || deltas[1] != "there" {
		t.Errorf("streamed deltas = %q, want two chunks [%q %q] — the UI renders these as they "+
			"arrive, so assembling without emitting is a turn that appears frozen until it ends",
			deltas, "hello ", "there")
	}
	if got.path != "/v1/chat/completions" {
		t.Errorf("request path = %q, want /v1/chat/completions", got.path)
	}
	if got.auth != "Bearer "+wantBearer {
		t.Errorf("Authorization = %q, want %q (the chart's derivation over the fixture's hooks "+
			"token)", got.auth, "Bearer "+wantBearer)
	}
	if got.sessKe != "sess-77" {
		t.Errorf("session-key header = %q, want %q — each key is an independent runtime "+
			"context, so a dropped one merges every conversation into one", got.sessKe, "sess-77")
	}
	if m, _ := got.body["model"].(string); m != testSentinel {
		t.Errorf("wire model = %q, want the configured sentinel %q. It is REQUIRED on this "+
			"endpoint; a missing or wrong value is a 400 mid-turn", m, testSentinel)
	}
}

// TestAToolEnabledTurnCarriesTheSameThreeInputsToTheResponsesEndpoint is the same
// assertion for the other wire format, which is a different path and a different
// request shape.
//
// ⚠ IT IS A SEPARATE TEST RATHER THAN A TABLE ROW BECAUSE THE TWO SHAPES SHARE
// NOTHING BUT THE HEADERS. Folding them together would need a fake that answers
// both protocols, and the assertions would have to branch — at which point a row
// silently exercising the wrong branch is invisible.
func TestAToolEnabledTurnCarriesTheSameThreeInputsToTheResponsesEndpoint(t *testing.T) {
	var got capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.auth = r.Header.Get("Authorization")
		got.sessKe = r.Header.Get("X-Openclaw-Session-Key")
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.completed\n")
		fmt.Fprint(w, `data: {"response":{"output":[{"type":"message","role":"assistant",`+
			`"content":[{"type":"output_text","text":"on it"}]}]}}`+"\n\n")
	}))
	defer srv.Close()

	res := &fixedResolver{ep: endpointOf(t, srv.URL)}
	gw, err := New(Config{Driver: res, Runtime: HooksSHA256(), Model: testSentinel, Client: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	reply, err := gw.ChatWithTools(context.Background(), fixtureAgent(), "sess-88",
		"you are a worker", "read task 12",
		[]agents.ToolDef{{Type: "function", Name: "task_get"}}, func(string, string) string { return "{}" }, nil)
	if err != nil {
		t.Fatalf("ChatWithTools: %v", err)
	}
	if reply != "on it" {
		t.Errorf("reply = %q, want %q", reply, "on it")
	}
	if got.path != "/v1/responses" {
		t.Errorf("request path = %q, want /v1/responses", got.path)
	}
	if got.auth != "Bearer "+wantBearer {
		t.Errorf("Authorization = %q, want %q", got.auth, "Bearer "+wantBearer)
	}
	if got.sessKe != "sess-88" {
		t.Errorf("session-key header = %q, want %q", got.sessKe, "sess-88")
	}
	if m, _ := got.body["model"].(string); m != testSentinel {
		t.Errorf("wire model = %q, want %q", m, testSentinel)
	}
	if instr, _ := got.body["instructions"].(string); instr != "you are a worker" {
		t.Errorf("instructions = %q, want the caller's system prompt — dropping it is an agent "+
			"that runs its first turn with no idea what it is", instr)
	}
}

// TestTheUnsupportedResponsesEndpointReachesTheCallerUnwrapped pins the fallback
// signal.
//
// 🔴 A WRAPPED ERROR BREAKS THE CALLER'S FALLBACK SILENTLY. api.Gateway's contract
// says ChatWithTools returns agents.ErrResponsesUnsupported so the caller can fall
// back to a toolless turn; a caller using errors.Is still works through a %w wrap,
// but a caller comparing with == does not, and the observable is a LOST TURN on an
// older runtime rather than a degraded one.
func TestTheUnsupportedResponsesEndpointReachesTheCallerUnwrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, &http.Request{})
	}))
	defer srv.Close()

	gw, err := New(Config{
		Driver:  &fixedResolver{ep: endpointOf(t, srv.URL)},
		Runtime: HooksSHA256(),
		Model:   testSentinel,
		Client:  srv.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = gw.ChatWithTools(context.Background(), fixtureAgent(), "s", "", "go", nil, nil, nil)
	if !errors.Is(err, agents.ErrResponsesUnsupported) {
		t.Fatalf("got %v, want agents.ErrResponsesUnsupported — the caller's fallback to a "+
			"toolless turn keys on it, and without it an older runtime loses the turn entirely", err)
	}
}

// TestTheGatewayResolvesTheEndpointByTheAgentsNameEveryTurn pins the mapping this
// package shares with the lifecycle adapter, and that it is asked per turn.
//
// 🔴 THE REF MUST BE agents.RefOf's, NOT A NAMESPACE, AND A WRONG ONE RESOLVES TO
// NOTHING RATHER THAN FAILING. That property is recorded on agents.RefOf; here it
// is observable because the fixture's namespace and name differ by construction.
//
// 🔴 AND IT MUST BE RESOLVED PER TURN RATHER THAN CACHED AT CONSTRUCTION. An
// instance's address changes when it is destroyed and recreated — a cached endpoint
// would point at a pod that no longer exists, and the failure would be a connection
// refused that looks like a crashed agent.
func TestTheGatewayResolvesTheEndpointByTheAgentsNameEveryTurn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ag := fixtureAgent()
	if ag.Namespace == ag.Name {
		t.Fatalf("instrument check FAILED: the fixture's name and namespace are both %q, so a "+
			"namespace-keyed ref would be indistinguishable from a name-keyed one", ag.Name)
	}

	res := &fixedResolver{ep: endpointOf(t, srv.URL)}
	gw, err := New(Config{Driver: res, Runtime: HooksSHA256(), Model: testSentinel, Client: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := gw.Chat(context.Background(), ag, "s", "hi", nil); err != nil {
			t.Fatalf("Chat turn %d: %v", i+1, err)
		}
	}

	if len(res.refs) != 2 {
		t.Fatalf("the driver was asked for an endpoint %d time(s) over two turns, want 2 — a "+
			"cached endpoint survives the instance it names", len(res.refs))
	}
	for i, ref := range res.refs {
		if ref.Name != ag.Name {
			t.Errorf("turn %d resolved ref.Name = %q, want the agent's slug %q (NOT its "+
				"namespace %q): a namespace-keyed ref resolves to nothing rather than failing",
				i+1, ref.Name, ag.Name, ag.Namespace)
		}
		if ref.ID != ag.ID {
			t.Errorf("turn %d resolved ref.ID = %d, want %d", i+1, ref.ID, ag.ID)
		}
	}
}

// TestAnAgentWithNoHooksTokenIsRefused is the guard runtime_test.go's empty-token
// row exists to motivate.
//
// 🔴 THE FAILURE IT PREVENTS IS A WELL-FORMED WRONG CREDENTIAL, NOT A MISSING ONE.
// sha256("gw-") is 64 hex characters, so without this check the turn is attempted
// and the runtime answers 401 — which reads as a rotated secret and sends the reader
// to the deployment's secrets instead of to a row the provisioner never finished.
//
// ⚠ AND IT MUST REFUSE BEFORE THE REQUEST, WHICH IS WHY THE SERVER COUNTS HITS. A
// version that refused after dialling would pass an assertion phrased only over the
// returned error.
func TestAnAgentWithNoHooksTokenIsRefused(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	res := &fixedResolver{ep: endpointOf(t, srv.URL)}
	gw, err := New(Config{Driver: res, Runtime: HooksSHA256(), Model: testSentinel, Client: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ag := fixtureAgent()
	ag.HooksToken = ""

	for _, turn := range []struct {
		name string
		run  func() (string, error)
	}{
		{"Chat", func() (string, error) {
			return gw.Chat(context.Background(), ag, "s", "hi", nil)
		}},
		{"ChatWithTools", func() (string, error) {
			return gw.ChatWithTools(context.Background(), ag, "s", "", "hi", nil, nil, nil)
		}},
	} {
		t.Run(turn.name, func(t *testing.T) {
			_, err := turn.run()
			if err == nil {
				t.Fatal("the turn succeeded over an agent with no hooks token, so it was sent " +
					"a bearer derived from the empty string — a well-formed credential the " +
					"runtime rejects with 401")
			}
			if !strings.Contains(err.Error(), ag.Name) {
				t.Errorf("the refusal does not name the agent (%q), so an operator cannot tell "+
					"which row is unfinished.\n  got: %v", ag.Name, err)
			}
		})
	}

	// 🔴 POSITIVE CONTROL ON THE HIT COUNTER. A zero here means nothing, unless the
	// counter can move at all: the same gateway with a real token must reach the
	// server, or "0 requests" is indistinguishable from a fake nothing ever dialled.
	if hits != 0 {
		t.Errorf("the runtime was contacted %d time(s) for an agent with no token — the refusal "+
			"must come before the request", hits)
	}
	if _, err := gw.Chat(context.Background(), fixtureAgent(), "s", "hi", nil); err != nil {
		t.Fatalf("positive control FAILED: a turn with a real hooks token errored: %v", err)
	}
	if hits != 1 {
		t.Errorf("positive control FAILED: the server recorded %d hit(s) for one valid turn, so "+
			"the zero above was not evidence of anything", hits)
	}
}

// TestAResolutionFailureNamesTheAgentAndPreservesTheCause pins the diagnostic path.
//
// ⚠ provision.ErrNoEndpoint IS A REAL STATE, NOT A BUG: an instance can exist and
// declare no reachable address. A caller distinguishing it from a transport failure
// needs errors.Is to work, and an operator needs to know WHICH agent — the wrapped
// message carries both.
func TestAResolutionFailureNamesTheAgentAndPreservesTheCause(t *testing.T) {
	gw, err := New(Config{
		Driver:  &fixedResolver{err: provision.ErrNoEndpoint},
		Runtime: HooksSHA256(),
		Model:   testSentinel,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = gw.Chat(context.Background(), fixtureAgent(), "s", "hi", nil)
	if !errors.Is(err, provision.ErrNoEndpoint) {
		t.Errorf("the resolution failure does not unwrap to provision.ErrNoEndpoint, so a "+
			"caller cannot tell \"this instance declares no address\" from a network error.\n"+
			"  got: %v", err)
	}
	if !strings.Contains(err.Error(), fixtureAgent().Name) {
		t.Errorf("the error does not name the agent.\n  got: %v", err)
	}
}

// TestTheDerivedURLsArePinnedForEveryEndpointShape is the URL half, pinned as whole
// strings rather than checked for a suffix.
//
// 🔴 A SUFFIX CHECK PASSES OVER A MALFORMED PREFIX. The shapes below are the ones
// provision.Endpoint can produce — a default scheme, an explicit one, a base path
// with and without a trailing slash — and the failure a double slash causes is a
// router treating //v1/responses as a different route, i.e. a 404 that reads as an
// unsupported endpoint and silently triggers the toolless fallback.
func TestTheDerivedURLsArePinnedForEveryEndpointShape(t *testing.T) {
	cases := []struct {
		name     string
		ep       provision.Endpoint
		wantChat string
		wantResp string
	}{
		{
			name:     "default scheme, no path",
			ep:       provision.Endpoint{Host: "agent-swift-otter.agents.example", Port: 18789},
			wantChat: "http://agent-swift-otter.agents.example:18789/v1/chat/completions",
			wantResp: "http://agent-swift-otter.agents.example:18789/v1/responses",
		},
		{
			name:     "https with a base path",
			ep:       provision.Endpoint{Scheme: "https", Host: "agents.example.test", Port: 443, Path: "/agent"},
			wantChat: "https://agents.example.test:443/agent/v1/chat/completions",
			wantResp: "https://agents.example.test:443/agent/v1/responses",
		},
		{
			// The trailing slash is the case the trim exists for.
			name:     "base path with a trailing slash",
			ep:       provision.Endpoint{Host: "127.0.0.1", Port: 8080, Path: "/agent/"},
			wantChat: "http://127.0.0.1:8080/agent/v1/chat/completions",
			wantResp: "http://127.0.0.1:8080/agent/v1/responses",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := agents.ChatCompletionsURL(c.ep); got != c.wantChat {
				t.Errorf("ChatCompletionsURL\n  got  %s\n  want %s", got, c.wantChat)
			}
			if got := agents.ResponsesURL(c.ep); got != c.wantResp {
				t.Errorf("ResponsesURL\n  got  %s\n  want %s", got, c.wantResp)
			}
			if strings.Contains(agents.ChatCompletionsURL(c.ep), "//v1") {
				t.Errorf("the derived chat URL contains a double slash before v1: %s",
					agents.ChatCompletionsURL(c.ep))
			}
		})
	}
}

// TestAToollessTurnSurfacesTheRuntimesOwnRefusal pins that a non-200 carries the
// runtime's message, not just its status code.
//
// 🔴 THREE OTHER PLACES ARGUE FROM THIS MESSAGE AND IT WAS BEING DISCARDED. The boot
// refusal for a missing sentinel justifies itself by saying the alternative is "an
// HTTP 400 with a message about the model field from inside a turn" — and on this
// path the error read `chat completions HTTP 400`, message dropped. The machine route
// POST /api/agents/{name}/messages goes through Chat rather than ChatWithTools, so
// this is the path that argument was written about. The tool path carried a snippet
// all along; the asymmetry was the defect.
func TestAToollessTurnSurfacesTheRuntimesOwnRefusal(t *testing.T) {
	const runtimeSaid = `{"error":{"message":"model: Invalid input: expected string"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, runtimeSaid)
	}))
	defer srv.Close()

	gw, err := New(Config{
		Driver:  &fixedResolver{ep: endpointOf(t, srv.URL)},
		Runtime: HooksSHA256(),
		Model:   testSentinel,
		Client:  srv.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = gw.Chat(context.Background(), fixtureAgent(), "s", "hi", nil)
	if err == nil {
		t.Fatal("a 400 from the runtime produced no error")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("the error does not carry the status.\n  got: %v", err)
	}
	// 🔴 THE MESSAGE IS THE ASSERTION. A status-only error sends the reader to guess
	// which of the request's several required fields the runtime objected to.
	if !strings.Contains(err.Error(), "model: Invalid input") {
		t.Errorf("the error does not carry the runtime's OWN message, so the diagnosis three "+
			"other places promise cannot be produced on this path.\n  got:  %v\n  want it to "+
			"contain: %q", err, "model: Invalid input")
	}
}
