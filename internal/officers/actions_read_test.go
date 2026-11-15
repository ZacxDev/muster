package officers_test

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// TestGitHubActionsReadIsPerOfficer: `github_actions_read` lands on the
// officer that declares it and on no other in the same list, and both of its
// pools carry it (the routine pool is read with Pool, as the registry hands it
// out). Absent and false are false.
func TestGitHubActionsReadIsPerOfficer(t *testing.T) {
	raw := `[{"id":"ci-watch:example-web","hq_url":"http://h/api/v1","repo":"example-org/widget","github_actions_read":true,
	          "tiers":{"routine":{"model":"haiku"},"judgement":{"model":"opus"}}},
	         {"id":"triage:example-web","hq_url":"http://h/api/v1","repo":"example-org/widget"},
	         {"id":"release:example-web","hq_url":"http://h/api/v1","repo":"example-org/widget","github_actions_read":false}]`
	tok := env(map[string]string{"MUSTER_OFFICER_TOKEN_CI_WATCH_EXAMPLE_WEB": "a", "MUSTER_OFFICER_TOKEN_TRIAGE_EXAMPLE_WEB": "b",
		"MUSTER_OFFICER_TOKEN_RELEASE_EXAMPLE_WEB": "c"})
	list, err := officers.Parse(raw, tok, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, o := range list {
		got[o.ID] = o.GitHubActionsRead
	}
	want := map[string]bool{"ci-watch:example-web": true, "triage:example-web": false, "release:example-web": false}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: GitHubActionsRead = %v, want %v", id, got[id], w)
		}
	}
	reg := officers.NewRegistry(list)
	for _, name := range []string{"officer-ci-watch-example-web-1", "officer-ci-watch-example-web-r1"} {
		p, _, ok := reg.ForAgentName(name)
		if !ok || !p.GitHubActionsRead {
			t.Errorf("%s: pool %+v (ok %v), want GitHubActionsRead", name, p, ok)
		}
	}
}

// TestGitHubActionsReadIsRefusedWithoutARepo: the opt-in widens the token
// minted for the officer's OWN repository, so an officer without `repo` is a
// boot refusal naming the officer and the field (the full text pinned); so is
// a host officer, which holds no App token at all. Control: the same
// declaration with a repo parses.
func TestGitHubActionsReadIsRefusedWithoutARepo(t *testing.T) {
	tok := env(map[string]string{"MUSTER_OFFICER_TOKEN_CI_WATCH_EXAMPLE_WEB": "a"})
	_, err := officers.Parse(`[{"id":"ci-watch:example-web","hq_url":"http://h/api/v1","github_actions_read":true}]`, tok, "muster-agent-")
	const want = "MUSTER_OFFICERS[0]: officer ci-watch:example-web: github_actions_read needs `repo`: Actions read is added to the " +
		"token minted for the officer's own repository, and it has none"
	if err == nil || err.Error() != want {
		t.Fatalf("Parse = %v, want exactly\n%s", err, want)
	}
	if _, err := officers.Parse(`[{"id":"ci-watch:example-web","hq_url":"http://h/api/v1","repo":"example-org/widget","github_actions_read":true}]`, tok, "muster-agent-"); err != nil {
		t.Fatalf("with a repo: %v", err)
	}
	_, err = officers.Parse(hostDecl(`,"github_actions_read":true`), noEnv, "muster-agent-")
	if err == nil || !strings.Contains(err.Error(), "github_actions_read (it widens the github connector's App token") {
		t.Fatalf("a host officer with github_actions_read: %v, want a refusal naming it", err)
	}
}
