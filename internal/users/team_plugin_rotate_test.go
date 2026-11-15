package users

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
)

// A TEAM VIEWER PLUGIN'S ROTATE MARK (team sharing 2b.3b; 0134), against real
// Postgres: the transactions that mark the team's host tokens mark its
// unrevoked viewer plugins too. Each case reads plugins.rotate_at directly.

// teamPlugin plants a viewer plugin of team (revoked when revoked), under
// System, and returns its id.
func teamPlugin(t *testing.T, pool *pgxpool.Pool, team int64, name string, revoked bool) int64 {
	t.Helper()
	ctx := authz.System(context.Background())
	t.Cleanup(func() { _, _ = pool.Exec(authz.System(context.Background()), `DELETE FROM plugins WHERE owner_id = $1`, team) })
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope, capabilities, origin, entry_url, revoked_at)
		VALUES ($1, $2, $2, $3, 'none', ARRAY['viewer'], 'https://*.viewer.example.net', 'https://t.viewer.example.net/e', CASE WHEN $4 THEN now() END) RETURNING id`,
		team, name, "rotate-plugin-hash-"+name, revoked).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func pluginMarked(t *testing.T, pool *pgxpool.Pool, id int64) bool {
	t.Helper()
	var m bool
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT rotate_at IS NOT NULL FROM plugins WHERE id = $1`, id).Scan(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestLeavingATeamMarksItsViewerPluginsRotate: removing the VIEWER marks
// nothing (the control); removing the OPERATOR marks the team's live viewer
// plugin and leaves its revoked one alone; and, on a second team, disabling
// its operator marks that team's plugin.
//
// 🔴 RED at 89e6c613: plugins had no rotate_at (0134), so nothing marked a
// team plugin's token after the person who saw it left.
func TestLeavingATeamMarksItsViewerPluginsRotate(t *testing.T) {
	s, pool := storeFor(t)
	ctx := context.Background()
	rt := newRotateTeam(t, s, pool, "prm")
	live := teamPlugin(t, pool, rt.team, "prm-live", false)
	gone := teamPlugin(t, pool, rt.team, "prm-gone", true)
	if err := s.RemoveTeamMemberAsManager(ctx, Actor{UserID: rt.ana, Via: authz.ViaSession}, rt.team, rt.cal); err != nil {
		t.Fatal(err)
	}
	if pluginMarked(t, pool, live) {
		t.Fatal("removing a VIEWER marked the team's viewer plugin rotate")
	}
	if err := s.RemoveTeamMemberAsManager(ctx, Actor{UserID: rt.ana, Via: authz.ViaSession}, rt.team, rt.ben); err != nil {
		t.Fatal(err)
	}
	if !pluginMarked(t, pool, live) {
		t.Fatal("removing an OPERATOR left the team's viewer plugin unmarked")
	}
	if pluginMarked(t, pool, gone) {
		t.Fatal("a revoked plugin was marked rotate")
	}
	rt2 := newRotateTeam(t, s, pool, "pdis")
	p2 := teamPlugin(t, pool, rt2.team, "pdis-live", false)
	if _, err := s.SetDisabledAs(ctx, &admin, rt2.ben, true); err != nil {
		t.Fatal(err)
	}
	if !pluginMarked(t, pool, p2) {
		t.Fatal("disabling an OPERATOR left the team's viewer plugin unmarked")
	}
}
