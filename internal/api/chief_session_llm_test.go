package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefusage"
)

// The chief sessions' spend door (chief_session_llm.go) in memory: the real
// handler and connector door, the chief LLM rig's fakes (chief_llm_test.go),
// and a spend book that does NOT apply row-level security — so the door's own
// owner check is what refuses another owner's session here. The database's
// half is chief_session_llm_pg_test.go.

// chiefSessionsKey is the chief sessions' own key in these tests: distinct
// from chiefAdminKey (MUSTER_AGENT_OPENROUTER_KEY), which must never reach the
// provider for a session.
const chiefSessionsKey = "sk-or-v1-sessions-9c8b7a6f5e4d3c2b1a09f8e7d6c5b4a39c8b7a6f5e4d3c2b"

// fakeSessionLLM is a SessionLLMStore with no row-level security.
type fakeSessionLLM struct {
	mu     sync.Mutex
	refs   map[string]chiefusage.SessionRef
	booked []chiefusage.SessionLLMUsage
	held   map[int64]fakeHold
	next   int64
}

// fakeHold is one reservation in fakeSessionLLM.
type fakeHold struct {
	session int64
	usd     float64
}

func (f *fakeSessionLLM) SessionRef(_ context.Context, key string) (chiefusage.SessionRef, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.refs[key]
	return r, ok, nil
}

func (f *fakeSessionLLM) spendLocked(id int64) chiefusage.SessionSpend {
	var sp chiefusage.SessionSpend
	for _, u := range f.booked {
		sp.DaySpent += u.CostUSD
		if u.SessionID == id {
			sp.SessionSpent += u.CostUSD
		}
	}
	for _, r := range f.held {
		sp.DayReserved += r.usd
		if r.session == id {
			sp.SessionReserved += r.usd
		}
	}
	return sp
}

func (f *fakeSessionLLM) Spend(_ context.Context, id int64, _ time.Time) (chiefusage.SessionSpend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spendLocked(id), nil
}

func (f *fakeSessionLLM) Reserve(_ context.Context, id int64, _ time.Time, _ time.Duration,
	decide func(chiefusage.SessionSpend) float64) (int64, chiefusage.SessionSpend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sp := f.spendLocked(id)
	usd := decide(sp)
	if !(usd > 0) {
		return 0, sp, nil
	}
	if f.held == nil {
		f.held = map[int64]fakeHold{}
	}
	f.next++
	f.held[f.next] = fakeHold{session: id, usd: usd}
	return f.next, sp, nil
}

func (f *fakeSessionLLM) Settle(_ context.Context, id int64, u chiefusage.SessionLLMUsage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.held, id)
	f.booked = append(f.booked, u)
	return nil
}

func (f *fakeSessionLLM) Release(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.held, id)
	return nil
}

func (f *fakeSessionLLM) SessionLLMReport(context.Context, time.Time) (chiefusage.SessionLLMReport, error) {
	return chiefusage.SessionLLMReport{}, nil
}

func (f *fakeSessionLLM) bookings() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.booked) }

// The session keys the fake knows. The admin's chief is agent 10 (owner 1),
// the member's chief agent 20 (owner 2): chiefLLMRows.
const (
	keyAdminSide   = "agent:zesty-stoat:side:7f3a"
	keyMemberSide  = "agent:member-chief:side:2c9d"
	keyAdminClaude = "agent:zesty-stoat:side:claude-5e1b"
	// keyOtherOwnerSameAgent names a row carrying the admin chief's agent id
	// under another owner: the agent half of the check passes it.
	keyOtherOwnerSameAgent = "agent:zesty-stoat:side:owner2-91aa"
)

func fakeSessions() *fakeSessionLLM {
	return &fakeSessionLLM{refs: map[string]chiefusage.SessionRef{
		keyAdminSide:   {ID: 701, OwnerID: 1, AgentID: 10, Model: "openrouter/vendor-a/alpha-7b"},
		keyMemberSide:  {ID: 702, OwnerID: 2, AgentID: 20, Model: "openrouter/vendor-b/beta-13b"},
		keyAdminClaude: {ID: 703, OwnerID: 1, AgentID: 10, Model: "sonnet"},
		// The admin chief's agent id under another owner: only the OWNER half
		// of the check can refuse it.
		keyOtherOwnerSameAgent: {ID: 704, OwnerID: 2, AgentID: 10, Model: "openrouter/vendor-c/beta-70b-instruct"},
	}}
}

