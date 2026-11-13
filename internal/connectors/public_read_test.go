package connectors_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
)

// The standing public read (public_read.go) at the proxy, with no database:
// the store below PANICS on every grant call (its embedded nil Store), so a
// standing read that read, filed or touched a grant fails the test loudly.

// noGrantStore is a Store whose only working method is Audit; it counts the
// grant lookups and requests it is asked for when ordinary is set.
type noGrantStore struct {
	connectors.Store
	ordinary bool
	mu       sync.Mutex
	lookups  int
	filed    int
	audits   []connectors.AuditEntry
}

func (s *noGrantStore) Audit(_ context.Context, e connectors.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audits = append(s.audits, e)
	return nil
}

func (s *noGrantStore) LatestGrant(context.Context, int64, string) (connectors.Grant, error) {
	if !s.ordinary {
		panic("a standing read looked up a grant")
	}
	s.mu.Lock()
	s.lookups++
	s.mu.Unlock()
	return connectors.Grant{}, connectors.ErrNotFound
}

func (s *noGrantStore) RequestGrant(_ context.Context, agentID int64, connector, _ string) (connectors.Grant, bool, error) {
	if !s.ordinary {
		panic("a standing read filed a grant")
	}
	s.mu.Lock()
	s.filed++
	s.mu.Unlock()
	return connectors.Grant{ID: 9, AgentID: agentID, Connector: connector, Status: connectors.StatusPending}, true, nil
}

// fakeGitHub answers GET /repos/<owner>/<name> (no Authorization) with meta
// (404 when absent), and every other request 200 "served"; it records each
// request as "METHOD host path?query auth".
type fakeGitHub struct {
	mu   sync.Mutex
	meta map[string]string
	seen []string
	// metaCalls counts the metadata (public-check) requests.
	metaCalls int
	// fail answers the metadata request with this status instead.
	fail int
	// moved answers the metadata request of these repositories 301.
	moved map[string]bool
}

func (g *fakeGitHub) RoundTrip(r *http.Request) (*http.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
	}
	answer := func(code int, body string) *http.Response {
		return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: r}
	}
	segs := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if r.URL.Host == connectors.GitHubAPIHost && len(segs) == 3 && segs[0] == "repos" && r.Method == http.MethodGet &&
		r.Header.Get("X-GitHub-Api-Version") != "" {
		g.metaCalls++
		if r.Header.Get("Authorization") != "" {
			return answer(500, `{"message":"the public check carried a credential"}`), nil
		}
		if g.fail != 0 {
			return answer(g.fail, `{"message":"rate limited"}`), nil
		}
		if g.moved[segs[1]+"/"+segs[2]] {
			resp := answer(http.StatusMovedPermanently, `{"message":"Moved Permanently"}`)
			resp.Header.Set("Location", "https://api.github.com/repositories/1")
			return resp, nil
		}
		m, ok := g.meta[segs[1]+"/"+segs[2]]
		if !ok {
			return answer(404, `{"message":"Not Found"}`), nil
		}
		return answer(200, m), nil
	}
	q := ""
	if r.URL.RawQuery != "" {
		q = "?" + r.URL.RawQuery
	}
	g.seen = append(g.seen, r.Method+" "+r.URL.Host+" "+r.URL.Path+q+" "+r.Header.Get("Authorization"))
	return answer(200, `"served"`), nil
}

func (g *fakeGitHub) snapshot() (seen []string, meta int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.seen...), g.metaCalls
}

// agentMusterToken stands in for the calling agent's own muster token.
var agentMusterToken = strings.Repeat("t", 24)

const publicMeta = `{"full_name":"Example-Org/Docs","private":false,"visibility":"public"}`

// standingProxy is a Proxy over st and a fake GitHub where example-org/docs is
// public, with a settable clock.
func standingProxy(st connectors.Store) (*connectors.Proxy, *fakeGitHub, *time.Time) {
	gh := &fakeGitHub{meta: map[string]string{"example-org/docs": publicMeta}}
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	p := &connectors.Proxy{Store: st, Policy: testPolicy(), Now: func() time.Time { return now }}
	p.SetTransport(gh)
	return p, gh, &now
}

var standingCaller = connectors.Caller{AgentID: 7, AgentName: "officer-marketing-example-1",
	PublicReadRepos: []string{"example-org/docs", "example-org/secret"}, ReadOnly: true}

