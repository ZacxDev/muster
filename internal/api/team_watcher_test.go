package api

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// TestTeamWatcherOfIsTheOneRuleForAnswerControls pins teamWatcherOf — the
// rule both the session view's question cards (handleMirrorAsks) and the
// Asks list (withAskBoxes) draw a team's questions by — over every role, and
// teamOperatorOn (the session view's composer and queue) as its complement on
// a team host's entry: the two cannot disagree about who may answer.
func TestTeamWatcherOfIsTheOneRuleForAnswerControls(t *testing.T) {
	const person, team, other = int64(41), int64(907), int64(908)
	as := func(role authz.TeamRole) context.Context {
		p := authz.Principal{UserID: person, Role: authz.RoleMember, Via: authz.ViaSession}
		if role != "" {
			ms, err := authz.NewMemberships([]authz.Membership{{Team: team, Role: role}})
			if err != nil {
				t.Fatal(err)
			}
			p.Teams = ms
		}
		return authz.WithPrincipal(context.Background(), p)
	}
	for _, c := range []struct {
		name  string
		ctx   context.Context
		owner int64
		want  bool
	}{
		{"the person's own ask", as(authz.TeamViewer), person, false},
		{"no owner", as(authz.TeamViewer), 0, false},
		{"a team the person views", as(authz.TeamViewer), team, true},
		{"a team the person operates", as(authz.TeamOperator), team, false},
		{"a team the person manages", as(authz.TeamManager), team, false},
		{"a team the person is not in (fail closed)", as(authz.TeamOperator), other, true},
		{"no principal (fail closed)", context.Background(), team, true},
	} {
		if got := teamWatcherOf(c.ctx, c.owner); got != c.want {
			t.Errorf("teamWatcherOf, %s: %v; want %v", c.name, got, c.want)
		}
		if c.owner <= 0 || c.ctx == context.Background() {
			continue
		}
		r := httptest.NewRequest("GET", "/", nil).WithContext(mirror.ForOwner(c.ctx, c.owner))
		if on := onTeamHost(r); on && teamOperatorOn(r) == c.want {
			t.Errorf("teamOperatorOn, %s: %v; want the complement of teamWatcherOf (%v)", c.name, teamOperatorOn(r), c.want)
		} else if !on && teamOperatorOn(r) {
			t.Errorf("teamOperatorOn, %s: true off a team host's entry", c.name)
		}
	}
}
