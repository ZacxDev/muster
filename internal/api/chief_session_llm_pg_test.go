package api

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefusage"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// The chief sessions' spend door against the database and a stub provider:
// the real handler and connector door, the real agents store (which decides
// who is the chief), chat_sessions rows, chief_session_llm_usage under
// row-level security, and the provider-reported cost booked and read back as
// the budget.
//
// Fixture values are pairwise distinct and distinct from every constant the
// assertions name: a request costs 0.004; the daily caps used are 0.011 and
// 0.5; the session cap 0.007; the default daily cap is 20.

// sessPGCost is what the stub provider reports per request.
const sessPGCost = 0.004

// sessPG is a server on a fresh database with the chief LLM ON (the agents'
// key, chiefAdminKey) and the session door ON (chiefSessionsKey), and the
// rows the tests address.
type sessPG struct {
	f    *connFixture
	up   *llmProvider
	logs *llmLogs
	sys  context.Context

	adminChief, adminWorker, memberChief int64
	// session keys, and the ids of X and Y
	keyX, keyY, keyPrimary, keyClaude, keyWorker, keyMember string
	idX, idY                                                int64
}

const (
	tokSessAdminChief  = "sess-pg-admin-chief-token-6f1e2d3c4b5a"
	tokSessAdminWorker = "sess-pg-admin-worker-token-6f1e2d3c4b"
	tokSessMemberChief = "sess-pg-member-chief-token-6f1e2d3c4b"
)

