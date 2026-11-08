package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/apitokens"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/credentials"
)

// The bring-your-own-credentials leg of the isolation walk. The world wires the
// REAL credential store (sealed, under row-level security), the REAL plane and
// the REAL account pool over it, so the walk's routes — the credentials page,
// the dispatch form, the dispatch itself — answer from the same decision
// production does.

// isoKinds is api.AgentKinds over the real pool and plane (cmd/muster-server's
// kindSet, which this package cannot import).
type isoKinds struct {
	pool  *ccpool.Pool
	plane *credentials.Plane
}

func (k isoKinds) Enabled() []string { return []string{agents.KindGateway, agents.KindClaudeCode} }
func (k isoKinds) ClaudeAccounts(ctx context.Context, owner int64) ([]string, error) {
	return k.pool.Names(ctx, owner)
}
func (k isoKinds) SelectClaudeAccount(ctx context.Context, owner int64, pin string) (string, error) {
	return k.pool.Select(ctx, owner, pin)
}
func (k isoKinds) Unavailable(ctx context.Context, owner int64) map[string]string {
	return k.plane.Unavailable(ctx, owner, k.Enabled())
}
func (k isoKinds) ClaudeAccountStates(ctx context.Context, owner int64) (map[string]ccpool.AccountState, error) {
	return k.pool.States(ctx, owner)
}

// isoCredPage is api.CredentialStore over the real store and plane.
type isoCredPage struct {
	*credentials.PGStore
	plane *credentials.Plane
}

func (c isoCredPage) UsesEnvironment(ctx context.Context, owner int64) bool {
	return c.plane.UsesEnvironment(ctx, owner)
}

// The environment pool (the admin's) in the iso world, and A's own token.
const (
	isoEnvAccount = "isoenvpool"
	isoEnvToken   = "sk-ant-oat01-iso-ENV-pool-token-000000"
	isoATok       = "sk-ant-oat01-iso-member-A-token-111111"
	isoAORKey     = "sk-or-v1-iso-member-A-api-key-0002222"
)

// TestAnotherUsersCredentialsCannotBeSeenOrUsed: B (a member with no
// credentials) cannot see A's account on the credentials page, cannot create
// an agent on it through POST /api/agents (pinned or not), and is told what
// to add; A — the control — sees and uses its own. (The dispatch form that
// also listed the caller's accounts was deleted, docs/design/chief-dispatch.md
// phase 2; POST /api/agents replaced its submit.)
func TestAnotherUsersCredentialsCannotBeSeenOrUsed(t *testing.T) {
	w := newIsoWorld(t)
	acct := w.objA.ccAccount
	ts := apitokens.NewPG(w.pool)
	createTok := map[int64]string{}
	for _, u := range []isoUser{w.a, w.b} {
		tok, _, err := ts.Create(isoAs(u), u.name+" create", 0, apitokens.ScopeAgentCreate)
		if err != nil {
			t.Fatalf("%s agent-create token: %v", u.name, err)
		}
		createTok[u.uid] = tok
	}
	for _, c := range []cred{{sessionUser: w.b.uid}, {gotrueUser: w.b.uid}} {
		body := w.do(w.h, "GET", "/account/credentials", "", c).Body.String()
		if strings.Contains(body, acct) {
			t.Errorf("B %s /account/credentials shows A's account %q", c, acct)
		}
		if strings.Contains(body, isoEnvAccount) {
			t.Errorf("B %s /account/credentials shows the ADMIN's environment pool", c)
		}
	}
	for _, f := range []string{
		`{"kind":"` + agents.KindClaudeCode + `","cc_account":"` + acct + `","prompt":"t","start":false}`,
		`{"kind":"` + agents.KindClaudeCode + `","cc_account":"` + isoEnvAccount + `","prompt":"t","start":false}`,
		`{"kind":"` + agents.KindClaudeCode + `","prompt":"t","start":false}`,
		`{"prompt":"t","start":false}`,
	} {
		rec := w.do(w.h, "POST", "/api/agents", f, cred{apiToken: createTok[w.b.uid]})
		if rec.Code != http.StatusConflict {
			t.Errorf("B created %s: %d %s; want 409", f, rec.Code, truncate(rec.Body.String(), 200))
		}
		if f == `{"prompt":"t","start":false}` && !strings.Contains(rec.Body.String(), credentials.NeedOpenRouter) {
			t.Errorf("B's gateway create does not say %q: %s", credentials.NeedOpenRouter, truncate(rec.Body.String(), 200))
		}
	}
	// No agent of B's runs on any account.
	var n int
	if err := w.pool.QueryRow(w.sys, `SELECT count(*) FROM agents WHERE owner_id=$1 AND cc_account <> ''`, w.b.uid).Scan(&n); err != nil || n != 0 {
		t.Fatalf("B has %d claude-code agents (%v)", n, err)
	}

	// The control: A sees its account (and not the admin's pool), and a
	// claude-code create by A lands on it.
	page := w.do(w.h, "GET", "/account/credentials", "", cred{sessionUser: w.a.uid}).Body.String()
	if !strings.Contains(page, acct) {
		t.Fatal("CONTROL: A's credentials page does not show A's own account")
	}
	if strings.Contains(page, isoATok) || strings.Contains(page, strings.TrimPrefix(isoATok, credentials.ClaudeTokenPrefix)) {
		t.Fatal("A's credentials page echoes A's token")
	}
	rec := w.do(w.h, "POST", "/api/agents", `{"kind":"`+agents.KindClaudeCode+`","prompt":"t","start":false}`, cred{apiToken: createTok[w.a.uid]})
	if rec.Code != http.StatusCreated {
		t.Fatalf("CONTROL: A's claude-code create: %d %s", rec.Code, truncate(rec.Body.String(), 300))
	}
	var got string
	if err := w.pool.QueryRow(w.sys, `SELECT cc_account FROM agents WHERE owner_id=$1 AND cc_account <> '' ORDER BY id DESC LIMIT 1`, w.a.uid).Scan(&got); err != nil || got != acct {
		t.Fatalf("CONTROL: A's claude-code agent runs on %q (%v); want A's own %q", got, err, acct)
	}
}

