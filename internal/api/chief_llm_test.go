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
	"github.com/ZacxDev/muster/internal/officers"
)

// The chief on the LLM proxy (chief_llm.go), through the real handler and the
// real connector door, against a stub provider. In memory: the agents store,
// the connector audit and the chief toggle are fakes.

// chiefAdminKey is a distinctive admin key (MUSTER_AGENT_OPENROUTER_KEY): it
// must reach the provider and appear in no log line, audit detail or response.
const chiefAdminKey = "sk-or-v1-envadmin-1f2e3d4c5b6a79881f2e3d4c5b6a79881f2e3d4c5b6a7988"

// The agents' own muster tokens: never forwarded upstream.
const (
	tokAdminChief  = "chief-llm-admin-chief-token-0123456789"
	tokAdminWorker = "chief-llm-admin-worker-token-012345678"
	tokMemberChief = "chief-llm-member-chief-token-012345678"
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

// chiefLLMRows: the admin's (user 1) chief and a worker, and a member's (user
// 2) chief.
func chiefLLMRows() []agents.Agent {
	cc := agents.KindClaudeCode
	return []agents.Agent{
		{ID: 10, Name: "zesty-stoat", DisplayName: "chief", OwnerID: 1, Kind: cc, HooksToken: tokAdminChief},
		{ID: 11, Name: "busy-worker", DisplayName: "worker", OwnerID: 1, Kind: cc, HooksToken: tokAdminWorker},
		{ID: 20, Name: "member-chief", DisplayName: "chief", OwnerID: 2, Kind: cc, HooksToken: tokMemberChief},
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

// chiefLLMPrefs: off lists the owners who turned the chief off; nobody else
// chose.
type chiefLLMPrefs struct{ off map[int64]bool }

func (p chiefLLMPrefs) Choice(_ context.Context, owner int64) (bool, bool, error) {
	if p.off[owner] {
		return true, false, nil
	}
	return false, false, nil
}
func (chiefLLMPrefs) SetEnabled(context.Context, int64, bool) error { return nil }

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

// chiefLLMOpts: enable calls EnableChiefLLM with cfg (Upstream filled in when
// unset); off lists the owners who turned their chief off.
type chiefLLMOpts struct {
	enable bool
	cfg    ChiefLLM
	off    map[int64]bool
}

func newChiefLLMRig(t *testing.T, o chiefLLMOpts) *chiefLLMRig {
	t.Helper()
	r := &chiefLLMRig{up: newChiefProvider(t), audit: &chiefLLMAudit{}, logs: &llmLogs{}}
	r.s = New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(r.logs, "", 0))
	r.s.UseExtensions(Extensions{Agents: &chiefLLMAgents{rows: chiefLLMRows()}, Connectors: r.audit, ChiefPrefs: chiefLLMPrefs{off: o.off},
		Provisioner: stubProvisioner{}, SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}})
	if o.enable {
		if o.cfg.Upstream == "" {
			o.cfg.Upstream = r.up.URL + "/api"
		}
		if err := r.s.EnableChiefLLM(o.cfg); err != nil {
			t.Fatal(err)
		}
	}
	r.h = r.s.Handler()
	return r
}

// chiefLLMOn is a rig with the chief LLM enabled on the admin key.
func chiefLLMOn(t *testing.T) *chiefLLMRig {
	t.Helper()
	return newChiefLLMRig(t, chiefLLMOpts{enable: true, cfg: ChiefLLM{Key: chiefAdminKey}})
}

func (r *chiefLLMRig) post(t *testing.T, tok string) *httptest.ResponseRecorder {
	t.Helper()
	return llmPost(t, r.h, "/llm/v1/messages?beta=true", map[string]string{"Authorization": "Bearer " + tok, "X-Muster-Token": tok})
}

// noKeyLeaked fails when the key appears in the logs, an audit detail or body.
func (r *chiefLLMRig) noKeyLeaked(t *testing.T, bodies ...string) {
	t.Helper()
	where := append([]string{r.logs.String()}, bodies...)
	for _, e := range r.audit.entries() {
		where = append(where, e.Detail)
	}
	for _, w := range where {
		if strings.Contains(w, chiefAdminKey) {
			t.Fatalf("the key leaked into %q", w)
		}
	}
}

// enableOfficer declares one `llm` officer on r's server.
func (r *chiefLLMRig) enableOfficer(t *testing.T) {
	t.Helper()
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
}

// TestTheAdminsChiefIsServedOnTheAdminKeyWithThePinnedModel: with the chief
// LLM on, the admin's chief is served on ChiefLLM.Key, pinned to the chief
// model, with its own muster token not forwarded, never refused by a budget,
// and audited as "chief".
//
// RED at the base (EnableChiefLLM does not exist; with it stubbed in, the
// chief is refused 503 "no officer on this server declares `llm`").
func TestTheAdminsChiefIsServedOnTheAdminKeyWithThePinnedModel(t *testing.T) {
	r := chiefLLMOn(t)
	w := r.post(t, tokAdminChief)
	if strings.Contains(w.Body.String(), "budget") {
		t.Fatalf("the admin's chief was refused by a budget: %d %s", w.Code, w.Body)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("the admin's chief: %d %s, want 200", w.Code, w.Body)
	}
	if got := r.up.calls(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	hdr, model := r.up.last()
	if got := hdr.Get("Authorization"); got != "Bearer "+chiefAdminKey {
		t.Errorf("upstream Authorization is not the admin key (%d bytes)", len(got))
	}
	if model != DefaultChiefLLMModel {
		t.Errorf("upstream model = %q, want the pin %q", model, DefaultChiefLLMModel)
	}
	if hdr.Get("X-Muster-Token") != "" || strings.Contains(strings.Join(hdr.Values("Authorization"), " "), tokAdminChief) {
		t.Errorf("the agent's own muster token was forwarded: %v", hdr)
	}
	a := r.audit.entries()
	if len(a) != 1 || a[0].AgentName != "zesty-stoat" || !strings.Contains(a[0].Detail, "officer=chief ") ||
		!strings.Contains(a[0].Detail, "result=ok") {
		t.Errorf("audit = %+v, want one ok row, labelled chief", a)
	}
	r.noKeyLeaked(t, w.Body.String())
}

// TestTheChiefModelIsTheConfiguredOne: ChiefLLM.Model replaces the default.
func TestTheChiefModelIsTheConfiguredOne(t *testing.T) {
	r := newChiefLLMRig(t, chiefLLMOpts{enable: true, cfg: ChiefLLM{Key: chiefAdminKey, Model: "vendor/another-chief-model"}})
	if w := r.post(t, tokAdminChief); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if _, model := r.up.last(); model != "vendor/another-chief-model" {
		t.Fatalf("upstream model = %q, want the configured one", model)
	}
}

// TestAnotherUsersChiefIsRefused: a member's chief — the chief by the same
// predicate, and on — is refused 403 with its own message, and the admin key
// is never sent for it.
func TestAnotherUsersChiefIsRefused(t *testing.T) {
	r := chiefLLMOn(t)
	w := r.post(t, tokMemberChief)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), chiefNotAdminRefusal) {
		t.Fatalf("a member's chief: %d %s, want 403 %q", w.Code, w.Body, chiefNotAdminRefusal)
	}
	if got := r.up.calls(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}
}

