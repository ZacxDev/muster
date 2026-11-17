package api

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// tsNamedUsers is tsUsers that also names its teams (teamNamer), as
// users.PGStore does.
type tsNamedUsers struct {
	tsUsers
	names map[int64]string
}

func (u *tsNamedUsers) TeamNames(_ context.Context, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	for _, id := range ids {
		if n, ok := u.names[id]; ok {
			out[id] = n
		}
	}
	return out, nil
}

// TestATeamAgentsCardCarriesItsTeam: the sidebar's entries (agentsListEntries)
// mark a TEAM agent's card with its team — its owner id and the team's
// display name — so the sidebar draws it in the team's group and the switcher
// labels it with its team; the viewer's own agents carry none. The team's id
// (tsTeam) is neither the viewer's (tsOperator) nor any agent's id.
//
// 🔴 RED with withAgentTeams not called (the mutant): no card carried a team.
func TestATeamAgentsCardCarriesItsTeam(t *testing.T) {
	u := &tsNamedUsers{tsUsers: newTSUsers(), names: map[int64]string{tsTeam: "Platform"}}
	store := &tsAgents{rows: []agents.Agent{
		teamChiefRow(), // id 40, the team's chief
		{ID: 44, Name: "team-wren", DisplayName: "team-wren", OwnerID: tsTeam, Kind: agents.KindClaudeCode},
		{ID: 41, Name: "keen-wren", DisplayName: "keen-wren", OwnerID: tsOperator, Kind: agents.KindClaudeCode},
		{ID: 99, Name: agents.ChiefNameFor(tsOperator), DisplayName: ChiefDisplayName, OwnerID: tsOperator, Kind: agents.KindClaudeCode},
	}}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Notes: stubNotesStore{}, Agents: store, Users: u, ChiefPrefs: chiefSwitchedOn{}})
	cards, _, err := s.agentsListEntries(personCtx(u.tsUsers, tsOperator))
	if err != nil {
		t.Fatal(err)
	}
	type team struct {
		owner int64
		name  string
	}
	want := map[int64]team{40: {tsTeam, "Platform"}, 44: {tsTeam, "Platform"}, 41: {}, 99: {}}
	if len(cards) != len(want) {
		t.Fatalf("%d cards, want %d", len(cards), len(want))
	}
	for _, c := range cards {
		w, ok := want[c.ID]
		if !ok {
			t.Errorf("unexpected card %d", c.ID)
			continue
		}
		if got := (team{c.TeamOwner, c.Team}); got != w {
			t.Errorf("card %d (%s) carries team %+v, want %+v", c.ID, c.Name, got, w)
		}
	}
}

// TestHomeWithNoChiefOpensNoTeamAgent: a member with no chief of their own
// and no recents is sent home (homeTarget) to their OWN first session, never
// to the team's chief — which navBefore draws first among chiefs — nor to any
// team agent. With nothing of their own, home is nowhere ("": the onboarding
// page), not a team agent.
//
// 🔴 RED with homeTarget's TeamOwner filter removed: home opened the team's
// chief.
func TestHomeWithNoChiefOpensNoTeamAgent(t *testing.T) {
	u := &tsNamedUsers{tsUsers: newTSUsers(), names: map[int64]string{tsTeam: "Platform"}}
	store := &tsAgents{rows: []agents.Agent{
		teamChiefRow(), // id 40
		{ID: 44, Name: "team-wren", DisplayName: "team-wren", OwnerID: tsTeam, Kind: agents.KindClaudeCode},
		{ID: 41, Name: "keen-wren", DisplayName: "keen-wren", OwnerID: tsOperator, Kind: agents.KindClaudeCode},
	}}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Notes: stubNotesStore{}, Agents: store, Users: u, ChiefPrefs: chiefSwitchedOn{}})
	r := httptest.NewRequest("GET", "/", nil).WithContext(personCtx(u.tsUsers, tsOperator))
	href, err := s.homeTarget(r, homeExcept{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(href, "/agents/keen-wren") {
		t.Errorf("home is %q; want the member's own agent keen-wren, not a team agent", href)
	}
	// Nothing of their own: nowhere, not the team's.
	store.rows = store.rows[:2]
	if href, err := s.homeTarget(r, homeExcept{}); err != nil || href != "" {
		t.Errorf("home with only team agents listed is %q (%v); want \"\" (onboarding), not a team agent", href, err)
	}
}
