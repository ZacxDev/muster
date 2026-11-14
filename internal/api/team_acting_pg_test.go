package api

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/users"
)

// The TEAM ACTING PRINCIPAL (team sharing phase 4, slice 4.1): a team-owned
// agent's token, and every per-owner background site that meets a team-owned
// row, acts as the team itself — muster.uid = the team, muster.teams empty,
// never scope "all" — or refuses by name. Before this slice every one of them
// built the team's principal and authz.WithPrincipal PANICKED on it: one
// stopped team agent crashed the whole reaper pass, and a team-owned agent's
// every tool call was a 403.

// newTeam creates a team managed by person manager (users.CreateTeam, as the
// admin's /admin/teams form does) and removes it, its members and every row it
// owns at cleanup.
func newTeam(t *testing.T, pool *pgxpool.Pool, manager int64) int64 {
	t.Helper()
	sys := authz.System(context.Background())
	team, err := users.NewPG(pool).CreateTeam(sys, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession},
		"acting-"+strconv.FormatInt(time.Now().UnixNano(), 36), manager)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM agents WHERE owner_id = $1`, `DELETE FROM notes WHERE owner_id = $1`,
			`DELETE FROM team_members WHERE team_id = $1`, `DELETE FROM audit_log WHERE user_id = $1 OR object = 'team:' || $1::text`,
			`DELETE FROM users WHERE id = $1`} {
			_, _ = pool.Exec(bg, q, team.ID)
		}
	})
	return team.ID
}

// scopeOf is what Postgres itself holds for one acquire under ctx: muster.uid,
// muster.teams and muster.scope (internal/db's prepareConn sets all three).
func scopeOf(t *testing.T, pool *pgxpool.Pool, ctx context.Context) (uid, teams, scope string) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT current_setting('muster.uid', true), current_setting('muster.teams', true),
		current_setting('muster.scope', true)`).Scan(&uid, &teams, &scope); err != nil {
		t.Fatal(err)
	}
	return uid, teams, scope
}

// TestTheReaperReapsATeamOwnedStoppedAgent is the slice's regression: a
// team-owned agent stopped past the window, in the same pass as a personal
// one. Before the team acting principal, reapPrincipal's
// authz.WithPrincipal panicked on the team — the WHOLE pass died and nobody's
// stopped agents were reaped. Now both are reaped, each as its own owner.
func TestTheReaperReapsATeamOwnedStoppedAgent(t *testing.T) {
	f := newReapAPIFixture(t)
	f.srv.ext.Users = users.NewPG(f.pool) // the real kind: a team reads as a team
	team := newTeam(t, f.pool, f.member)
	personal := f.agentOn(t, 1, "rpperson", nil)
	teams := f.agentOn(t, team, "rpteam", nil)
	f.stopAged(t, personal.ID, 15)
	f.stopAged(t, teams.ID, 15)
	var n int
	if msg := panicOf(func() { n = f.reap(t) }); msg != "" {
		t.Fatalf("the reaper pass PANICKED on a team-owned stopped agent (nobody's agents were reaped): %s", msg)
	}
	if n < 2 {
		t.Fatalf("the pass started %d teardown(s); want the personal and the team agent's", n)
	}
	f.mustBeGone(t, personal.ID)
	f.mustBeGone(t, teams.ID)
	f.destroyedOnly(t, []int64{personal.ID, teams.ID}, personal.ID, teams.ID)
}

// panicOf runs fn and returns its panic as text ("" when it did not panic).
func panicOf(fn func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprint(r)
			if msg == "" {
				msg = "(empty panic)"
			}
		}
	}()
	fn()
	return ""
}

// perOwnerSite is one function in this package that mints a request principal
// for an OWNER it was handed (a call of authz.WithPrincipal), and what it must
// do when that owner is a team.
type perOwnerSite struct {
	// mint calls the site for owner team (and agent a, owned by the team, for
	// the sites that take an agent) and returns the context it built.
	mint func(s *Server, ctx context.Context, team int64, a agents.Agent) context.Context
	// actsAsTeam: the site acts AS THE TEAM (the team acting principal through
	// via). False: it REFUSES a team — refusal says how.
	actsAsTeam bool
	via        authz.Via
	refusal    string
}

const (
	// refusedByWithPrincipal: the site's door is outside authz's
	// teamActingVias, so WithPrincipal panics on a team. Its owners are
	// persons by construction (users.PersonIDs) or the phase that shares
	// through it (2b, 3) has not landed; the panic is the backstop.
	refusedByWithPrincipal = "panics: a team never signs in, and that door does not act as a team"
	// refusedToolless: turnPrincipal answers false — the turn goes toolless.
	refusedToolless = "false: a team's gateway turn goes toolless (no team gateway agents in phase 4)"
)

