package authz

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// TeamRole is a person's role in a team (docs/design/team-sharing.md). The set
// is closed and ordered: viewer < operator < manager.
type TeamRole string

const (
	TeamViewer   TeamRole = "viewer"
	TeamOperator TeamRole = "operator"
	TeamManager  TeamRole = "manager"
)

// Valid reports whether r is one of the three roles.
func (r TeamRole) Valid() bool {
	return r == TeamViewer || r == TeamOperator || r == TeamManager
}

// Membership is one person's role in one team.
type Membership struct {
	Team int64
	Role TeamRole
}

// Memberships is a person's memberships in ONE canonical, comparable form:
// "team:role" pairs sorted by team id and joined by commas ("" = none), so a
// Principal stays comparable with == and the same set always reads the same.
// Build it with NewMemberships; the zero value is "no teams".
type Memberships string

// NewMemberships is ms in canonical form. A team listed twice, a non-positive
// team id or an unknown role is an error, never a guess.
func NewMemberships(ms []Membership) (Memberships, error) {
	sorted := append([]Membership(nil), ms...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Team < sorted[j].Team })
	parts := make([]string, 0, len(sorted))
	for i, m := range sorted {
		if m.Team <= 0 {
			return "", fmt.Errorf("authz: membership names team %d", m.Team)
		}
		if !m.Role.Valid() {
			return "", fmt.Errorf("authz: membership of team %d has unknown role %q", m.Team, m.Role)
		}
		if i > 0 && sorted[i-1].Team == m.Team {
			return "", fmt.Errorf("authz: team %d listed twice", m.Team)
		}
		parts = append(parts, strconv.FormatInt(m.Team, 10)+":"+string(m.Role))
	}
	return Memberships(strings.Join(parts, ",")), nil
}

// List is the memberships, by team id.
func (m Memberships) List() []Membership {
	if m == "" {
		return nil
	}
	var out []Membership
	for _, part := range strings.Split(string(m), ",") {
		id, role, _ := strings.Cut(part, ":")
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			continue // unreachable for a value NewMemberships built
		}
		out = append(out, Membership{Team: n, Role: TeamRole(role)})
	}
	return out
}

// RoleIn is the role in team, and whether there is one.
func (m Memberships) RoleIn(team int64) (TeamRole, bool) {
	for _, x := range m.List() {
		if x.Team == team {
			return x.Role, true
		}
	}
	return "", false
}

// teamVias are the doors whose principal carries its team memberships into
// the database (Principal.TeamScope): a PERSON is present — the browser
// session, a paired phone's signed action. NOT a personal API token (mst_):
// it is held by scripts and agents as often as by a person, and a
// team-scoped token is a later decision (operator, phase 2a). NOT an
// agent's door (a member's PERSONAL chief or agent must never read the
// team's rows: a team's data reaches only the team's chief, decision 10),
// a mirror host's (team hosts are phase 2b), a connect or relay door, or any
// background loop. Every other door and every background
// loop acts with none, so a per-owner sweep acting as a person never meets
// that person's teams' rows (the users.PersonIDs lesson, #654 round 1), and a
// machine credential an owner holds (an agent's token, a mirror host's, a
// plugin's, a phone's) reaches only its owner's own rows until the phase that
// shares through it decides otherwise. TestEveryDoorSaysWhetherItCarriesTeams
// pins every Via to this set or its complement.
var teamVias = map[Via]bool{
	ViaSession: true,
	ViaDevice:  true,
}

// TeamScope is the memberships this principal's database queries may use:
// Teams for a person's own request doors (teamVias), none otherwise.
func (p Principal) TeamScope() Memberships {
	if teamVias[p.Via] {
		return p.Teams
	}
	return ""
}
