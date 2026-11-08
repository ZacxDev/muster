package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/apitokens"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
)

// ---------------------------------------------------------------------------
// POST /api/agents (agent_create_api.go): the machine route that replaced the
// web UI's dispatch form (docs/design/chief-dispatch.md, phase 2).
// ---------------------------------------------------------------------------

// agentCreateTokens remembers, per server, the fake token store and the one
// agent-create token installed on it, so a fixture built before the route
// existed gets a caller without being rebuilt.
var agentCreateTokens sync.Map // *Server -> agentCreateFixture

type agentCreateFixture struct {
	store *fakeAPITokens
	tok   string
}

// agentCreateTokenFor returns an agent-create token for the operator (user 1)
// on s, installing a fake token store when s has none.
func agentCreateTokenFor(t *testing.T, s *Server) string {
	t.Helper()
	if v, ok := agentCreateTokens.Load(s); ok {
		return v.(agentCreateFixture).tok
	}
	f, ok := s.ext.APITokens.(*fakeAPITokens)
	if !ok {
		f = newFakeAPITokens()
		s.ext.APITokens = f
	}
	tok := f.add(t, authz.OperatorUserID)
	f.mu.Lock()
	rec := f.byTok[tok]
	rec.Scope = apitokens.ScopeAgentCreate
	f.byTok[tok] = rec
	f.mu.Unlock()
	agentCreateTokens.Store(s, agentCreateFixture{store: f, tok: tok})
	return tok
}

// apiCreateAgent drives POST /api/agents through the production chain as the
// operator's agent-create token. start true is what the deleted form's
// action=dispatch did; false is "Save for later".
func apiCreateAgent(t *testing.T, s *Server, h http.Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return apiCreateAgentAs(t, h, agentCreateTokenFor(t, s), body)
}

func apiCreateAgentAs(t *testing.T, h http.Handler, bearer string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/agents", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func createErr(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON (%d): %s", rec.Code, rec.Body)
	}
	s, _ := out["error"].(string)
	return s
}

