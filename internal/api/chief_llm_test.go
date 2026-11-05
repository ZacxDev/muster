package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/credentials"
	"github.com/ZacxDev/muster/internal/officers"
)

// The chief on the LLM proxy (chief_llm.go), through the real handler and the
// real connector door, against a stub provider. In memory: the agents store,
// the connector audit and the chief toggle are fakes; the key resolver is the
// real credentials.Plane over a fake per-owner store, so "admin = the
// environment's key, member = their own" is the plane's rule, not the test's.

// Distinctive keys: each must reach the provider for its own owner only, and
// none may appear in a log line, an audit detail or a response.
const (
	chiefEnvKey    = "sk-or-v1-envadmin-1f2e3d4c5b6a79881f2e3d4c5b6a79881f2e3d4c5b6a7988"
	chiefMemberKey = "sk-or-v1-membertwo-0a9b8c7d6e5f40310a9b8c7d6e5f40310a9b8c7d6e5f4031"
	chiefOffKey    = "sk-or-v1-memberoff-77aa66bb55cc44dd77aa66bb55cc44dd77aa66bb55cc44dd"
)

// The agents' own muster tokens: never forwarded upstream.
const (
	tokAdminChief  = "chief-llm-admin-chief-token-0123456789"
	tokAdminWorker = "chief-llm-admin-worker-token-012345678"
	tokMemberChief = "chief-llm-member-chief-token-012345678"
	tokNoKeyChief  = "chief-llm-nokey-chief-token-0123456789"
	tokOffChief    = "chief-llm-off-chief-token-012345678901"
)

// chiefLLMAgents is an agents store holding chiefLLMRows, read as row-level
// security would: a principal sees its own owner's rows.
type chiefLLMAgents struct {
	agents.Store
	rows []agents.Agent
}

func (s *chiefLLMAgents) List(ctx context.Context) ([]agents.Agent, error) {
	owner := requestOwner(ctx)
	var out []agents.Agent
	for _, a := range s.rows {
		if owner <= 0 || a.OwnerID == owner {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *chiefLLMAgents) Get(ctx context.Context, id int64) (agents.Agent, error) {
	for _, a := range s.rows {
		if a.ID == id {
			return a, nil
		}
	}
	return agents.Agent{}, errors.New("no such agent")
}

func (s *chiefLLMAgents) GetByHooksToken(_ context.Context, tok string) (agents.Agent, error) {
	for _, a := range s.rows {
		if a.HooksToken == tok {
			return a, nil
		}
	}
	return agents.Agent{}, errors.New("no such token")
}

func chiefLLMRows() []agents.Agent {
	cc := agents.KindClaudeCode
	return []agents.Agent{
		{ID: 10, Name: "zesty-stoat", DisplayName: "chief", OwnerID: 1, Kind: cc, HooksToken: tokAdminChief},
		{ID: 11, Name: "busy-worker", DisplayName: "worker", OwnerID: 1, Kind: cc, HooksToken: tokAdminWorker},
		{ID: 20, Name: "member-chief", DisplayName: "chief", OwnerID: 2, Kind: cc, HooksToken: tokMemberChief},
		{ID: 30, Name: "nokey-chief", DisplayName: "chief", OwnerID: 3, Kind: cc, HooksToken: tokNoKeyChief},
		{ID: 40, Name: "off-chief", DisplayName: "chief", OwnerID: 4, Kind: cc, HooksToken: tokOffChief},
	}
}

// chiefLLMAudit is a connector store that keeps the audit rows.
type chiefLLMAudit struct {
	connectors.Store
	mu   sync.Mutex
	rows []connectors.AuditEntry
}

func (a *chiefLLMAudit) Audit(_ context.Context, e connectors.AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, e)
	return nil
}

func (a *chiefLLMAudit) entries() []connectors.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]connectors.AuditEntry(nil), a.rows...)
}

// chiefLLMPrefs: owner 4 turned the chief off; nobody else chose.
type chiefLLMPrefs struct{}

func (chiefLLMPrefs) Choice(_ context.Context, owner int64) (bool, bool, error) {
	if owner == 4 {
		return true, false, nil
	}
	return false, false, nil
}
func (chiefLLMPrefs) SetEnabled(context.Context, int64, bool) error { return nil }

// chiefLLMPersonal is the per-owner credential store: owners 2 and 4 stored a
// key, owner 3 none.
type chiefLLMPersonal struct{}

func (chiefLLMPersonal) ClaudeTokens(context.Context, int64) (map[string]string, error) {
	return nil, nil
}
func (chiefLLMPersonal) OpenRouterKey(_ context.Context, owner int64) (string, bool, error) {
	switch owner {
	case 2:
		return chiefMemberKey, true, nil
	case 4:
		return chiefOffKey, true, nil
	}
	return "", false, nil
}
func (p chiefLLMPersonal) HasOpenRouterKey(ctx context.Context, owner int64) (bool, error) {
	_, ok, err := p.OpenRouterKey(ctx, owner)
	return ok, err
}