// TestAnAgentCreateNamingAnotherUsersTaskIsNotFound: B's agent-create token
// naming A's task (note_id) is "no such task" — the task is read under B's
// row-level security — and creates nothing; A naming the same task creates an
// agent on it (the control).
func TestAnAgentCreateNamingAnotherUsersTaskIsNotFound(t *testing.T) {
	w := newIsoWorld(t)
	ts := apitokens.NewPG(w.pool)
	tok := map[int64]string{}
	for _, u := range []isoUser{w.a, w.b} {
		tk, _, err := ts.Create(isoAs(u), u.name+" create", 0, apitokens.ScopeAgentCreate)
		if err != nil {
			t.Fatalf("%s agent-create token: %v", u.name, err)
		}
		tok[u.uid] = tk
	}
	body := `{"note_id":` + strconv.FormatInt(w.objA.task, 10) + `,"start":false}`
	// Counted around B's request: every agent of B's, and every agent on A's
	// task — B's request must change neither.
	counts := func() (bAgents, onTask int) {
		t.Helper()
		if err := w.pool.QueryRow(w.sys, `SELECT count(*) FILTER (WHERE owner_id=$1), count(*) FILTER (WHERE note_id=$2) FROM agents`,
			w.b.uid, w.objA.task).Scan(&bAgents, &onTask); err != nil {
			t.Fatal(err)
		}
		return
	}
	bBefore, n := counts()
	rec := w.do(w.h, "POST", "/api/agents", body, cred{apiToken: tok[w.b.uid]})
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no such task") {
		t.Fatalf("B naming A's task: %d %s; want 404 no such task", rec.Code, truncate(rec.Body.String(), 200))
	}
	if bAfter, onTask := counts(); bAfter != bBefore || onTask != n {
		t.Fatalf("🔴 B's refused request created an agent: B's agents %d -> %d, agents on A's task %d -> %d", bBefore, bAfter, n, onTask)
	}
	if rec := w.do(w.h, "POST", "/api/agents", body, cred{apiToken: tok[w.a.uid]}); rec.Code != http.StatusCreated {
		t.Fatalf("CONTROL: A naming A's own task: %d %s", rec.Code, truncate(rec.Body.String(), 300))
	}
	var after int
	if err := w.pool.QueryRow(w.sys, `SELECT count(*) FROM agents WHERE note_id=$1`, w.objA.task).Scan(&after); err != nil || after != n+1 {
		t.Fatalf("CONTROL: agents on A's task %d -> %d (%v); want one more", n, after, err)
	}
}

// isoAs is u's context for minting through a real store as u (the world's
// as2, which is local to newIsoWorld).
func isoAs(u isoUser) context.Context {
	role := authz.RoleMember
	if u.uid == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: u.uid, Role: role, Via: authz.ViaSession})
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;").Replace(s)
}
