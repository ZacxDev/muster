package githubapp

import (
	"context"
	"testing"
)

// gadgetReadMintBody is the one mint body a read repository may ever be minted
// with: that repository alone, and the four READ levels (GitHub adds metadata
// read implicitly). Pinned literally.
const gadgetReadMintBody = `{"repositories":["gadget"],"permissions":{"contents":"read","pull_requests":"read","checks":"read","statuses":"read"}}`

// TestAReadRepoGetsAReadOnlyTokenForThatRepository: a caller whose ReadRepos
// (an officer's `read_repos`) names example-org/gadget gets, for a request
// that targets gadget — in every shape targetRepo routes (git smart-HTTP, the
// REST API, any case, an escaped byte) — a token minted for gadget ALONE, with
// the read literal.
//
// 🔴 THE APP IS IN WRITE MODE AND THE CALLER IS NOT ReadOnly, so neither of the
// two existing reasons to mint read applies: the read levels can only come
// from the read-repository rule itself. Control: the same caller's request to
// its own repository is minted with the WRITE literal, so the fixture is one
// where a mint that ignored the rule would visibly differ.
func TestAReadRepoGetsAReadOnlyTokenForThatRepository(t *testing.T) {
	app, f, _ := newTestAppAccess(t, AccessWrite)
	ctx := context.Background()
	me := agent(7, "example-org/widget")
	me.ReadRepos = []string{"example-org/gadget"}

	first := ""
	for _, tc := range []struct{ host, rest string }{
		{DefaultGitHost, "example-org/gadget.git/info/refs"},
		{DefaultGitHost, "Example-Org/Gadget/git-upload-pack"},
		{DefaultAPIHost, "repos/example-org/gadget/pulls"},
		{DefaultAPIHost, "REPOS/Example-Org/GADGET/contents/README.md"},
		{DefaultAPIHost, "repos/example-org/gad%67et/commits"},
	} {
		tok, anon, err := app.Credential(ctx, me, tc.host, tc.rest)
		if err != nil || anon || tok == "" {
			t.Fatalf("%s/%s: tok=%q anon=%v err=%v, want a token", tc.host, tc.rest, tok, anon, err)
		}
		if first == "" {
			first = tok
		} else if tok != first {
			t.Fatalf("%s/%s: a second token %q for the same read repository (cache key), want %q", tc.host, tc.rest, tok, first)
		}
	}
	m := f.mintsSoFar()
	if len(m) != 1 || m[0].body != gadgetReadMintBody || m[0].token != first {
		t.Fatalf("mints = %+v, want exactly one, the read literal for gadget:\n%s", m, gadgetReadMintBody)
	}

	// Control: the caller's own repository is minted in the App's (write) mode,
	// as it always was, and is a different token.
	own, _, err := app.Credential(ctx, me, DefaultGitHost, "example-org/widget.git/info/refs")
	if err != nil {
		t.Fatal(err)
	}
	m = f.mintsSoFar()
	if len(m) != 2 || m[1].body != writeMintBody || own == first {
		t.Fatalf("control: the own-repository mint = %+v, want the write literal for widget", m)
	}
	// A path naming no repository is the own repository's token, never the read
	// repository's.
	if tok, anon, err := app.Credential(ctx, me, DefaultAPIHost, "graphql"); err != nil || anon || tok != own {
		t.Fatalf("graphql: tok=%q anon=%v err=%v, want the own repository's token %q", tok, anon, err, own)
	}
}

// TestAReadRepoIsReadOnlyForAReadOnlyCallerInEitherMode: an officer instance
// (ReadOnly) on a read-mode and on a write-mode App gets the read literal for
// its read repository — the rule never yields write, whatever the mode.
func TestAReadRepoIsReadOnlyForAReadOnlyCallerInEitherMode(t *testing.T) {
	for _, mode := range []Access{AccessRead, AccessWrite} {
		app, f, _ := newTestAppAccess(t, mode)
		me := agent(7, "example-org/widget")
		me.ReadOnly = true
		me.ReadRepos = []string{"example-org/gadget"}
		if _, _, err := app.Credential(context.Background(), me, DefaultAPIHost, "repos/example-org/gadget/pulls/1/files"); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if m := f.mintsSoFar(); len(m) != 1 || m[0].body != gadgetReadMintBody {
			t.Fatalf("%s mode: mints = %+v, want the read literal for gadget", mode, m)
		}
	}
}

// TestARepositoryOutsideTheSetIsStillAnonymous: with ReadRepos set, a request
// to any repository that is neither the caller's own nor one of its read
// repositories is still forwarded anonymously and mints nothing — the rule
// widens exactly the listed set. Positive control: the listed one mints.
func TestARepositoryOutsideTheSetIsStillAnonymous(t *testing.T) {
	app, f, _ := newTestApp(t)
	me := agent(7, "example-org/widget")
	me.ReadRepos = []string{"example-org/gadget"}
	for _, tc := range []struct{ host, rest string }{
		{DefaultGitHost, "example-org/gizmo.git/info/refs"},
		{DefaultGitHost, "other-org/gadget.git/git-upload-pack"},
		{DefaultAPIHost, "repos/example-org/gadgets/pulls"},
		{DefaultAPIHost, "repos/example-org/gadge/pulls"},
		{DefaultAPIHost, "repos/other-org/gadget"},
	} {
		tok, anon, err := app.Credential(context.Background(), me, tc.host, tc.rest)
		if err != nil || !anon || tok != "" {
			t.Errorf("%s/%s: tok=%q anon=%v err=%v, want anonymous with no token", tc.host, tc.rest, tok, anon, err)
		}
	}
	if n := len(f.mintsSoFar()); n != 0 {
		t.Fatalf("%d token(s) minted for repositories outside the set", n)
	}
	if tok, anon, err := app.Credential(context.Background(), me, DefaultAPIHost, "repos/example-org/gadget"); err != nil || anon || tok == "" {
		t.Fatalf("positive control: the read repository: tok=%q anon=%v err=%v", tok, anon, err)
	}
	// A caller WITHOUT ReadRepos is unchanged: gadget is anonymous for it.
	plain := agent(8, "example-org/widget")
	if tok, anon, err := app.Credential(context.Background(), plain, DefaultAPIHost, "repos/example-org/gadget"); err != nil || !anon || tok != "" {
		t.Fatalf("a caller with no read repositories: tok=%q anon=%v err=%v, want anonymous", tok, anon, err)
	}
}
