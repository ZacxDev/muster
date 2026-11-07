package connectors_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/connectors"
)

// Two pinned repositories at two DIFFERENT commits, and a third commit that is
// neither: a check that compared against the wrong pin, or against any pin,
// would pass a read it must refuse.
const (
	pinWidgetSHA = "3f9c1e0a7b2d4c6e8f0a1b3c5d7e9f1a2b4c6d8e"
	pinDocsSHA   = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
	laterSHA     = "9e8d7c6b5a49382716f5e4d3c2b1a0f9e8d7c6b5"
	pinAPIHost   = "api.fake.test:4431"
)

var testPins = []connectors.RepoPin{
	{Repo: "Example-Org/Widget", SHA: pinWidgetSHA},
	{Repo: "example-org/docs", SHA: strings.ToUpper(pinDocsSHA)},
}

// TestCheckPinnedReadServesOnlyContentsAndTreesAtThePinnedCommit is the pinned
// read's allowlist, case by case: the two served shapes at exactly the pinned
// commit of a pinned repository pass, and everything that could show what
// came after it — git, history, pull requests, search, GraphQL, no ref,
// another ref, another repository, a write — is refused with ErrPinnedRead.
func TestCheckPinnedReadServesOnlyContentsAndTreesAtThePinnedCommit(t *testing.T) {
	for _, c := range []struct {
		name, method, host, rest, query string
		ok                              bool
	}{
		{"a file at the pin", "GET", pinAPIHost, "repos/example-org/widget/contents/deploy/rules.yaml", "ref=" + pinWidgetSHA, true},
		{"a file at the pin, HEAD", "HEAD", pinAPIHost, "repos/example-org/widget/contents/deploy/rules.yaml", "ref=" + pinWidgetSHA, true},
		{"the root listing at the pin", "GET", pinAPIHost, "repos/example-org/widget/contents", "ref=" + pinWidgetSHA, true},
		{"a directory listing with a trailing slash", "GET", pinAPIHost, "repos/example-org/widget/contents/deploy/", "ref=" + pinWidgetSHA, true},
		{"the repository named in another case, the sha upper-cased", "GET", strings.ToUpper(pinAPIHost), "repos/EXAMPLE-ORG/Widget/contents/a.txt", "ref=" + strings.ToUpper(pinWidgetSHA), true},
		{"the second repository at ITS pin", "GET", pinAPIHost, "repos/example-org/docs/contents/README.md", "ref=" + pinDocsSHA, true},
		{"the tree at the pin, recursive", "GET", pinAPIHost, "repos/example-org/widget/git/trees/" + pinWidgetSHA, "recursive=1", true},
		{"the tree at the pin", "GET", pinAPIHost, "repos/example-org/widget/git/trees/" + pinWidgetSHA, "", true},

		{"contents with no ref (the default branch)", "GET", pinAPIHost, "repos/example-org/widget/contents/a.txt", "", false},
		{"contents at a later commit", "GET", pinAPIHost, "repos/example-org/widget/contents/a.txt", "ref=" + laterSHA, false},
		{"contents at the OTHER repository's pin", "GET", pinAPIHost, "repos/example-org/widget/contents/a.txt", "ref=" + pinDocsSHA, false},
		{"contents at a branch name", "GET", pinAPIHost, "repos/example-org/widget/contents/a.txt", "ref=main", false},
		{"contents with ref twice", "GET", pinAPIHost, "repos/example-org/widget/contents/a.txt", "ref=" + pinWidgetSHA + "&ref=" + laterSHA, false},
		{"contents with another query key", "GET", pinAPIHost, "repos/example-org/widget/contents/a.txt", "ref=" + pinWidgetSHA + "&per_page=5", false},
		{"the tree at a later commit", "GET", pinAPIHost, "repos/example-org/widget/git/trees/" + laterSHA, "recursive=1", false},
		{"the tree at a branch", "GET", pinAPIHost, "repos/example-org/widget/git/trees/main", "recursive=1", false},
		{"the tree with another query key", "GET", pinAPIHost, "repos/example-org/widget/git/trees/" + pinWidgetSHA, "ref=x", false},
		{"a blob by id", "GET", pinAPIHost, "repos/example-org/widget/git/blobs/" + laterSHA, "", false},
		{"commits", "GET", pinAPIHost, "repos/example-org/widget/commits", "sha=" + pinWidgetSHA, false},
		{"the pinned commit itself", "GET", pinAPIHost, "repos/example-org/widget/commits/" + pinWidgetSHA, "", false},
		{"compare from the pin", "GET", pinAPIHost, "repos/example-org/widget/compare/" + pinWidgetSHA + "...main", "", false},
		{"pull requests", "GET", pinAPIHost, "repos/example-org/widget/pulls", "state=all", false},
		{"issues", "GET", pinAPIHost, "repos/example-org/widget/issues", "", false},
		{"branches", "GET", pinAPIHost, "repos/example-org/widget/branches", "", false},
		{"the repository's metadata", "GET", pinAPIHost, "repos/example-org/widget", "", false},
		{"a tarball at the pin", "GET", pinAPIHost, "repos/example-org/widget/tarball/" + pinWidgetSHA, "", false},
		{"search", "GET", pinAPIHost, "search/code", "q=repo:example-org/widget", false},
		{"graphql", "POST", pinAPIHost, "graphql", "", false},
		{"a repository that is not pinned", "GET", pinAPIHost, "repos/example-org/other/contents/a.txt", "ref=" + pinWidgetSHA, false},
		{"git over the git host", "GET", "github.fake.test:4431", "example-org/widget.git/info/refs", "service=git-upload-pack", false},
		{"the contents shape on the git host", "GET", "github.fake.test:4431", "repos/example-org/widget/contents/a.txt", "ref=" + pinWidgetSHA, false},
		{"a write at the pin", "PUT", pinAPIHost, "repos/example-org/widget/contents/a.txt", "ref=" + pinWidgetSHA, false},
		{"a dot-dot escape out of contents", "GET", pinAPIHost, "repos/example-org/widget/contents/..%2F..%2Fpulls", "ref=" + pinWidgetSHA, false},
		{"a double-encoded escape", "GET", pinAPIHost, "repos/example-org/widget/contents/%252e%252e/pulls", "ref=" + pinWidgetSHA, false},
		{"an empty segment", "GET", pinAPIHost, "repos//widget/contents/a.txt", "ref=" + pinWidgetSHA, false},
	} {
		err := connectors.CheckPinnedRead(c.method, pinAPIHost, c.host, c.rest, c.query, testPins)
		switch {
		case c.ok && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case !c.ok && err == nil:
			t.Errorf("%s: %s %s/%s?%s was SERVED, want a refusal", c.name, c.method, c.host, c.rest, c.query)
		case !c.ok && !errors.Is(err, connectors.ErrPinnedRead):
			t.Errorf("%s: refusal %v does not wrap ErrPinnedRead", c.name, err)
		}
	}
}

