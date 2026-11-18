package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/githubapp"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/podagent"
)

// TestTheOfficerKickoffNamesTheAppsConnector: the connector name the officer's
// first message tells it to request is the GitHub App's own connector name.
func TestTheOfficerKickoffNamesTheAppsConnector(t *testing.T) {
	if officers.GitHubConnector != githubapp.ConnectorName {
		t.Fatalf("officers.GitHubConnector = %q, the App's connector is %q", officers.GitHubConnector, githubapp.ConnectorName)
	}
}

// TestTheOfficerKickoffTeachesTheRealProxyRecipe (E): the two ways the
// officer's first message says to read its repository are the real mechanism,
// spelled from their sources — the REST URL and header ProxyRecipe teaches
// every agent (for the github connector on GitHub's API host, the env names
// podagent exports), and git routed with ccd's own connect-git subcommand for
// GitHub's git host.
func TestTheOfficerKickoffTeachesTheRealProxyRecipe(t *testing.T) {
	o := officers.Officer{ID: "triage:example-web", MaxInstances: 1, Repo: "example-org/widget"}
	text := officers.KickoffText(o, 1)
	rest := "$" + podagent.EnvConnectURL + "/connect/" + githubapp.ConnectorName + "/" + githubapp.DefaultAPIHost + "/repos/example-org/widget/<path>"
	header := "Authorization: Bearer $" + podagent.EnvAgentToken
	recipe := ProxyRecipe(githubapp.ConnectorName, githubapp.DefaultAPIHost)
	for _, want := range []string{rest, header, "`ccd connect-git " + githubapp.ConnectorName + "`",
		"https://" + githubapp.DefaultGitHost + "/example-org/widget.git"} {
		if !strings.Contains(text, want) {
			t.Errorf("the officer kickoff does not carry %q:\n%s", want, text)
		}
	}
	// The pieces are ProxyRecipe's own (the URL prefix and the header).
	if !strings.Contains(recipe, "$"+podagent.EnvConnectURL+"/connect/"+githubapp.ConnectorName+"/"+githubapp.DefaultAPIHost+"/") ||
		!strings.Contains(recipe, header) {
		t.Fatalf("ProxyRecipe no longer teaches the URL/header the officer kickoff spells: %s", recipe)
	}
}