// TestAnAgentCreateTokenCreatesAnAgentAsItsOwner: start true dispatches
// (kickoff), start false creates the row stopped (no kickoff); both answer 201
// with the agent, created AS THE TOKEN'S OWNER (the principal the store sees).
func TestAnAgentCreateTokenCreatesAnAgentAsItsOwner(t *testing.T) {
	for _, start := range []bool{true, false} {
		prov := newPreflightProvisioner("")
		store := &preflightStore{}
		s, h := preflightServer(t, prov, store)
		rec := apiCreateAgent(t, s, h, map[string]any{"prompt": "fix the build", "start": start})
		if rec.Code != http.StatusCreated {
			t.Fatalf("start=%v: %d %s; want 201", start, rec.Code, rec.Body)
		}
		var out struct {
			Agent   agentJSON `json:"agent"`
			Started bool      `json:"started"`
			Href    string    `json:"href"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Started != start || out.Agent.Name == "" || !strings.HasPrefix(out.Href, "/agents/") {
			t.Fatalf("start=%v: answer %+v", start, out)
		}
		rows := store.rows()
		if len(rows) != 1 || rows[0].PendingNote != "fix the build" {
			t.Fatalf("start=%v: rows %+v; want one carrying the prompt", start, rows)
		}
		if got := store.creators(); len(got) != 1 || got[0] != authz.OperatorUserID {
			t.Fatalf("start=%v: created as %v; want the token's owner (%d)", start, got, authz.OperatorUserID)
		}
		<-prov.dispatchC
		if d := prov.dispatches(); len(d) != 1 || d[0] != start {
			t.Fatalf("start=%v: Dispatch kickoff = %v", start, d)
		}
	}
}

// TestAFullTokenCannotCreateAgents: the CLI's ordinary FULL token is refused on
// POST /api/agents with the agent-create sentence — while the same token still
// reaches another machine route — and the handler never runs (no row). A
// summary token gets the same sentence here, not its own "mint a full token"
// one, which would be wrong on this route.
//
// Mutation check: drop the explicitScopeRoutes exception in tokenScopeAdmits and
// the full-token case goes red (the handler runs and creates the row).
func TestAFullTokenCannotCreateAgents(t *testing.T) {
	prov := newPreflightProvisioner("")
	store := &preflightStore{}
	s, h := preflightServer(t, prov, store)
	f := newFakeAPITokens()
	s.ext.APITokens = f
	full := f.add(t, authz.OperatorUserID)
	sum := f.add(t, authz.OperatorUserID)
	f.mu.Lock()
	r := f.byTok[sum]
	r.Scope = apitokens.ScopeSummary
	f.byTok[sum] = r
	f.mu.Unlock()
	for name, tok := range map[string]string{"full": full, "summary": sum} {
		rec := apiCreateAgentAs(t, h, tok, map[string]any{"prompt": "x", "start": false})
		if rec.Code != http.StatusForbidden || createErr(t, rec) != AgentCreateScopeRefusal {
			t.Errorf("a %s token on POST /api/agents: %d %s; want 403 %q", name, rec.Code, rec.Body, AgentCreateScopeRefusal)
		}
	}
	if rows := store.rows(); len(rows) != 0 {
		t.Fatalf("a refused token created %d row(s)", len(rows))
	}
	// Control: the full token still reaches the roster.
	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req.Header.Set("Authorization", "Bearer "+full)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("CONTROL: the full token on GET /api/agents: %d %s", rec.Code, rec.Body)
	}
}

// TestAnAgentCreateTokenReachesNothingElse: on every other machine route the
// agent-create token is refused 403 with its own sentence, before the handler.
func TestAnAgentCreateTokenReachesNothingElse(t *testing.T) {
	f := newScopedTokens()
	tok := f.addScoped(t, 7, apitokens.ScopeAgentCreate)
	h := scopeTestMux(t, f)
	for _, c := range summaryTokenRefused {
		code, body := bearerReach(h, c.method, c.path, tok)
		var out map[string]any
		_ = json.Unmarshal([]byte(body), &out)
		if code != http.StatusForbidden || out["error"] != AgentCreateTokenRefusal {
			t.Errorf("%s %s with an agent-create token: %d %s; want 403 %q", c.method, c.path, code, truncate(body, 200), AgentCreateTokenRefusal)
		}
	}
	if code, _ := bearerReach(h, http.MethodGet, attentionSummaryPath, tok); code != http.StatusForbidden {
		t.Errorf("an agent-create token on the bar summary: %d; want 403", code)
	}
	if lists, revokes := f.counts(); lists != 0 || revokes != 0 {
		t.Fatalf("an agent-create token reached the store's List %d / Revoke %d time(s)", lists, revokes)
	}
	// Control: the door admits it on its one route (the handler then refuses
	// this fixture's missing provisioner, which is past the door).
	if code, body := bearerReach(h, http.MethodPost, "/api/agents", tok); code == http.StatusForbidden || code == http.StatusUnauthorized {
		t.Fatalf("CONTROL: the agent-create token on POST /api/agents was refused at the door: %d %s", code, body)
	}
}

// TestTheSharedHookSecretCannotCreateAgents: the hook secret (ViaHook, which
// acts as user 1 whoever holds it) passes requireArmedHookToken and is refused
// by the handler; nothing is created.
func TestTheSharedHookSecretCannotCreateAgents(t *testing.T) {
	prov := newPreflightProvisioner("")
	store := &preflightStore{}
	s, h := preflightServer(t, prov, store)
	_ = s
	req := httptest.NewRequest(http.MethodPost, "/api/agents", strings.NewReader(`{"prompt":"x","start":false}`))
	req.Header.Set("X-Muster-Token", testHookToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || createErr(t, rec) != AgentCreateViaRefusal {
		t.Fatalf("the hook secret on POST /api/agents: %d %s; want 403 %q", rec.Code, rec.Body, AgentCreateViaRefusal)
	}
	if rows := store.rows(); len(rows) != 0 {
		t.Fatalf("the hook secret created %d row(s)", len(rows))
	}
}

// TestAnAgentCreateRequestMustSayWhetherToStart and refuses unknown fields:
// both are 400 before anything is created.
func TestAnAgentCreateRequestIsCheckedBeforeTheRow(t *testing.T) {
	prov := newPreflightProvisioner("")
	store := &preflightStore{}
	s, h := preflightServer(t, prov, store)
	if rec := apiCreateAgent(t, s, h, map[string]any{"prompt": "x"}); rec.Code != http.StatusBadRequest || createErr(t, rec) != AgentCreateStartRequired {
		t.Errorf("no start: %d %s; want 400 %q", rec.Code, rec.Body, AgentCreateStartRequired)
	}
	if rec := apiCreateAgent(t, s, h, map[string]any{"prompt": "x", "start": false, "action": "save"}); rec.Code != http.StatusBadRequest ||
		!strings.Contains(createErr(t, rec), "unknown field") {
		t.Errorf("an unknown field: %d %s; want 400 naming it", rec.Code, rec.Body)
	}
	if rows := store.rows(); len(rows) != 0 {
		t.Fatalf("a refused request created %d row(s)", len(rows))
	}
}

// TestAnAgentCreateBodyOverTheLimitIsRefusedByName: a body over 1 MiB is a 413
// naming the limit (not "unexpected EOF"), and creates nothing; a prompt that
// keeps the body just under it creates the agent with the whole prompt.
func TestAnAgentCreateBodyOverTheLimitIsRefusedByName(t *testing.T) {
	prov := newPreflightProvisioner("")
	store := &preflightStore{}
	s, h := preflightServer(t, prov, store)
	// {"prompt":"…","start":false} is 27 bytes of JSON around the prompt.
	over := strings.Repeat("p", agentCreateMaxBody-26)
	if rec := apiCreateAgent(t, s, h, map[string]any{"prompt": over, "start": false}); rec.Code != http.StatusRequestEntityTooLarge ||
		createErr(t, rec) != AgentCreateTooLarge {
		t.Fatalf("a body of %d bytes: %d %.200s; want 413 %q", agentCreateMaxBody+1, rec.Code, rec.Body, AgentCreateTooLarge)
	}
	if rows := store.rows(); len(rows) != 0 {
		t.Fatalf("an oversize body created %d row(s)", len(rows))
	}
	// The sentence names the limit the code enforces: the byte count of the
	// constant, and the documented 1 MiB (1024 KiB).
	if !strings.Contains(AgentCreateTooLarge, "("+strconv.Itoa(agentCreateMaxBody)+" bytes)") ||
		!strings.Contains(AgentCreateTooLarge, "1024 KiB (1048576 bytes)") {
		t.Fatalf("the 413 sentence %q does not name the enforced limit %d (documented: 1 MiB)", AgentCreateTooLarge, agentCreateMaxBody)
	}
	under := strings.Repeat("u", agentCreateMaxBody-27)
	if rec := apiCreateAgent(t, s, h, map[string]any{"prompt": under, "start": false}); rec.Code != http.StatusCreated {
		t.Fatalf("a body of exactly %d bytes: %d %.200s; want 201", agentCreateMaxBody, rec.Code, rec.Body)
	}
	if rows := store.rows(); len(rows) != 1 || len(rows[0].PendingNote) != len(under) {
		t.Fatalf("the at-limit prompt was not kept whole")
	}
}

// listedTokens is mintScopeTokens with the list a test names.
type listedTokens struct {
	mintScopeTokens
	list []apitokens.Token
}

func (l listedTokens) List(context.Context) ([]apitokens.Token, error) { return l.list, nil }

// TestTheMintPageOffersCreateAgents: the token page's third choice is "Create
// agents", and the list labels such a token as itself — never as a bar token.
func TestTheMintPageOffersCreateAgents(t *testing.T) {
	var got []string
	s := New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "mint-scope-operator-pw"}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{APITokens: listedTokens{mintScopeTokens: mintScopeTokens{got: &got},
		list: []apitokens.Token{{ID: 4, Name: "creator", Scope: apitokens.ScopeAgentCreate}}}, Users: stubUsers{}})
	mux := http.NewServeMux()
	s.registerAll(mux)
	page := tokenPageDo(t, mux, s, http.MethodGet, "").Body.String()
	for _, want := range []string{
		`<input type="radio" name="scope" value="agent-create" class="h-5 w-5">` + "\nCreate agents (reaches only POST /api/agents, and nothing else)</label>",
		`data-token-scope="agent-create">Create agents · created`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the token page does not carry %q", want)
		}
	}
	if strings.Contains(page, `data-token-scope="agent-create">Bar summary only`) {
		t.Error("an agent-create token is labelled a bar token")
	}
	if rec := tokenPageDo(t, mux, s, http.MethodPost, "name=creator&scope=agent-create"); rec.Code != http.StatusOK {
		t.Fatalf("mint agent-create: %d", rec.Code)
	}
	if strings.Join(got, ",") != "agent-create" {
		t.Fatalf("the store was asked for scopes %q; want agent-create", got)
	}
}

// createNotes is a notes store holding the tasks a test names; any other id is
// not found, as row-level security answers another user's task.
type createNotes struct {
	notes.Store
	byID map[int64]notes.Note
}

func (c createNotes) Get(_ context.Context, id int64) (notes.Note, error) {
	if n, ok := c.byID[id]; ok {
		return n, nil
	}
	return notes.Note{}, pgx.ErrNoRows
}

// TestAnAgentCreateOnATaskReadsTheTask: note_id's body is the first turn; a
// gated task refuses start: true with the gate's reason (409) and nothing is
// created, while start: false creates it stopped; a task the caller cannot
// read is 404 "no such task" — never an agent created without the task.
func TestAnAgentCreateOnATaskReadsTheTask(t *testing.T) {
	prov := newPreflightProvisioner("")
	store := &preflightStore{}
	s, h := preflightServer(t, prov, store)
	s.ext.Notes = createNotes{byID: map[int64]notes.Note{
		41: {ID: 41, Body: "the open task's body", Status: notes.StatusComplete},
		42: {ID: 42, Body: "the gated task's body", Tags: []string{"gate:needs-owner-ok"}, Status: notes.StatusComplete},
	}}
	if rec := apiCreateAgent(t, s, h, map[string]any{"note_id": 42, "start": true}); rec.Code != http.StatusConflict ||
		createErr(t, rec) != "task is gated (needs-owner-ok) — remove the gate: tag to dispatch" {
		t.Fatalf("a gated task, start=true: %d %s; want 409 with the gate's reason", rec.Code, rec.Body)
	}
	if rec := apiCreateAgent(t, s, h, map[string]any{"note_id": 999, "prompt": "x", "start": false}); rec.Code != http.StatusNotFound ||
		createErr(t, rec) != "no such task" {
		t.Fatalf("an unreadable task: %d %s; want 404 no such task", rec.Code, rec.Body)
	}
	if rows := store.rows(); len(rows) != 0 {
		t.Fatalf("a refused create made %d row(s)", len(rows))
	}
	if rec := apiCreateAgent(t, s, h, map[string]any{"note_id": 42, "start": false}); rec.Code != http.StatusCreated {
		t.Fatalf("a gated task, start=false: %d %s; want 201 (a stopped create provisions nothing)", rec.Code, rec.Body)
	}
	if rec := apiCreateAgent(t, s, h, map[string]any{"note_id": 41, "prompt": "ignored", "start": false}); rec.Code != http.StatusCreated {
		t.Fatalf("an open task: %d %s", rec.Code, rec.Body)
	}
	rows := store.rows()
	if len(rows) != 2 || rows[0].PendingNote != "the gated task's body" || rows[1].PendingNote != "the open task's body" ||
		rows[1].NoteID == nil || *rows[1].NoteID != 41 {
		t.Fatalf("rows %+v; want two, each carrying its task's body and id", rows)
	}
}
