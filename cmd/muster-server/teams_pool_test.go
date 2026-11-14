package main

import (
	"context"
	"strings"
	"testing"
)

// fakeTeamNamer answers TeamNames as users.PGStore does: the ids among those
// asked that are TEAMS, with their names; a person's id and an unknown id are
// left out alike.
type fakeTeamNamer map[int64]string

func (f fakeTeamNamer) TeamNames(_ context.Context, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	for _, id := range ids {
		if n, ok := f[id]; ok {
			out[id] = n
		}
	}
	return out, nil
}

// TestBootRefusesAPersonInTheTeamServerPool: MUSTER_TEAMS_ON_SERVER_POOL lends
// the server's Claude pool to every id it lists, so an id that is not a team —
// a person's (the operator's own, 1) or one naming nobody — refuses to boot,
// naming the id. The control is a list of teams only, accepted with their
// names; an empty list needs no database; a list with no database is refused.
func TestBootRefusesAPersonInTheTeamServerPool(t *testing.T) {
	ctx := context.Background()
	teams := fakeTeamNamer{12: "platform", 14: "design"}

	got, err := checkTeamsOnServerPool(ctx, teams, map[int64]bool{12: true, 14: true})
	if err != nil || got[12] != "platform" || got[14] != "design" || len(got) != 2 {
		t.Fatalf("control: two teams: %v, %v; want both accepted with their names", got, err)
	}
	for _, c := range []struct {
		name string
		ids  map[int64]bool
		want string
	}{
		{"a person", map[int64]bool{12: true, 1: true}, "lists 1, which is not a team"},
		{"nobody", map[int64]bool{99: true}, "lists 99, which is not a team"},
		{"two", map[int64]bool{1: true, 99: true, 14: true}, "lists 1, 99, which are not a team"},
	} {
		if _, err := checkTeamsOnServerPool(ctx, teams, c.ids); err == nil || !strings.Contains(err.Error(), c.want) ||
			!strings.Contains(err.Error(), envTeamsOnServerPool) {
			t.Errorf("%s: %v; want a boot refusal naming %s and %q", c.name, err, envTeamsOnServerPool, c.want)
		}
	}
	if got, err := checkTeamsOnServerPool(ctx, nil, nil); err != nil || got != nil {
		t.Fatalf("unset: %v, %v; want nothing to check", got, err)
	}
	if _, err := checkTeamsOnServerPool(ctx, nil, map[int64]bool{12: true}); err == nil || !strings.Contains(err.Error(), "no database") {
		t.Fatalf("no database: %v; want a refusal", err)
	}
}

// TestTheTeamServerPoolParses: the variable parses as an id list, and a
// malformed one refuses to boot naming it. (Beside the Compose chief it is
// refused too: TestTheComposeChiefRefusesAnAgentPlaneVariableBesideIt's table.)
func TestTheTeamServerPoolParses(t *testing.T) {
	cfg, err := loadEnv(t, map[string]string{envTeamsOnServerPool: " 12, 14 ,"})
	if err != nil || len(cfg.TeamsOnServerPool) != 2 || !cfg.TeamsOnServerPool[12] || !cfg.TeamsOnServerPool[14] {
		t.Fatalf("parse: %v, %v", cfg.TeamsOnServerPool, err)
	}
	if _, err := loadEnv(t, map[string]string{envTeamsOnServerPool: "platform"}); err == nil || !strings.Contains(err.Error(), envTeamsOnServerPool) {
		t.Fatalf("a malformed list: %v; want a boot refusal naming the variable", err)
	}
}
