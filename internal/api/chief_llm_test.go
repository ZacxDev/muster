package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/metrics"
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
// listErr, when set, fails every List (an injected read failure); lists
// counts the List calls.
type chiefLLMAgents struct {
	agents.Store
	rows []agents.Agent

	mu      sync.Mutex
	listErr error
	lists   int
}

func (s *chiefLLMAgents) failLists(err error) { s.mu.Lock(); s.listErr = err; s.mu.Unlock() }

func (s *chiefLLMAgents) listCalls() int { s.mu.Lock(); defer s.mu.Unlock(); return s.lists }

func (s *chiefLLMAgents) List(ctx context.Context) ([]agents.Agent, error) {
	s.mu.Lock()
	s.lists++
	err := s.listErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
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
//
// status, when set (answer), is answered instead of a stream, with an error
// body.
type chiefProvider struct {
	*httptest.Server
	mu     sync.Mutex
	hdrs   []http.Header
	model  []string
	status int
}

func (p *chiefProvider) answer(status int) { p.mu.Lock(); p.status = status; p.mu.Unlock() }

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
		status := p.status
		p.mu.Unlock()
		if r.URL.Path != "/api/v1/messages" {
			http.NotFound(w, r)
			return
		}
		if status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"Key limit exceeded","code":`+strconv.Itoa(status)+`}}`)
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
	ag    *chiefLLMAgents
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
	r := &chiefLLMRig{up: newChiefProvider(t), audit: &chiefLLMAudit{}, logs: &llmLogs{}, ag: &chiefLLMAgents{rows: chiefLLMRows()}}
	r.s = New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(r.logs, "", 0))
	r.s.UseExtensions(Extensions{Agents: r.ag, Connectors: r.audit, ChiefPrefs: chiefLLMPrefs{off: o.off},
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

// chiefCreditRemedyText is the literal the chief must read when its key's
// credit limit is spent (pinned, not derived from chiefKeyRemedy).
const chiefCreditRemedyText = "it is the server's MUSTER_AGENT_OPENROUTER_KEY: check it, and its credit limit, then restart muster"

// TestTheChiefIsToldToCheckItsCreditLimitOnA402AndA403: through the real
// route, a provider 402 (a spent credit limit) and a provider 403 both end the
// chief's turn with a 400 that names MUSTER_AGENT_OPENROUTER_KEY and its credit
// limit. The 402 is not counted as a refused key; the 403 is.
//
// The 402 half is RED at 528fc231 (the 402 was relayed with the provider's
// body). The 403 half is an INVARIANT GUARD, green there too.
func TestTheChiefIsToldToCheckItsCreditLimitOnA402AndA403(t *testing.T) {
	r := chiefLLMOn(t)
	auth402 := metricsOfficerLLMUpstreamAuth("chief", "402")
	r.up.answer(http.StatusPaymentRequired)
	w := r.post(t, tokAdminChief)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), chiefCreditRemedyText) ||
		!strings.Contains(w.Body.String(), "HTTP 402") {
		t.Fatalf("a 402 to the chief: %d %s, want 400 with %q", w.Code, w.Body, chiefCreditRemedyText)
	}
	if got := metricsOfficerLLMUpstreamAuth("chief", "402") - auth402; got != 0 {
		t.Errorf("a 402 was counted as %v refused-key answers", got)
	}
	if strings.Contains(r.logs.String(), "THE PROVIDER REFUSES") {
		t.Errorf("a 402 logged the refused-key line:\n%s", r.logs.String())
	}
	r.up.answer(http.StatusForbidden)
	w = r.post(t, tokAdminChief)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), chiefCreditRemedyText) ||
		!strings.Contains(w.Body.String(), "HTTP 403") {
		t.Fatalf("a 403 to the chief: %d %s, want 400 with %q", w.Code, w.Body, chiefCreditRemedyText)
	}
	if got := r.up.calls(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
	r.noKeyLeaked(t, w.Body.String())
}

// TestAFailedAgentsReadIsA503NotTheOfficerRefusal: with the chief LLM on and
// the agents list unreadable, the admin's chief is refused 503 with the
// agents-read message, not passed on to the officer checks ("no officer
// declares `llm`" with none declared, "not a routine-tier instance" beside
// one), and nothing is sent.
//
// RED at 528fc231 (the read error was "not the chief": the officer refusals).
func TestAFailedAgentsReadIsA503NotTheOfficerRefusal(t *testing.T) {
	for _, withOfficer := range []bool{false, true} {
		r := chiefLLMOn(t)
		if withOfficer {
			r.enableOfficer(t)
		}
		r.ag.failLists(errors.New("injected: agents list unreadable"))
		w := r.post(t, tokAdminChief)
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), ChiefLLMAgentsReadRefusal) {
			t.Errorf("officer %v, an unreadable agents list: %d %s, want 503 %q", withOfficer, w.Code, w.Body, ChiefLLMAgentsReadRefusal)
			continue
		}
		if got := r.up.calls(); got != 0 {
			t.Fatalf("officer %v: upstream calls = %d, want 0", withOfficer, got)
		}
		if !strings.Contains(r.logs.String(), "injected: agents list unreadable") {
			t.Errorf("officer %v: the read error was not logged:\n%s", withOfficer, r.logs.String())
		}
	}
}

// TestAnOfficerRequestDoesNotReadTheAgentsList: with the chief LLM on, a
// routine instance of an `llm` officer is served on the officer path without
// an agents-list read (its name alone rules out the chief). The chief's own
// request, which must read it, is the positive control.
//
// RED at 528fc231 (every request read the list to ask whether it was the
// chief).
func TestAnOfficerRequestDoesNotReadTheAgentsList(t *testing.T) {
	const tokRoutine = "chief-llm-officer-routine-token-0123"
	r := chiefLLMOn(t)
	r.ag.rows = append(r.ag.rows, agents.Agent{ID: 30, Name: "officer-triage-example-web-r1", DisplayName: "triage",
		OwnerID: 1, Kind: agents.KindClaudeCode, HooksToken: tokRoutine})
	r.enableOfficer(t)
	before := r.ag.listCalls()
	w := r.post(t, tokRoutine)
	if w.Code != http.StatusOK {
		t.Fatalf("a routine instance: %d %s, want 200", w.Code, w.Body)
	}
	if _, model := r.up.last(); model != "vendor/pinned-model" {
		t.Fatalf("a routine instance: upstream model %q, want the officer's pin (the officer path)", model)
	}
	if got := r.ag.listCalls() - before; got != 0 {
		t.Errorf("a routine instance's request read the agents list %d times, want 0", got)
	}
	before = r.ag.listCalls()
	if w := r.post(t, tokAdminChief); w.Code != http.StatusOK {
		t.Fatalf("the chief: %d %s", w.Code, w.Body)
	}
	if got := r.ag.listCalls() - before; got < 1 {
		t.Fatalf("positive control: the chief's request read the agents list %d times, want >= 1", got)
	}
}

func metricsOfficerLLMUpstreamAuth(officer, status string) float64 {
	return testutil.ToFloat64(metrics.OfficerLLMUpstreamAuth.WithLabelValues(officer, status))
}
