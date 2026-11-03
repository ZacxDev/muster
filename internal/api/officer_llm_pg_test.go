package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/llmproxy"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

// The officer LLM proxy at its seam, from the configuration to the database:
// MUSTER_OFFICERS (officers.Parse, the `llm` block and its key) → the
// registry → POST /llm/v1/messages on the real handler (and through the
// connect listener's filter) → the instance's own token → the routine
// allowlist → internal/llmproxy → a stub provider → officer_llm_usage and the
// connector audit trail. internal/llmproxy's own tests cover the stream, the
// budget arithmetic and the key scrub against an in-memory store.

// llmPGKey is the officer's provider key in these tests.
const llmPGKey = "sk-or-v1-pgtest-a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718"

// llmProvider is a stub OpenRouter: an Anthropic-shaped stream reporting
// usage, the provider that served it and its cost.
type llmProvider struct {
	*httptest.Server
	mu    sync.Mutex
	n     int
	auth  []string
	model []string
	cost  float64
}

func newLLMProvider(t *testing.T, cost float64) *llmProvider {
	t.Helper()
	p := &llmProvider{cost: cost}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		p.mu.Lock()
		p.n++
		p.auth = append(p.auth, r.Header.Get("Authorization"))
		p.model = append(p.model, body.Model)
		cost := p.cost
		p.mu.Unlock()
		if r.URL.Path != "/api/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_pg\",\"model\":%q,\"provider\":\"ExampleProvider\",\"usage\":{\"input_tokens\":1000,\"cache_read_input_tokens\":300,\"output_tokens\":1}}}\n\n", body.Model)
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7,\"cost\":%v}}\n\n", cost)
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(p.Server.Close)
	return p
}

func (p *llmProvider) calls() int { p.mu.Lock(); defer p.mu.Unlock(); return p.n }

// llmLogs is a log sink the handler writes while the test reads.
type llmLogs struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *llmLogs) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *llmLogs) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// llmOfficerServer is a server on f's database with the officers raw
// declares, the officer key in the environment, and the provider at up.
func llmOfficerServer(t *testing.T, f *connFixture, raw, up string, logs io.Writer) (*Server, http.Handler) {
	t.Helper()
	list, err := officers.Parse(raw, func(k string) string {
		switch {
		case strings.HasPrefix(k, officers.LLMKeyEnvPrefix):
			return llmPGKey
		case strings.HasPrefix(k, officers.TokenEnvPrefix):
			return "hq-token-" + k
		}
		return ""
	}, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, AuthConfig{HookToken: connTestHookToken, ServiceToken: connTestServiceToken, UIPassword: connTestPassword},
		log.New(logs, "", 0))
	s.UseExtensions(Extensions{Agents: agents.NewPG(f.pool), Connectors: f.store, Provisioner: stubProvisioner{}, Kinds: stubKinds{pick: "acct-one"}})
	if _, err := s.EnableOfficers(OfficersConfig{Officers: list, InstallSession: discardSession,
		LLMStore: llmproxy.NewPGStore(f.pool), LLMUpstream: up + "/api"}); err != nil {
		t.Fatal(err)
	}
	return s, s.Handler()
}

// llmCleanup deletes the usage rows of the officers these tests declare.
func llmCleanup(t *testing.T, f *connFixture, ids ...string) {
	t.Helper()
	del := func() {
		for _, id := range ids {
			_, _ = f.pool.Exec(dbtest.AsOperator(context.Background()), `DELETE FROM officer_llm_usage WHERE officer = $1`, id)
		}
	}
	del()
	t.Cleanup(del)
}

const llmClientBody = `{"model":"claude-sonnet-4-5","stream":true,"max_tokens":32000,"messages":[{"role":"user","content":"hi"}]}`

