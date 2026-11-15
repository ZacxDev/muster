package mirror

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
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
// and a live connection's recheck keeps it live; revoked, it is refused. The
// CONTROL is a person's token, which authenticates as before.
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
	if got, err := s.Authenticate(sys, ptok); err != nil || got.ID != prec.ID {
		t.Fatalf("control: a person's token = %+v, %v; want it", got, err)
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
	if got.ID != rec.ID || got.OwnerID != team || got.Host != "build-01" {
		t.Fatalf("a team's token authenticated as %+v; want token %d of team %d, host build-01", got, rec.ID, team)
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

