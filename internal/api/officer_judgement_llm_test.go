package api

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/llmproxy"
	"github.com/ZacxDev/muster/internal/officers"
)

// The judgement pool on the LLM proxy (tiers.judgement.llm): its instances are
// routed with no subscription token, admitted by the proxy door, pinned to the
// judgement model, and spend the judgement pool's OWN budget — a routine pool
// at its budget does not refuse a judgement request, nor the other way round.

const (
	jlOfficer        = "triage:example-web"
	jlJudgementModel = "deepseek/deepseek-v4-pro"
	jlRoutineModel   = "vendor/pinned-model"
	jlKeyEnv         = officers.LLMKeyEnvPrefix + "TRIAGE_EXAMPLE_WEB"
	tokJudgement1    = "judgement-llm-instance-one-token-0123"
	tokRoutine1      = "judgement-llm-routine-r1-token-012345"
)

func jlEnv(k string) string {
	if strings.HasPrefix(k, officers.LLMKeyEnvPrefix) || strings.HasPrefix(k, officers.TokenEnvPrefix) {
		return "fixture-" + k
	}
	return ""
}

// poolStore is an llmproxy.Store keyed by officer, pool and day.
type poolStore struct {
	mu     sync.Mutex
	spent  map[string]float64
	booked []llmproxy.Usage
}

func (p *poolStore) key(officer, pool string, day time.Time) string {
	return officer + "|" + pool + "|" + day.UTC().Format("2006-01-02")
}

func (p *poolStore) SpentToday(_ context.Context, officer, pool string, day time.Time) (float64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spent[p.key(officer, pool, day)], nil
}

func (p *poolStore) Book(_ context.Context, u llmproxy.Usage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.spent[p.key(u.Officer, u.Pool, u.Day)] += u.CostUSD
	p.booked = append(p.booked, u)
	return nil
}

func (p *poolStore) set(pool string, usd float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.spent[p.key(jlOfficer, pool, time.Now())] = usd
}

func (p *poolStore) last() llmproxy.Usage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.booked[len(p.booked)-1]
}

// newJudgementLLMRig: the chief rig's door and provider, with two officer
// instance rows and one officer whose tiers are tiers.
func newJudgementLLMRig(t *testing.T, tiers string) (*chiefLLMRig, *poolStore) {
	t.Helper()
	r := newChiefLLMRig(t, chiefLLMOpts{})
	cc := agents.KindClaudeCode
	r.ag.rows = append(r.ag.rows,
		agents.Agent{ID: 31, Name: "officer-triage-example-web-1", OwnerID: 1, Kind: cc, HooksToken: tokJudgement1},
		agents.Agent{ID: 32, Name: "officer-triage-example-web-r1", OwnerID: 1, Kind: cc, HooksToken: tokRoutine1})
	list, err := officers.Parse(`[{"id":"`+jlOfficer+`","hq_url":"http://hq.invalid/api/v1",`+tiers+`}]`, jlEnv, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	st := &poolStore{spent: map[string]float64{}}
	if _, err := r.s.EnableOfficers(OfficersConfig{Officers: list, InstallSession: discardSession, LLMStore: st,
		LLMUpstream: r.up.URL + "/api"}); err != nil {
		t.Fatal(err)
	}
	return r, st
}

const bothPoolsOnProxy = `"tiers":{"routine":{"model":"` + jlRoutineModel + `"},` +
	`"judgement":{"model":"` + jlJudgementModel + `","llm":{"daily_budget_usd":20}}},"llm":{"daily_budget_usd":5}`

// TestAJudgementProxyInstanceIsServedOnItsPoolsModelAndBudget: the judgement
// instance is admitted, pinned to tiers.judgement.model, sent on the
// officer's own key, and booked to the judgement pool.
func TestAJudgementProxyInstanceIsServedOnItsPoolsModelAndBudget(t *testing.T) {
	r, st := newJudgementLLMRig(t, bothPoolsOnProxy)
	w := r.post(t, tokJudgement1)
	if w.Code != http.StatusOK {
		t.Fatalf("judgement instance: %d %s, want 200", w.Code, w.Body)
	}
	hdr, model := r.up.last()
	if model != jlJudgementModel || hdr.Get("Authorization") != "Bearer fixture-"+jlKeyEnv {
		t.Fatalf("judgement instance: model %q auth %q, want the judgement pin on the officer's key", model, hdr.Get("Authorization"))
	}
	if u := st.last(); u.Pool != officers.TierJudgement || u.Instance != 1 || u.Officer != jlOfficer {
		t.Fatalf("booked %+v, want the judgement pool's instance 1", u)
	}
	if w := r.post(t, tokRoutine1); w.Code != http.StatusOK {
		t.Fatalf("routine instance: %d %s", w.Code, w.Body)
	}
	if _, model := r.up.last(); model != jlRoutineModel {
		t.Fatalf("routine instance: model %q, want the routine pin", model)
	}
	if u := st.last(); u.Pool != officers.TierRoutine || u.Instance != 1 {
		t.Fatalf("booked %+v, want the routine pool's r1", u)
	}
}

// TestEachPoolSpendsItsOwnBudget: a pool at its budget is refused (naming
// it) while the other pool is served.
func TestEachPoolSpendsItsOwnBudget(t *testing.T) {
	r, st := newJudgementLLMRig(t, bothPoolsOnProxy)
	st.set(officers.TierRoutine, 5) // the routine budget, spent
	if w := r.post(t, tokRoutine1); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "routine pool has spent its daily LLM budget") {
		t.Fatalf("routine at its budget: %d %s, want 400 naming the routine pool", w.Code, w.Body)
	}
	if w := r.post(t, tokJudgement1); w.Code != http.StatusOK {
		t.Fatalf("judgement beside a spent routine budget: %d %s, want 200", w.Code, w.Body)
	}
	st.set(officers.TierRoutine, 0)
	// Over the routine budget ($5), under the judgement one ($20): served, so
	// the judgement pool is checked against its own budget, not the routine's.
	st.set(officers.TierJudgement, 10)
	if w := r.post(t, tokJudgement1); w.Code != http.StatusOK {
		t.Fatalf("judgement at $10 of its $20: %d %s, want 200", w.Code, w.Body)
	}
	st.set(officers.TierJudgement, 20) // the judgement budget, spent
	if w := r.post(t, tokJudgement1); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "judgement pool has spent its daily LLM budget") {
		t.Fatalf("judgement at its budget: %d %s, want 400 naming the judgement pool", w.Code, w.Body)
	}
	if w := r.post(t, tokRoutine1); w.Code != http.StatusOK {
		t.Fatalf("routine beside a spent judgement budget: %d %s, want 200", w.Code, w.Body)
	}
}

