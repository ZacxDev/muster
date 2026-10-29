package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"log"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/githubapp"
)

// The scope sentences, LITERALLY. The write one is byte-identical to the
// constant githubAppScope used to be.
const (
	wantWriteScope = "Contents and Pull requests read and write, Checks and Commit statuses read"
	wantReadScope  = "READ ONLY: Contents, Pull requests, Checks and Commit statuses read — no push, no merge, no pull-request write"
)

// presenceStore is a non-nil connector store whose methods are never called:
// the code under test asks only whether one is configured.
type presenceStore struct{ connectors.Store }

func accessApp(t *testing.T, access githubapp.Access) *githubapp.App {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app, err := githubapp.New(githubapp.Config{AppID: 12345, PrivateKey: k, Access: access,
		Accounts: []githubapp.Account{{Login: "example-org", ID: 4200}}})
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// TestTheConnectorScopeDescriptionFollowsTheAccessMode: what the approve card
// and connector_requests_list say approving a `github` request grants
// (grantsDescription, via githubAppScope) names the App's ACTUAL mode — the
// read sentence and never "read and write" in read mode, the unchanged write
// sentence otherwise. The App is the real githubapp.App, so the text is read
// from the same mode the mint uses.
func TestTheConnectorScopeDescriptionFollowsTheAccessMode(t *testing.T) {
	admin := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
	for _, c := range []struct {
		access     githubapp.Access
		want, never string
	}{
		{"", wantWriteScope, "READ ONLY"},
		{githubapp.AccessWrite, wantWriteScope, "READ ONLY"},
		{githubapp.AccessRead, wantReadScope, "read and write"},
	} {
		app := accessApp(t, c.access)
		if got := githubAppScope(app); got != c.want {
			t.Errorf("access %q: githubAppScope = %q, want %q", c.access, got, c.want)
		}
		s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
		s.UseExtensions(Extensions{Connectors: connectors.WithVirtual(presenceStore{}, app, authz.IsAdmin), GitHubApp: app})
		desc, _ := s.grantsDescription(admin, connectors.Grant{Connector: githubapp.ConnectorName, AgentID: 7},
			connectors.Connector{}, "example-org/widget")
		if !strings.Contains(desc, "("+c.want+")") || strings.Contains(desc, c.never) {
			t.Errorf("access %q: the approve description is %q, want it to carry (%s) and never %q", c.access, desc, c.want, c.never)
		}
	}
}

// TestTheGitHubPresetHelpFollowsTheAccessMode: the stored-token preset's help
// recommends a read/write token in write mode (unchanged) and a read-only one
// in read mode — saying the setting does not limit a stored token.
func TestTheGitHubPresetHelpFollowsTheAccessMode(t *testing.T) {
	help := func(access githubapp.Access) string {
		s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
		s.UseExtensions(Extensions{GitHubApp: &listingApp{access: access}})
		for _, p := range s.presetViews() {
			if p.Key == connectors.GitHubPreset.Key {
				return p.Help
			}
		}
		t.Fatal("no github preset")
		return ""
	}
	if got := help(githubapp.AccessWrite); got != connectors.GitHubPreset.Help {
		t.Errorf("write mode preset help changed: %q", got)
	}
	got := help(githubapp.AccessRead)
	if got != githubPresetReadOnlyHelp || strings.Contains(got, "read/write") || !strings.Contains(got, "does NOT limit a stored token") {
		t.Errorf("read mode preset help = %q", got)
	}
}

// TestATaskAgentIsNeverToldToPushInReadMode: the close-out protocol of an agent
// working a task on a repository tells it to push and open a PR in write mode
// (unchanged), and in read mode tells it it CANNOT, naming the variable, with
// no push or pull-request recipe in the prompt at all.
func TestATaskAgentIsNeverToldToPushInReadMode(t *testing.T) {
	note := int64(3)
	a := agents.Agent{ID: 7, Name: "quiet-fixture", Repo: "example-org/widget", NoteID: &note}
	prompt := func(access githubapp.Access) string {
		s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
		s.UseExtensions(Extensions{Connectors: presenceStore{}, GitHubApp: &listingApp{access: access}})
		return s.AgentInstructions(a)
	}
	w := prompt(githubapp.AccessWrite)
	if !strings.Contains(w, "Push through muster's proxy") || !strings.Contains(w, "/repos/example-org/widget/pulls") {
		t.Fatalf("control: write mode lost its push steps:\n%s", w)
	}
	r := prompt(githubapp.AccessRead)
	if !strings.Contains(r, readOnlyCloseOut) {
		t.Fatalf("read mode: the read-only close-out is missing:\n%s", r)
	}
	for _, never := range []string{"Push through muster's proxy", "/pulls", "git push", "push <branch>", "with the PR URL"} {
		if strings.Contains(r, never) {
			t.Errorf("read mode prompt still says %q:\n%s", never, r)
		}
	}
}
