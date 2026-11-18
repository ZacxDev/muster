package api

import (
	"context"
	"encoding/json"
	"errors"
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
)

// PR #337 audit round 1, at the seam (PG, the real proxy, a fake GitHub):
//
//   - B: an officer's App token is ALWAYS read-only, even in write mode.
//   - C: an officer may request only `github`, and no standing (shared)
//     approval ever approves an officer's request.
//   - D: a grant records the repository it was approved for; an officer whose
//     configured repo then differs (another one, or none→some) has its grant
//     re-opened.

const (
	r1OfficerID   = "triage:r1-web"
	r1OfficerTok  = "r1-officer-agent-token-0123456789ab"
	r1TaskTok     = "r1-task-agent-token-0123456789abcde"
	r1TaskTok2    = "r1-task-agent-token-2-0123456789abc"
	r1RepoWidget  = "example-org/widget"
	r1RepoOther   = "example-org/other"
	r1StoredToken = "ghp_FAKE-officer-r1-stored-0123456789"
)

// r1Officers is MUSTER_OFFICERS with one officer whose repo is repo ("" none).
func r1Officers(t *testing.T, repo string) []officers.Officer {
	t.Helper()
	field := ""
	if repo != "" {
		field = `,"repo":"` + repo + `"`
	}
	list, err := officers.Parse(`[{"id":"`+r1OfficerID+`","hq_url":"https://hq.example.invalid/api/v1","max_instances":1`+field+`}]`,
		func(k string) string {
			if strings.HasPrefix(k, officers.TokenEnvPrefix) {
				return "hq-token-" + k
			}
			return ""
		}, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// officerServer is a Server over f with the officer runtime's needs (agent
// provisioner, claude-code kind) and the connector store conns, the GitHub App
// app when not nil.
func officerServer(t *testing.T, f *connFixture, conns connectors.Store, app *githubapp.App) (*Server, http.Handler) {
	t.Helper()
	s := New(nil, AuthConfig{HookToken: connTestHookToken, ServiceToken: connTestServiceToken, UIPassword: connTestPassword},
		log.New(io.Discard, "", 0))
	ext := Extensions{Agents: agents.NewPG(f.pool), Connectors: conns, ConnectorPolicy: fakeTestPolicy(),
		Provisioner: stubProvisioner{}, Kinds: stubKinds{pick: "acct-one"}}
	if app != nil {
		ext.GitHubApp = app
	}
	s.UseExtensions(ext)
	return s, s.Handler()
}

// enableOfficers (re)declares MUSTER_OFFICERS on srv — a restart with a new
// configuration, as far as the officer runtime is concerned.
func enableOfficers(t *testing.T, srv *Server, list []officers.Officer) {
	t.Helper()
	srv.officerRT = nil
	if _, err := srv.EnableOfficers(OfficersConfig{Officers: list, InstallSession: discardSession}); err != nil {
		t.Fatal(err)
	}
}

// requestConnector files agent tok's request for connector name, returning the
// HTTP status and the agent's view.
func requestConnector(t *testing.T, h http.Handler, tok, name string) (int, AgentConnectorView, string) {
	t.Helper()
	w := serveCall(t, h, "POST", "/agent/connectors/"+name+"/request", strings.NewReader(`{"reason":"r1"}`),
		map[string]string{"X-Muster-Token": tok, "Content-Type": "application/json"})
	var v AgentConnectorView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return w.Code, v, w.Body.String()
}

// approveOnTab approves request id on the Connectors tab, returning the page.
func approveOnTab(t *testing.T, srv *Server, h http.Handler, id int64) string {
	t.Helper()
	op := map[string]string{"Cookie": sessionCookieFor(srv), "HX-Request": "true", "Origin": "http://example.com"}
	w := serveCall(t, h, "POST", "/ui/connector-requests/"+strconv.FormatInt(id, 10)+"/approve", nil, op)
	if w.Code != http.StatusOK {
		t.Fatalf("approve %d: %d %s", id, w.Code, w.Body)
	}
	return w.Body.String()
}

// taskAgent seeds an ordinary (non-officer) agent with token tok on repo.
func taskAgent(t *testing.T, f *connFixture, name, tok, repo string) int64 {
	t.Helper()
	id := f.seedAgent(t, name, tok)
	if _, err := f.pool.Exec(f.ctx, `UPDATE agents SET repo=$2 WHERE id=$1`, id, repo); err != nil {
		t.Fatal(err)
	}
	return id
}

// cloneVia clones repo through muster's proxy (h) as agent tok, as ccd's git
// routing sends it.
func cloneVia(t *testing.T, h http.Handler, up *fakeGitHubUpstream, tok, repo string) ([]byte, error) {
	t.Helper()
	muster := httptest.NewServer(h)
	t.Cleanup(muster.Close)
	proxy := muster.URL + "/connect/github/"
	c := exec.Command("git", "-c", "http."+proxy+".extraHeader=X-Muster-Token: "+tok,
		"-c", "http."+proxy+".followRedirects=false",
		"clone", "-q", proxy+"github.fake.test:"+up.port()+"/"+repo+".git", filepath.Join(t.TempDir(), "checkout"))
	c.Env = gitTestEnv(t)
	return c.CombinedOutput()
}

func mintsOf(up *fakeGitHubUpstream) []string {
	up.mu.Lock()
	defer up.mu.Unlock()
	return append([]string(nil), up.mints...)
}

const (
	r1ReadMint  = `{"repositories":["widget"],"permissions":{"contents":"read","pull_requests":"read","checks":"read","statuses":"read"}}`
	r1WriteMint = `{"repositories":["widget"],"permissions":{"contents":"write","pull_requests":"write","checks":"read","statuses":"read"}}`
)

// TestAnOfficersAppTokenIsReadOnlyEvenInWriteMode (B): on a deployment whose
// MUSTER_GITHUB_APP_ACCESS is write (here: the default, unset), an officer's
// clone of its repository through the proxy is served by a token minted with
// the four READ permissions — and connector_requests_list describes its
// request as READ ONLY. Control, same App, same repository: a task agent's
// clone is minted with write, as before.
func TestAnOfficersAppTokenIsReadOnlyEvenInWriteMode(t *testing.T) {
	f := newConnFixture(t)
	up := newFakeGitHubUpstream(t)
	up.addRepo(t, r1RepoWidget)
	_, _, app := appServerAccess(t, f, up, "")
	if app.Access() != githubapp.AccessWrite {
		t.Fatalf("control: the App is in %q mode, want write", app.Access())
	}
	srv, h := officerServer(t, f, connectors.WithVirtual(f.store, app, authz.IsAdmin), app)
	enableOfficers(t, srv, r1Officers(t, r1RepoWidget))
	startOfficerInstance(t, f, srv, r1OfficerID, 1, r1OfficerTok)

	code, filed, body := requestConnector(t, h, r1OfficerTok, "github")
	if code != http.StatusAccepted {
		t.Fatalf("filing: %d %s", code, body)
	}
	mirrored := toolCaller{asker: attention.Asker{MirrorHost: "ws", MirrorSession: focusSID, Label: "operator"}}
	var listed struct {
		Requests []connectorRequestView `json:"requests"`
	}
	if err := json.Unmarshal([]byte(srv.toolConnectorRequestsList(asUser(1), mirrored)), &listed); err != nil || len(listed.Requests) != 1 {
		t.Fatalf("connector_requests_list: %+v %v", listed, err)
	}
	if g := listed.Requests[0].Grants; !strings.Contains(g, "READ ONLY") || strings.Contains(g, "read and write") {
		t.Fatalf("an officer's request in write mode is described as %q, want READ ONLY", g)
	}
	approveOnTab(t, srv, h, filed.GrantID)
	if out, err := cloneVia(t, h, up, r1OfficerTok, r1RepoWidget); err != nil {
		t.Fatalf("the officer's clone: %v\n%s", err, out)
	}
	if m := mintsOf(up); len(m) != 1 || m[0] != r1ReadMint {
		t.Fatalf("officer mints in write mode = %q, want exactly %s", m, r1ReadMint)
	}

	// Control: a task agent on the same repository, same App: write, as before.
	taskAgent(t, f, "r1-task-write", r1TaskTok, r1RepoWidget)
	_, tfiled, _ := requestConnector(t, h, r1TaskTok, "github")
	approveOnTab(t, srv, h, tfiled.GrantID)
	if out, err := cloneVia(t, h, up, r1TaskTok, r1RepoWidget); err != nil {
		t.Fatalf("the task agent's clone: %v\n%s", err, out)
	}
	if m := mintsOf(up); len(m) != 2 || m[1] != r1WriteMint {
		t.Fatalf("task agent mint in write mode = %q, want %s second", m, r1WriteMint)
	}
}

// TestAnOfficerMayUseOnlyTheGitHubConnector (C): an officer asking for any
// other connector its owner holds is refused with a clear error at every door
// — the request route (400), the connector_request tool, and the proxy (403,
// BEFORE it would file an implicit request) — and nothing is filed. A request
// filed before the rule (a row in the table) cannot be approved on the tab or
// over MCP. Control: a task agent's request for the same connector is filed.
func TestAnOfficerMayUseOnlyTheGitHubConnector(t *testing.T) {
	f := newConnFixture(t)
	if _, err := f.store.CreateConnector(f.ctx, connectors.NewConnector{Name: "chat", Hosts: []string{"api.chat.fake.test"},
		Scheme: connectors.SchemeBearer, Secret: r1StoredToken}); err != nil {
		t.Fatal(err)
	}
	srv, h := officerServer(t, f, f.store, nil)
	enableOfficers(t, srv, r1Officers(t, r1RepoWidget))
	off := startOfficerInstance(t, f, srv, r1OfficerID, 1, r1OfficerTok)

	const want = `officer-triage-r1-web-1 is an instance of officer triage:r1-web, and an officer may use only the "github" connector (to read its repository); "chat" was refused`
	code, _, body := requestConnector(t, h, r1OfficerTok, "chat")
	var refusal struct {
		Error string `json:"error"`
	}
	if code != http.StatusBadRequest || json.Unmarshal([]byte(body), &refusal) != nil || refusal.Error != want {
		t.Fatalf("request route: %d %s\nwant 400 carrying %q", code, body, want)
	}
	if out := srv.toolConnectorRequest(asUser(1), off, "chat", "r1"); jsonError(out) != want {
		t.Fatalf("connector_request tool: %s", out)
	}
	w := serveCall(t, h, "GET", "/connect/chat/api.chat.fake.test/v1/x", nil, map[string]string{"X-Muster-Token": r1OfficerTok})
	if w.Code != http.StatusForbidden || jsonError(w.Body.String()) != want {
		t.Fatalf("proxy: %d %s", w.Code, w.Body)
	}
	if _, err := f.store.LatestGrant(asUser(1), off.ID, "chat"); !errors.Is(err, connectors.ErrNotFound) {
		t.Fatalf("a refused officer request left a grant row (err %v)", err)
	}
	// github itself is still allowed (filed, pending).
	if code, v, body := requestConnector(t, h, r1OfficerTok, "github"); code != http.StatusAccepted || v.Grant != connectors.StatusPending {
		t.Fatalf("the officer's github request: %d %s", code, body)
	}

	// A legacy row: refused on the tab and over MCP, left pending.
	legacy, _, err := f.store.RequestGrant(asUser(1), off.ID, "chat", "filed before the rule")
	if err != nil {
		t.Fatal(err)
	}
	if page := approveOnTab(t, srv, h, legacy.ID); !strings.Contains(page, "may use only the &#34;github&#34; connector") &&
		!strings.Contains(page, `may use only the "github" connector`) {
		t.Fatalf("the tab approved (or did not explain refusing) an officer's chat grant:\n%s", page)
	}
	mirrored := toolCaller{asker: attention.Asker{MirrorHost: "ws", MirrorSession: focusSID, Label: "operator"}}
	if out := srv.toolConnectorRequestDecide(asUser(1), mirrored, `{"id":`+strconv.FormatInt(legacy.ID, 10)+`,"decision":"approve"}`); jsonError(out) != want+"; the request was not approved" {
		t.Fatalf("connector_request_decide: %s", out)
	}
	if g, err := f.store.GetGrant(asUser(1), legacy.ID); err != nil || g.Status != connectors.StatusPending {
		t.Fatalf("the legacy officer grant is %+v (err %v), want still pending", g, err)
	}

	// Control: a task agent may request chat.
	taskAgent(t, f, "r1-task-chat", r1TaskTok, r1RepoWidget)
	if code, v, body := requestConnector(t, h, r1TaskTok, "chat"); code != http.StatusAccepted || v.Grant != connectors.StatusPending {
		t.Fatalf("control: the task agent's chat request: %d %s", code, body)
	}
}

// TestAStandingApprovalNeverApprovesAnOfficer (C, the audit's exact case): the
// owner approves a task agent's request for a STORED github connector, which
// is shared as a standing grant keyed on the connector name alone. An officer
// instance asking for github afterwards is NOT approved from it — it waits for
// its own decision — while a second task agent asking is (control: the shared
// approval works). And the officer's own approval is never shared.
func TestAStandingApprovalNeverApprovesAnOfficer(t *testing.T) {
	f := newConnFixture(t)
	if _, err := f.store.CreateConnector(f.ctx, connectors.NewConnector{Name: "github", Hosts: []string{"github.fake.test"},
		Scheme: connectors.SchemeBearer, Secret: r1StoredToken}); err != nil {
		t.Fatal(err)
	}
	srv, h := officerServer(t, f, f.store, nil)
	enableOfficers(t, srv, r1Officers(t, r1RepoWidget))
	taskAgent(t, f, "r1-task-one", r1TaskTok, r1RepoWidget)
	_, first, _ := requestConnector(t, h, r1TaskTok, "github")
	approveOnTab(t, srv, h, first.GrantID)
	if sg, err := f.store.ListStandingGrants(asUser(1)); err != nil || len(sg) != 1 || sg[0].Connector != "github" || sg[0].Repo != "" {
		t.Fatalf("control: the task agent's approval was not shared as one stored-github standing grant: %+v %v", sg, err)
	}

	startOfficerInstance(t, f, srv, r1OfficerID, 1, r1OfficerTok)
	code, ov, body := requestConnector(t, h, r1OfficerTok, "github")
	if code != http.StatusAccepted || ov.Grant != connectors.StatusPending {
		t.Fatalf("the officer's request under a shared approval: %d %s — want it filed and PENDING", code, body)
	}
	if v := agentView(t, h, r1OfficerTok); v.Grant != connectors.StatusPending {
		t.Fatalf("the officer reads %+v, want pending", v)
	}
	// Asking again (the path that applies a standing grant to a waiting
	// request) still does not approve it.
	if _, again, _ := requestConnector(t, h, r1OfficerTok, "github"); again.Grant != connectors.StatusPending {
		t.Fatalf("asking again approved the officer from the shared approval: %+v", again)
	}

	// Control: another task agent IS approved from the shared approval.
	taskAgent(t, f, "r1-task-two", r1TaskTok2, r1RepoWidget)
	if _, tv, body := requestConnector(t, h, r1TaskTok2, "github"); tv.Grant != connectors.StatusApproved {
		t.Fatalf("control: the second task agent was not approved from the shared approval: %s", body)
	}

	// The officer's own approval is explicit, and not shared.
	before, err := f.store.ListStandingGrants(asUser(1))
	if err != nil {
		t.Fatal(err)
	}
	if page := approveOnTab(t, srv, h, ov.GrantID); !strings.Contains(page, "officer-triage-r1-web-1 approved for github.") ||
		strings.Contains(page, "approved for github. Shared") {
		t.Fatalf("the officer's approval notice is not the unshared one:\n%s", page)
	}
	after, err := f.store.ListStandingGrants(asUser(1))
	if err != nil || len(after) != len(before) {
		t.Fatalf("standing grants %d -> %d (err %v): an officer's approval created one", len(before), len(after), err)
	}
}

// TestAnOfficersGrantIsReopenedWhenItsRepoChanges (D): the officer's github
// grant, approved for example-org/widget, is put back to pending when its
// configured repo becomes example-org/other — the clone of the new repository
// mints nothing until the owner approves again, and the audit says why. An
// officer approved with NO repo (the card said "approving grants NOTHING
// usable") is re-opened when it is given one. Control: re-declaring the same
// repo leaves the approval alone, and a task agent's grant is untouched.
func TestAnOfficersGrantIsReopenedWhenItsRepoChanges(t *testing.T) {
	f := newConnFixture(t)
	up := newFakeGitHubUpstream(t)
	up.addRepo(t, r1RepoWidget)
	up.addRepo(t, r1RepoOther)
	_, _, app := appServerAccess(t, f, up, githubapp.AccessRead)
	srv, h := officerServer(t, f, connectors.WithVirtual(f.store, app, authz.IsAdmin), app)
	enableOfficers(t, srv, r1Officers(t, r1RepoWidget))
	startOfficerInstance(t, f, srv, r1OfficerID, 1, r1OfficerTok)
	_, filed, _ := requestConnector(t, h, r1OfficerTok, "github")
	approveOnTab(t, srv, h, filed.GrantID)
	taskAgent(t, f, "r1-task-repo", r1TaskTok, r1RepoWidget)
	_, tfiled, _ := requestConnector(t, h, r1TaskTok, "github")
	approveOnTab(t, srv, h, tfiled.GrantID)

	// Control: the same repo re-declared — still approved.
	enableOfficers(t, srv, r1Officers(t, r1RepoWidget))
	if v := agentView(t, h, r1OfficerTok); v.Grant != connectors.StatusApproved {
		t.Fatalf("control: an unchanged repo re-opened the grant: %+v", v)
	}
	if out, err := cloneVia(t, h, up, r1OfficerTok, r1RepoWidget); err != nil {
		t.Fatalf("control: the officer could not read %s: %v\n%s", r1RepoWidget, err, out)
	}
	mintsBefore := len(mintsOf(up))

	// widget -> other: re-opened, nothing minted for other.
	enableOfficers(t, srv, r1Officers(t, r1RepoOther))
	if out, err := cloneVia(t, h, up, r1OfficerTok, r1RepoOther); err == nil {
		t.Fatalf("the officer read %s on an approval made for %s:\n%s", r1RepoOther, r1RepoWidget, out)
	}
	if m := mintsOf(up); len(m) != mintsBefore {
		t.Fatalf("a token was minted on the stale approval: %q", m[mintsBefore:])
	}
	if v := agentView(t, h, r1OfficerTok); v.Grant != connectors.StatusPending {
		t.Fatalf("after the repo changed the officer reads %+v, want pending", v)
	}
	audit, err := f.store.ListAudit(asUser(1), 50)
	if err != nil {
		t.Fatal(err)
	}
	const why = "re-opened: the agent's repository changed (approved for example-org/widget, now example-org/other)"
	found := false
	for _, e := range audit {
		found = found || e.Detail == why
	}
	if !found {
		t.Fatalf("no audit line %q in %+v", why, audit)
	}
	// Re-approved: now for other, and the clone works.
	if g, err := f.store.LatestGrant(asUser(1), agentIDByToken(t, srv, r1OfficerTok), "github"); err != nil || g.ID != filed.GrantID {
		t.Fatalf("the re-opened grant is not the same request: %+v %v", g, err)
	}
	approveOnTab(t, srv, h, filed.GrantID)
	if out, err := cloneVia(t, h, up, r1OfficerTok, r1RepoOther); err != nil {
		t.Fatalf("after re-approval the officer could not read %s: %v\n%s", r1RepoOther, err, out)
	}

	// The task agent's grant was never touched.
	if v := agentView(t, h, r1TaskTok); v.Grant != connectors.StatusApproved {
		t.Fatalf("control: the task agent's grant reads %+v", v)
	}

	// none -> some: an officer approved with no repo is re-opened when given one.
	enableOfficers(t, srv, r1Officers(t, ""))
	if v := agentView(t, h, r1OfficerTok); v.Grant != connectors.StatusPending {
		t.Fatalf("other -> none: the officer reads %+v, want pending", v)
	}
	approveOnTab(t, srv, h, filed.GrantID) // approved for NO repository
	if v := agentView(t, h, r1OfficerTok); v.Grant != connectors.StatusApproved {
		t.Fatalf("control: approved with no repo, the officer reads %+v", v)
	}
	enableOfficers(t, srv, r1Officers(t, r1RepoWidget))
	n := len(mintsOf(up))
	if out, err := cloneVia(t, h, up, r1OfficerTok, r1RepoWidget); err == nil {
		t.Fatalf("none -> some: the officer read %s on an approval made for no repository:\n%s", r1RepoWidget, out)
	}
	if m := mintsOf(up); len(m) != n {
		t.Fatalf("none -> some: minted on the stale approval: %q", m[n:])
	}
	if v := agentView(t, h, r1OfficerTok); v.Grant != connectors.StatusPending {
		t.Fatalf("none -> some: the officer reads %+v, want pending", v)
	}
}

// agentIDByToken is the agent whose per-agent token is tok.
func agentIDByToken(t *testing.T, srv *Server, tok string) int64 {
	t.Helper()
	a, err := srv.agentByToken(dbtest.AsOperator(context.Background()), tok)
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}

// jsonError is the "error" field of a JSON object s ("" when it has none).
func jsonError(s string) string {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(s), &e)
	return e.Error
}
