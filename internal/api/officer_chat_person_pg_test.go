package api

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
)

// TestAChatPersonPrincipalAdmitsATeamManager: the officer door's rule for who
// may decide a team officer's work in its chat (chatPersonPrincipal, behind
// officer_confirm_card and the refusal cards) admits the team's MANAGER and
// refuses its viewer and its operator (deciding is a manager's), acting as
// the manager, never as the team. 🔴 RED at the base (f65b7e8ef): it read the role from
// TeamScope(), which a principal minted on the officer door never carries
// (teamVias), so it refused every manager — a team officer's chat could never
// put a confirm card in front of its manager. CONTROL: the owner of a personal
// officer is admitted.
func TestAChatPersonPrincipalAdmitsATeamManager(t *testing.T) {
	r := newDeclRig(t, declRigOpts{})
	const operator = int64(987303)
	dbtest.OwnerExec(t, dbtest.DSN(t), `INSERT INTO users (id, kind, role, display_name) OVERRIDING SYSTEM VALUE VALUES (987303, 'person', 'member', 'decl-operator') ON CONFLICT DO NOTHING`)
	t.Cleanup(func() {
		dbtest.OwnerExec(t, dbtest.DSN(t), `DELETE FROM team_members WHERE team_id = 987302`)
		dbtest.OwnerExec(t, dbtest.DSN(t), `DELETE FROM users WHERE id = 987303`)
	})
	if _, err := r.f.pool.Exec(authz.System(context.Background()),
		`INSERT INTO team_members (team_id, user_id, role, added_by) VALUES (987302, 1, 'manager', 1), (987302, 987301, 'viewer', 1), (987302, 987303, 'operator', 1)`); err != nil {
		t.Fatal(err)
	}
	team := officers.Officer{ID: "team:ops", Owner: declTeam, Backend: officers.BackendBuiltin}
	pctx, ok := r.s.chatPersonPrincipal(context.Background(), team, 1)
	if !ok {
		t.Fatalf("the team's manager (user 1) was refused")
	}
	// It acts as the MANAGER, never as the team.
	if p, ok := authz.From(pctx); !ok || p.UserID != 1 || p.Team || p.Via != authz.ViaOfficer {
		t.Errorf("the manager's principal: %+v (set %v); want user 1, a person, on the officer door", p, ok)
	}
	for who, uid := range map[string]int64{"viewer": declOther, "operator": operator} {
		if _, ok := r.s.chatPersonPrincipal(context.Background(), team, uid); ok {
			t.Errorf("the team's %s was admitted", who)
		}
	}
	own := officers.Officer{ID: "own:main", Owner: declOther, Backend: officers.BackendBuiltin}
	if _, ok := r.s.chatPersonPrincipal(context.Background(), own, declOther); !ok {
		t.Errorf("control: the officer's owner was refused")
	}
	if _, ok := r.s.chatPersonPrincipal(context.Background(), own, 1); ok {
		t.Errorf("control: another person was admitted to a personal officer")
	}
}