// TestAnOfficersConfiguredRepoIsItsAppTokensOneRepository is the officer's
// `repo` at its seam, end to end from the configuration:
//
//	MUSTER_OFFICERS (officers.Parse) → EnableOfficers' registry → the wake
//	loop's Start (officerFleet.Start → createAndDispatchAgent, a real agents
//	row) → the connector door (the instance's own token) → callerOf →
//	connectors.Caller.Repo → githubapp.App.Credential → the mint.
//
// In read mode (MUSTER_GITHUB_APP_ACCESS=read) the officer with
// "repo":"example-org/widget" files its `github` request, the owner approves
// it, and a clone of example-org/widget through muster's real proxy succeeds
// with a token minted for EXACTLY that one repository with the four READ
// permissions. A clone of another repository is sent anonymously (no second
// mint), and an officer declared without a repo gets nothing minted at all.
// The instance row carries NO repository: nothing is checked out.
func TestAnOfficersConfiguredRepoIsItsAppTokensOneRepository(t *testing.T) {
	f := newConnFixture(t)
	up := newFakeGitHubUpstream(t)
	up.addRepo(t, "example-org/widget")
	up.addRepo(t, "example-org/other")

	const withRepo, noRepo = "release:seam-web", "triage:seam-web"
	raw := `[{"id":"` + withRepo + `","hq_url":"https://hq.example.invalid/api/v1","max_instances":2,"repo":"example-org/widget"},
	         {"id":"` + noRepo + `","hq_url":"https://hq.example.invalid/api/v1"}]`
	list, err := officers.Parse(raw, func(k string) string {
		if strings.HasPrefix(k, officers.TokenEnvPrefix) {
			return "hq-token-" + k
		}
		return ""
	}, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}

	srv, h := officerAppServer(t, f, up, githubapp.AccessRead)
	if _, err := srv.EnableOfficers(OfficersConfig{Officers: list, InstallSession: discardSession}); err != nil {
		t.Fatal(err)
	}
	op := map[string]string{"Cookie": sessionCookieFor(srv), "HX-Request": "true", "Origin": "http://example.com"}

	instance := func(id string, n int, tok string) agents.Agent {
		t.Helper()
		return startOfficerInstance(t, f, srv, id, n, tok)
	}
	const tok, tokNone = "officer-repo-agent-token-0123456789", "officer-norepo-agent-token-012345678"
	a := instance(withRepo, 1, tok)
	b := instance(noRepo, 1, tokNone)

	// No checkout: the row names no repository (a row repository is what
	// makes ccd clone it), yet the connector caller carries the configured one.
	if a.Repo != "" || b.Repo != "" {
		t.Fatalf("an officer instance row carries a repository (it would be checked out): %q %q", a.Repo, b.Repo)
	}
	if c := srv.callerOf(a); c.Repo != "example-org/widget" || c.AgentID != a.ID || c.AgentName != a.Name {
		t.Fatalf("callerOf(the officer with a repo) = %+v, want Repo example-org/widget for agent %d", c, a.ID)
	}
	if c := srv.callerOf(b); c.Repo != "" {
		t.Fatalf("callerOf(the officer without a repo) = %+v, want no Repo", c)
	}

	// Each files its github request with its own token; the owner approves both.
	approve := func(token string) {
		t.Helper()
		w := serveCall(t, h, "POST", "/agent/connectors/github/request", strings.NewReader(`{"reason":"read the repository"}`),
			map[string]string{"X-Muster-Token": token, "Content-Type": "application/json"})
		if w.Code != http.StatusAccepted && w.Code != http.StatusOK { // 200: already pending
			t.Fatalf("filing the request: %d %s", w.Code, w.Body)
		}
		var filed AgentConnectorView
		_ = json.Unmarshal(w.Body.Bytes(), &filed)
		if filed.Grant != connectors.StatusPending {
			t.Fatalf("an officer's request was %q at once; every officer grant needs an explicit decision", filed.Grant)
		}
		w = serveCall(t, h, "POST", "/ui/connector-requests/"+strconv.FormatInt(filed.GrantID, 10)+"/approve", nil, op)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "approved for github") {
			t.Fatalf("approve: %d %s", w.Code, w.Body)
		}
		if v := agentView(t, h, token); v.Grant != connectors.StatusApproved {
			t.Fatalf("after approval the agent sees %+v", v)
		}
	}
	// The officer with a repo files its request; the operator lists it and
	// approves it over MCP from a mirrored session (connector_requests_list,
	// connector_request_decide — the officers deployment's procedure). Both read
	// the officer's configured repository: the listing names it and says the
	// App's READ-ONLY token for it, never "NOTHING usable"; the decision carries
	// no warning and is NOT shared (an officer's approval is its own:
	// excludedFromSharing, PR #337 round 1).
	w := serveCall(t, h, "POST", "/agent/connectors/github/request", strings.NewReader(`{"reason":"read the repository"}`),
		map[string]string{"X-Muster-Token": tok, "Content-Type": "application/json"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("filing the request: %d %s", w.Code, w.Body)
	}
	var filed AgentConnectorView
	_ = json.Unmarshal(w.Body.Bytes(), &filed)
	mirrored := toolCaller{asker: attention.Asker{MirrorHost: "ws", MirrorSession: focusSID, Label: "operator"}}
	var listed struct {
		Requests []connectorRequestView `json:"requests"`
	}
	if err := json.Unmarshal([]byte(srv.toolConnectorRequestsList(asUser(1), mirrored)), &listed); err != nil || len(listed.Requests) != 1 {
		t.Fatalf("connector_requests_list: %+v %v", listed, err)
	}
	if v := listed.Requests[0]; v.ID != filed.GrantID || v.Repo != "example-org/widget" ||
		!strings.Contains(v.Grants, "each request to example-org/widget (this agent's own repository)") || !strings.Contains(v.Grants, "READ ONLY") ||
		strings.Contains(v.Grants, "NOTHING usable") {
		t.Fatalf("connector_requests_list describes the officer's request as %+v", v)
	}
	var decided struct {
		Status   string `json:"status"`
		Warning  string `json:"warning"`
		Standing int64  `json:"standing_grant"`
		Shared   string `json:"shared"`
	}
	out := srv.toolConnectorRequestDecide(asUser(1), mirrored, `{"id":`+strconv.FormatInt(filed.GrantID, 10)+`,"decision":"approve"}`)
	if err := json.Unmarshal([]byte(out), &decided); err != nil || decided.Status != connectors.StatusApproved || decided.Warning != "" ||
		decided.Standing != 0 || decided.Shared != "" {
		t.Fatalf("connector_request_decide: %s", out)
	}
	if v := agentView(t, h, tok); v.Grant != connectors.StatusApproved {
		t.Fatalf("after approval the officer sees %+v", v)
	}
	// The officer's second instance asks too: it is on the same repository,
	// but nothing was shared, so its request waits for its own decision.
	const tok2 = "officer-repo-agent-token-2-012345678"
	instance(withRepo, 2, tok2)
	w = serveCall(t, h, "POST", "/agent/connectors/github/request", strings.NewReader(`{"reason":"read the repository"}`),
		map[string]string{"X-Muster-Token": tok2, "Content-Type": "application/json"})
	var second AgentConnectorView
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil || second.Grant != connectors.StatusPending {
		t.Fatalf("the second instance's request was not left pending for its owner: %d %s", w.Code, w.Body)
	}
	approve(tok2)
	approve(tokNone)

	muster := httptest.NewServer(h)
	t.Cleanup(muster.Close)
	proxy := muster.URL + "/connect/github/"
	env := gitTestEnv(t)
	clone := func(token, repo string) ([]byte, error) {
		c := exec.Command("git", "-c", "http."+proxy+".extraHeader=X-Muster-Token: "+token,
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
	if m := mints(); len(m) != 0 {
		t.Fatalf("minted before any request: %q", m)
	}

	// The officer's own repository: one read-only token, for it alone.
	if out, err := clone(tok, "example-org/widget"); err != nil {
		t.Fatalf("the officer could not read its configured repository: %v\n%s", err, out)
	}
	const wantMint = `{"repositories":["widget"],"permissions":{"contents":"read","pull_requests":"read","checks":"read","statuses":"read"}}`
	if m := mints(); len(m) != 1 || m[0] != wantMint {
		t.Fatalf("mints = %q, want exactly one, the read set for widget: %s", m, wantMint)
	}

	// Another repository: anonymous (the fake refuses a private repository
	// without a token), and nothing more is minted.
	up.mu.Lock()
	seenBefore := len(up.gitSeen)
	up.mu.Unlock()
	if out, err := clone(tok, "example-org/other"); err == nil {
		t.Fatalf("the officer read a repository it was not configured with:\n%s", out)
	}
	up.mu.Lock()
	otherSeen := append([]string(nil), up.gitSeen[seenBefore:]...)
	up.mu.Unlock()
	if len(otherSeen) == 0 {
		t.Fatal("control: the clone of example-org/other never reached the fake GitHub")
	}
	for _, s := range otherSeen {
		if !strings.HasPrefix(s, "/example-org/other.git/") || strings.TrimSpace(strings.TrimPrefix(s, strings.SplitN(s, " ", 2)[0])) != "" {
			t.Fatalf("a request for another repository carried a credential: %q", s)
		}
	}
	if m := mints(); len(m) != 1 {
		t.Fatalf("reading another repository minted: %q", m)
	}

	// The officer with no repo: approved, yet nothing is minted for it.
	if out, err := clone(tokNone, "example-org/widget"); err == nil {
		t.Fatalf("an officer with no repo read example-org/widget:\n%s", out)
	}
	if m := mints(); len(m) != 1 {
		t.Fatalf("an officer with no repo got a token minted: %q", m)
	}
	// ...and the refusal is the App's own (no repository), at muster's proxy.
	w = serveCall(t, h, "GET", "/connect/github/github.fake.test:"+up.port()+"/example-org/widget.git/info/refs?service=git-upload-pack", nil,
		map[string]string{"X-Muster-Token": tokNone})
	if w.Code == http.StatusOK || !strings.Contains(w.Body.String(), "dispatched without one") {
		t.Fatalf("the no-repo officer's request was not refused by the App: %d %s", w.Code, w.Body)
	}
}

// officerAppServer is appServerAccess plus what the officer runtime needs: an
// agent provisioner and the claude-code kind.
func officerAppServer(t *testing.T, f *connFixture, up *fakeGitHubUpstream, access githubapp.Access) (*Server, http.Handler) {
	t.Helper()
	_, _, app := appServerAccess(t, f, up, access)
	s := New(nil, AuthConfig{HookToken: connTestHookToken, ServiceToken: connTestServiceToken, UIPassword: connTestPassword},
		log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: agents.NewPG(f.pool), Connectors: connectors.WithVirtual(f.store, app, authz.IsAdmin),
		GitHubApp: app, ConnectorPolicy: fakeTestPolicy(), Provisioner: stubProvisioner{}, Kinds: stubKinds{pick: "acct-one"}})
	return s, s.Handler()
}

// startOfficerInstance is the wake loop's own create of instance n of officer
// id on srv (officerFleet.Start: a real agents row, with no repository), given
// per-agent token tok as the provisioner would.
func startOfficerInstance(t *testing.T, f *connFixture, srv *Server, id string, n int, tok string) agents.Agent {
	t.Helper()
	ctx := dbtest.AsOperator(context.Background())
	o, err := srv.officerRT.reg.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	name := officers.InstanceName(o.ID, n)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(dbtest.AsOperator(context.Background()), `DELETE FROM agents WHERE name=$1`, name)
	})
	if err := (officerFleet{s: srv}).Start(ctx, o, n, officers.KickoffText(o, n)); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	a, err := srv.ext.Agents.GetByName(srv.officerPrincipal(ctx, o.Owner), name)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.ext.Agents.(*agents.PGStore).SetHooksToken(ctx, a.ID, tok); err != nil {
		t.Fatal(err)
	}
	a.HooksToken = tok
	return a
}
