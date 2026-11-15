package api

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// TestOnlyTheOptedInOfficersCallerAsksForActionsRead is github_actions_read at
// its seam, from the configuration: MUSTER_OFFICERS (officers.Parse) → the
// registry → callerOf (connectors.Caller.ActionsRead, which the GitHub App
// turns into `actions: read` on the own repository's token; githubapp's
// TestAnActionsReadCallerIsMintedActionsReadNeverWrite pins that half).
//
//   - both pools of the officer that declares it ask for it;
//   - another officer in the same list, and an ordinary agent, do not;
//   - the grant's description says so for the one officer and not the other
//     (shipGrantsDescription, a shipping officer's: the CI-watch case).
func TestOnlyTheOptedInOfficersCallerAsksForActionsRead(t *testing.T) {
	const watch, other = "ci-watch:example-web", "triage:example-web"
	list, err := officers.Parse(`[{"id":"`+watch+`","hq_url":"http://hq.invalid/api/v1","repo":"example-org/widget","github_actions_read":true,`+
		`"tiers":{"routine":{"model":"haiku"},"judgement":{"model":"opus"}}},`+
		`{"id":"`+other+`","hq_url":"http://hq.invalid/api/v1","repo":"example-org/widget"}]`, func(k string) string {
		if strings.HasPrefix(k, officers.TokenEnvPrefix) {
			return "fixture-" + k
		}
		return ""
	}, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: &officerAgents{}, Provisioner: stubProvisioner{}, SessionLiveness: stubLiveness{},
		Kinds: stubKinds{pick: "acct-one"}, Connectors: presenceStore{}})
	if _, err := s.EnableOfficers(OfficersConfig{Officers: list, InstallSession: discardSession}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		want bool
	}{
		{"officer-ci-watch-example-web-1", true},
		{"officer-ci-watch-example-web-r1", true},
		{officers.InstanceName(other, 1), false},
		{"brave-otter", false},
	} {
		caller := s.callerOf(agents.Agent{ID: 9, Name: c.name, Kind: agents.KindClaudeCode, Repo: "example-org/widget"})
		if caller.ActionsRead != c.want {
			t.Errorf("%s: callerOf ActionsRead = %v, want %v", c.name, caller.ActionsRead, c.want)
		}
	}
	for _, o := range list {
		o.Ship = &officers.Ship{Stage: officers.StageDraft, Base: "main", BranchPrefix: "officer/",
			Repos: []officers.ShipRepo{{Repo: o.Repo, Base: "main", BranchPrefix: "officer/"}}}
		got := strings.Contains(shipGrantsDescription(o), actionsReadScope)
		if got != (o.ID == watch) {
			t.Errorf("%s: shipGrantsDescription carries the Actions read sentence: %v, want %v\n%s", o.ID, got, o.ID == watch, shipGrantsDescription(o))
		}
	}
}
