package agents

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/users"
)

// A TEAM-OWNED AGENT'S CREDENTIALS NEVER SIT IN ITS ROW (team sharing phase 4,
// slice 4.4, the merge precondition from #658; migration 0131). agents is
// shared with a team's members since 0131, so the row a viewer reads must carry
// no credential: Create and both mints still write agent_secrets for a team
// owner, and the old agents.hooks_token / agents.gateway_secret stay ''.

type teamSecretsWorld struct {
	store                  *PGStore
	pool                   *pgxpool.Pool
	team, op, viewer, none int64
	teamCtx                context.Context
}

func newTeamSecretsWorld(t *testing.T, ctx context.Context) *teamSecretsWorld {
	t.Helper()
	store := requirePGStore(t, ctx)
	pool := store.pool.(*pgxpool.Pool)
	sys := authz.System(context.Background())
	sfx := time.Now().Format("150405.000000")
	person := func(name string) int64 {
		var id int64
		if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, name+sfx).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	w := &teamSecretsWorld{store: store, pool: pool, op: person("ts-op"), viewer: person("ts-viewer"), none: person("ts-none")}
	us := users.NewPG(pool)
	by := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	team, err := us.CreateTeam(sys, by, "secrets team "+sfx, w.op)
	if err != nil {
		t.Fatal(err)
	}
	w.team = team.ID
	if err := us.SetTeamMember(sys, by, w.team, w.op, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	if err := us.SetTeamMember(sys, by, w.team, w.viewer, authz.TeamViewer); err != nil {
		t.Fatal(err)
	}
	// The team acting principal: how the team's own agent and its background
	// work act (internal/api teamActingPrincipal).
	w.teamCtx = authz.WithPrincipal(ctx, authz.Principal{UserID: w.team, Team: true, Role: authz.RoleMember, Via: authz.ViaAgent})
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = pool.Exec(bg, `DELETE FROM agents WHERE owner_id = ANY($1)`, []int64{w.team, w.op})
		_, _ = pool.Exec(bg, `DELETE FROM team_members WHERE team_id = $1`, w.team)
		_, _ = pool.Exec(bg, `DELETE FROM audit_log WHERE object = $1 OR team_id = $2::bigint`, users.TeamObject(w.team), w.team)
		_, _ = pool.Exec(bg, `DELETE FROM users WHERE id = ANY($1)`, []int64{w.team, w.op, w.viewer, w.none})
	})
	return w
}

// member is uid's browser session principal, with the memberships a request
// reads (users.Account), so muster.teams carries them.
func (w *teamSecretsWorld) member(t *testing.T, ctx context.Context, uid int64) context.Context {
	t.Helper()
	a, err := users.NewPG(w.pool).Account(authz.System(ctx), uid)
	if err != nil {
		t.Fatal(err)
	}
	return authz.WithPrincipal(ctx, authz.Principal{UserID: uid, Role: a.Role, Via: authz.ViaSession, Teams: a.Teams})
}

// rowSecrets is agent id's OLD columns, read through ctx's principal; n is how
// many rows ctx saw (0 or 1).
func rowSecrets(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) (hooks, gw string, n int) {
	t.Helper()
	err := pool.QueryRow(ctx, `SELECT hooks_token, gateway_secret FROM agents WHERE id = $1`, id).Scan(&hooks, &gw)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return hooks, gw, 1
}

