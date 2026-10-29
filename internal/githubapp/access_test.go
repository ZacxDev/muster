package githubapp

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The two agent-mint bodies, LITERALLY. writeMintBody is byte-identical to the
// body muster minted before the access mode existed (it is the same literal
// TestTheAgentMintCarriesExactlyItsOneRepositoryAndFourPermissions pins).
const (
	writeMintBody = `{"repositories":["widget"],"permissions":{"contents":"write","pull_requests":"write","checks":"read","statuses":"read"}}`
	readMintBody  = `{"repositories":["widget"],"permissions":{"contents":"read","pull_requests":"read","checks":"read","statuses":"read"}}`
)

// TestReadModeMintsExactlyFourReadPermissions pins the read-mode agent mint's
// body literally: the agent's one repository and contents, pull_requests,
// checks and statuses all `read` — nothing else, nothing wider. A mutant that
// leaves either write level in place (or adds a permission) changes these
// bytes.
func TestReadModeMintsExactlyFourReadPermissions(t *testing.T) {
	app, f, _ := newTestAppAccess(t, AccessRead)
	tok, _, err := app.Credential(context.Background(), agent(7, "example-org/widget"), DefaultGitHost, "example-org/widget.git/info/refs")
	if err != nil {
		t.Fatal(err)
	}
	m := f.mintsSoFar()
	if len(m) != 1 {
		t.Fatalf("%d mints, want 1", len(m))
	}
	if m[0].body != readMintBody {
		t.Fatalf("the read-mode mint request body is\n%s\nwant exactly\n%s", m[0].body, readMintBody)
	}
	if tok != m[0].token || m[0].installation != 42 {
		t.Fatalf("Credential returned %q on installation %d, want the minted %q on 42", tok, m[0].installation, m[0].token)
	}
	if app.Access() != AccessRead {
		t.Fatalf("Access() = %q, want read", app.Access())
	}
}

// TestWriteModeIsTheDefaultAndByteIdentical: Access unset and Access "write"
// both mint EXACTLY the body muster minted before the mode existed.
func TestWriteModeIsTheDefaultAndByteIdentical(t *testing.T) {
	for _, access := range []Access{"", AccessWrite} {
		app, f, _ := newTestAppAccess(t, access)
		if _, _, err := app.Credential(context.Background(), agent(7, "example-org/widget"), DefaultAPIHost, "repos/example-org/widget/pulls"); err != nil {
			t.Fatal(err)
		}
		m := f.mintsSoFar()
		if len(m) != 1 || m[0].body != writeMintBody {
			t.Fatalf("access %q: mints %+v, want exactly one with body\n%s", access, m, writeMintBody)
		}
		if app.Access() != AccessWrite {
			t.Fatalf("access %q: Access() = %q, want write", access, app.Access())
		}
	}
}

// TestReadModeNoMintPathAsksForWrite drives EVERY path that mints — an agent's
// git request, its API request, its pull-request listing, the repository
// listing and the default-branch read — in read mode, and asserts no mint body
// anywhere asks for a write level: the agent mints are the read literal and
// the listing mint stays metadata read. The positive control is that each
// path really minted (the counts).
func TestReadModeNoMintPathAsksForWrite(t *testing.T) {
	app, f, c := newTestAppAccess(t, AccessRead)
	ctx := context.Background()
	if _, _, err := app.Credential(ctx, agent(7, "example-org/widget"), DefaultGitHost, "example-org/widget.git/git-receive-pack"); err != nil {
		t.Fatal(err)
	}
	c.set(c.now().Add(2 * time.Hour)) // past expiry: the next agent call mints again
	if _, _, err := app.Credential(ctx, agent(7, "example-org/widget"), DefaultAPIHost, "repos/example-org/widget/pulls"); err != nil {
		t.Fatal(err)
	}
	// The fake answers the PR listing 404; the mint before it is what counts.
	_, _ = app.ListPullRequests(ctx, 8, "example-org/widget", "open")
	if _, err := app.Repos(ctx); err != nil {
		t.Fatal(err)
	}
	_, _ = app.DefaultBranch(ctx, "example-org/widget")
	m := f.mintsSoFar()
	var agentMints, listingMints int
	for _, x := range m {
		if strings.Contains(x.body, "write") {
			t.Errorf("read mode minted a WRITE token: %s", x.body)
		}
		switch x.body {
		case readMintBody:
			agentMints++
		case `{"permissions":{"metadata":"read"}}`:
			listingMints++
		default:
			t.Errorf("an unexpected mint body: %s", x.body)
		}
	}
	if agentMints != 3 || listingMints != 1 {
		t.Fatalf("positive control: %d agent mints (want 3: git, api, pull requests) and %d listing mints (want 1); all: %+v",
			agentMints, listingMints, m)
	}
}