func newSessPG(t *testing.T, daily, session float64) *sessPG {
	t.Helper()
	p := &sessPG{up: newLLMProvider(t, sessPGCost), logs: &llmLogs{}}
	p.f = newConnFixtureWith(t, func(ext *Extensions, pool *pgxpool.Pool) {
		ext.ChiefPrefs = chiefLLMPrefs{}
		ext.ChiefUsage = chiefusage.NewPG(pool)
		ext.Provisioner, ext.SessionLiveness, ext.Kinds = stubProvisioner{}, stubLiveness{}, stubKinds{pick: "acct-one"}
	})
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member') ON CONFLICT (id) DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `TRUNCATE chief_session_llm_usage, chief_turn_usage`)
	p.sys = authz.System(context.Background())
	agent := func(owner int64, name, display, tok string) int64 {
		var id int64
		if err := p.f.pool.QueryRow(p.sys, `INSERT INTO agents (owner_id, name, namespace, hooks_token, display_name)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`, owner, name, "ns-"+name, tok, display).Scan(&id); err != nil {
			t.Fatalf("seed agent %s: %v", name, err)
		}
		t.Cleanup(func() {
			_, _ = p.f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, id)
		})
		return id
	}
	p.adminChief = agent(1, "sess-pg-chief", ChiefDisplayName, tokSessAdminChief)
	p.adminWorker = agent(1, "sess-pg-worker", "worker", tokSessAdminWorker)
	p.memberChief = agent(2, "sess-pg-member-chief", ChiefDisplayName, tokSessMemberChief)
	chat := func(agentID, owner int64, key, runtime, model string, primary bool) int64 {
		var id int64
		if err := p.f.pool.QueryRow(p.sys, `INSERT INTO chat_sessions (agent_id, owner_id, session_key, runtime, model, is_default)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, agentID, owner, key, runtime, model, primary).Scan(&id); err != nil {
			t.Fatalf("seed session %s: %v", key, err)
		}
		return id
	}
	p.keyX, p.keyY = "sess-pg:x", "sess-pg:y"
	p.keyPrimary, p.keyClaude, p.keyWorker, p.keyMember = "sess-pg:primary", "sess-pg:claude", "sess-pg:worker", "sess-pg:member"
	p.idX = chat(p.adminChief, 1, p.keyX, "claude-code", "openrouter/vendor-x/model-x1", false)
	p.idY = chat(p.adminChief, 1, p.keyY, "claude-code", "openrouter/vendor-y/model-y2", false)
	chat(p.adminChief, 1, p.keyPrimary, "claude-code", "openrouter/vendor-p/model-p3", true)
	chat(p.adminChief, 1, p.keyClaude, "claude-code", "sonnet", false)
	chat(p.adminWorker, 1, p.keyWorker, "claude-code", "openrouter/vendor-w/model-w4", false)
	chat(p.memberChief, 2, p.keyMember, "claude-code", "openrouter/vendor-m/model-m5", false)

	s := p.f.srv
	s.logger.SetOutput(p.logs)
	if err := s.EnableChiefLLM(ChiefLLM{Key: chiefAdminKey, Upstream: p.up.URL + "/api", Routed: claudeCodeRouted}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableChiefSessionsLLM(ChiefSessionsLLM{Key: chiefSessionsKey, DailyUSD: daily, SessionUSD: session,
		Store: chiefusage.NewPG(p.f.pool), Upstream: p.up.URL + "/api"}); err != nil {
		t.Fatal(err)
	}
	p.f.h = s.Handler()
	return p
}

func (p *sessPG) post(t *testing.T, tok, key string) *httptest.ResponseRecorder {
	t.Helper()
	return llmPost(t, p.f.h, "/llm/v1/messages?beta=true", map[string]string{"Authorization": "Bearer " + tok, "X-Muster-Token": tok,
		ChiefSessionHeader: key})
}

// booked is session id's row(s) in chief_session_llm_usage: requests, USD,
// input, cache read, output tokens, the model.
func (p *sessPG) booked(t *testing.T, id int64) (int64, float64, int64, int64, int64, string) {
	t.Helper()
	var n, in, read, out int64
	var usd float64
	var model string
	if err := p.f.pool.QueryRow(p.sys, `SELECT coalesce(sum(requests), 0)::bigint, coalesce(sum(cost_usd), 0)::float8,
		coalesce(sum(input_tokens), 0)::bigint, coalesce(sum(cache_read_tokens), 0)::bigint, coalesce(sum(output_tokens), 0)::bigint,
		coalesce(max(model), '') FROM chief_session_llm_usage WHERE session_id = $1`, id).Scan(&n, &usd, &in, &read, &out, &model); err != nil {
		t.Fatal(err)
	}
	return n, usd, in, read, out, model
}

func usdNear(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestAChiefSessionIsServedOnItsOwnKeyPinnedAndBooked: the admin's chief, for
// its OpenRouter side chat X, is served on the SESSIONS' key — never the
// agents' key — pinned to X's model, and the provider-reported cost lands in
// chief_session_llm_usage under the admin, with the tokens and the model.
func TestAChiefSessionIsServedOnItsOwnKeyPinnedAndBooked(t *testing.T) {
	p := newSessPG(t, 0.5, 0)
	w := p.post(t, tokSessAdminChief, p.keyX)
	if w.Code != http.StatusOK {
		t.Fatalf("session X: %d %s", w.Code, w.Body)
	}
	if n := p.up.calls(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1", n)
	}
	p.up.mu.Lock()
	auth, model := p.up.auth[0], p.up.model[0]
	p.up.mu.Unlock()
	if auth != "Bearer "+chiefSessionsKey {
		t.Errorf("upstream Authorization is not the sessions' key (%d bytes; the agents' key: %v)", len(auth), auth == "Bearer "+chiefAdminKey)
	}
	if strings.Contains(auth, chiefAdminKey) {
		t.Error("MUSTER_AGENT_OPENROUTER_KEY reached the provider for a chief session")
	}
	if model != "vendor-x/model-x1" {
		t.Errorf("upstream model = %q, want X's pin", model)
	}
	n, usd, in, read, out, m := p.booked(t, p.idX)
	if n != 1 || !usdNear(usd, sessPGCost) || in != 1000 || read != 300 || out != 7 || m != "openrouter/vendor-x/model-x1" {
		t.Errorf("booked = %d requests $%v in %d read %d out %d model %q", n, usd, in, read, out, m)
	}
	var owner int64
	if err := p.f.pool.QueryRow(p.sys, `SELECT owner_id FROM chief_session_llm_usage WHERE session_id = $1`, p.idX).Scan(&owner); err != nil || owner != 1 {
		t.Errorf("the row's owner = %d (%v), want the admin", owner, err)
	}
	for _, leak := range []string{chiefSessionsKey, chiefAdminKey} {
		if strings.Contains(p.logs.String(), leak) || strings.Contains(w.Body.String(), leak) {
			t.Errorf("a key leaked into the log or the response")
		}
	}
}

// TestTheDailyCapIsA400AndThePreTurnCheckRefuses: at a daily cap of 0.011 and
// 0.004 a request, three requests are served (spent 0, 0.004, 0.008 before
// each), the fourth is refused 400 — never 429 — with nothing sent; the
// pre-turn check admits before and refuses after, for X and for Y alike (the
// cap is the owner's).
func TestTheDailyCapIsA400AndThePreTurnCheckRefuses(t *testing.T) {
	p := newSessPG(t, 0.011, 0)
	sp, err := p.f.srv.CheckChiefSessionSpend(asAdmin(context.Background()), p.idX)
	if err != nil || sp.Refusal != "" || !usdNear(sp.DailyCapUSD, 0.011) {
		t.Fatalf("pre-turn before any spend: %+v %v, want admitted", sp, err)
	}
	for i := 0; i < 3; i++ {
		if w := p.post(t, tokSessAdminChief, p.keyX); w.Code != http.StatusOK {
			t.Fatalf("request %d: %d %s", i+1, w.Code, w.Body)
		}
	}
	w := p.post(t, tokSessAdminChief, p.keyX)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("over the daily cap: %d %s, want 400", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "today's $0.01 cap") || !strings.Contains(w.Body.String(), "$0.0120") {
		t.Errorf("the refusal does not name the spent and cap amounts: %s", w.Body)
	}
	if n := p.up.calls(); n != 3 {
		t.Fatalf("upstream calls = %d, want 3 (the refused request sent nothing)", n)
	}
	for _, id := range []int64{p.idX, p.idY} {
		sp, err := p.f.srv.CheckChiefSessionSpend(asAdmin(context.Background()), id)
		if err != nil || sp.Refusal == "" || !usdNear(sp.DaySpentUSD, 3*sessPGCost) {
			t.Errorf("pre-turn for session %d after the cap: %+v %v, want refused at $0.012", id, sp, err)
		}
	}
	if w := p.post(t, tokSessAdminChief, p.keyY); w.Code != http.StatusBadRequest {
		t.Errorf("session Y over the owner's daily cap: %d, want 400", w.Code)
	}
}

// TestASessionCapInXStillLetsYRun: at a session cap of 0.007, session X is
// refused (400) after two requests, and the pre-turn check refuses X; session
// Y is still served and admitted.
func TestASessionCapInXStillLetsYRun(t *testing.T) {
	p := newSessPG(t, 0.5, 0.007)
	for i := 0; i < 2; i++ {
		if w := p.post(t, tokSessAdminChief, p.keyX); w.Code != http.StatusOK {
			t.Fatalf("X request %d: %d %s", i+1, w.Code, w.Body)
		}
	}
	w := p.post(t, tokSessAdminChief, p.keyX)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "this session has spent") {
		t.Fatalf("X over its session cap: %d %s, want 400", w.Code, w.Body)
	}
	if sp, err := p.f.srv.CheckChiefSessionSpend(asAdmin(context.Background()), p.idX); err != nil || sp.Refusal == "" || !usdNear(sp.SessionSpentUSD, 2*sessPGCost) {
		t.Errorf("pre-turn X: %+v %v, want refused at $0.008", sp, err)
	}
	if w := p.post(t, tokSessAdminChief, p.keyY); w.Code != http.StatusOK {
		t.Fatalf("Y: %d %s, want served", w.Code, w.Body)
	}
	if sp, err := p.f.srv.CheckChiefSessionSpend(asAdmin(context.Background()), p.idY); err != nil || sp.Refusal != "" {
		t.Errorf("pre-turn Y: %+v %v, want admitted", sp, err)
	}
	if n := p.up.calls(); n != 3 {
		t.Fatalf("upstream calls = %d, want 3", n)
	}
}

// TestTheSessionDoorRefusesWhatIsNotThisChiefsOpenRouterSideChat: each is a
// 400 with nothing sent and nothing booked — an unknown key, another owner's
// session (row-level security hides it), a session of another agent of the
// SAME owner (the door's own check), the primary chat, a Claude session, a
// non-chief agent, and a member's chief on its own session. Then X is served
// (positive control).
func TestTheSessionDoorRefusesWhatIsNotThisChiefsOpenRouterSideChat(t *testing.T) {
	p := newSessPG(t, 0.5, 0)
	for _, c := range []struct {
		name, tok, key, want string
	}{
		{"unknown key", tokSessAdminChief, "sess-pg:no-such-session", "is not one of this chief's"},
		{"another owner's session", tokSessAdminChief, p.keyMember, "is not one of this chief's"},
		{"another agent of the same owner", tokSessAdminChief, p.keyWorker, "is not one of this chief's"},
		{"the primary chat", tokSessAdminChief, p.keyPrimary, "primary chat"},
		{"a Claude session", tokSessAdminChief, p.keyClaude, "does not run on OpenRouter"},
		{"a worker, not a chief", tokSessAdminWorker, p.keyWorker, "only a chief's sessions"},
		{"a member's chief", tokSessMemberChief, p.keyMember, "only the admin's chief"},
	} {
		w := p.post(t, c.tok, c.key)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%s: %d %s, want 400 %q", c.name, w.Code, w.Body, c.want)
		}
	}
	if n := p.up.calls(); n != 0 {
		t.Fatalf("%d refused requests reached the provider", n)
	}
	var rows int64
	if err := p.f.pool.QueryRow(p.sys, `SELECT count(*) FROM chief_session_llm_usage`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("%d rows booked by refused requests (%v)", rows, err)
	}
	if w := p.post(t, tokSessAdminChief, p.keyX); w.Code != http.StatusOK {
		t.Fatalf("control: X: %d %s", w.Code, w.Body)
	}
}

// TestTheUsagePageShowsTheChiefSessionsSection: after a booked request, /usage
// shows today's spend against the daily cap, the day, and the session's row
// with its model and USD.
func TestTheUsagePageShowsTheChiefSessionsSection(t *testing.T) {
	p := newSessPG(t, 0.5, 0)
	if w := p.post(t, tokSessAdminChief, p.keyY); w.Code != http.StatusOK {
		t.Fatalf("Y: %d %s", w.Code, w.Body)
	}
	w := p.f.call(t, http.MethodGet, "/usage?days=30", nil, map[string]string{"Cookie": sessionCookieFor(p.f.srv)})
	if w.Code != http.StatusOK {
		t.Fatalf("/usage: %d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, want := range []string{
		`data-testid="usage-chief-sessions"`,
		`data-testid="usage-chief-sessions-today" data-spent="0.004000" data-cap="0.50"`,
		`data-chief-session="` + strconv.FormatInt(p.idY, 10) + `" data-usd="0.004000"`,
		"openrouter/vendor-y/model-y2",
		"Chief sessions on OpenRouter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/usage lacks %q", want)
		}
	}
	if strings.Count(body, "data-chief-sessions-day=") != 30 {
		t.Errorf("want 30 days in the 30-day window, got %d", strings.Count(body, "data-chief-sessions-day="))
	}
}