// perOwnerSites is EVERY function in this package that calls
// authz.WithPrincipal (TestEveryPerOwnerPrincipalSiteIsTeamSafe scans the
// package's source and fails when the set grows OR shrinks), but admit, whose
// doors TestOnlyTheAgentDoorsAdmitATeam walks route by route. internal/authz's
// withPrincipalLedger is the type-resolved ledger of the same sites; the two
// outside this package are e2e/mktoken (a fixture, persons only) and
// internal/plugins' RedeemPair (a pairing's owner is found by
// users.PersonIDs, so never a team).
var perOwnerSites = map[string]perOwnerSite{
	"reapPrincipal": {actsAsTeam: true, via: authz.ViaReap,
		mint: func(s *Server, ctx context.Context, team int64, _ agents.Agent) context.Context {
			return s.reapPrincipal(ctx, team)
		}},
	"attentionPrincipal": {actsAsTeam: true, via: authz.ViaAttention, // also the task claim sweep's
		mint: func(s *Server, ctx context.Context, team int64, _ agents.Agent) context.Context {
			return s.attentionPrincipal(ctx, team)
		}},
	"routePrincipal": {actsAsTeam: true, via: authz.ViaRoute,
		mint: func(s *Server, ctx context.Context, team int64, _ agents.Agent) context.Context {
			return s.routePrincipal(ctx, team)
		}},
	"hostDispatchPrincipal": {actsAsTeam: true, via: authz.ViaHostDispatch,
		mint: func(s *Server, ctx context.Context, team int64, _ agents.Agent) context.Context {
			return s.hostDispatchPrincipal(ctx, team)
		}},
	"jobPrincipal": {actsAsTeam: true, via: authz.ViaJobQueue,
		mint: func(s *Server, ctx context.Context, team int64, _ agents.Agent) context.Context {
			return s.jobPrincipal(ctx, team)
		}},
	// The chief's own write from a turn: the SAME owner, its door narrowed to
	// the agent's. Handed the team acting principal of a team chief's tool
	// call, it stays the team.
	"chiefWriteContext": {actsAsTeam: true, via: authz.ViaAgent,
		mint: func(_ *Server, ctx context.Context, team int64, a agents.Agent) context.Context {
			return chiefWriteContext(authz.WithPrincipal(ctx, authz.Principal{UserID: team, Team: true, Role: authz.RoleMember, Via: authz.ViaAgent}), a)
		}},
	"turnPrincipal": {refusal: refusedToolless,
		mint: func(s *Server, ctx context.Context, _ int64, a agents.Agent) context.Context {
			out, ok := s.turnPrincipal(ctx, a, authz.ViaKickoff)
			if ok {
				return out
			}
			return nil
		}},
	"focusPrincipal": {refusal: refusedByWithPrincipal,
		mint: func(s *Server, ctx context.Context, team int64, _ agents.Agent) context.Context {
			return s.focusPrincipal(ctx, team)
		}},
	"askPrincipal": {refusal: refusedByWithPrincipal,
		mint: func(s *Server, ctx context.Context, team int64, _ agents.Agent) context.Context {
			return s.askPrincipal(ctx, team)
		}},
	"officerPrincipal": {refusal: refusedByWithPrincipal,
		mint: func(s *Server, ctx context.Context, team int64, _ agents.Agent) context.Context {
			return s.officerPrincipal(ctx, team)
		}},
	"queuedPrincipal": {refusal: refusedByWithPrincipal,
		mint: func(s *Server, ctx context.Context, team int64, _ agents.Agent) context.Context {
			return s.queuedPrincipal(ctx, team)
		}},
	"stepUpPrincipal": {refusal: refusedByWithPrincipal,
		mint: func(s *Server, ctx context.Context, team int64, _ agents.Agent) context.Context {
			return s.stepUpPrincipal(ctx, team)
		}},
}