// TestATeamAgentsRowCarriesNoCredential: a team agent created with a token and
// minted a turn credential holds both in agent_secrets and NEITHER in its row —
// read as System, as an operator and as a viewer of the team (who DO see the
// row: 0131 shares it). CONTROL: the team operator's own agent, written by the
// same statements, still carries its dual-written token in the row (so the
// query reads the column, and the blank is about the team owner).
//
// 🔴 RED at origin/main 006bbe5c: the row holds the token (System reads it)
// and the members see no row at all (agents is personal there).
func TestATeamAgentsRowCarriesNoCredential(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 60*time.Second)
	defer cancel()
	w := newTeamSecretsWorld(t, ctx)
	sfx := time.Now().Format("150405.000000")
	tok, gw := "team-hooks-"+sfx, "team-turn-credential-"+sfx
	ta, err := w.store.Create(w.teamCtx, Agent{Name: "team-sec-" + sfx, Namespace: "ns-team-sec-" + sfx,
		Status: StatusRunning, HooksToken: tok, Kind: KindClaudeCode, CCAccount: "acct"})
	if err != nil {
		t.Fatal(err)
	}
	if ta.OwnerID != w.team {
		t.Fatalf("fixture: the team agent is owned by %d; want the team %d", ta.OwnerID, w.team)
	}
	if err := w.store.SetGatewaySecret(w.teamCtx, ta.ID, gw); err != nil {
		t.Fatal(err)
	}
	tok2 := tok + "-reminted"
	if err := w.store.SetHooksToken(w.teamCtx, ta.ID, tok2); err != nil {
		t.Fatal(err)
	}
	// Control: a person's agent through the same three statements.
	opCtx := w.member(t, ctx, w.op)
	pa, err := w.store.Create(opCtx, Agent{Name: "person-sec-" + sfx, Namespace: "ns-person-sec-" + sfx,
		Status: StatusRunning, HooksToken: "person-" + tok, Kind: KindClaudeCode, CCAccount: "acct"})
	if err != nil {
		t.Fatal(err)
	}
	if h, _, n := rowSecrets(t, authz.System(ctx), w.pool, pa.ID); n != 1 || h != "person-"+tok {
		t.Fatalf("control: a person's agent row holds %q (%d rows); want its dual-written token", h, n)
	}

	if s, owner, ok := secretsRow(t, w.pool, ta.ID); !ok || owner != w.team || s.HooksToken != tok2 || s.GatewaySecret != gw {
		t.Fatalf("agent_secrets for the team agent: (%v, %v, owner %d, row %v); want both credentials, owned by the team %d",
			s.HooksToken == tok2, s.GatewaySecret == gw, owner, ok, w.team)
	}
	for who, c := range map[string]struct {
		ctx  context.Context
		rows int
	}{
		"System":              {authz.System(ctx), 1},
		"the team's operator": {opCtx, 1},
		"the team's viewer":   {w.member(t, ctx, w.viewer), 1},
		"a non-member":        {w.member(t, ctx, w.none), 0},
	} {
		h, g, n := rowSecrets(t, c.ctx, w.pool, ta.ID)
		if n != c.rows {
			t.Errorf("%s sees %d rows of the team agent; want %d", who, n, c.rows)
		}
		if h != "" || g != "" {
			t.Errorf("LEAK: %s reads the team agent's row with hooks_token %v / gateway_secret %v set; want both ''",
				who, h != "", g != "")
		}
	}
}

// TestOnlyTheTeamReadsATeamAgentsSecrets: Secrets — the one read of an agent's
// credentials — answers the team acting principal (and System) and fails
// CLOSED for a member who can see the row: pgx.ErrNoRows, never two empty
// strings. CONTROL: the member reads their own agent's secrets.
//
// 🔴 RED at origin/main 006bbe5c for the operator leg only after 0131 shares the
// row: at main the member cannot see the agent at all (ErrNoRows for the wrong
// reason), so the RED that matters is the mutation (the owner arm removed:
// empty credentials, no error).
func TestOnlyTheTeamReadsATeamAgentsSecrets(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 60*time.Second)
	defer cancel()
	w := newTeamSecretsWorld(t, ctx)
	sfx := time.Now().Format("150405.000000")
	tok := "team-only-" + sfx
	ta, err := w.store.Create(w.teamCtx, Agent{Name: "team-only-" + sfx, Namespace: "ns-team-only-" + sfx,
		Status: StatusRunning, HooksToken: tok, Kind: KindClaudeCode, CCAccount: "acct"})
	if err != nil {
		t.Fatal(err)
	}
	for who, ctx := range map[string]context.Context{"the team acting principal": w.teamCtx, "System": authz.System(ctx)} {
		if s, err := w.store.Secrets(ctx, ta.ID); err != nil || s.HooksToken != tok {
			t.Errorf("%s reads the team agent's secrets: token %v, %v; want the token", who, s.HooksToken == tok, err)
		}
	}
	opCtx := w.member(t, ctx, w.op)
	if _, err := w.store.Get(opCtx, ta.ID); err != nil {
		t.Fatalf("fixture: the team's operator cannot read the team agent's row: %v", err)
	}
	if s, err := w.store.Secrets(opCtx, ta.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("the team's operator reading the team agent's secrets: (token set %v, %v); want pgx.ErrNoRows",
			s.HooksToken != "", err)
	}
	own, err := w.store.Create(opCtx, Agent{Name: "own-" + sfx, Namespace: "ns-own-" + sfx, Status: StatusRunning,
		HooksToken: "own-" + tok, Kind: KindClaudeCode, CCAccount: "acct"})
	if err != nil {
		t.Fatal(err)
	}
	if s, err := w.store.Secrets(opCtx, own.ID); err != nil || s.HooksToken != "own-"+tok {
		t.Errorf("control: the operator reading their own agent's secrets: %v, %v", s.HooksToken == "own-"+tok, err)
	}
}