// chiefProvider is a stub Anthropic-Messages provider that records each
// request's headers and model.
type chiefProvider struct {
	*httptest.Server
	mu    sync.Mutex
	hdrs  []http.Header
	model []string
}

func newChiefProvider(t *testing.T) *chiefProvider {
	t.Helper()
	p := &chiefProvider{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		p.mu.Lock()
		p.hdrs = append(p.hdrs, r.Header.Clone())
		p.model = append(p.model, body.Model)
		p.mu.Unlock()
		if r.URL.Path != "/api/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_chief\",\"model\":\""+body.Model+"\",\"provider\":\"ExampleProvider\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3,\"cost\":0.0007}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(p.Server.Close)
	return p
}

func (p *chiefProvider) calls() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.hdrs) }

func (p *chiefProvider) last() (http.Header, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hdrs[len(p.hdrs)-1], p.model[len(p.model)-1]
}

type chiefLLMRig struct {
	s     *Server
	h     http.Handler
	up    *chiefProvider
	audit *chiefLLMAudit
	logs  *llmLogs
}

// newChiefLLMRig is a server with the chief LLM enabled (cfg's Keys and
// Upstream filled in when unset) unless enable is false.
func newChiefLLMRig(t *testing.T, enable bool, cfg ChiefLLM) *chiefLLMRig {
	t.Helper()
	r := &chiefLLMRig{up: newChiefProvider(t), audit: &chiefLLMAudit{}, logs: &llmLogs{}}
	r.s = New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(r.logs, "", 0))
	r.s.UseExtensions(Extensions{Agents: &chiefLLMAgents{rows: chiefLLMRows()}, Connectors: r.audit, ChiefPrefs: chiefLLMPrefs{},
		Provisioner: stubProvisioner{}, SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}})
	if enable {
		if cfg.Keys == nil {
			cfg.Keys = &credentials.Plane{EnvOpenRouterKey: chiefEnvKey, Personal: chiefLLMPersonal{}}
		}
		if cfg.Upstream == "" {
			cfg.Upstream = r.up.URL + "/api"
		}
		if err := r.s.EnableChiefLLM(cfg); err != nil {
			t.Fatal(err)
		}
	}
	r.h = r.s.Handler()
	return r
}

func (r *chiefLLMRig) post(t *testing.T, tok string) *httptest.ResponseRecorder {
	t.Helper()
	return llmPost(t, r.h, "/llm/v1/messages?beta=true", map[string]string{"Authorization": "Bearer " + tok, "X-Muster-Token": tok})
}

// noKeyLeaked fails when any key appears in the logs, an audit detail or body.
func (r *chiefLLMRig) noKeyLeaked(t *testing.T, bodies ...string) {
	t.Helper()
	where := append([]string{r.logs.String()}, bodies...)
	for _, e := range r.audit.entries() {
		where = append(where, e.Detail)
	}
	for _, k := range []string{chiefEnvKey, chiefMemberKey, chiefOffKey} {
		for _, w := range where {
			if strings.Contains(w, k) {
				t.Fatalf("key %s… leaked into %q", k[:16], w)
			}
		}
	}
}

// TestTheChiefIsServedOnItsOwnersKeyWithThePinnedModel: with the chief LLM
// on, an admin's chief is served on the environment's key and a member's chief
// on that member's own key — never the other's — each pinned to the chief
// model, with the agent's own muster token not forwarded, never refused by a
// budget, and audited as "chief".
//
// RED at the base (EnableChiefLLM does not exist; with it stubbed in, the
// chief is refused 503 "no officer on this server declares `llm`").
func TestTheChiefIsServedOnItsOwnersKeyWithThePinnedModel(t *testing.T) {
	r := newChiefLLMRig(t, true, ChiefLLM{})
	var bodies []string
	for i, c := range []struct {
		who, tok, key string
	}{
		{"the admin's chief", tokAdminChief, chiefEnvKey},
		{"a member's chief", tokMemberChief, chiefMemberKey},
	} {
		w := r.post(t, c.tok)
		bodies = append(bodies, w.Body.String())
		if strings.Contains(w.Body.String(), "budget") {
			t.Fatalf("%s: refused by a budget: %d %s", c.who, w.Code, w.Body)
		}
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s, want 200", c.who, w.Code, w.Body)
		}
		if got := r.up.calls(); got != i+1 {
			t.Fatalf("%s: upstream calls = %d, want %d", c.who, got, i+1)
		}
		hdr, model := r.up.last()
		if got := hdr.Get("Authorization"); got != "Bearer "+c.key {
			t.Errorf("%s: upstream Authorization is not its owner's key (%d bytes)", c.who, len(got))
		}
		if model != DefaultChiefLLMModel {
			t.Errorf("%s: upstream model = %q, want the pin %q", c.who, model, DefaultChiefLLMModel)
		}
		if hdr.Get("X-Muster-Token") != "" || strings.Contains(strings.Join(hdr.Values("Authorization"), " "), c.tok) {
			t.Errorf("%s: the agent's own muster token was forwarded: %v", c.who, hdr)
		}
	}
	a := r.audit.entries()
	if len(a) != 2 || a[0].AgentName != "zesty-stoat" || a[1].AgentName != "member-chief" ||
		!strings.Contains(a[0].Detail, "officer=chief ") || !strings.Contains(a[0].Detail, "result=ok") {
		t.Errorf("audit = %+v, want one ok row per chief, labelled chief", a)
	}
	r.noKeyLeaked(t, bodies...)
}

