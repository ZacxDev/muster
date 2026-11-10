package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/apitokens"
	"github.com/ZacxDev/muster/internal/ccusage"
	"github.com/ZacxDev/muster/internal/officers"
)

const ccUsageServiceToken = "cc-usage-test-service-token-0123456789abcdef"

// fakeCCUsage is CCUsageStore in memory, recording the owner each read was
// made for, so a test can tell "refused at the door" (no read) from "read".
type fakeCCUsage struct {
	rows   map[string]ccusage.Row
	agents []ccusage.AgentUse
	jobs   []ccusage.JobCount
	owners []int64
}

func (f *fakeCCUsage) Latest(_ context.Context, owner int64) (map[string]ccusage.Row, error) {
	f.owners = append(f.owners, owner)
	return f.rows, nil
}

func (f *fakeCCUsage) AgentsSince(context.Context, int64, time.Time, time.Time) ([]ccusage.AgentUse, error) {
	return f.agents, nil
}

func (f *fakeCCUsage) JobCounts(context.Context, int64, time.Time) ([]ccusage.JobCount, error) {
	return f.jobs, nil
}

func pct(v float64) *float64 { return &v }

func newCCUsageFixture(t *testing.T) (http.Handler, *scopedTokens, *fakeCCUsage) {
	t.Helper()
	reset := time.Date(2026, 11, 10, 20, 0, 0, 0, time.UTC)
	store := &fakeCCUsage{rows: map[string]ccusage.Row{
		"alpha": {Account: "alpha", ReadAt: time.Now().Add(-time.Minute), CheckedAt: time.Now().Add(-time.Minute),
			Source: ccusage.SourceRateLimitHeaders, Status: "allowed", RepresentativeClaim: "seven_day", UsageAPI: ccusage.UsageAPIRefused,
			TokenFP: "0123456789ab", Windows: map[string]ccusage.Window{
				ccusage.FiveHour: {Utilization: pct(42), ResetsAt: &reset},
				ccusage.SevenDay: {Utilization: pct(17.5), ResetsAt: &reset},
				"seven_day_opus": {Utilization: pct(3), ResetsAt: &reset},
			}},
		"beta": {Account: "beta", CheckedAt: time.Now(), ErrorKind: ccusage.KindForbidden, Error: "refused", TokenFP: "ba9876543210"},
	}, jobs: []ccusage.JobCount{{CCAccount: "alpha", RequestedBy: "pr-review", Last24h: 2, Last7d: 5}},
		agents: []ccusage.AgentUse{{Name: "keen-wren", CCAccount: "alpha", LastActive: time.Now()}}}
	toks := newScopedTokens()
	s := New(nil, AuthConfig{HookToken: tokTestHook, ServiceToken: ccUsageServiceToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{APITokens: toks, Users: stubUsers{}, CCUsage: store, CCUsageInterval: 5 * time.Minute})
	return s.Handler(), toks, store
}

func ccUsageGet(h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestCCAccountUsageIsRefusedWithoutTheScope: the route answers only the
// service tier (token AND actor) and a personal token whose scope reaches it
// (account-usage, or full). No credential, the hook token, the service token
// without an actor, a bar-summary token and an agent-create token are refused
// BEFORE anything is read (the store records no read).
func TestCCAccountUsageIsRefusedWithoutTheScope(t *testing.T) {
	h, toks, store := newCCUsageFixture(t)
	summary := toks.addScoped(t, 1, apitokens.ScopeSummary)
	create := toks.addScoped(t, 1, apitokens.ScopeAgentCreate)
	refused := []struct {
		name string
		hdr  map[string]string
		code int
	}{
		{"no credential", nil, http.StatusUnauthorized},
		{"the hook token", map[string]string{"X-Muster-Token": tokTestHook, ActorHeader: "hq"}, http.StatusUnauthorized},
		{"a wrong service token", map[string]string{"Authorization": "Bearer " + ccUsageServiceToken + "x", ActorHeader: "hq"}, http.StatusUnauthorized},
		{"the service token with no actor", map[string]string{"Authorization": "Bearer " + ccUsageServiceToken}, http.StatusForbidden},
		{"a bar-summary token", map[string]string{"Authorization": "Bearer " + summary}, http.StatusForbidden},
		{"an agent-create token", map[string]string{"Authorization": "Bearer " + create}, http.StatusForbidden},
	}
	for _, c := range refused {
		if rec := ccUsageGet(h, CCAccountUsagePath, c.hdr); rec.Code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, rec.Code, rec.Body, c.code)
		}
	}
	if len(store.owners) != 0 {
		t.Fatalf("refused requests read the store %d time(s)", len(store.owners))
	}
	usage := toks.addScoped(t, 1, apitokens.ScopeAccountUsage)
	full := toks.addScoped(t, 1, apitokens.ScopeFull)
	for _, c := range []struct {
		name string
		hdr  map[string]string
	}{
		{"the service tier", map[string]string{"Authorization": "Bearer " + ccUsageServiceToken, ActorHeader: "hq"}},
		{"an account-usage token", map[string]string{"Authorization": "Bearer " + usage}},
		{"a full token", map[string]string{"Authorization": "Bearer " + full}},
	} {
		if rec := ccUsageGet(h, CCAccountUsagePath, c.hdr); rec.Code != http.StatusOK {
			t.Errorf("%s: %d %s, want 200", c.name, rec.Code, rec.Body)
		}
	}
	if len(store.owners) != 3 || store.owners[0] != 1 {
		t.Fatalf("admitted reads were made for %v, want three for user 1", store.owners)
	}
}

