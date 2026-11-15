package mirror

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/users"
)

// teamFixture plants team id (users.kind = 'team') as the login role, and
// returns the store.
func teamFixture(t *testing.T, team int64, name string) (*PGStore, *pgxpool.Pool) {
	t.Helper()
	s, pool, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, kind, role, display_name) OVERRIDING SYSTEM VALUE
		VALUES ($1, 'team', 'member', $2) ON CONFLICT (id) DO NOTHING`, team, name)
	t.Cleanup(func() {
		dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE team_id = $1`, team)
		dbtest.OwnerExec(t, dsn, `DELETE FROM mirror_tokens WHERE owner_id = $1`, team)
		dbtest.OwnerExec(t, dsn, `DELETE FROM users WHERE id = $1`, team)
	})
	return s, pool
}

// asTeam is the team acting principal of team, at the team manager's door:
// how /teams/{id}/hosts mints (internal/api teamAdminContext).
func asTeam(team int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: team, Team: true, Role: authz.RoleMember, Via: authz.ViaTeamAdmin})
}

// TestATeamMirrorTokenAuthenticatesAsTheTeam (team sharing 2b.2): a token
// minted AS THE TEAM (owner_id = the team) authenticates as the team's, says
// so (Team), and a live connection's recheck keeps it live. The CONTROL is a
// person's token, which authenticates with Team false.
//
// 🔴 RED at the base (ed4155b6): the lookup's owner set was
// users.EnabledPersonIDs, so a team's token was refused as if unknown.
func TestATeamMirrorTokenAuthenticatesAsTheTeam(t *testing.T) {
	const team = 900213
	s, _ := teamFixture(t, team, "mirror-team-auth")
	sys := authz.System(context.Background())
	ptok, prec, err := s.CreateToken(authz.Operator(context.Background()), "ws-person")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Authenticate(sys, ptok); err != nil || got.ID != prec.ID || got.Team {
		t.Fatalf("control: a person's token = %+v, %v; want it, not a team's", got, err)
	}
	tok, rec, err := s.CreateToken(asTeam(team), "build-01")
	if err != nil {
		t.Fatalf("mint as the team: %v", err)
	}
	if rec.OwnerID != team {
		t.Fatalf("a token minted as the team is owned by %d; want the team %d", rec.OwnerID, team)
	}
	got, err := s.Authenticate(sys, tok)
	if err != nil {
		t.Fatalf("a team's token was refused: %v", err)
	}
	if got.ID != rec.ID || got.OwnerID != team || !got.Team || got.Host != "build-01" {
		t.Fatalf("a team's token authenticated as %+v; want token %d of team %d, Team true, host build-01", got, rec.ID, team)
	}
	if live, err := s.TokenLive(sys, rec.ID); err != nil || !live {
		t.Fatalf("a team's live token rechecked as live=%v, %v; want true", live, err)
	}
	// Revoked, it is refused like any other.
	if ok, err := s.RevokeToken(asTeam(team), rec.ID); err != nil || !ok {
		t.Fatalf("revoke as the team: %v, %v", ok, err)
	}
	if _, err := s.Authenticate(sys, tok); !errors.Is(err, ErrBadToken) {
		t.Fatalf("a revoked team token authenticated: %v", err)
	}
}

// TestTheCLIMintsATeamTokenOnlyForATeam: `mirror token create --team`
// (CreateTeamToken, under System) mints a token owned by the team and writes
// the team's audit row (team.host.token.create, via cli) beside it; handed a
// PERSON's id it mints nothing (ErrNotATeam) — the CLI is never a way to mint
// someone's personal token under System.
func TestTheCLIMintsATeamTokenOnlyForATeam(t *testing.T) {
	const team = 900217
	s, pool := teamFixture(t, team, "mirror-team-cli")
	sys := authz.System(context.Background())
	cli := users.Actor{Via: "cli"}
	if _, _, err := s.CreateTeamToken(sys, cli, 2, "ws-not-a-team"); !errors.Is(err, ErrNotATeam) {
		t.Fatalf("CreateTeamToken for a person: %v; want ErrNotATeam", err)
	}
	tok, rec, err := s.CreateTeamToken(sys, cli, team, "build-02")
	if err != nil {
		t.Fatal(err)
	}
	if rec.OwnerID != team || !rec.Team {
		t.Fatalf("the CLI minted %+v; want a token of team %d", rec, team)
	}
	if got, err := s.Authenticate(sys, tok); err != nil || got.OwnerID != team || !got.Team {
		t.Fatalf("the CLI's team token authenticated as %+v, %v", got, err)
	}
	var n int
	if err := pool.QueryRow(sys, `SELECT count(*) FROM audit_log WHERE team_id = $1 AND action = 'team.host.token.create'
		AND via = 'cli' AND object = 'mirror_token:' || $2::bigint AND detail->>'host' = 'build-02'`, team, rec.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d audit rows for the CLI's mint; want 1 (team.host.token.create, via cli)", n)
	}
}