// TestTheChiefModelIsTheConfiguredOne: ChiefLLM.Model replaces the default.
func TestTheChiefModelIsTheConfiguredOne(t *testing.T) {
	r := newChiefLLMRig(t, true, ChiefLLM{Model: "vendor/another-chief-model"})
	if w := r.post(t, tokAdminChief); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if _, model := r.up.last(); model != "vendor/another-chief-model" {
		t.Fatalf("upstream model = %q, want the configured one", model)
	}
}

// TestTheChiefIsRefusedWhenItsOwnerTurnedItOff: owner 4's chief, which has a
// key, is refused because the chief is off (chiefActing), and nothing is sent.
func TestTheChiefIsRefusedWhenItsOwnerTurnedItOff(t *testing.T) {
	r := newChiefLLMRig(t, true, ChiefLLM{})
	w := r.post(t, tokOffChief)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "your owner turned the chief off") {
		t.Fatalf("an off chief: %d %s, want 403 \"your owner turned the chief off\"", w.Code, w.Body)
	}
	if got := r.up.calls(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}
	r.noKeyLeaked(t, w.Body.String())
}

// notRoutineBody and unconfiguredBody are today's refusals, byte for byte.
const (
	notRoutineBody   = "{\"error\":{\"message\":\"muster llm proxy: this agent is not a routine-tier instance of an officer that declares `llm`\",\"type\":\"permission_error\"},\"type\":\"error\"}"
	unconfiguredBody = "{\"error\":{\"message\":\"muster llm proxy: no officer on this server declares `llm`\",\"type\":\"api_error\"},\"type\":\"error\"}"
)

// TestANonChiefAgentIsStillRefused: with the chief LLM on, a worker of the
// same owner as an admitted chief is refused exactly as before — today's
// status and body — and nothing is sent. Green at the base by construction
// (there is no chief path there), so it is not red/green coverage: it guards
// the new admission against widening (admitting any agent, or dropping the
// chief predicate, turns it red).
func TestANonChiefAgentIsStillRefused(t *testing.T) {
	r := newChiefLLMRig(t, true, ChiefLLM{})
	w := r.post(t, tokAdminWorker)
	if w.Code != http.StatusServiceUnavailable || w.Body.String() != unconfiguredBody {
		t.Fatalf("a worker, no officers: %d %s, want 503 %s", w.Code, w.Body, unconfiguredBody)
	}
	if got := r.up.calls(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}
}

// TestAChiefWhoseOwnerHasNoOpenRouterKeyIsRefusedAndNothingIsSent: owner 3
// stored no key: a 400 that says where to add one, and the provider is never
// called (the admitted case's count of 1 is the positive control, in
// TestTheChiefIsServedOnItsOwnersKeyWithThePinnedModel). An admin whose
// environment has no key gets the same refusal.
func TestAChiefWhoseOwnerHasNoOpenRouterKeyIsRefusedAndNothingIsSent(t *testing.T) {
	for _, c := range []struct {
		who string
		env string
		tok string
	}{
		{"a member with no key", chiefEnvKey, tokNoKeyChief},
		{"an admin with no environment key", "", tokAdminChief},
	} {
		r := newChiefLLMRig(t, true, ChiefLLM{Keys: &credentials.Plane{EnvOpenRouterKey: c.env, Personal: chiefLLMPersonal{}}})
		w := r.post(t, c.tok)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Account → Credentials") {
			t.Fatalf("%s: %d %s, want 400 naming Account → Credentials", c.who, w.Code, w.Body)
		}
		if got := r.up.calls(); got != 0 {
			t.Fatalf("%s: upstream calls = %d, want 0", c.who, got)
		}
	}
}

