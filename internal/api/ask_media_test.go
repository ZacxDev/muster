package api

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
)

// TestOnlyTheAsksOwnerOrAMemberOfItsTeamSeesItsAttachments is askViewerSees —
// the Go check behind the attachment route, which holds with row-level security
// rolled back — over every kind of viewer, with no database.
func TestOnlyTheAsksOwnerOrAMemberOfItsTeamSeesItsAttachments(t *testing.T) {
	const owner, other, team, otherTeam = 20, 21, 30, 31
	personal := attention.Ask{ID: 1, Owner: owner}
	teams := attention.Ask{ID: 2, Owner: team}
	as := func(p authz.Principal) context.Context { return authz.WithPrincipal(context.Background(), p) }
	member := func(uid int64, teamID int64, role authz.TeamRole) authz.Principal {
		ms, err := authz.NewMemberships([]authz.Membership{{Team: teamID, Role: role}})
		if err != nil {
			t.Fatal(err)
		}
		return authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession, Teams: ms}
	}
	for _, c := range []struct {
		name string
		ctx  context.Context
		ask  attention.Ask
		want bool
	}{
		{"the owner", as(authz.Principal{UserID: owner, Role: authz.RoleMember, Via: authz.ViaSession}), personal, true},
		{"another user", as(authz.Principal{UserID: other, Role: authz.RoleMember, Via: authz.ViaSession}), personal, false},
		{"the owner's teammate, on a personal ask", as(member(other, team, authz.TeamManager)), personal, false},
		{"a viewer of the owning team", as(member(other, team, authz.TeamViewer)), teams, true},
		{"a member of another team", as(member(other, otherTeam, authz.TeamManager)), teams, false},
		{"no principal", context.Background(), personal, false},
		{"the system scope", authz.System(context.Background()), personal, false},
		{"an ask with no owner", as(authz.Principal{UserID: owner, Role: authz.RoleMember, Via: authz.ViaSession}), attention.Ask{ID: 3}, false},
	} {
		if got := askViewerSees(c.ctx, c.ask); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
