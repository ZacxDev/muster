package authz

import (
	"context"
	"strings"
	"testing"
)

// TestTeamRoleAtLeast pins the role order the handlers gate on (viewer <
// operator < manager) — the order migration 0127's muster_team_ids reads —
// and that an unknown role reaches nothing and is reached by nothing.
func TestTeamRoleAtLeast(t *testing.T) {
	roles := []TeamRole{TeamViewer, TeamOperator, TeamManager}
	for i, r := range roles {
		for j, min := range roles {
			if got, want := r.AtLeast(min), i >= j; got != want {
				t.Errorf("%s.AtLeast(%s) = %v; want %v", r, min, got, want)
			}
		}
		if r.AtLeast("owner") || TeamRole("owner").AtLeast(r) || TeamRole("").AtLeast(TeamViewer) {
			t.Errorf("an unknown role compares as a role (%s)", r)
		}
	}
}

func TestMembershipsAreCanonical(t *testing.T) {
	a, err := NewMemberships([]Membership{{15, TeamViewer}, {3, TeamManager}, {12, TeamOperator}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewMemberships([]Membership{{12, TeamOperator}, {15, TeamViewer}, {3, TeamManager}})
	if err != nil {
		t.Fatal(err)
	}
	if a != b || a != "3:manager,12:operator,15:viewer" {
		t.Fatalf("memberships = %q and %q; want one canonical form", a, b)
	}
	if r, ok := a.RoleIn(12); !ok || r != TeamOperator {
		t.Errorf("RoleIn(12) = %q, %v", r, ok)
	}
	if r, ok := a.RoleIn(4); ok || r != "" {
		t.Errorf("RoleIn(4) = %q, %v; want none", r, ok)
	}
	if got := a.List(); len(got) != 3 || got[0] != (Membership{3, TeamManager}) || got[2] != (Membership{15, TeamViewer}) {
		t.Errorf("List = %v", got)
	}
	if none, err := NewMemberships(nil); err != nil || none != "" || none.List() != nil {
		t.Errorf("no memberships = %q, %v", none, err)
	}
	for name, ms := range map[string][]Membership{
		"a team twice":    {{3, TeamViewer}, {3, TeamManager}},
		"an unknown role": {{3, "owner"}},
		"team zero":       {{0, TeamViewer}},
	} {
		if _, err := NewMemberships(ms); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestWithPrincipalRefusesATeam is the backstop behind internal/api's admit:
// no code may attach a team principal at a door outside teamActingVias (here,
// the browser session; TestTheTeamActingViasAreExactlyTheListedDoors walks
// every door). The control is the same principal with Team false.
func TestWithPrincipalRefusesATeam(t *testing.T) {
	p := Principal{UserID: 12, Role: RoleMember, Via: ViaSession}
	_ = WithPrincipal(context.Background(), p) // control: attaches
	p.Team = true
	defer func() {
		r := recover()
		if r == nil || !strings.Contains(r.(string), "a team never signs in") {
			t.Fatalf("WithPrincipal(team) = %v; want its team refusal", r)
		}
	}()
	_ = WithPrincipal(context.Background(), p)
}