// TestAnAccountUsageTokenReachesNothingElse: the scope reaches its one route;
// on any other the token is refused 403 with the account-usage sentence.
func TestAnAccountUsageTokenReachesNothingElse(t *testing.T) {
	h, toks, _ := newCCUsageFixture(t)
	usage := toks.addScoped(t, 1, apitokens.ScopeAccountUsage)
	for _, rt := range summaryTokenRefused {
		code, body := bearerReach(h, rt.method, rt.path, usage)
		if code != http.StatusForbidden || !strings.Contains(body, AccountUsageTokenRefusal) {
			t.Errorf("%s %s with an account-usage token: %d %s, want 403 naming the scope", rt.method, rt.path, code, body)
		}
	}
	if code, body := bearerReach(h, http.MethodGet, attentionSummaryPath, usage); code != http.StatusForbidden {
		t.Errorf("the attention summary with an account-usage token: %d %s, want 403", code, body)
	}
}

// TestCCAccountUsageAnswersNumbersAndWhoUsedIt: the answer carries each
// account's windows, read time, error and attribution — and never the token
// fingerprint the row holds.
func TestCCAccountUsageAnswersNumbersAndWhoUsedIt(t *testing.T) {
	h, toks, _ := newCCUsageFixture(t)
	rec := ccUsageGet(h, CCAccountUsagePath, map[string]string{"Authorization": "Bearer " + toks.addScoped(t, 1, apitokens.ScopeAccountUsage)})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, fp := range []string{"0123456789ab", "ba9876543210"} {
		if strings.Contains(body, fp) {
			t.Fatalf("the answer carries the token fingerprint %s: %s", fp, body)
		}
	}
	var got ccUsageAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.PollIntervalSeconds != 300 || len(got.Accounts) != 2 || got.Accounts[0].Account != "alpha" || got.Accounts[1].Account != "beta" {
		t.Fatalf("answer: %+v", got)
	}
	a := got.Accounts[0]
	if a.Source != ccusage.SourceRateLimitHeaders || a.Status != "allowed" || a.RepresentativeClaim != "seven_day" {
		t.Fatalf("alpha source/status: %+v", a)
	}
	if strings.Contains(body, `"usage_api_next_at"`) || strings.Contains(body, `"usage_api":`) {
		t.Fatalf("the answer carries the poller's internal state: %s", body)
	}
	if a.FiveHour == nil || *a.FiveHour.Utilization != 42 || *a.FiveHour.ResetsAt != "2026-11-10T20:00:00Z" ||
		a.SevenDay == nil || *a.SevenDay.Utilization != 17.5 || a.Windows["seven_day_opus"].Utilization == nil || a.ReadAt == nil || a.Error != "" {
		t.Fatalf("alpha: %+v", a)
	}
	if len(a.UsedBy.Last24h.Jobs) != 1 || a.UsedBy.Last24h.Jobs[0] != (ccusage.JobUse{RequestedBy: "pr-review", Jobs: 2}) ||
		a.UsedBy.Last7d.Jobs[0].Jobs != 5 || a.UsedBy.Last24h.InteractiveAgents != 1 {
		t.Fatalf("alpha used by: %+v", a.UsedBy)
	}
	b := got.Accounts[1]
	if b.FiveHour != nil || b.SevenDay != nil || b.ReadAt != nil || b.ErrorKind != ccusage.KindForbidden || b.Error != "refused" {
		t.Fatalf("beta: %+v", b)
	}
	if b.UsedBy.Last24h.Jobs == nil || b.UsedBy.Last24h.Officers == nil {
		t.Fatalf("beta's empty attribution must be empty lists, not null: %+v", b.UsedBy)
	}
}