// TestTheChiefIsRefusedWhenTheAdminTurnedItOff: the admin's chief, with the
// chief off, is refused (chiefActing) and nothing is sent.
func TestTheChiefIsRefusedWhenTheAdminTurnedItOff(t *testing.T) {
	r := newChiefLLMRig(t, chiefLLMOpts{enable: true, cfg: ChiefLLM{Key: chiefAdminKey}, off: map[int64]bool{1: true}})
	w := r.post(t, tokAdminChief)
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
// admin is refused exactly as before — today's status and body — and nothing
// is sent. Green at the base by construction (there is no chief path there),
// so it is not red/green coverage: it guards the new admission against
// widening (admitting any agent, or dropping the chief predicate, turns it
// red).
func TestANonChiefAgentIsStillRefused(t *testing.T) {
	r := chiefLLMOn(t)
	w := r.post(t, tokAdminWorker)
	if w.Code != http.StatusServiceUnavailable || w.Body.String() != unconfiguredBody {
		t.Fatalf("a worker, no officers: %d %s, want 503 %s", w.Code, w.Body, unconfiguredBody)
	}
	if got := r.up.calls(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}
}

// TestWithAnEmptyKeyTheChiefIsRefusedAndNothingIsSent: EnableChiefLLM with
// no key succeeds, and each request is refused 400 naming
// MUSTER_AGENT_OPENROUTER_KEY in .env; the provider is never called (the
// admitted case's count of 1 is the positive control, in
// TestTheAdminsChiefIsServedOnTheAdminKeyWithThePinnedModel).
func TestWithAnEmptyKeyTheChiefIsRefusedAndNothingIsSent(t *testing.T) {
	r := newChiefLLMRig(t, chiefLLMOpts{enable: true, cfg: ChiefLLM{Key: "  "}})
	w := r.post(t, tokAdminChief)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "set MUSTER_AGENT_OPENROUTER_KEY in .env") ||
		strings.Contains(w.Body.String(), "Account") {
		t.Fatalf("an empty key: %d %s, want 400 naming MUSTER_AGENT_OPENROUTER_KEY in .env", w.Code, w.Body)
	}
	if got := r.up.calls(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}
}