func (r *chiefLLMRig) enableSessions(t *testing.T, st chiefusage.SessionLLMStore) {
	t.Helper()
	if err := r.s.EnableChiefSessionsLLM(ChiefSessionsLLM{Key: chiefSessionsKey, DailyUSD: 3.5, Store: st,
		Upstream: r.up.URL + "/api"}); err != nil {
		t.Fatal(err)
	}
	r.h = r.s.Handler()
}

func (r *chiefLLMRig) postSession(t *testing.T, tok, session string) *httptest.ResponseRecorder {
	t.Helper()
	return llmPost(t, r.h, "/llm/v1/messages?beta=true", map[string]string{"Authorization": "Bearer " + tok, "X-Muster-Token": tok,
		ChiefSessionHeader: session})
}

// llmOutcome is what a caller of the proxy observes of one request.
type llmOutcome struct {
	Code          int
	Body          string
	UpstreamCalls int
	UpstreamAuth  string
	UpstreamModel string
}

func (r *chiefLLMRig) outcome(t *testing.T, tok string) llmOutcome {
	t.Helper()
	before := r.up.calls()
	w := r.post(t, tok)
	o := llmOutcome{Code: w.Code, Body: w.Body.String(), UpstreamCalls: r.up.calls() - before}
	if o.UpstreamCalls > 0 {
		h, m := r.up.last()
		o.UpstreamAuth, o.UpstreamModel = h.Get("Authorization"), m
	}
	return o
}

// TestAnUnheaderedRequestIsUnchangedByTheSessionDoor: every request WITHOUT
// X-Muster-Chief-Session — the admin's chief, a worker, a member's chief, with
// the chief LLM on and with it off — gets exactly the answer it got before the
// door was opened: the same status and body, the same upstream calls, on the
// same key and model. And nothing is booked to the sessions' book.
func TestAnUnheaderedRequestIsUnchangedByTheSessionDoor(t *testing.T) {
	for _, chiefLLM := range []bool{true, false} {
		r := newChiefLLMRig(t, chiefLLMOpts{enable: chiefLLM, cfg: ChiefLLM{Key: chiefAdminKey}})
		toks := []string{tokAdminChief, tokAdminWorker, tokMemberChief}
		before := map[string]llmOutcome{}
		for _, tok := range toks {
			before[tok] = r.outcome(t, tok)
		}
		st := fakeSessions()
		r.enableSessions(t, st)
		for _, tok := range toks {
			after := r.outcome(t, tok)
			if after != before[tok] {
				t.Errorf("chief llm %v, %s: with the session door open = %+v, before = %+v", chiefLLM, tok, after, before[tok])
			}
		}
		if chiefLLM && before[tokAdminChief].UpstreamAuth != "Bearer "+chiefAdminKey {
			t.Errorf("instrument check: the admin's chief was not served on the admin key before the door (%+v)", before[tokAdminChief])
		}
		if n := st.bookings(); n != 0 {
			t.Errorf("chief llm %v: %d unheadered requests were booked to the sessions' book", chiefLLM, n)
		}
	}
}

// TestAHeaderedRequestNeverFallsToTheChiefsOwnRoute: with the chief LLM ON
// (MUSTER_AGENT_OPENROUTER_KEY) and the session door OFF, a request carrying
// the header — even an empty one — is refused 400 with nothing sent: it is
// never served as the chief's own request on the agents' key.
func TestAHeaderedRequestNeverFallsToTheChiefsOwnRoute(t *testing.T) {
	r := chiefLLMOn(t)
	for _, key := range []string{keyAdminSide, ""} {
		w := r.postSession(t, tokAdminChief, key)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), ChiefSessionsOffRefusal) {
			t.Errorf("header %q with the door off: %d %s, want 400 %q", key, w.Code, w.Body, ChiefSessionsOffRefusal)
		}
	}
	if n := r.up.calls(); n != 0 {
		t.Fatalf("%d requests reached the provider", n)
	}
	// Positive control: the same rig serves the unheadered chief.
	if w := r.post(t, tokAdminChief); w.Code != http.StatusOK {
		t.Fatalf("control: the unheadered chief: %d %s", w.Code, w.Body)
	}
}