func serveStanding(p *connectors.Proxy, c connectors.Caller, method, host, rest, query string) *httptest.ResponseRecorder {
	target := "/connect/github/" + host + "/" + rest
	if query != "" {
		target += "?" + query
	}
	r := httptest.NewRequest(method, target, strings.NewReader(""))
	// The agent's own muster token, as the connect door receives it: it must
	// never reach GitHub (the assertions below see no Authorization at all).
	r.Header.Set("Authorization", "Bearer "+agentMusterToken)
	r.Header.Set("X-Muster-Token", agentMusterToken)
	w := httptest.NewRecorder()
	p.Serve(w, r, c, "github", host, rest)
	return w
}

// TestAStandingReadOfAPublicRepositoryIsServedAnonymouslyWithNoGrant: each
// READ shape (a REST GET and HEAD, git's info/refs for upload-pack and its
// fetch POST, in any case of the name) is forwarded with NO Authorization —
// neither the agent's own token nor any credential — and no grant is looked
// up, filed or touched (the store panics on any).
func TestAStandingReadOfAPublicRepositoryIsServedAnonymouslyWithNoGrant(t *testing.T) {
	st := &noGrantStore{}
	p, gh, _ := standingProxy(st)
	for _, c := range []struct{ method, host, rest, query, want string }{
		{"GET", "api.github.com", "repos/example-org/docs/contents/docs/officers.md", "ref=v0.221.0",
			"GET api.github.com /repos/example-org/docs/contents/docs/officers.md?ref=v0.221.0 "},
		{"HEAD", "API.GITHUB.COM", "repos/Example-Org/DOCS/readme", "", "HEAD api.github.com /repos/Example-Org/DOCS/readme "},
		{"GET", "github.com", "example-org/docs.git/info/refs", "service=git-upload-pack",
			"GET github.com /example-org/docs.git/info/refs?service=git-upload-pack "},
		{"POST", "github.com", "example-org/docs.git/git-upload-pack", "", "POST github.com /example-org/docs.git/git-upload-pack "},
	} {
		w := serveStanding(p, standingCaller, c.method, c.host, c.rest, c.query)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s/%s: %d %s, want 200", c.method, c.host, c.rest, w.Code, w.Body)
		}
		seen, _ := gh.snapshot()
		if len(seen) == 0 || seen[len(seen)-1] != c.want {
			t.Fatalf("%s %s/%s: GitHub saw %q, want %q (no Authorization)", c.method, c.host, c.rest, seen, c.want)
		}
	}
	if _, meta := gh.snapshot(); meta != 1 {
		t.Fatalf("the public check ran %d times for 4 reads within its TTL, want 1", meta)
	}
	if n := len(st.audits); n != 4 || !strings.Contains(st.audits[0].Detail, "standing public read of example-org/docs") {
		t.Fatalf("audits = %+v, want 4 naming the standing read", st.audits)
	}
}

// TestAStandingReadRefusesAWrite: every write shape aimed at a declared PUBLIC
// repository is refused 403 by muster — never forwarded, and never handed to
// the ordinary path (the store panics on a grant call).
func TestAStandingReadRefusesAWrite(t *testing.T) {
	p, gh, _ := standingProxy(&noGrantStore{})
	for _, c := range []struct{ method, host, rest, query string }{
		{"POST", "api.github.com", "repos/example-org/docs/issues", ""},
		{"PUT", "api.github.com", "repos/example-org/docs/contents/README.md", ""},
		{"PATCH", "api.github.com", "repos/example-org/docs", ""},
		{"DELETE", "api.github.com", "repos/example-org/docs/git/refs/heads/main", ""},
		{"POST", "api.github.com", "repos/example-org/docs/git/refs", ""},
		{"POST", "github.com", "example-org/docs.git/git-receive-pack", ""},
		{"GET", "github.com", "example-org/docs.git/info/refs", "service=git-receive-pack"},
		{"GET", "github.com", "example-org/docs.git/info/refs", "service=git-upload-pack&service=git-receive-pack"},
		{"GET", "github.com", "example-org/docs.git/git-receive-pack", ""},
		{"POST", "github.com", "example-org/docs/sub/git-upload-pack", ""},
		{"OPTIONS", "api.github.com", "repos/example-org/docs", ""},
	} {
		w := serveStanding(p, standingCaller, c.method, c.host, c.rest, c.query)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "serves reads only") {
			t.Fatalf("%s %s/%s?%s: %d %s, want 403 reads only", c.method, c.host, c.rest, c.query, w.Code, w.Body)
		}
	}
	if seen, meta := gh.snapshot(); len(seen) != 0 || meta != 0 {
		t.Fatalf("a refused write reached GitHub: %q (metadata calls %d)", seen, meta)
	}
}

