package authz

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// actsAsTeam is EVERY request door and whether a TEAM principal (the team
// acting principal) may come through it. It fails when requestVias grows or
// shrinks, so a new door decides here whether a team acts through it, and it
// is the literal statement of teamActingVias: the walk below drives
// WithPrincipal itself, so a set that drifted from this table is caught by
// behaviour, not by comparing two spellings of one map.
var actsAsTeam = map[Via]bool{
	// A team-owned agent's own token: the team is the agent row's owner.
	ViaAgent: true, ViaConnect: true, ViaRelay: true,
	// Per-owner background work meeting a team-owned row.
	ViaReap: true, ViaAttention: true, ViaRoute: true, ViaHostDispatch: true, ViaJobQueue: true,
	// A team officer's runtime (phase 3): an officer whose MUSTER_OFFICERS
	// owner is a team runs as the team.
	ViaOfficer: true,
	// A team manager's lifecycle door (slice 4.5): stop, start, destroy and
	// rotate a team agent, and provision the team's chief, as the team.
	ViaTeamAdmin: true,
	// A TEAM HOST (team sharing 2b.2): a team mirror token's MCP door acts as
	// the team (internal/api serves it teamHostTools only), and the question
	// watcher reads a team host's window as the team.
	ViaMirrorHost: true, ViaAsk: true,
	// A TEAM VIEWER PLUGIN (team sharing 2b.3b): its token's door acts as the
	// team (internal/api serves it teamPluginRoutes only), and a member's
	// preview of a team session reads the team's plugins and writes its
	// hand-off as the team (teamPreviewPrincipal).
	ViaPlugin: true,
	// A SERVER APP acting for one person (core PR 1h): NOT a team door. The
	// grant names a person (serverGrantLive admits persons only), and a
	// server app never acts as a team.
	ViaServerPlugin: false,
	// A TEAM HOST'S SEND QUEUE (team sharing 2b.4): the dispatcher probes and
	// pastes into a team host's window as the team; who may still send is
	// the row's queued_by, checked at delivery (internal/api).
	ViaQueue: true,
	// A team-owned gateway agent's turn: NOT a team door in phase 4.
	// internal/api's turnPrincipal refuses a team first; WithPrincipal's
	// panic here is the second guard (TestATeamGatewayTurnIsRefusedAtTheDoor).
	ViaKickoff: false, ViaAgentMessage: false,
	// The scheduled host job runner: NOT a team door. A team host's jobs are
	// refused (a job writes a personal page) and internal/api's
	// hostJobPrincipal refuses a team first; WithPrincipal's panic here is
	// the second guard.
	ViaHostJob: false,
	// Never a team.
	ViaSession: false, ViaDevice: false, ViaToken: false, ViaHook: false, ViaService: false,
	ViaRunner: false, ViaPageLink: false,
	ViaFocus: false,
}

// panicOf runs f and returns its panic message ("" when it did not panic).
func panicOf(f func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprint(r)
			if msg == "" {
				msg = "(empty panic)"
			}
		}
	}()
	f()
	return ""
}

func TestTheTeamActingViasAreExactlyTheListedDoors(t *testing.T) {
	var missing, stale []string
	for v := range requestVias {
		if _, ok := actsAsTeam[v]; !ok {
			missing = append(missing, string(v))
		}
	}
	for v := range actsAsTeam {
		if !requestVias[v] {
			stale = append(stale, string(v))
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("request doors with no actsAsTeam entry (decide whether a team acts through each): %s", strings.Join(missing, ", "))
	}
	if len(stale) > 0 {
		t.Errorf("actsAsTeam lists doors that are not request doors: %s", strings.Join(stale, ", "))
	}
	accepted := 0
	for v, want := range actsAsTeam {
		p := Principal{UserID: 42, Team: true, Role: RoleMember, Via: v}
		var got Principal
		msg := panicOf(func() { got = MustFrom(WithPrincipal(context.Background(), p)) })
		switch {
		case want && msg != "":
			t.Errorf("%s: the team acting principal was refused: %s", v, msg)
		case want && got != p:
			t.Errorf("%s: WithPrincipal attached %+v; want %+v", v, got, p)
		case want:
			accepted++
			if got.TeamScope() != "" || got.All() {
				t.Errorf("%s: the team acting principal has TeamScope %q, All %v; want none, false", v, got.TeamScope(), got.All())
			}
		case !strings.Contains(msg, "a team never signs in, and that door does not act as a team"):
			t.Errorf("%s: a team principal was not refused with the door refusal (panic %q)", v, msg)
		}
		// Control: the same door with a PERSON is accepted, so each refusal
		// above is a fact about the team, not about the door.
		if m := panicOf(func() { WithPrincipal(context.Background(), Principal{UserID: 42, Role: RoleMember, Via: v}) }); m != "" {
			t.Errorf("control %s: a person was refused: %s", v, m)
		}
	}
	if accepted != 14 {
		t.Fatalf("accepted a team at %d doors; the ledger names 14", accepted)
	}
}

// TestATeamGatewayTurnIsRefusedAtTheDoor: a team principal at a gateway turn's
// doors (kickoff, agent-message) panics with the door refusal. It is the
// second guard behind internal/api's turnPrincipal, which refuses a team
// first; the CONTROL is a person at the same doors, accepted.
func TestATeamGatewayTurnIsRefusedAtTheDoor(t *testing.T) {
	for _, v := range []Via{ViaKickoff, ViaAgentMessage} {
		if m := panicOf(func() { WithPrincipal(context.Background(), Principal{UserID: 42, Role: RoleMember, Via: v}) }); m != "" {
			t.Fatalf("control %s: a person was refused: %s", v, m)
		}
		team := Principal{UserID: 42, Team: true, Role: RoleMember, Via: v}
		if m := panicOf(func() { WithPrincipal(context.Background(), team) }); !strings.Contains(m, "a team never signs in, and that door does not act as a team") {
			t.Errorf("%s: a team gateway turn's principal: panic %q; want the door refusal", v, m)
		}
	}
}

// TestATeamActsOnlyAsAMemberWithNoTeams: at a door that DOES act as a team,
// the team acting principal is role member with no memberships of its own;
// anything else is refused with its own message. The control is the
// well-formed principal at the same door.
func TestATeamActsOnlyAsAMemberWithNoTeams(t *testing.T) {
	ms, err := NewMemberships([]Membership{{7, TeamOperator}})
	if err != nil {
		t.Fatal(err)
	}
	for v, ok := range actsAsTeam {
		if !ok {
			continue
		}
		good := Principal{UserID: 42, Team: true, Role: RoleMember, Via: v}
		if m := panicOf(func() { WithPrincipal(context.Background(), good) }); m != "" {
			t.Fatalf("control %s: %s", v, m)
		}
		admin := good
		admin.Role = RoleAdmin
		if m := panicOf(func() { WithPrincipal(context.Background(), admin) }); !strings.Contains(m, "a team acts as a member") {
			t.Errorf("%s: a team principal with role admin: panic %q; want the member refusal", v, m)
		}
		withTeams := good
		withTeams.Teams = ms
		if m := panicOf(func() { WithPrincipal(context.Background(), withTeams) }); !strings.Contains(m, "it must carry none") {
			t.Errorf("%s: a team principal carrying teams: panic %q; want the no-teams refusal", v, m)
		}
		nobody := good
		nobody.UserID = 0
		if m := panicOf(func() { WithPrincipal(context.Background(), nobody) }); !strings.Contains(m, "with no user") {
			t.Errorf("%s: a team principal with no id: panic %q; want the no-user refusal", v, m)
		}
	}
}