// TestTheSessionDoorRefusesAnotherOwnersSessionOnItsOwnCheck: the book here
// applies no row-level security, so another owner's session IS read; the
// door's owner check (chiefSessionFor) is what refuses it — 400, nothing sent,
// nothing booked. The admin's own session, on the same rig, is served
// (positive control), and a Claude session is refused.
func TestTheSessionDoorRefusesAnotherOwnersSessionOnItsOwnCheck(t *testing.T) {
	r := chiefLLMOn(t)
	st := fakeSessions()
	r.enableSessions(t, st)

	w := r.postSession(t, tokAdminChief, keyMemberSide)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "is not one of this chief's") {
		t.Fatalf("another owner's session: %d %s, want 400 not this chief's", w.Code, w.Body)
	}
	if n := r.up.calls(); n != 0 || st.bookings() != 0 {
		t.Fatalf("another owner's session: %d upstream calls, %d bookings", n, st.bookings())
	}
	w = r.postSession(t, tokAdminChief, keyOtherOwnerSameAgent)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "is not one of this chief's") {
		t.Fatalf("another owner's row with this chief's agent id: %d %s, want 400 not this chief's", w.Code, w.Body)
	}
	if n := r.up.calls(); n != 0 || st.bookings() != 0 {
		t.Fatalf("another owner's row with this chief's agent id: %d upstream calls, %d bookings", n, st.bookings())
	}
	w = r.postSession(t, tokAdminChief, keyAdminClaude)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "does not run on OpenRouter") {
		t.Fatalf("a Claude session: %d %s, want 400", w.Code, w.Body)
	}
	if w := r.postSession(t, tokAdminChief, keyAdminSide); w.Code != http.StatusOK {
		t.Fatalf("control: the admin's own OpenRouter session: %d %s", w.Code, w.Body)
	}
	hdr, model := r.up.last()
	if hdr.Get("Authorization") != "Bearer "+chiefSessionsKey || model != "vendor-a/alpha-7b" {
		t.Fatalf("control: upstream key/model = %d bytes / %q", len(hdr.Get("Authorization")), model)
	}
	r.noKeyLeaked(t, w.Body.String())
	if strings.Contains(r.logs.String(), chiefSessionsKey) {
		t.Fatal("the sessions' key leaked into the log")
	}
}