// TestParseAccessAcceptsOnlyReadAndWrite: unset is write; read and write in
// any case; every near-miss is an error, never a fallback to the default.
func TestParseAccessAcceptsOnlyReadAndWrite(t *testing.T) {
	for in, want := range map[string]Access{"": AccessWrite, "  ": AccessWrite, "write": AccessWrite, "WRITE": AccessWrite,
		"read": AccessRead, " Read ": AccessRead} {
		got, err := ParseAccess(in)
		if err != nil || got != want {
			t.Errorf("ParseAccess(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"readonly", "read-only", "ro", "rw", "r", "w", "none", "true", "admin", "read,write"} {
		if got, err := ParseAccess(in); err == nil {
			t.Errorf("ParseAccess(%q) = %q, accepted", in, got)
		}
	}
}

// TestNewRefusesAnUnknownAccessMode: a Config built around ParseAccess (a
// caller that skipped it) cannot smuggle in a mode the mint does not know.
func TestNewRefusesAnUnknownAccessMode(t *testing.T) {
	_, err := New(Config{AppID: 1, PrivateKey: key(t), Access: "readonly", Accounts: []Account{{Login: "example-org", ID: 4200}}})
	if err == nil || !strings.Contains(err.Error(), "readonly") {
		t.Fatalf("New with access readonly: err = %v, want a refusal", err)
	}
}

// TestScopeTextFollowsTheMode pins the user-visible scope sentence of each
// mode literally. Write mode's is byte-identical to the constant it replaced;
// read mode's names no write level at all.
func TestScopeTextFollowsTheMode(t *testing.T) {
	if got, want := AccessWrite.Scope(), "Contents and Pull requests read and write, Checks and Commit statuses read"; got != want {
		t.Errorf("write scope = %q, want %q", got, want)
	}
	if got, want := AccessRead.Scope(), "READ ONLY: Contents, Pull requests, Checks and Commit statuses read — no push, no merge, no pull-request write"; got != want {
		t.Errorf("read scope = %q, want %q", got, want)
	}
	if strings.Contains(AccessRead.Scope(), "read and write") {
		t.Errorf("the read scope claims write: %q", AccessRead.Scope())
	}
}

// TestAReadOnlyCallerIsMintedReadInWriteMode: a caller marked ReadOnly (an
// officer instance, connectors.Caller.ReadOnly) gets the READ literal from a
// write-mode App; the same agent's write token, minted first and still cached,
// is never served to its read-only request (the mode is part of the cache
// key), and the read token is not served back to a write request either.
// Control: a caller not marked gets the write literal.
func TestAReadOnlyCallerIsMintedReadInWriteMode(t *testing.T) {
	app, f, _ := newTestAppAccess(t, AccessWrite)
	ctx := context.Background()
	write := agent(7, "example-org/widget")
	read := write
	read.ReadOnly = true
	wtok, _, err := app.Credential(ctx, write, DefaultGitHost, "example-org/widget.git/info/refs")
	if err != nil {
		t.Fatal(err)
	}
	rtok, _, err := app.Credential(ctx, read, DefaultGitHost, "example-org/widget.git/info/refs")
	if err != nil {
		t.Fatal(err)
	}
	m := f.mintsSoFar()
	if len(m) != 2 || m[0].body != writeMintBody || m[1].body != readMintBody {
		t.Fatalf("mints = %+v, want the write literal then the read literal", m)
	}
	if rtok != m[1].token || rtok == wtok {
		t.Fatalf("the read-only caller got %q, want the read token %q (the write one is %q)", rtok, m[1].token, wtok)
	}
	// Both are cached under their own mode: no further mint, each served its own.
	if again, _, _ := app.Credential(ctx, read, DefaultGitHost, "example-org/widget.git/info/refs"); again != rtok {
		t.Fatalf("a cached read-only request got %q, want %q", again, rtok)
	}
	if again, _, _ := app.Credential(ctx, write, DefaultGitHost, "example-org/widget.git/info/refs"); again != wtok {
		t.Fatalf("a cached write request got %q, want %q", again, wtok)
	}
	if n := len(f.mintsSoFar()); n != 2 {
		t.Fatalf("%d mints after the cached requests, want 2", n)
	}
}