func llmPost(t *testing.T, h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	m := map[string]string{"Content-Type": "application/json", "Anthropic-Version": "2023-06-01"}
	for k, v := range hdr {
		m[k] = v
	}
	return serveCall(t, h, http.MethodPost, path, strings.NewReader(llmClientBody), m)
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

// TestTheLLMProxyServesOnlyARoutineInstanceOfAnLLMOfficer: a listed routine
// instance is served (with the officer's key and pinned model); every other
// caller — the same officer's unlisted instance, an officer without `llm`,
// an ordinary agent, the shared hook and service tokens, no token — is
// refused and the provider never hears of it.
func TestTheLLMProxyServesOnlyARoutineInstanceOfAnLLMOfficer(t *testing.T) {
	f := newConnFixture(t)
	up := newLLMProvider(t, 0.001)
	const id, plain = "routine:llm-auth", "plain:llm-auth"
	llmCleanup(t, f, id, plain)
	raw := `[{"id":"` + id + `","hq_url":"https://hq.example.invalid/api/v1","max_instances":2,
	          "llm":{"model":"vendor/pinned-model","daily_budget_usd":5,"routine_agents":["officer-routine-llm-auth-1"]}},
	         {"id":"` + plain + `","hq_url":"https://hq.example.invalid/api/v1"}]`
	logs := &llmLogs{}
	srv, h := llmOfficerServer(t, f, raw, up.URL, logs)
	const tokRoutine, tokUnlisted, tokPlain, tokAgent = "llm-routine-agent-token-0123456789", "llm-unlisted-agent-token-0123456789",
		"llm-plain-officer-token-0123456789", "llm-ordinary-agent-token-0123456789"
	startOfficerInstance(t, f, srv, id, 1, tokRoutine)
	startOfficerInstance(t, f, srv, id, 2, tokUnlisted)
	startOfficerInstance(t, f, srv, plain, 1, tokPlain)
	f.seedAgent(t, "llm-ordinary-agent", tokAgent)

	w := llmPost(t, h, "/llm/v1/messages?beta=true", bearer(tokRoutine))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "message_stop") {
		t.Fatalf("the routine instance: %d %s", w.Code, w.Body)
	}
	up.mu.Lock()
	if up.auth[0] != "Bearer "+llmPGKey || up.model[0] != "vendor/pinned-model" {
		t.Errorf("the provider got auth %q model %q, want the officer's key and the pinned model", up.auth[0], up.model[0])
	}
	up.mu.Unlock()
	// X-Muster-Token is the other spelling an agent's token may take.
	if w := llmPost(t, h, "/llm/v1/messages", map[string]string{"X-Muster-Token": tokRoutine}); w.Code != http.StatusOK {
		t.Fatalf("the routine instance by X-Muster-Token: %d %s", w.Code, w.Body)
	}

	before := testutil.ToFloat64(metrics.OfficerLLMRefused.WithLabelValues(id, "not_routine"))
	for _, c := range []struct {
		name string
		hdr  map[string]string
		code int
	}{
		{"the same officer's unlisted instance", bearer(tokUnlisted), http.StatusForbidden},
		{"an officer without llm", bearer(tokPlain), http.StatusForbidden},
		{"an ordinary agent", bearer(tokAgent), http.StatusForbidden},
		{"the shared hook token", bearer(connTestHookToken), http.StatusForbidden},
		{"the shared service token", bearer(connTestServiceToken), http.StatusForbidden},
		{"no token", nil, http.StatusUnauthorized},
		{"an unknown token", bearer("llm-nobody-token-0123456789abcdef"), http.StatusUnauthorized},
	} {
		if w := llmPost(t, h, "/llm/v1/messages", c.hdr); w.Code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, w.Code, w.Body, c.code)
		}
	}
	if n := up.calls(); n != 2 {
		t.Fatalf("the provider got %d requests, want 2 (the routine instance's only)", n)
	}
	if got := testutil.ToFloat64(metrics.OfficerLLMRefused.WithLabelValues(id, "not_routine")) - before; got != 1 {
		t.Errorf("not_routine for %s moved by %v, want 1 (the unlisted instance)", id, got)
	}
	if strings.Contains(logs.String(), llmPGKey) {
		t.Fatal("the provider key is in the server log")
	}
}

