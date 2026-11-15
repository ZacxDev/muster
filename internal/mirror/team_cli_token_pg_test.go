package mirror

import (
	"context"
	"errors"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// TestAuthenticateSaysWhetherTheOwnerIsATeam: Token.Team is what pins a team
// host's connection (teamhold.go), so Authenticate must set it for a team's
// token and only for one.
func TestAuthenticateSaysWhetherTheOwnerIsATeam(t *testing.T) {
	const team = 900219
	s, _ := teamFixture(t, team, "mirror-team-kind")
	sys := authz.System(context.Background())
	ptok, _, err := s.CreateToken(authz.Operator(context.Background()), "ws-kind")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Authenticate(sys, ptok); err != nil || got.Team {
		t.Fatalf("a person's token = %+v, %v; want Team false", got, err)
	}
	tok, _, err := s.CreateToken(asTeam(team), "build-kind")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Authenticate(sys, tok); err != nil || !got.Team {
		t.Fatalf("a team's token = %+v, %v; want Team true", got, err)
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
