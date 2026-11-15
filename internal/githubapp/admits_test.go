package githubapp

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// TestAdmitsIsAnAdminOrAListedTeamsOwnPrincipal pins the one rule of who may
// use the deployment's App (MUSTER_TEAMS_ON_GITHUB_APP, team sharing 4.8):
// an admin; the team acting principal of a LISTED team, through each door a
// team acts through; and nobody else — not an unlisted team, not a manager of
// the listed team on their own principal, not System, not a bare context.
// Fixture ids are pairwise distinct: the listed team 12, an unlisted team 14,
// the member 21.
func TestAdmitsIsAnAdminOrAListedTeamsOwnPrincipal(t *testing.T) {
	admits := Admits(map[int64]bool{12: true})
	bg := context.Background()
	team := func(id int64, via authz.Via) context.Context {
		return authz.WithPrincipal(bg, authz.Principal{UserID: id, Team: true, Role: authz.RoleMember, Via: via})
	}
	mships, err := authz.NewMemberships([]authz.Membership{{Team: 12, Role: authz.TeamManager}})
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		ctx  context.Context
		want bool
	}{
		"an admin":                         {authz.WithPrincipal(bg, authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession}), true},
		"the listed team's agent":          {team(12, authz.ViaAgent), true},
		"the listed team's connect door":   {team(12, authz.ViaConnect), true},
		"the listed team's manager's door": {team(12, authz.ViaTeamAdmin), true},
		"an unlisted team's agent":         {team(14, authz.ViaAgent), false},
		"a manager of the listed team, as themself": {authz.WithPrincipal(bg, authz.Principal{UserID: 21, Role: authz.RoleMember,
			Via: authz.ViaSession, Teams: mships}), false},
		"a member's agent": {authz.WithPrincipal(bg, authz.Principal{UserID: 21, Role: authz.RoleMember, Via: authz.ViaAgent}), false},
		"System":           {authz.System(bg), false},
		"no principal":     {bg, false},
	} {
		if got := admits(c.ctx); got != c.want {
			t.Errorf("%s: admitted %v; want %v", name, got, c.want)
		}
	}
	if Admits(nil)(team(12, authz.ViaAgent)) {
		t.Error("with no list, a team was admitted")
	}
}