// TestChiefSessionUpstreamModel: the provider is asked for the catalogue id;
// an id OpenRouter owns keeps its own prefix.
func TestChiefSessionUpstreamModel(t *testing.T) {
	for in, want := range map[string]string{
		"openrouter/vendor-a/alpha-7b": "vendor-a/alpha-7b",
		"openrouter/auto":              "openrouter/auto",
	} {
		if got := chiefSessionUpstreamModel(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

// ---- the model check and the search ---------------------------------------

// sessionCatalog is the fixture catalogue: ids, names and prices pairwise
// distinct.
var sessionCatalog = []CatalogModel{
	{ID: "vendor-a/alpha-7b", Name: "Vendor A: Alpha 7B", ContextLength: 32000, Pricing: CatalogPricing{Prompt: "0.0000011", Completion: "0.0000023"}},
	{ID: "vendor-b/beta-13b", Name: "Vendor B: Beta 13B", ContextLength: 64000, Pricing: CatalogPricing{Prompt: "0.0000031", Completion: "0.0000047"}},
	{ID: "vendor-c/beta-70b-instruct", Name: "Vendor C: Beta 70B Instruct", ContextLength: 128000, Pricing: CatalogPricing{Prompt: "0.0000053", Completion: "0.0000067"}},
	{ID: "vendor-d/gamma-turbo-preview", Name: "Vendor D: Gamma Turbo Preview", ContextLength: 256000, Pricing: CatalogPricing{Prompt: "0.0000071", Completion: "0.0000089"}},
	{ID: "openrouter/auto", Name: "Auto Router"},
}

// TestTheChiefSessionModelCheck: an exact id (bare and in the stored form) and
// a case or name variant of one model pass; a value that only RESEMBLES a model
// — even one matching a single model fuzzily — is refused with the closest ids,
// and one naming several models is refused as ambiguous with them.
func TestTheChiefSessionModelCheck(t *testing.T) {
	cases := []struct {
		in, model string
		ambiguous bool
		suggest   []string
	}{
		{in: "vendor-a/alpha-7b", model: "openrouter/vendor-a/alpha-7b"},
		{in: "openrouter/vendor-b/beta-13b", model: "openrouter/vendor-b/beta-13b"},
		{in: "openrouter/auto", model: "openrouter/auto"},
		{in: "VENDOR-A/Alpha-7B", model: "openrouter/vendor-a/alpha-7b"},
		{in: "Vendor B: Beta 13B", model: "openrouter/vendor-b/beta-13b"},
		// A unique fuzzy match is NOT accepted: the picker's search finds it,
		// the stored-model check refuses it.
		{in: "gamma turbo"},
		{in: "beta"},
		{in: "zzqx-nothing-like-it"},
	}
	for _, c := range cases {
		got := resolveChiefSessionModel(c.in, sessionCatalog)
		if got.Model != c.model {
			t.Errorf("%q: model %q (refusal %q), want %q", c.in, got.Model, got.Refusal, c.model)
		}
		if (got.Refusal == "") != (c.model != "") {
			t.Errorf("%q: refusal %q with model %q", c.in, got.Refusal, got.Model)
		}
		if got.Ambiguous != c.ambiguous {
			t.Errorf("%q: ambiguous %v, want %v", c.in, got.Ambiguous, c.ambiguous)
		}
		if c.suggest != nil && strings.Join(got.Suggestions, ",") != strings.Join(c.suggest, ",") {
			t.Errorf("%q: suggestions %v, want %v", c.in, got.Suggestions, c.suggest)
		}
		if c.model == "" && len(got.Suggestions) == 0 {
			t.Errorf("%q: refused with no closest candidates", c.in)
		}
	}
	// Ambiguous: a name variant of two models (the same display name twice).
	twin := append([]CatalogModel{}, sessionCatalog...)
	twin = append(twin, CatalogModel{ID: "vendor-e/alpha-7b-mirror", Name: "Vendor A: Alpha 7B"})
	got := resolveChiefSessionModel("Vendor A: Alpha 7B", twin)
	if got.Model != "" || !got.Ambiguous ||
		strings.Join(got.Suggestions, ",") != "openrouter/vendor-a/alpha-7b,openrouter/vendor-e/alpha-7b-mirror" {
		t.Errorf("ambiguous name: %+v, want refused with both", got)
	}
	// The picker's search still finds what the check refuses.
	if hits := rankCatalog("gamma turbo", sessionCatalog, 3); len(hits) != 1 || hits[0].ID != "vendor-d/gamma-turbo-preview" {
		t.Errorf("search for \"gamma turbo\" = %+v, want the one model", hits)
	}
}

// agedCatalog is an AgedModelCatalog answering a fixed copy, age and error.
type agedCatalog struct {
	cat []CatalogModel
	at  time.Time
	err error
}

func (a agedCatalog) Models(context.Context) ([]CatalogModel, error) { return a.cat, a.err }
func (a agedCatalog) ModelsAt(context.Context) ([]CatalogModel, time.Time, error) {
	return a.cat, a.at, a.err
}

// TestTheChiefSessionModelCheckFailsClosedOnAnOldCatalogue: a failed refresh
// with a copy under 24 hours old still decides; one older, or none, refuses
// with the catalogue reason — never a pass on the id's shape.
func TestTheChiefSessionModelCheckFailsClosedOnAnOldCatalogue(t *testing.T) {
	down := errors.New("catalogue unreachable")
	for _, c := range []struct {
		name string
		cat  ModelCatalog
		ok   bool
	}{
		{"fresh", agedCatalog{cat: sessionCatalog, at: time.Now()}, true},
		{"stale 23h, refresh failed", agedCatalog{cat: sessionCatalog, at: time.Now().Add(-23 * time.Hour), err: down}, true},
		{"stale 25h, refresh failed", agedCatalog{cat: sessionCatalog, at: time.Now().Add(-25 * time.Hour), err: down}, false},
		{"none", agedCatalog{err: down}, false},
	} {
		s := New(nil, AuthConfig{UIPassword: testUIPassword}, log.New(&llmLogs{}, "", 0))
		s.UseExtensions(Extensions{ModelCatalog: c.cat})
		got := s.ResolveChiefSessionModel(context.Background(), "openrouter/vendor-a/alpha-7b")
		if c.ok && got.Model != "openrouter/vendor-a/alpha-7b" {
			t.Errorf("%s: %+v, want the model", c.name, got)
		}
		if !c.ok && (got.Model != "" || got.Refusal != ChiefSessionCatalogUnreadable) {
			t.Errorf("%s: %+v, want the catalogue refusal", c.name, got)
		}
	}
}

// TestTheModelSearchIsOwnerOnly: the search answers the owner whose chief the
// door serves (the admin, user 1), refuses a member (403) and a request with
// no session, and is not there with the door off (404). The admin's answer is
// the fuzzy matches with id, name, context length and prices.
func TestTheModelSearchIsOwnerOnly(t *testing.T) {
	r := newChiefLLMRig(t, chiefLLMOpts{})
	r.s.ext.ModelCatalog = agedCatalog{cat: sessionCatalog, at: time.Now()}
	call := func(uid int64) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/chief/openrouter-models?q=beta", nil)
		if uid > 0 {
			req = req.WithContext(authz.WithPrincipal(req.Context(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession}))
		}
		w := httptest.NewRecorder()
		r.s.handleChiefSessionModels(w, req)
		return w
	}
	if w := call(1); w.Code != http.StatusNotFound {
		t.Fatalf("door off: %d, want 404", w.Code)
	}
	r.enableSessions(t, fakeSessions())
	if w := call(2); w.Code != http.StatusForbidden {
		t.Fatalf("a member: %d %s, want 403", w.Code, w.Body)
	}
	if w := call(0); w.Code != http.StatusForbidden {
		t.Fatalf("no principal: %d %s, want 403", w.Code, w.Body)
	}
	w := call(1)
	if w.Code != http.StatusOK {
		t.Fatalf("the admin: %d %s", w.Code, w.Body)
	}
	var hits []chiefSessionModelHit
	if err := json.Unmarshal(w.Body.Bytes(), &hits); err != nil {
		t.Fatal(err)
	}
	want := []chiefSessionModelHit{
		{ID: "openrouter/vendor-b/beta-13b", Name: "Vendor B: Beta 13B", ContextLength: 64000, Pricing: CatalogPricing{Prompt: "0.0000031", Completion: "0.0000047"}},
		{ID: "openrouter/vendor-c/beta-70b-instruct", Name: "Vendor C: Beta 70B Instruct", ContextLength: 128000, Pricing: CatalogPricing{Prompt: "0.0000053", Completion: "0.0000067"}},
	}
	if len(hits) != len(want) || hits[0] != want[0] || hits[1] != want[1] {
		t.Fatalf("hits = %+v, want %+v", hits, want)
	}
	// Through the mux: no cookie is not served.
	req := httptest.NewRequest(http.MethodGet, "/api/chief/openrouter-models?q=beta", nil)
	rw := httptest.NewRecorder()
	r.h.ServeHTTP(rw, req)
	if rw.Code == http.StatusOK {
		t.Fatalf("no session through the mux: %d, want a refusal", rw.Code)
	}
}