// TestAStandingReadRefusesARepositoryGitHubDoesNotShowAsPublic: a declared
// repository is served only when GitHub's anonymous answer is 200 with that
// full name, private false and visibility public. Every other answer is a 403
// naming why, and the read itself never reaches GitHub.
func TestAStandingReadRefusesARepositoryGitHubDoesNotShowAsPublic(t *testing.T) {
	for _, c := range []struct{ name, meta, why string }{
		{"absent or private (404)", "", "private, or does not exist"},
		{"private true", `{"full_name":"example-org/secret","private":true,"visibility":"private"}`, "says it is private"},
		{"private missing", `{"full_name":"example-org/secret","visibility":"public"}`, "says it is private"},
		{"internal", `{"full_name":"example-org/secret","private":false,"visibility":"internal"}`, `visibility is \"internal\"`},
		{"another name", `{"full_name":"example-org/elsewhere","private":false,"visibility":"public"}`, `names it \"example-org/elsewhere\"`},
		{"renamed (301, not followed)", "moved", "redirects it elsewhere"},
	} {
		p, gh, _ := standingProxy(&noGrantStore{})
		switch c.meta {
		case "":
		case "moved":
			gh.moved = map[string]bool{"example-org/secret": true}
			gh.meta["example-org/secret"] = publicMeta
		default:
			gh.meta["example-org/secret"] = c.meta
		}
		w := serveStanding(p, standingCaller, "GET", "api.github.com", "repos/example-org/secret/contents/x", "")
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "PUBLIC repositories only") ||
			!strings.Contains(w.Body.String(), c.why) {
			t.Fatalf("%s: %d %s, want 403 naming %q", c.name, w.Code, w.Body, c.why)
		}
		if seen, meta := gh.snapshot(); len(seen) != 0 || meta != 1 {
			t.Fatalf("%s: the refused read reached GitHub (%q) or the check ran %d times", c.name, seen, meta)
		}
	}
}

// TestTheStandingReadIsLimitedToTheDeclaredRepositories: a request for a
// repository the caller does not declare — or a path that names none, or a
// caller with no PublicReadRepos at all (every task agent and HQ officer) —
// is NOT a standing read: it takes the ordinary path, which looks up and files
// a grant, and nothing reaches GitHub.
func TestTheStandingReadIsLimitedToTheDeclaredRepositories(t *testing.T) {
	for _, c := range []struct {
		name             string
		caller           connectors.Caller
		host, rest, conn string
	}{
		{"another repository", standingCaller, "api.github.com", "repos/example-org/other/contents/x", "github"},
		{"another repository by git", standingCaller, "github.com", "example-org/other.git/info/refs", "github"},
		{"a prefix of a declared one", standingCaller, "api.github.com", "repos/example-org/doc/contents/x", "github"},
		{"a path naming no repository", standingCaller, "api.github.com", "graphql", "github"},
		{"a traversal out of a declared one", standingCaller, "api.github.com", "repos/example-org/docs/../../other/x", "github"},
		{"another connector", standingCaller, "api.github.com", "repos/example-org/docs/contents/x", "svc"},
		{"no standing read", connectors.Caller{AgentID: 7, AgentName: "task-agent", ReadRepos: []string{"example-org/docs"}},
			"api.github.com", "repos/example-org/docs/contents/x", "github"},
	} {
		st := &noGrantStore{ordinary: true}
		p, gh, _ := standingProxy(st)
		r := httptest.NewRequest("GET", "/connect/"+c.conn+"/"+c.host+"/"+c.rest, nil)
		w := httptest.NewRecorder()
		p.Serve(w, r, c.caller, c.conn, c.host, c.rest)
		var body connectors.RefusalBody
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != http.StatusForbidden || body.Grant != connectors.StatusPending || st.lookups != 1 || st.filed != 1 {
			t.Fatalf("%s: %d %s lookups=%d filed=%d, want the ordinary path's pending grant", c.name, w.Code, w.Body, st.lookups, st.filed)
		}
		if seen, meta := gh.snapshot(); len(seen) != 0 || meta != 0 {
			t.Fatalf("%s: reached GitHub without a grant: %q (metadata %d)", c.name, seen, meta)
		}
	}
}