// TestCheckPinnedReadRefusesEverythingWithNoPins: an item that pins nothing
// serves nothing, not even a read at some commit.
func TestCheckPinnedReadRefusesEverythingWithNoPins(t *testing.T) {
	err := connectors.CheckPinnedRead("GET", pinAPIHost, pinAPIHost, "repos/example-org/widget/contents/a.txt", "ref="+pinWidgetSHA, nil)
	if !errors.Is(err, connectors.ErrPinnedRead) || !strings.Contains(err.Error(), "pins no repository") {
		t.Fatalf("no pins: %v, want the pinned-read refusal naming that nothing is pinned", err)
	}
}

// TestARefusalNamesWhatIsServed: the agent can correct its request from the
// refusal alone — it names both shapes and every pin.
func TestARefusalNamesWhatIsServed(t *testing.T) {
	err := connectors.CheckPinnedRead("GET", pinAPIHost, pinAPIHost, "repos/example-org/widget/commits", "", testPins)
	if err == nil {
		t.Fatal("commits was served")
	}
	for _, want := range []string{"contents/<path>?ref=<sha>", "git/trees/<sha>?recursive=1",
		"example-org/widget@" + pinWidgetSHA, "example-org/docs@" + pinDocsSHA} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%v", want, err)
		}
	}
}

// TestThePinnedProxyRefusesBeforeTheUpstreamAndAudits is the policy at the
// proxy's own door (Proxy.Serve with Caller.ReadPin): an approved caller's
// read at the pin reaches the upstream; a read at a later commit, git-style
// history and a resolver failure are each a 403 carrying ErrPinnedRead's
// text, never reach the upstream, and are audited as refused. A request to
// another connector is not the policy's.
func TestThePinnedProxyRefusesBeforeTheUpstreamAndAudits(t *testing.T) {
	f := newFixture(t, connectors.SchemeBasic)
	resolveErr := error(nil)
	pinned := f.agentA
	pinned.ReadPin = &connectors.ReadPin{Connector: "svc", APIHost: f.host, Pins: func(context.Context) ([]connectors.RepoPin, error) {
		if resolveErr != nil {
			return nil, resolveErr
		}
		return testPins, nil
	}}
	f.approve(t, pinned)
	f.agentA = pinned

	if w := f.do(t, "GET", "/connect/svc/"+f.host+"/repos/example-org/widget/contents/a.txt?ref="+pinWidgetSHA, nil); w.Code != http.StatusOK {
		t.Fatalf("a read at the pin: %d %s", w.Code, w.Body)
	}
	if got := f.up.last(t).URL.RawQuery; got != "ref="+pinWidgetSHA {
		t.Fatalf("the upstream saw query %q", got)
	}
	served := f.up.hits.Load()

	refused := func(name, path string) {
		t.Helper()
		w := f.do(t, "GET", path, nil)
		var body connectors.RefusalBody
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != http.StatusForbidden || !strings.Contains(body.Error, connectors.ErrPinnedRead.Error()) {
			t.Errorf("%s: %d %s, want 403 with the pinned-read refusal", name, w.Code, w.Body)
		}
		if n := f.up.hits.Load(); n != served {
			t.Errorf("%s reached the upstream (%d hits, want %d)", name, n, served)
		}
	}
	refused("a later commit", "/connect/svc/"+f.host+"/repos/example-org/widget/contents/a.txt?ref="+laterSHA)
	refused("history", "/connect/svc/"+f.host+"/repos/example-org/widget/commits")
	resolveErr = errors.New("you hold no claimed item")
	refused("a resolver failure", "/connect/svc/"+f.host+"/repos/example-org/widget/contents/a.txt?ref="+pinWidgetSHA)

	list, err := f.store.ListAudit(f.ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range list {
		if e.Kind == "proxy" && e.AgentID == pinned.AgentID && e.Status == http.StatusForbidden && e.Detail == "pinned read refused" {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("%d pinned refusals audited, want 3", n)
	}

	// Another connector is not the policy's: a pin on "other" leaves svc open.
	resolveErr = nil
	pinned.ReadPin = &connectors.ReadPin{Connector: "other", APIHost: f.host, Pins: pinned.ReadPin.Pins}
	f.agentA = pinned
	if w := f.do(t, "GET", "/connect/svc/"+f.host+"/repos/example-org/widget/commits", nil); w.Code != http.StatusOK {
		t.Fatalf("a pin on another connector refused svc: %d %s", w.Code, w.Body)
	}
}