// TestCCAccountUsageListsTheConfiguredAccounts: with the account pool wired,
// the answer lists exactly the owner's configured accounts — one never read is
// listed with null windows, and a reading of an account no longer configured is
// not listed.
func TestCCAccountUsageListsTheConfiguredAccounts(t *testing.T) {
	store := &fakeCCUsage{rows: map[string]ccusage.Row{
		"alpha":   {Account: "alpha", ReadAt: time.Now(), Windows: map[string]ccusage.Window{ccusage.FiveHour: {Utilization: pct(1)}}},
		"retired": {Account: "retired", ReadAt: time.Now()},
	}}
	toks := newScopedTokens()
	s := New(nil, AuthConfig{HookToken: tokTestHook, ServiceToken: ccUsageServiceToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{APITokens: toks, Users: stubUsers{}, CCUsage: store, Kinds: stubKinds{accounts: []string{"gamma", "alpha"}}})
	rec := ccUsageGet(s.Handler(), CCAccountUsagePath, map[string]string{"Authorization": "Bearer " + toks.addScoped(t, 1, apitokens.ScopeAccountUsage)})
	var got ccUsageAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body)
	}
	if len(got.Accounts) != 2 || got.Accounts[0].Account != "alpha" || got.Accounts[1].Account != "gamma" {
		t.Fatalf("accounts = %+v, want alpha then gamma", got.Accounts)
	}
	if got.Accounts[0].FiveHour == nil || got.Accounts[1].FiveHour != nil || got.Accounts[1].ReadAt != nil || got.PollIntervalSeconds != 0 {
		t.Fatalf("answer = %+v", got)
	}
}

// TestAProxiedOfficerInstanceIsNotCreditedToItsAccount: officerPoolOf answers
// onProxy for a judgement instance whose pool is on the LLM proxy (it holds no
// Claude token) and not for the same officer's Claude-account routine
// instance, so the usage answer counts only the latter on the account.
func TestAProxiedOfficerInstanceIsNotCreditedToItsAccount(t *testing.T) {
	list, err := officers.Parse(`[{"id":"`+jlOfficer+`","hq_url":"http://hq.invalid/api/v1",`+
		`"tiers":{"routine":{"model":"haiku"},"judgement":{"model":"`+jlJudgementModel+`","llm":{"daily_budget_usd":20}}}}]`, jlEnv, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeCCUsage{rows: map[string]ccusage.Row{}, agents: []ccusage.AgentUse{
		{Name: "officer-triage-example-web-1", CCAccount: "alpha", LastActive: time.Now()},
		{Name: "officer-triage-example-web-r1", CCAccount: "alpha", LastActive: time.Now()},
	}}
	toks := newScopedTokens()
	s := New(nil, AuthConfig{HookToken: tokTestHook, ServiceToken: ccUsageServiceToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{APITokens: toks, Users: stubUsers{}, CCUsage: store, Kinds: stubKinds{accounts: []string{"alpha"}, pick: "alpha"},
		Agents: &officerAgents{}, Provisioner: stubProvisioner{}, SessionLiveness: stubLiveness{}, Connectors: presenceStore{}})
	if _, err := s.EnableOfficers(OfficersConfig{Officers: list, LLMStore: nopLLMStore{}, InstallSession: discardSession}); err != nil {
		t.Fatal(err)
	}
	if _, _, onProxy, ok := s.officerPoolOf("officer-triage-example-web-1"); !ok || !onProxy {
		t.Fatalf("the judgement instance: onProxy=%v ok=%v, want both true", onProxy, ok)
	}
	if _, _, onProxy, ok := s.officerPoolOf("officer-triage-example-web-r1"); !ok || onProxy {
		t.Fatalf("the routine instance: onProxy=%v ok=%v, want an officer instance off the proxy", onProxy, ok)
	}
	rec := ccUsageGet(s.Handler(), CCAccountUsagePath, map[string]string{"Authorization": "Bearer " + toks.addScoped(t, 1, apitokens.ScopeAccountUsage)})
	var got ccUsageAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK || len(got.Accounts) != 1 {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body)
	}
	want := []ccusage.OfficerUse{{Officer: jlOfficer, Pool: officers.TierRoutine, Instances: 1}}
	if o := got.Accounts[0].UsedBy.Last24h.Officers; len(o) != 1 || o[0] != want[0] {
		t.Fatalf("alpha's officers = %+v, want only the routine instance %+v", o, want)
	}
}