// TestAJudgementInstanceWithoutItsOwnBlockIsStillRefused: beside a routine
// `llm` only, the judgement instance is refused 403 and nothing is sent.
func TestAJudgementInstanceWithoutItsOwnBlockIsStillRefused(t *testing.T) {
	r, _ := newJudgementLLMRig(t, `"tiers":{"routine":{"model":"`+jlRoutineModel+`"},"judgement":{"model":"opus"}},"llm":{"daily_budget_usd":5}`)
	if w := r.post(t, tokJudgement1); w.Code != http.StatusForbidden || w.Body.String() != notRoutineBody {
		t.Fatalf("judgement without tiers.judgement.llm: %d %s, want 403 %s", w.Code, w.Body, notRoutineBody)
	}
	if got := r.up.calls(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}
}

// TestAJudgementProxyInstanceIsRoutedWithNoToken: the session a judgement
// proxy instance gets is on the LLM route with no model, and the pod spec
// built from it carries CCD_LLM_ROUTE=muster and no CLAUDE_CODE_OAUTH_TOKEN;
// the routine instance of the same officer (on Claude) keeps its model and
// its token.
func TestAJudgementProxyInstanceIsRoutedWithNoToken(t *testing.T) {
	list, err := officers.Parse(`[{"id":"`+jlOfficer+`","hq_url":"http://hq.invalid/api/v1",`+
		`"tiers":{"routine":{"model":"haiku"},"judgement":{"model":"`+jlJudgementModel+`","llm":{"daily_budget_usd":20}}}}]`, jlEnv, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: &officerAgents{}, Provisioner: stubProvisioner{}, SessionLiveness: stubLiveness{},
		Kinds: stubKinds{pick: "acct-one"}, Connectors: presenceStore{}})
	var installed func(agents.Agent) (agentspec.ClaudeCodeSession, bool)
	if _, err := s.EnableOfficers(OfficersConfig{Officers: list, LLMStore: nopLLMStore{},
		InstallSession: func(f func(agents.Agent) (agentspec.ClaudeCodeSession, bool)) { installed = f }}); err != nil {
		t.Fatal(err)
	}
	const subToken = "fixture-subscription-token-must-not-ship"
	cfg := agentspec.Config{ImageRepo: "ghcr.io/example-org/agent-runtime", ImageTag: "1.0.0", APIBaseURL: "http://muster.example.test:8105",
		ConnectURL: "http://muster.example.test:18191", ConnectPort: 18191,
		ClaudeCode: &agentspec.ClaudeCodeConfig{Image: "ghcr.io/example-org/claude-code-agent:0a1b2c3", Accounts: map[string]string{"alpha": subToken}}}
	build := func(name string) (agentspec.ClaudeCodeSession, map[string]string) {
		a := agents.Agent{ID: 77, Name: name, Kind: agents.KindClaudeCode, CCAccount: "alpha", HooksToken: "fixture-hooks-" + name,
			GatewaySecret: "fixture-turn-" + name}
		sess, ok := installed(a)
		if !ok {
			t.Fatalf("%s: not an officer instance", name)
		}
		spec, err := agentspec.Build(a, cfg, agentspec.Options{ClaudeCodeSession: sess})
		if err != nil {
			t.Fatalf("%s: Build: %v", name, err)
		}
		env := map[string]string{}
		for _, e := range spec.Env {
			env[e.Name] = e.Value
		}
		for _, e := range spec.Secrets {
			env[e.Name] = e.Value
		}
		return sess, env
	}
	sess, env := build("officer-triage-example-web-1")
	if !sess.LLMRoute || sess.Model != "" || !sess.BypassPermissions {
		t.Fatalf("judgement proxy session %+v, want LLMRoute, no model, bypass", sess)
	}
	if _, ok := env["CLAUDE_CODE_OAUTH_TOKEN"]; ok || env[agentspec.EnvLLMRoute] != agentspec.LLMRouteMuster {
		t.Fatalf("judgement proxy pod env: token present %v, %s=%q", ok, agentspec.EnvLLMRoute, env[agentspec.EnvLLMRoute])
	}
	sess, env = build("officer-triage-example-web-r1")
	if sess.LLMRoute || sess.Model != "haiku" {
		t.Fatalf("routine Claude session %+v, want no route and model haiku", sess)
	}
	if env["CLAUDE_CODE_OAUTH_TOKEN"] != subToken {
		t.Fatal("the routine Claude instance lost its subscription token")
	}
	if _, ok := env[agentspec.EnvLLMRoute]; ok {
		t.Fatal("the routine Claude instance is on the LLM route")
	}
}
