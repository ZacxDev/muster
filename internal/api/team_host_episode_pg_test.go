package api

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// TestATeamHostOpensNoEpisodeWhileTheStateArmIsOff (team sharing 2b.2; removed
// by 2b.3 with the state pass's team arm): with session state on, a TEAM
// host's waiting window opens NO session_episodes row — the state pass that
// settles and pushes an episode iterates persons only, so the row would be an
// orphan — while the same window for a PERSON opens one (the control).
//
// 🔴 RED at the base (93e002f6): attentionMirrorChanged opened the team's
// episode like a person's.
func TestATeamHostOpensNoEpisodeWhileTheStateArmIsOff(t *testing.T) {
	f := newStateFixture(t, false)
	f.noGrace()
	f.srv.ext.Users = users.NewPG(f.pool)
	team := newTeam(t, f.pool, f.member)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM session_episodes WHERE owner_id = $1`, team)
	})
	f.mirror.mu.Lock()
	f.mirror.teamHost = team
	f.mirror.mu.Unlock()
	f.waitWithSecrets()
	count := func(owner int64) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM session_episodes WHERE owner_id = $1`, owner).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	f.srv.AttentionMirrorChanged(team)
	if n := count(team); n != 0 {
		t.Fatalf("a team host's waiting window opened %d episode(s); want none until 2b.3's state arm", n)
	}
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	if n := count(authz.OperatorUserID); n != 1 {
		t.Fatalf("CONTROL: a person's waiting window opened %d episode(s); want 1", n)
	}
}