// TestTheLLMProxyServesExactlyOnePath: through the connect listener's own
// filter, POST /llm/v1/messages reaches the proxy and every other method or
// path under /llm/ is a 404 — before any credential is read, so a valid
// routine token gets no further — and the provider is never called.
func TestTheLLMProxyServesExactlyOnePath(t *testing.T) {
	f := newConnFixture(t)
	up := newLLMProvider(t, 0.001)
	const id = "routine:llm-path"
	llmCleanup(t, f, id)
	raw := `[{"id":"` + id + `","hq_url":"https://hq.example.invalid/api/v1",
	          "llm":{"model":"vendor/pinned-model","daily_budget_usd":5,"routine_agents":["officer-routine-llm-path-1"]}}]`
	srv, h := llmOfficerServer(t, f, raw, up.URL, io.Discard)
	const tok = "llm-path-agent-token-0123456789abcd"
	startOfficerInstance(t, f, srv, id, 1, tok)
	connect := OnlyConnectSurface(h)

	before := testutil.ToFloat64(metrics.OfficerLLMRefused.WithLabelValues("none", "path"))
	refused := 0
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/llm/v1/messages"},
		{http.MethodPut, "/llm/v1/messages"},
		{http.MethodPost, "/llm/v1/messages/count_tokens"},
		{http.MethodPost, "/llm/v1/models"},
		{http.MethodGet, "/llm/v1/models"},
		{http.MethodPost, "/llm/v1/complete"},
		{http.MethodPost, "/llm/api/v1/messages"},
		{http.MethodPost, "/llm/"},
	} {
		w := serveCall(t, connect, c.method, c.path, strings.NewReader(llmClientBody), bearer(tok))
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "only POST /llm/v1/messages") {
			t.Errorf("%s %s: %d %s, want the proxy's 404", c.method, c.path, w.Code, w.Body)
			continue
		}
		refused++
	}
	if got := testutil.ToFloat64(metrics.OfficerLLMRefused.WithLabelValues("none", "path")) - before; int(got) != refused || refused != 8 {
		t.Errorf("refused{path} moved by %v over %d refusals, want 8", got, refused)
	}
	// Outside /llm/ the listener's own 404.
	if w := serveCall(t, connect, http.MethodPost, "/llmx/v1/messages", strings.NewReader(llmClientBody), bearer(tok)); w.Code != http.StatusNotFound {
		t.Errorf("/llmx: %d", w.Code)
	}
	if n := up.calls(); n != 0 {
		t.Fatalf("the provider got %d requests from refused paths", n)
	}
	// The one path, through the same filter: served.
	if w := llmPost(t, connect, "/llm/v1/messages?beta=true", bearer(tok)); w.Code != http.StatusOK {
		t.Fatalf("POST /llm/v1/messages through the connect listener: %d %s", w.Code, w.Body)
	}
	if n := up.calls(); n != 1 {
		t.Fatalf("the provider got %d, want 1", n)
	}
}