// withPrincipalCallers is every function or method in this package's non-test
// source whose body calls authz.WithPrincipal, by name.
func withPrincipalCallers(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	files := 0
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, p, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		files++
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "WithPrincipal" {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "authz" {
						seen[fn.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	if files < 50 {
		t.Fatalf("scanned %d source files; the scan is not reading the package", files)
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// TestEveryPerOwnerPrincipalSiteIsTeamSafe: each per-owner principal site,
// handed a TEAM owner, either acts as the team acting principal — no panic,
// the principal is {team, Team, member, no Teams} through the site's door,
// and POSTGRES holds muster.uid = the team, muster.teams = ” and no "all"
// scope, under which the team's own agent is visible and a member's personal
// agent is not — or refuses in the way it is ledgered to. The ledger is held
// to the package's source in both directions.
func TestEveryPerOwnerPrincipalSiteIsTeamSafe(t *testing.T) {
	// The ledger is the package's WithPrincipal callers, exactly.
	callers := withPrincipalCallers(t)
	var unlisted []string
	inSource := map[string]bool{}
	for _, c := range callers {
		inSource[c] = true
		if _, ok := perOwnerSites[c]; !ok && c != "admit" {
			unlisted = append(unlisted, c)
		}
	}
	var gone []string
	for n := range perOwnerSites {
		if !inSource[n] {
			gone = append(gone, n)
		}
	}
	sort.Strings(gone)
	if len(unlisted) > 0 || len(gone) > 0 || !inSource["admit"] {
		t.Fatalf("perOwnerSites is out of step with this package's authz.WithPrincipal callers %v: unlisted %v, listed but gone %v",
			callers, unlisted, gone)
	}

	f := newConnFixtureWith(t, func(ext *Extensions, _ *pgxpool.Pool) {})
	f.srv.ext.Users = users.NewPG(f.pool)
	sys := authz.System(context.Background())
	var member int64
	if err := f.pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`,
		"acting-m-"+strconv.FormatInt(time.Now().UnixNano(), 36)).Scan(&member); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM agents WHERE owner_id = $1`, `DELETE FROM team_members WHERE user_id = $1`,
			`DELETE FROM audit_log WHERE user_id = $1`, `DELETE FROM users WHERE id = $1`} {
			_, _ = f.pool.Exec(bg, q, member)
		}
	})
	team := newTeam(t, f.pool, member)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	mk := func(owner int64, name string) agents.Agent {
		a, err := agents.NewPG(f.pool).Create(asUser(owner), agents.Agent{Name: name + suffix, Namespace: "devpod-" + name + suffix,
			Kind: agents.KindGateway, Status: agents.StatusRunning})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	teamAgent, memberAgent := mk(team, "actteam-"), mk(member, "actmember-")
	wantUID := strconv.FormatInt(team, 10)

	// Control: the same check sees a PERSON's own row under a person's
	// principal, so "only the team's row" below is a fact about the uid.
	if got := visibleAgents(t, f.pool, asUser(member), teamAgent.ID, memberAgent.ID); len(got) != 1 || got[0] != memberAgent.ID {
		t.Fatalf("control: the member sees agents %v; want only their own %d", got, memberAgent.ID)
	}

	names := make([]string, 0, len(perOwnerSites))
	for n := range perOwnerSites {
		names = append(names, n)
	}
	sort.Strings(names)
	acting, refusing := 0, 0
	for _, name := range names {
		site := perOwnerSites[name]
		t.Run(name, func(t *testing.T) {
			var ctx context.Context
			msg := panicOf(func() { ctx = site.mint(f.srv, context.Background(), team, teamAgent) })
			if !site.actsAsTeam {
				refusing++
				switch site.refusal {
				case refusedByWithPrincipal:
					if !strings.Contains(msg, "a team never signs in, and that door does not act as a team") {
						t.Fatalf("%s with a team owner: panic %q, ctx %v; want WithPrincipal's door refusal", name, msg, ctx)
					}
				case refusedToolless:
					if msg != "" || ctx != nil {
						t.Fatalf("%s with a team-owned agent: panic %q, ok %v; want false, no panic", name, msg, ctx != nil)
					}
				default:
					t.Fatalf("%s: unknown refusal %q", name, site.refusal)
				}
				return
			}
			acting++
			if msg != "" {
				t.Fatalf("%s PANICKED on a team owner: %s", name, msg)
			}
			p, ok := authz.From(ctx)
			want := authz.Principal{UserID: team, Team: true, Role: authz.RoleMember, Via: site.via}
			if !ok || p != want {
				t.Fatalf("%s with a team owner attached %+v (ok %v); want the team acting principal %+v", name, p, ok, want)
			}
			uid, teams, scope := scopeOf(t, f.pool, ctx)
			if uid != wantUID || teams != "" || scope != "" {
				t.Fatalf("%s: Postgres holds muster.uid %q, muster.teams %q, muster.scope %q; want %q, '', ''", name, uid, teams, scope, wantUID)
			}
			if got := visibleAgents(t, f.pool, ctx, teamAgent.ID, memberAgent.ID); len(got) != 1 || got[0] != teamAgent.ID {
				t.Fatalf("%s: the team acting principal sees agents %v; want only the team's own %d (never the member's %d)",
					name, got, teamAgent.ID, memberAgent.ID)
			}
		})
	}
	if acting != 6 || refusing != 6 {
		t.Fatalf("%d sites acted as the team and %d refused; the ledger names 6 and 6", acting, refusing)
	}

	// A door OUTSIDE authz's teamActingVias still refuses the team, through
	// the very builder every site uses.
	for _, via := range []authz.Via{authz.ViaSession, authz.ViaDevice, authz.ViaToken, authz.ViaMirrorHost, authz.ViaPlugin} {
		msg := panicOf(func() { authz.WithPrincipal(context.Background(), f.srv.principalFor(context.Background(), team, via)) })
		if !strings.Contains(msg, "a team never signs in, and that door does not act as a team") {
			t.Errorf("a team principal through %s: panic %q; want the door refusal", via, msg)
		}
	}
}

// visibleAgents is which of ids ctx's principal can read.
func visibleAgents(t *testing.T, pool *pgxpool.Pool, ctx context.Context, ids ...int64) []int64 {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id FROM agents WHERE id = ANY($1) ORDER BY id`, ids)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestATeamOwnedAgentsTokenReachesMCPAsTheTeam: a team-owned chief's own token
// on /mcp is admitted as the team acting principal — Postgres holds
// muster.uid = the team and muster.teams = ” for the request — and its
// tasks_list shows the TEAM's task (the positive control: the board read is
// the team's) and NEVER a team member's personal task. Before this slice the
// door answered 403.
func TestATeamOwnedAgentsTokenReachesMCPAsTheTeam(t *testing.T) {
	f := newAttnFixture(t, func(ext *Extensions, pool *pgxpool.Pool) { ext.Users = users.NewPG(pool) })
	team := newTeam(t, f.pool, f.member)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	tok := "team-chief-token-" + suffix
	f.agentFor(t, team, agents.ChiefNameFor(team), "chief", tok)
	teamTask, err := f.notes.Create(asUser(team), notes.Note{Title: "team canary " + suffix, Body: "the team's", Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	personal, err := f.notes.Create(asUser(f.member), notes.Note{Title: "member canary " + suffix, Body: "the member's own", Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	type door struct {
		p                 authz.Principal
		uid, teams, scope string
	}
	var doors []door
	f.srv.principalProbe = func(r *http.Request) {
		p, _ := authz.From(r.Context())
		d := door{p: p}
		_ = f.pool.QueryRow(r.Context(), `SELECT current_setting('muster.uid', true), current_setting('muster.teams', true),
			current_setting('muster.scope', true)`).Scan(&d.uid, &d.teams, &d.scope)
		doors = append(doors, d)
	}
	text, isErr, rpc := f.tool(t, tok, "tasks_list", map[string]any{})
	if rpc != nil || isErr {
		t.Fatalf("tasks_list by the team's chief: rpc=%+v text=%s", rpc, text)
	}
	if len(doors) == 0 {
		t.Fatal("the request reached no admit hand-off")
	}
	// /mcp is the connector surface's door (requireConnectorAgent).
	want := authz.Principal{UserID: team, Team: true, Role: authz.RoleMember, Via: authz.ViaConnect}
	for _, d := range doors {
		if d.p != want || d.uid != strconv.FormatInt(team, 10) || d.teams != "" || d.scope != "" {
			t.Fatalf("the team chief's /mcp request acted as %+v with muster.uid %q, muster.teams %q, muster.scope %q; want %+v, %d, '', ''",
				d.p, d.uid, d.teams, d.scope, want, team)
		}
	}
	if !strings.Contains(text, `"id":`+strconv.FormatInt(teamTask.ID, 10)+`,`) {
		t.Fatalf("the team chief's tasks_list lacks the team's own task %d (positive control): %.400s", teamTask.ID, text)
	}
	if strings.Contains(text, "member canary") || strings.Contains(text, `"id":`+strconv.FormatInt(personal.ID, 10)+`,`) {
		t.Fatalf("the team chief's tasks_list shows a member's PERSONAL task %d: %.400s", personal.ID, text)
	}
	// And by id: the team's task is the chief's to read (control), the
	// member's is not.
	if got, _, _ := f.tool(t, tok, "task_get", map[string]any{"id": teamTask.ID}); !strings.Contains(got, "team canary") {
		t.Fatalf("control: the team chief's task_get could not read the team's own task %d: %.400s", teamTask.ID, got)
	}
	if got, _, _ := f.tool(t, tok, "task_get", map[string]any{"id": personal.ID}); strings.Contains(got, "member canary") {
		t.Fatalf("the team chief's task_get read a member's PERSONAL task: %.400s", got)
	}
}