// TestWithoutTheChiefLLMTheChiefIsRefusedAsBefore is an INVARIANT GUARD (it
// passes at the base too: it pins that the off state changes nothing, not
// this change's behaviour): with EnableChiefLLM never called, a chief is
// refused byte for byte as before — 503 with no officer on `llm`, 403
// not-routine beside one — and nothing is sent.
func TestWithoutTheChiefLLMTheChiefIsRefusedAsBefore(t *testing.T) {
	r := newChiefLLMRig(t, chiefLLMOpts{})
	if w := r.post(t, tokAdminChief); w.Code != http.StatusServiceUnavailable || w.Body.String() != unconfiguredBody {
		t.Fatalf("no chief llm, no officer: %d %s, want 503 %s", w.Code, w.Body, unconfiguredBody)
	}
	r.enableOfficer(t)
	for _, tok := range []string{tokAdminChief, tokMemberChief} {
		if w := r.post(t, tok); w.Code != http.StatusForbidden || w.Body.String() != notRoutineBody {
			t.Fatalf("no chief llm, an llm officer: %d %s, want 403 %s", w.Code, w.Body, notRoutineBody)
		}
	}
	if got := r.up.calls(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}
}

// TestBesideAnLLMOfficerTheChiefLLMAdmitsOnlyTheChief: with both on, the
// admin's chief is served and a worker is refused with today's not-routine
// body.
func TestBesideAnLLMOfficerTheChiefLLMAdmitsOnlyTheChief(t *testing.T) {
	r := chiefLLMOn(t)
	r.enableOfficer(t)
	if w := r.post(t, tokAdminWorker); w.Code != http.StatusForbidden || w.Body.String() != notRoutineBody {
		t.Fatalf("a worker beside an llm officer: %d %s, want 403 %s", w.Code, w.Body, notRoutineBody)
	}
	if got := r.up.calls(); got != 0 {
		t.Fatalf("upstream calls after the worker = %d, want 0", got)
	}
	if w := r.post(t, tokAdminChief); w.Code != http.StatusOK {
		t.Fatalf("the chief beside an llm officer: %d %s, want 200", w.Code, w.Body)
	}
	if hdr, model := r.up.last(); model != DefaultChiefLLMModel || hdr.Get("Authorization") != "Bearer "+chiefAdminKey {
		t.Fatalf("the chief beside an llm officer: model %q, want the chief pin on the admin key", model)
	}
}

// TestEnableChiefLLMRefusesWhatItCannotServe: no connector store (the
// proxy's door and its audit) is an error for the caller to refuse to boot on.
func TestEnableChiefLLMRefusesWhatItCannotServe(t *testing.T) {
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: &chiefLLMAgents{}})
	if err := s.EnableChiefLLM(ChiefLLM{Key: chiefAdminKey}); err == nil || !strings.Contains(err.Error(), "connector store") {
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