// TestTheLLMBudgetIsPersistedAndSurvivesARestart: usage is booked to
// officer_llm_usage at the provider's cost; the request after the day's spend
// reaches the budget is refused without reaching the provider; a NEW server
// on the same database (a restart, with nothing in memory) refuses too; and
// every request — served or refused — has a connector audit row naming the
// provider and its result, with the key nowhere in it.
func TestTheLLMBudgetIsPersistedAndSurvivesARestart(t *testing.T) {
	f := newConnFixture(t)
	up := newLLMProvider(t, 0.006)
	const id = "routine:llm-budget"
	llmCleanup(t, f, id)
	raw := `[{"id":"` + id + `","hq_url":"https://hq.example.invalid/api/v1",
	          "llm":{"model":"vendor/pinned-model","daily_budget_usd":0.01,"routine_agents":["officer-routine-llm-budget-1"]}}]`
	logs := &llmLogs{}
	srv, h := llmOfficerServer(t, f, raw, up.URL, logs)
	const tok = "llm-budget-agent-token-0123456789ab"
	startOfficerInstance(t, f, srv, id, 1, tok)

	for i := 1; i <= 2; i++ {
		if w := llmPost(t, h, "/llm/v1/messages", bearer(tok)); w.Code != http.StatusOK {
			t.Fatalf("request %d under the budget: %d %s", i, w.Code, w.Body)
		}
	}
	var reqs, in, out, cacheRead int64
	var usd float64
	if err := f.pool.QueryRow(dbtest.AsOperator(context.Background()), `SELECT requests, input_tokens, output_tokens, cache_read_tokens,
		cost_usd::float8 FROM officer_llm_usage WHERE officer = $1 AND instance = 1 AND day = (now() AT TIME ZONE 'UTC')::date`, id).
		Scan(&reqs, &in, &out, &cacheRead, &usd); err != nil {
		t.Fatalf("the usage row: %v", err)
	}
	if reqs != 2 || in != 2000 || out != 14 || cacheRead != 600 || usd < 0.01199 || usd > 0.01201 {
		t.Fatalf("usage row: requests %d in %d out %d cache_read %d usd %v; want 2, 2000, 14, 600, 0.012", reqs, in, out, cacheRead, usd)
	}
	// The row is what the budget is checked against: the next request is over.
	if w := llmPost(t, h, "/llm/v1/messages", bearer(tok)); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "daily LLM budget") {
		t.Fatalf("at the cap: %d %s, want 400", w.Code, w.Body)
	}
	if n := up.calls(); n != 2 {
		t.Fatalf("the provider got %d, want 2", n)
	}

	// A restart: a new server, a new proxy, nothing in memory.
	_, h2 := llmOfficerServer(t, f, raw, up.URL, logs)
	if w := llmPost(t, h2, "/llm/v1/messages", bearer(tok)); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "daily LLM budget") {
		t.Fatalf("after a restart: %d %s, want the budget refusal", w.Code, w.Body)
	}
	if n := up.calls(); n != 2 {
		t.Fatalf("after a restart the provider got %d, want still 2", n)
	}

	audit, err := f.store.ListAudit(dbtest.AsOperator(context.Background()), 100)
	if err != nil {
		t.Fatal(err)
	}
	var ok, refused int
	for _, e := range audit {
		if e.Kind != "llm" {
			continue
		}
		if e.Connector != "llm" || e.Method != http.MethodPost || e.Path != "/api/v1/messages" || !strings.Contains(e.Detail, "officer="+id) {
			t.Errorf("audit row %+v", e)
		}
		if strings.Contains(e.Detail, llmPGKey) {
			t.Fatal("the key is in an audit row")
		}
		switch {
		case strings.Contains(e.Detail, "result=ok") && strings.Contains(e.Detail, "provider=ExampleProvider") &&
			strings.Contains(e.Detail, "requested_model=claude-sonnet-4-5") && strings.Contains(e.Detail, "usd=0.006000"):
			ok++
		case strings.Contains(e.Detail, "result=refused_budget"):
			refused++
		}
	}
	if ok != 2 || refused != 2 {
		t.Fatalf("audit: %d served rows and %d refused rows, want 2 and 2:\n%+v", ok, refused, audit)
	}
	if strings.Contains(logs.String(), llmPGKey) {
		t.Fatal("the key is in the server log")
	}
}

// llmItemsRig is an instance of an officer with a 2000-token item cap (each
// stub request is 1000 + 300 + 7 = 1307 tokens, so the cap admits two), its
// HQ, and a way to call its officer tools.
type llmItemsRig struct {
	h    http.Handler
	hq   *hqtest.Server
	up   *llmProvider
	tok  string
	call func(tool, args string)
}

