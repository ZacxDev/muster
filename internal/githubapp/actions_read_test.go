package githubapp

import (
	"context"
	"strings"
	"testing"
)

// The agent-mint bodies of a caller that opted in to Actions read
// (connectors.Caller.ActionsRead — an officer declaring github_actions_read),
// LITERALLY: the four levels of the mode, then `actions: read`. Never write,
// whatever the mode.
const (
	writeActionsMintBody = `{"repositories":["widget"],"permissions":{"contents":"write","pull_requests":"write","checks":"read","statuses":"read","actions":"read"}}`
	readActionsMintBody  = `{"repositories":["widget"],"permissions":{"contents":"read","pull_requests":"read","checks":"read","statuses":"read","actions":"read"}}`
)

// TestAnActionsReadCallerIsMintedActionsReadNeverWrite: in BOTH modes (and for
// a read-only caller in write mode, an officer's case) a caller with
// ActionsRead gets its mode's four levels plus actions READ — the literal
// bodies. The write mode is the one a mutant granting actions at the token's
// own level would turn into `actions: write`.
func TestAnActionsReadCallerIsMintedActionsReadNeverWrite(t *testing.T) {
	for _, c := range []struct {
		name     string
		access   Access
		readOnly bool
		want     string
	}{
		{"write mode", AccessWrite, false, writeActionsMintBody},
		{"read mode", AccessRead, false, readActionsMintBody},
		{"read-only caller in write mode", AccessWrite, true, readActionsMintBody},
	} {
		app, f, _ := newTestAppAccess(t, c.access)
		caller := agent(7, "example-org/widget")
		caller.ReadOnly, caller.ActionsRead = c.readOnly, true
		if _, _, err := app.Credential(context.Background(), caller, DefaultAPIHost, "repos/example-org/widget/actions/jobs/1/logs"); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		m := f.mintsSoFar()
		if len(m) != 1 || m[0].body != c.want {
			t.Fatalf("%s: mints %+v, want exactly one with body\n%s", c.name, m, c.want)
		}
		if strings.Contains(m[0].body, `"actions":"write"`) {
			t.Fatalf("%s: the mint asks for actions write: %s", c.name, m[0].body)
		}
	}
}

// TestACallerWithoutActionsReadAsksForNoActionsPermission: every other caller
// (every other officer, every task agent) mints the four-permission literal
// with no `actions` key at all, in both modes.
func TestACallerWithoutActionsReadAsksForNoActionsPermission(t *testing.T) {
	for access, want := range map[Access]string{AccessWrite: writeMintBody, AccessRead: readMintBody} {
		app, f, _ := newTestAppAccess(t, access)
		if _, _, err := app.Credential(context.Background(), agent(7, "example-org/widget"), DefaultAPIHost, "repos/example-org/widget/actions/runs"); err != nil {
			t.Fatal(err)
		}
		m := f.mintsSoFar()
		if len(m) != 1 || m[0].body != want || strings.Contains(m[0].body, "actions") {
			t.Fatalf("access %q: mints %+v, want exactly one with body\n%s", access, m, want)
		}
	}
}

// TestActionsReadIsForTheOwnRepositoryOnly: an ActionsRead caller's request to
// one of its read repositories is minted the read literal for THAT repository,
// with no actions permission: the opt-in widens the caller's own repository's
// token and nothing else.
func TestActionsReadIsForTheOwnRepositoryOnly(t *testing.T) {
	app, f, _ := newTestAppAccess(t, AccessWrite)
	caller := agent(7, "example-org/widget")
	caller.ReadOnly, caller.ActionsRead, caller.ReadRepos = true, true, []string{"example-org/gadget"}
	if _, _, err := app.Credential(context.Background(), caller, DefaultAPIHost, "repos/example-org/gadget/actions/runs"); err != nil {
		t.Fatal(err)
	}
	m := f.mintsSoFar()
	if len(m) != 1 || m[0].body != gadgetReadMintBody {
		t.Fatalf("mints %+v, want exactly one with body\n%s", m, gadgetReadMintBody)
	}
}

// TestAnActionsTokenIsCachedApart: one agent minted with and without
// ActionsRead (its officer's opt-in changed) gets two tokens, each served back
// to its own kind of request: a token without Actions is never reused for an
// ActionsRead request, nor the other way round.
func TestAnActionsTokenIsCachedApart(t *testing.T) {
	app, f, _ := newTestAppAccess(t, AccessRead)
	ctx := context.Background()
	plain := agent(7, "example-org/widget")
	withActions := plain
	withActions.ActionsRead = true
	ptok, _, err := app.Credential(ctx, plain, DefaultAPIHost, "repos/example-org/widget/pulls")
	if err != nil {
		t.Fatal(err)
	}
	atok, _, err := app.Credential(ctx, withActions, DefaultAPIHost, "repos/example-org/widget/pulls")
	if err != nil {
		t.Fatal(err)
	}
	m := f.mintsSoFar()
	if len(m) != 2 || m[0].body != readMintBody || m[1].body != readActionsMintBody || ptok == atok {
		t.Fatalf("mints %+v (tokens %q, %q), want the plain literal then the actions literal, two tokens", m, ptok, atok)
	}
	if again, _, _ := app.Credential(ctx, plain, DefaultAPIHost, "repos/example-org/widget/pulls"); again != ptok {
		t.Fatalf("a cached plain request got %q, want %q", again, ptok)
	}
	if again, _, _ := app.Credential(ctx, withActions, DefaultAPIHost, "repos/example-org/widget/pulls"); again != atok {
		t.Fatalf("a cached actions request got %q, want %q", again, atok)
	}
	if n := len(f.mintsSoFar()); n != 2 {
		t.Fatalf("%d mints after the cached requests, want 2", n)
	}
}
