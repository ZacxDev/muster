package users

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
)

// TEAM HOST TOKENS' ROTATE MARK (team sharing 2b.2; team_hosts.go), against
// real Postgres. Each case reads mirror_tokens.rotate_at (0133) directly, so
// it names no 2b.2 symbol.

// rotateTeam is a team with a manager (ana), an operator (ben) and a viewer
// (cal), and one unrevoked host token of the team's, planted under System.
type rotateTeam struct {
	team, ana, ben, cal, token int64
}

func newRotateTeam(t *testing.T, s *PGStore, pool *pgxpool.Pool, tag string) rotateTeam {
	t.Helper()
	ctx := context.Background()
	// Runs before storeFor's reset (cleanups are LIFO): the tokens reference
	// the team's users row.
	t.Cleanup(func() {
		_, _ = pool.Exec(authz.System(context.Background()), `DELETE FROM mirror_tokens WHERE owner_id IN (SELECT id FROM users WHERE kind = 'team')`)
	})
	var rt rotateTeam
	rt.ana = addPerson(t, pool, tag+"-ana@example.test")
	rt.ben = addPerson(t, pool, tag+"-ben@example.test")
	rt.cal = addPerson(t, pool, tag+"-cal@example.test")
	team, err := s.CreateTeam(ctx, admin, "rotate-"+tag, rt.ana)
	if err != nil {
		t.Fatal(err)
	}
	rt.team = team.ID
	if err := s.SetTeamMember(ctx, admin, rt.team, rt.ben, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTeamMember(ctx, admin, rt.team, rt.cal, authz.TeamViewer); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(authz.System(ctx), `INSERT INTO mirror_tokens (owner_id, host, token_hash) VALUES ($1, 'build-01', $2) RETURNING id`,
		rt.team, "rotate-test-hash-"+tag).Scan(&rt.token); err != nil {
		t.Fatal(err)
	}
	return rt
}

// marked reports whether token id carries a rotate mark.
func marked(t *testing.T, pool *pgxpool.Pool, id int64) bool {
	t.Helper()
	var m bool
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT rotate_at IS NOT NULL FROM mirror_tokens WHERE id = $1`, id).Scan(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestRemovingAnOperatorMarksTheTeamsHostTokensRotate: removing a VIEWER
// marks nothing (the negative control: a viewer never held a token through
// the team); removing the OPERATOR marks the team's token, in the same
// transaction as the removal; and a revoked token is never marked.
//
// 🔴 RED at the base (93e002f6): rotate_at had no writer.
func TestRemovingAnOperatorMarksTheTeamsHostTokensRotate(t *testing.T) {
	s, pool := storeFor(t)
	ctx := context.Background()
	rt := newRotateTeam(t, s, pool, "rm")
	var revoked int64
	if err := pool.QueryRow(authz.System(ctx), `INSERT INTO mirror_tokens (owner_id, host, token_hash, revoked_at) VALUES ($1, 'old', 'rotate-test-revoked', now()) RETURNING id`,
		rt.team).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveTeamMemberAsManager(ctx, Actor{UserID: rt.ana, Via: authz.ViaSession}, rt.team, rt.cal); err != nil {
		t.Fatal(err)
	}
	if marked(t, pool, rt.token) {
		t.Fatal("removing a VIEWER marked the team's host token rotate")
	}
	if err := s.RemoveTeamMemberAsManager(ctx, Actor{UserID: rt.ana, Via: authz.ViaSession}, rt.team, rt.ben); err != nil {
		t.Fatal(err)
	}
	if !marked(t, pool, rt.token) {
		t.Fatal("removing an OPERATOR left the team's host token unmarked")
	}
	if marked(t, pool, revoked) {
		t.Fatal("a revoked token was marked rotate")
	}
}

// TestDowngradingAnOperatorToViewerMarksTheTeamsHostTokensRotate: a manager
// dropping to OPERATOR marks nothing (still an operator), and the operator
// dropping to VIEWER marks the token.
func TestDowngradingAnOperatorToViewerMarksTheTeamsHostTokensRotate(t *testing.T) {
	s, pool := storeFor(t)
	ctx := context.Background()
	rt := newRotateTeam(t, s, pool, "down")
	// A second manager, so ana may step down to operator.
	if err := s.SetTeamMember(ctx, admin, rt.team, rt.cal, authz.TeamManager); err != nil {
		t.Fatal(err)
	}
	if err := s.ReroleTeamMemberAsManager(ctx, Actor{UserID: rt.cal, Via: authz.ViaSession}, rt.team, rt.ana, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	if marked(t, pool, rt.token) {
		t.Fatal("a manager dropping to OPERATOR marked the token (still an operator)")
	}
	if err := s.ReroleTeamMemberAsManager(ctx, Actor{UserID: rt.cal, Via: authz.ViaSession}, rt.team, rt.ben, authz.TeamViewer); err != nil {
		t.Fatal(err)
	}
	if !marked(t, pool, rt.token) {
		t.Fatal("an operator dropping to VIEWER left the token unmarked")
	}
}

// TestDisablingAnOperatorMarksTheTeamsHostTokensRotate: SetDisabledAs does
// not touch memberships, so it carries its own trigger. Disabling cal (a
// viewer) is the control: no mark. Disabling ben (the operator) marks team's
// token, in the disable's transaction.
func TestDisablingAnOperatorMarksTheTeamsHostTokensRotate(t *testing.T) {
	s, pool := storeFor(t)
	ctx := context.Background()
	rt := newRotateTeam(t, s, pool, "dis")
	if _, err := s.SetDisabledAs(ctx, &admin, rt.cal, true); err != nil {
		t.Fatal(err)
	}
	if marked(t, pool, rt.token) {
		t.Fatal("disabling a VIEWER marked the team's host token rotate")
	}
	if _, err := s.SetDisabledAs(ctx, &admin, rt.ben, true); err != nil {
		t.Fatal(err)
	}
	if !marked(t, pool, rt.token) {
		t.Fatal("disabling an OPERATOR left the team's host token unmarked")
	}
}