// TestWithoutTheChiefLLMTheChiefIsRefusedAsBefore is an INVARIANT GUARD (it
// passes at the base too: it pins that the field's zero value changes
// nothing, not this change's behaviour): with EnableChiefLLM never called, a
// chief is refused byte for byte as before — 503 with no officer on `llm`, 403
// not-routine beside one — and nothing is sent.
func TestWithoutTheChiefLLMTheChiefIsRefusedAsBefore(t *testing.T) {
	r := newChiefLLMRig(t, false, ChiefLLM{})
	if w := r.post(t, tokAdminChief); w.Code != http.StatusServiceUnavailable || w.Body.String() != unconfiguredBody {
		t.Fatalf("no chief llm, no officer: %d %s, want 503 %s", w.Code, w.Body, unconfiguredBody)
	}
	list, err := officers.Parse(`[{"id":"triage:example-web","hq_url":"http://hq.invalid/api/v1",`+
		`"tiers":{"routine":{"model":"vendor/pinned-model","max_instances":1},"judgement":{"model":"opus","max_instances":1}},`+
		`"llm":{"daily_budget_usd":5}}]`, func(k string) string {
		if strings.HasPrefix(k, officers.LLMKeyEnvPrefix) || strings.HasPrefix(k, officers.TokenEnvPrefix) {
			return "fixture-" + k
		}
		return ""
	}, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.EnableOfficers(OfficersConfig{Officers: list, InstallSession: discardSession, LLMStore: nopLLMStore{},
		LLMUpstream: r.up.URL + "/api"}); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{tokAdminChief, tokMemberChief} {
		if w := r.post(t, tok); w.Code != http.StatusForbidden || w.Body.String() != notRoutineBody {
			t.Fatalf("no chief llm, an llm officer: %d %s, want 403 %s", w.Code, w.Body, notRoutineBody)
		}
	}
	if got := r.up.calls(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}
}

// TestBesideAnLLMOfficerTheChiefLLMAdmitsOnlyTheChief: with both on, the chief
// is served and a worker is refused with today's not-routine body.
func TestBesideAnLLMOfficerTheChiefLLMAdmitsOnlyTheChief(t *testing.T) {
	r := newChiefLLMRig(t, true, ChiefLLM{})
	list, err := officers.Parse(`[{"id":"triage:example-web","hq_url":"http://hq.invalid/api/v1",`+
		`"tiers":{"routine":{"model":"vendor/pinned-model","max_instances":1},"judgement":{"model":"opus","max_instances":1}},`+
		`"llm":{"daily_budget_usd":5}}]`, func(k string) string {
		if strings.HasPrefix(k, officers.LLMKeyEnvPrefix) || strings.HasPrefix(k, officers.TokenEnvPrefix) {
			return "fixture-" + k
		}
		return ""
	}, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.EnableOfficers(OfficersConfig{Officers: list, InstallSession: discardSession, LLMStore: nopLLMStore{},
		LLMUpstream: r.up.URL + "/api"}); err != nil {
		t.Fatal(err)
	}
	if w := r.post(t, tokAdminWorker); w.Code != http.StatusForbidden || w.Body.String() != notRoutineBody {
		t.Fatalf("a worker beside an llm officer: %d %s, want 403 %s", w.Code, w.Body, notRoutineBody)
	}
	if got := r.up.calls(); got != 0 {
		t.Fatalf("upstream calls after the worker = %d, want 0", got)
	}
	if w := r.post(t, tokAdminChief); w.Code != http.StatusOK {
		t.Fatalf("the chief beside an llm officer: %d %s, want 200", w.Code, w.Body)
	}
	if hdr, model := r.up.last(); model != DefaultChiefLLMModel || hdr.Get("Authorization") != "Bearer "+chiefEnvKey {
		t.Fatalf("the chief beside an llm officer: model %q, want the chief pin on the owner's key", model)
	}
}

// TestEnableChiefLLMRefusesWhatItCannotServe: no key resolver, or no
// connector store (the proxy's door and its audit), is an error for the
// caller to refuse to boot on.
func TestEnableChiefLLMRefusesWhatItCannotServe(t *testing.T) {
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: &chiefLLMAgents{}})
	if err := s.EnableChiefLLM(ChiefLLM{}); err == nil || !strings.Contains(err.Error(), "OpenRouter key") {
		t.Errorf("no key resolver: err = %v", err)
	}
	if err := s.EnableChiefLLM(ChiefLLM{Keys: &credentials.Plane{}}); err == nil || !strings.Contains(err.Error(), "connector store") {
		t.Errorf("no connector store: err = %v", err)
	}
	if s.chiefLLM != nil {
		t.Errorf("a refused EnableChiefLLM left the chief LLM on")
	}
}

// TestTheDefaultChiefModelIsPinned pins the literal (an OpenRouter id).
func TestTheDefaultChiefModelIsPinned(t *testing.T) {
	if DefaultChiefLLMModel != "deepseek/deepseek-v4.1-flash" {
		t.Fatalf("DefaultChiefLLMModel = %q", DefaultChiefLLMModel)
	}
}
