package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/githubapp"
	"github.com/ZacxDev/muster/internal/officers"
)

// TestAnOfficersReadReposAreReadOnlyThroughOneApproval is `read_repos` at its
// seam, end to end from the configuration:
//
//	MUSTER_OFFICERS (officers.Parse) → the registry → callerOf
//	(Caller.ReadRepos) → the approve card, connector_requests_list and
//	connector_request_decide → the recorded scope (Grant.Repo) → the real proxy
//	→ githubapp.App.Credential → the mint; then a config change under the
//	approved grant.
//
// 🔴 THE APP IS IN WRITE MODE (the default), so a read-only mint for a read
// repository can only come from the read_repos rule — not from a read-mode
// deployment. What it pins:
//
//   - every surface that says what approving grants lists the officer's repo
//     AND each read repository, each READ ONLY, and never "read and write";
//   - one approval serves them all: a clone of a read repository is minted a
//     token for THAT repository with the four READ levels;
//   - a repository outside {repo} ∪ read_repos is still anonymous, minting
//     nothing;
//   - the grant is recorded for the whole set, so declaring the same set in
//     another order and case leaves it approved, and changing the set puts it
//     back to pending.
func TestAnOfficersReadReposAreReadOnlyThroughOneApproval(t *testing.T) {
	f := newConnFixture(t)
	up := newFakeGitHubUpstream(t)
	for _, r := range []string{"example-org/widget", "example-org/gadget", "example-org/docs", "example-org/other"} {
		up.addRepo(t, r)
	}

	const id = "triage:readset-web"
	raw := `[{"id":"` + id + `","hq_url":"https://hq.example.invalid/api/v1","repo":"example-org/widget",
	         "read_repos":["Example-Org/Gadget","example-org/docs"]}]`
	list, err := officers.Parse(raw, func(k string) string {
		if strings.HasPrefix(k, officers.TokenEnvPrefix) {
			return "hq-token-" + k
		}
		return ""
	}, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	srv, h := officerAppServer(t, f, up, githubapp.AccessWrite)
	if _, err := srv.EnableOfficers(OfficersConfig{Officers: list, InstallSession: discardSession}); err != nil {
		t.Fatal(err)
	}
	op := map[string]string{"Cookie": sessionCookieFor(srv), "HX-Request": "true", "Origin": "http://example.com"}
	const tok = "officer-readset-agent-token-0123456"
	a := startOfficerInstance(t, f, srv, id, 1, tok)

	wantRead := []string{"example-org/docs", "example-org/gadget"}
	if c := srv.callerOf(a); c.Repo != "example-org/widget" || !reflect.DeepEqual(c.ReadRepos, wantRead) || !c.ReadOnly {
		t.Fatalf("callerOf = %+v, want Repo example-org/widget, ReadRepos %q, ReadOnly", c, wantRead)
	}

	w := serveCall(t, h, "POST", "/agent/connectors/github/request", strings.NewReader(`{"reason":"read the repositories"}`),
		map[string]string{"X-Muster-Token": tok, "Content-Type": "application/json"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("filing the request: %d %s", w.Code, w.Body)
	}
	var filed AgentConnectorView
	_ = json.Unmarshal(w.Body.Bytes(), &filed)

	// Every surface names the whole set, each READ ONLY.
	listsAll := func(where, text string) {
		t.Helper()
		for _, want := range []string{"each request to example-org/widget", "READ ONLY: Contents",
			"example-org/docs (READ ONLY)", "example-org/gadget (READ ONLY)", "other repositories are reached anonymously"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not carry %q:\n%s", where, want, text)
			}
		}
		if strings.Contains(text, "read and write") || strings.Contains(text, "NOTHING usable") {
			t.Errorf("%s claims write or nothing:\n%s", where, text)
		}
	}
	card := requestCard(t, serveCall(t, h, http.MethodGet, "/ui/connectors", nil, op).Body.String(), "github")
	// The grants paragraph alone: requestCard runs to the end of the page for
	// the last card, and the page's connector list states the write-mode App's
	// own scope ("read and write") for task agents.
	i := strings.Index(card, "data-request-grants")
	if i < 0 {
		t.Fatalf("the approve card has no grants line:\n%s", card)
	}
	grantsLine := card[i:]
	if j := strings.Index(grantsLine, "</p>"); j >= 0 {
		grantsLine = grantsLine[:j]
	}
	listsAll("the approve card", grantsLine)

	mirrored := toolCaller{asker: attention.Asker{MirrorHost: "ws", MirrorSession: focusSID, Label: "operator"}}
	var listed struct {
		Requests []connectorRequestView `json:"requests"`
	}
	if err := json.Unmarshal([]byte(srv.toolConnectorRequestsList(asUser(1), mirrored)), &listed); err != nil || len(listed.Requests) != 1 {
		t.Fatalf("connector_requests_list: %+v %v", listed, err)
	}
	if v := listed.Requests[0]; v.Repo != "example-org/widget" || !reflect.DeepEqual(v.ReadRepos, wantRead) {
		t.Fatalf("connector_requests_list: %+v, want repo example-org/widget and read_repos %q", v, wantRead)
	}
	listsAll("connector_requests_list", listed.Requests[0].Grants)

	out := srv.toolConnectorRequestDecide(asUser(1), mirrored, `{"id":`+strconv.FormatInt(filed.GrantID, 10)+`,"decision":"approve"}`)
	var decided struct {
		Status    string   `json:"status"`
		Grants    string   `json:"grants"`
		ReadRepos []string `json:"read_repos"`
		Standing  int64    `json:"standing_grant"`
	}
	if err := json.Unmarshal([]byte(out), &decided); err != nil || decided.Status != connectors.StatusApproved ||
		!reflect.DeepEqual(decided.ReadRepos, wantRead) || decided.Standing != 0 {
		t.Fatalf("connector_request_decide: %s", out)
	}
	listsAll("connector_request_decide", decided.Grants)

	// The approval is recorded for the whole set (connectors.RepoScope).
	var recorded string
	if err := f.pool.QueryRow(authz.System(f.ctx), `SELECT coalesce(repo, '') FROM connector_grants WHERE id = $1`, filed.GrantID).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	const wantScope = "example-org/widget (+ read: example-org/docs, example-org/gadget)"
	if recorded != wantScope {
		t.Fatalf("the grant is recorded for %q, want %q", recorded, wantScope)
	}

	muster := httptest.NewServer(h)
	t.Cleanup(muster.Close)
	proxy := muster.URL + "/connect/github/"
	env := gitTestEnv(t)
	clone := func(repo string) ([]byte, error) {
		c := exec.Command("git", "-c", "http."+proxy+".extraHeader=X-Muster-Token: "+tok,
			"-c", "http."+proxy+".followRedirects=false",
			"clone", "-q", proxy+"github.fake.test:"+up.port()+"/"+repo+".git", filepath.Join(t.TempDir(), "checkout"))
		c.Env = env
		return c.CombinedOutput()
	}
	mints := func() []string {
		up.mu.Lock()
		defer up.mu.Unlock()
		return append([]string(nil), up.mints...)
	}

	// A read repository: one token, for it alone, READ — in a write-mode App.
	if out, err := clone("example-org/gadget"); err != nil {
		t.Fatalf("the officer could not read its read repository: %v\n%s", err, out)
	}
	const wantGadget = `{"repositories":["gadget"],"permissions":{"contents":"read","pull_requests":"read","checks":"read","statuses":"read"}}`
	if m := mints(); len(m) != 1 || m[0] != wantGadget {
		t.Fatalf("mints = %q, want exactly one, the read set for gadget: %s", m, wantGadget)
	}

	// Outside the set: anonymous, and nothing more is minted.
	if out, err := clone("example-org/other"); err == nil {
		t.Fatalf("the officer read a repository outside repo and read_repos:\n%s", out)
	}
	if m := mints(); len(m) != 1 {
		t.Fatalf("reading a repository outside the set minted: %q", m)
	}

	// The same set, reordered and re-cased (the registry is built by hand so
	// RepoScope's own canonical form is what is under test, not Parse's): the
	// grant stays approved.
	swap := func(read []string) {
		o := list[0]
		o.ReadRepos = read
		srv.officerRT.reg = officers.NewRegistry([]officers.Officer{o})
	}
	swap([]string{"example-org/gadget", "EXAMPLE-ORG/Docs"})
	if v := agentView(t, h, tok); v.Grant != connectors.StatusApproved {
		t.Fatalf("a reordered read_repos re-opened the grant: %+v", v)
	}
	// Another set: the grant is pending again until the owner approves it.
	swap([]string{"example-org/gadget"})
	if v := agentView(t, h, tok); v.Grant != connectors.StatusPending {
		t.Fatalf("a changed read_repos left the grant %q, want pending", v.Grant)
	}
}
