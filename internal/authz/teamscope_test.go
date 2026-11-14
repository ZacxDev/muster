package authz

import (
	"sort"
	"strings"
	"testing"
)

// carriesTeams is EVERY door a request principal can come through, and
// whether its database queries see the person's teams (TeamScope). It fails
// when requestVias grows or shrinks, so a new door must decide here whether a
// team's rows reach it (phase 2a of docs/design/team-sharing.md).
var carriesTeams = map[Via]bool{
	ViaSession:      true,  // the person, in the browser
	ViaToken:        false, // a personal API token: no team access (a team-scoped token may come later)
	ViaHook:         false, // the shared machine token (acts as user 1)
	ViaService:      false, // the service token (acts as user 1)
	ViaAgent:        false, // an agent of the person (their PERSONAL chief included): never the team's rows (decision 10)
	ViaConnect:      false,
	ViaRelay:        false,
	ViaRunner:       false,
	ViaPlugin:       false,
	ViaMirrorHost:   false, // a host's token: team hosts are phase 2b
	ViaDevice:       true,  // a paired phone: a person signing an action
	ViaPageLink:     false,
	ViaQueue:        false, // the send queue's dispatcher
	ViaFocus:        false,
	ViaRoute:        false,
	ViaAttention:    false,
	ViaAsk:          false,
	ViaReap:         false,
	ViaOfficer:      false,
	ViaKickoff:      false,
	ViaAgentMessage: false,
	ViaHostDispatch: false,
	ViaJobQueue:     false,
	ViaTeamAdmin:    false, // a manager's action, run AS the team: the team acting principal carries no Teams
}

func TestEveryDoorSaysWhetherItCarriesTeams(t *testing.T) {
	var missing, stale []string
	for v := range requestVias {
		if _, ok := carriesTeams[v]; !ok {
			missing = append(missing, string(v))
		}
	}
	for v := range carriesTeams {
		if !requestVias[v] {
			stale = append(stale, string(v))
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("request doors with no carriesTeams entry (decide whether a team's rows reach each): %s", strings.Join(missing, ", "))
	}
	if len(stale) > 0 {
		t.Errorf("carriesTeams lists doors that are not request doors: %s", strings.Join(stale, ", "))
	}
	if len(requestVias) < 15 {
		t.Fatalf("requestVias has %d doors; the ledger is reading nothing", len(requestVias))
	}
	ms, err := NewMemberships([]Membership{{12, TeamOperator}})
	if err != nil {
		t.Fatal(err)
	}
	for v, want := range carriesTeams {
		got := Principal{UserID: 7, Role: RoleMember, Via: v, Teams: ms}.TeamScope()
		if (got == ms) != want || (!want && got != "") {
			t.Errorf("%s: TeamScope = %q; want teams carried = %v", v, got, want)
		}
	}
	if got := (Principal{Role: RoleAdmin, Via: ViaSystem, Teams: ms}).TeamScope(); got != "" {
		t.Errorf("System's TeamScope = %q; want none", got)
	}
}