func newLLMItemsRig(t *testing.T, id, tok string) *llmItemsRig {
	t.Helper()
	f := newConnFixture(t)
	up := newLLMProvider(t, 0.0001)
	llmCleanup(t, f, id)
	hq := hqtest.New(map[string]string{"hq-token-" + officers.TokenEnvPrefix + officers.TokenEnvSuffix(id): id})
	t.Cleanup(hq.Close)
	raw := `[{"id":"` + id + `","hq_url":"` + hq.BaseURL() + `",
	          "llm":{"model":"vendor/pinned-model","daily_budget_usd":5,"max_tokens_per_item":2000,"routine_agents":["` +
		officers.InstanceName(id, 1) + `"]}}]`
	srv, h := llmOfficerServer(t, f, raw, up.URL, io.Discard)
	a := startOfficerInstance(t, f, srv, id, 1, tok)
	return &llmItemsRig{h: h, hq: hq, up: up, tok: tok, call: func(tool, args string) {
		t.Helper()
		if out := srv.dispatchAgentTool(context.Background(), a, tool, args); isToolErr(out) {
			t.Fatalf("%s %s: %s", tool, args, out)
		}
	}}
}

// expect sends one proxied request per wanted status, in order.
func (r *llmItemsRig) expect(t *testing.T, what string, want ...int) {
	t.Helper()
	for i, code := range want {
		if w := llmPost(t, r.h, "/llm/v1/messages", bearer(r.tok)); w.Code != code {
			t.Fatalf("%s, request %d: %d %s, want %d", what, i+1, w.Code, w.Body, code)
		}
	}
}

// TestAnOfficerToolClaimStartsTheItemMeter is the claim seam: the item an
// instance holds is the one its officer_claim names (dispatchAgentTool →
// llmClaimed), so once the cap ends a runaway turn on one item, claiming the
// NEXT item — with the first still held, so no complete or release resets
// anything — starts the count at zero.
func TestAnOfficerToolClaimStartsTheItemMeter(t *testing.T) {
	const id = "routine:llm-claims"
	r := newLLMItemsRig(t, id, "llm-claims-agent-token-0123456789a")
	itemA := r.hq.AddItem(id, "routine.check", "a")
	itemB := r.hq.AddItem(id, "routine.check", "b")

	r.call(officers.ToolClaim, `{"item_id":"`+itemA+`"}`)
	r.expect(t, "on item "+itemA, http.StatusOK, http.StatusOK, http.StatusBadRequest)
	r.call(officers.ToolClaim, `{"item_id":"`+itemB+`"}`)
	r.expect(t, "on item "+itemB+", claimed while "+itemA+" is still held", http.StatusOK, http.StatusOK, http.StatusBadRequest)
	if n := r.up.calls(); n != 4 {
		t.Fatalf("the provider got %d, want 4", n)
	}
}

// TestAnOfficerToolCompleteEndsTheItemMeter and
// TestAnOfficerToolReleaseEndsTheItemMeter are the end seams: officer_complete
// and officer_release (dispatchAgentTool → llmEnded) end the held item, so the
// instance's next request — holding nothing — starts at zero. One test each,
// so a broken hook on either fails by its own name.
func TestAnOfficerToolCompleteEndsTheItemMeter(t *testing.T) {
	llmEndSeam(t, "routine:llm-completes", "llm-completes-agent-token-012345", officers.ToolComplete,
		`,"outcome":"done","summary":"checked"`)
}

func TestAnOfficerToolReleaseEndsTheItemMeter(t *testing.T) {
	llmEndSeam(t, "routine:llm-releases", "llm-releases-agent-token-0123456", officers.ToolRelease, ``)
}

func llmEndSeam(t *testing.T, id, tok, tool, extra string) {
	t.Helper()
	r := newLLMItemsRig(t, id, tok)
	item := r.hq.AddItem(id, "routine.check", "a")
	r.call(officers.ToolClaim, `{"item_id":"`+item+`"}`)
	r.expect(t, "on item "+item, http.StatusOK, http.StatusOK, http.StatusBadRequest)
	r.call(tool, `{"item_id":"`+item+`"`+extra+`}`)
	r.expect(t, "after "+tool+" of "+item, http.StatusOK)
}