// TestTheStandingReadsPublicVerdictIsCachedThenRechecked: a public verdict is
// reused for 10 minutes and asked again after; a private one for a minute; a
// check GitHub could not answer is a 502, never cached.
func TestTheStandingReadsPublicVerdictIsCachedThenRechecked(t *testing.T) {
	p, gh, now := standingProxy(&noGrantStore{})
	read := func(repo string) int {
		return serveStanding(p, standingCaller, "GET", "api.github.com", "repos/"+repo+"/readme", "").Code
	}
	if read("example-org/docs") != 200 || read("example-org/docs") != 200 {
		t.Fatal("public reads refused")
	}
	*now = now.Add(9 * time.Minute)
	if read("example-org/docs") != 200 {
		t.Fatal("public read refused within the TTL")
	}
	if _, meta := gh.snapshot(); meta != 1 {
		t.Fatalf("checked %d times within 10 minutes, want 1", meta)
	}
	// The repository goes private: the next check after the TTL refuses it.
	delete(gh.meta, "example-org/docs")
	*now = now.Add(2 * time.Minute)
	if c := read("example-org/docs"); c != http.StatusForbidden {
		t.Fatalf("a repository made private is still served after the TTL: %d", c)
	}
	if _, meta := gh.snapshot(); meta != 2 {
		t.Fatalf("checked %d times, want 2", meta)
	}
	// Made public again: the private verdict holds for a minute, then not.
	gh.meta["example-org/docs"] = publicMeta
	*now = now.Add(30 * time.Second)
	if c := read("example-org/docs"); c != http.StatusForbidden {
		t.Fatalf("the private verdict was not reused within its minute: %d", c)
	}
	*now = now.Add(31 * time.Second)
	if c := read("example-org/docs"); c != http.StatusOK {
		t.Fatalf("the private verdict outlived its minute: %d", c)
	}
	// GitHub cannot answer the re-check (its anonymous rate limit): the last
	// PUBLIC verdict keeps serving, and the check is asked again each time.
	*now = now.Add(11 * time.Minute)
	gh.fail = http.StatusForbidden
	_, before := gh.snapshot()
	if c := read("example-org/docs"); c != http.StatusOK {
		t.Fatalf("a failed re-check of a repository last seen public: %d, want it still served", c)
	}
	if c := read("example-org/docs"); c != http.StatusOK {
		t.Fatalf("a second failed re-check: %d", c)
	}
	if _, after := gh.snapshot(); after != before+2 {
		t.Fatalf("a failed re-check was cached: %d checks, want %d", after, before+2)
	}
	// With no verdict yet, a failed check is a 502 (fail closed), uncached.
	gh.meta["example-org/secret"] = `{"full_name":"example-org/secret","private":false,"visibility":"public"}`
	if c := read("example-org/secret"); c != http.StatusBadGateway {
		t.Fatalf("a failed first check: %d, want 502", c)
	}
	gh.fail = 0
	if c := read("example-org/secret"); c != http.StatusOK {
		t.Fatalf("a failed first check was cached: %d", c)
	}
}

// TestGitHubTargetRepoIsTheAppsRule pins the shared rule literally, so the
// standing read and the App decide a path's repository the same way.
func TestGitHubTargetRepoIsTheAppsRule(t *testing.T) {
	for _, c := range []struct {
		host, rest, repo string
		scoped, err      bool
	}{
		{"github.com", "o/r.git/info/refs", "o/r", true, false},
		{"github.com", "o/r", "o/r", true, false},
		{"github.com", "o", "", false, true},
		{"api.github.com", "repos/o/r/pulls", "o/r", true, false},
		{"api.github.com", "REPOS/o/r", "o/r", true, false},
		{"api.github.com", "graphql", "", false, false},
		{"api.github.com", "repositories/1/x", "", false, true},
		{"api.github.com", "repos/o/r/../x", "", false, true},
		{"api.github.com", "%2Frepos/o/r", "", false, true},
		{"api.github.com", "repos/o/r%252Fx", "", false, true},
		{"elsewhere.com", "o/r", "", false, true},
	} {
		repo, scoped, err := connectors.GitHubTargetRepo("github.com", "api.github.com", c.host, c.rest)
		if repo != c.repo || scoped != c.scoped || (err != nil) != c.err {
			t.Errorf("GitHubTargetRepo(%s, %s) = %q %v %v", c.host, c.rest, repo, scoped, err)
		}
	}
}
