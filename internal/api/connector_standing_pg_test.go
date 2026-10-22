package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
)

// SHARED APPROVALS (standing grants, migration 0043; connector_standing.go),
// over a real Postgres, through the real doors: an agent's connector_request
// over POST /mcp, the proxy's own access check, the Connectors tab's Approve /
// Deny / Revoke, and a mirrored session's connector_request_decide.
//
// Approve once; the approval applies to all of the owner's agents.

// ask files (or finds) agent token tok's request for connector name over POST
// /mcp, as an agent does, and returns the grant status the agent is told and
// the grant's id.
func (f *decideFixture) ask(t *testing.T, tok, name string) (string, int64) {
	t.Helper()
	text, isErr, rpc := f.tool(t, tok, "connector_request", map[string]any{"name": name, "reason": "work"})
	if isErr || rpc != nil {
		t.Fatalf("connector_request %s: %s %+v", name, text, rpc)
	}
	var v struct {
		Grant   string `json:"grant"`
		GrantID int64  `json:"grantId"`
	}
	if err := json.Unmarshal([]byte(text), &v); err != nil || v.GrantID == 0 {
		t.Fatalf("connector_request %s: %s", name, text)
	}
	return v.Grant, v.GrantID
}

// standingOf is the standing grant id grant id names (0 for none), read from
// the row.
func (f *decideFixture) standingOf(t *testing.T, id int64) int64 {
	t.Helper()
	var sg int64
	if err := f.pool.QueryRow(authz.System(context.Background()),
		`SELECT coalesce(standing_grant_id, 0) FROM connector_grants WHERE id = $1`, id).Scan(&sg); err != nil {
		t.Fatalf("read grant %d: %v", id, err)
	}
	return sg
}

// standingRow is one standing grant, read from its row.
type standingRow struct {
	id           int64
	source, repo string
	connector    string
	revoked      bool
}

// standingFrom is the NEWEST standing grant created from origin grant id (one
// grant can originate two: re-approved after its credential source changed).
func (f *decideFixture) standingFrom(t *testing.T, origin int64) standingRow {
	t.Helper()
	var r standingRow
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT id, source, repo, connector_name, revoked_at IS NOT NULL
		FROM connector_standing_grants WHERE origin_grant_id = $1 ORDER BY id DESC LIMIT 1`, origin).Scan(&r.id, &r.source, &r.repo, &r.connector, &r.revoked); err != nil {
		t.Fatalf("the standing grant from request %d: %v", origin, err)
	}
	return r
}

func (f *decideFixture) standingCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM connector_standing_grants`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// prompts is how many push notifications the operator (user 1) was sent for
// connector request id — the prompt a request the owner must decide raises.
func (f *decideFixture) prompts(id int64) int {
	f.srv.WaitForPushes()
	n := 0
	for _, p := range f.pusher.to(ep("attn-operator")) {
		if p.Tag == "connector-"+strconv.FormatInt(id, 10) {
			n++
		}
	}
	return n
}

// agentWith creates one of owner's agents with a repository (and, for a
// pool worker, a claude-code kind in pool), cleaned up with the test.
func (f *decideFixture) agentWith(t *testing.T, owner int64, name, repo, pool, token string) agents.Agent {
	t.Helper()
	in := agents.Agent{Name: name, Namespace: "devpod-" + name, Status: agents.StatusRunning, HooksToken: token, Repo: repo}
	if pool != "" {
		in.Kind, in.CCAccount, in.WorkerPool = agents.KindClaudeCode, "acct", pool
	}
	a, err := agents.NewPG(f.pool).Create(asUser(owner), in)
	if err != nil {
		t.Fatalf("agent %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, a.ID)
	})
	return a
}

// approveOn decides request id with approve through door ("tab" or "mcp").
func (f *decideFixture) approveOn(t *testing.T, door string, id int64) string {
	t.Helper()
	if door == "mcp" {
		text, isErr := f.decide(t, f.ws, focusSID, id, "approve")
		if isErr {
			t.Fatalf("connector_request_decide approve %d: %s", id, text)
		}
		return text
	}
	w := f.call(t, http.MethodPost, "/ui/connector-requests/"+strconv.FormatInt(id, 10)+"/approve", nil, f.operator())
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="connectors-notice"`) {
		t.Fatalf("approve %d on the tab: %d %s", id, w.Code, w.Body)
	}
	return w.Body.String()
}

// TestApprovingOnceApprovesEveryOtherAgentAutomatically is the feature, through
// BOTH approval doors: the owner approves one agent's request; another of the
// owner's agents then asks for the same connector and is told "approved" at
// once — no pending row it waits on, no prompt on the owner's phone. Its grant
// says where that came from: decided_by 'standing-grant' (never 'operator'),
// decided_via 'standing', naming the standing grant the approval created, and
// its audit line names the standing grant and the request it was shared from.
// The CONTROL is the first request: it WAS pending and DID prompt.
func TestApprovingOnceApprovesEveryOtherAgentAutomatically(t *testing.T) {
	for _, door := range []string{"tab", "mcp"} {
		t.Run(door, func(t *testing.T) {
			f := newDecideFixture(t)
			if st, first := f.ask(t, f.opTok, "svc"); st != "pending" {
				t.Fatalf("the first request is %s; want pending", st)
			} else {
				f.approveOn(t, door, first)
				sg := f.standingFrom(t, first)
				if sg.connector != "svc" || sg.source != connectors.SourceStored || sg.repo != "" || sg.revoked {
					t.Fatalf("the standing grant: %+v", sg)
				}
				st, second := f.ask(t, f.op2Tok, "svc")
				if st != "approved" {
					t.Fatalf("another agent's request for the approved connector is %s; want approved at once", st)
				}
				if g := f.grantRow(t, second); g != (grantRow{status: "approved", by: "standing-grant", source: "stored", via: "standing"}) {
					t.Fatalf("the automatic approval: %+v", g)
				}
				if got := f.standingOf(t, second); got != sg.id {
					t.Fatalf("the automatic approval names standing grant %d; want %d", got, sg.id)
				}
				want := "approved automatically by standing grant " + strconv.FormatInt(sg.id, 10) +
					" (shared from your approval of request " + strconv.FormatInt(first, 10) + ")"
				if got := f.auditDetails(t, f.op2.Name); len(got) != 2 || got[0] != "requested" || got[1] != want {
					t.Fatalf("the second agent's audit lines: %q", got)
				}
				if n := f.prompts(second); n != 0 {
					t.Fatalf("the automatically approved request prompted the owner %d time(s)", n)
				}
				if n := f.prompts(first); n != 1 {
					t.Fatalf("control: the first request prompted the owner %d time(s), want 1", n)
				}
				if st2, _, _ := f.tool(t, f.op2Tok, "connector_status", map[string]any{"name": "svc"}); !strings.Contains(st2, "/connect/svc/api.example.com/") {
					t.Fatalf("the second agent's status has no usage: %s", st2)
				}
			}
		})
	}
}

// TestAnApprovalApprovesTheRequestsAlreadyWaiting: two more of the owner's
// agents are already waiting on the same connector when the owner approves the
// first; both are approved in the same step, through either door, and the door
// says so (the tab's notice; the tool's also_approved).
func TestAnApprovalApprovesTheRequestsAlreadyWaiting(t *testing.T) {
	for _, door := range []string{"tab", "mcp"} {
		t.Run(door, func(t *testing.T) {
			f := newDecideFixture(t)
			op3Tok := "attn-op3-token-" + strconv.FormatInt(time.Now().UnixNano(), 36)
			op3 := f.agentFor(t, 1, "attnop3-"+strconv.FormatInt(time.Now().UnixNano(), 36), "", op3Tok)
			_, first := f.ask(t, f.opTok, "svc")
			_, w2 := f.ask(t, f.op2Tok, "svc")
			_, w3 := f.ask(t, op3Tok, "svc")
			out := f.approveOn(t, door, first)
			sg := f.standingFrom(t, first)
			for _, id := range []int64{w2, w3} {
				if g := f.grantRow(t, id); g.status != "approved" || g.via != "standing" || f.standingOf(t, id) != sg.id {
					t.Fatalf("waiting request %d after the approval: %+v", id, g)
				}
			}
			if door == "tab" && !strings.Contains(out, "Shared with all your agents. 2 waiting requests approved too.") {
				t.Fatalf("the tab's notice does not say what was shared:\n%s", out)
			}
			if door == "mcp" && (!strings.Contains(out, `"standing_grant":`+strconv.FormatInt(sg.id, 10)) ||
				!strings.Contains(out, `"also_approved":[`+strconv.FormatInt(w2, 10)+`,`+strconv.FormatInt(w3, 10)+`]`)) {
				t.Fatalf("the tool's answer does not say what was shared: %s", out)
			}
			if got := f.auditDetails(t, op3.Name); len(got) != 2 || !strings.HasPrefix(got[1], "approved automatically by standing grant ") {
				t.Fatalf("op3's audit lines: %q", got)
			}
		})
	}
}

// fakeApp is a virtual `github` connector standing in for the GitHub App: the
// standing-grant key reads only its presence (SourceApp) and the agents'
// repositories.
type fakeApp struct{}

func (fakeApp) Describe() connectors.Connector {
	return connectors.Connector{Name: "github", Platform: "github", Hosts: []string{"api.github.com", "github.com"},
		Scheme: connectors.SchemeBasic, CreatedAt: connectors.VirtualCreatedAt}
}
func (fakeApp) Credential(context.Context, connectors.Caller, string, string) (string, bool, error) {
	return "", true, nil
}
func (fakeApp) Rejected(connectors.Caller) {}
func (fakeApp) Problem(int64) string       { return "" }

// TestAnAppApprovalIsSharedOnlyForTheSameRepository: with the GitHub App, an
// approval for an agent on example-org/widget approves another agent on the
// same repository (spelled in another case: GitHub names are case-insensitive)
// — but NOT an agent on another repository, nor one with no repository, whether
// it was already waiting or asks after. The standing grant is keyed (app,
// example-org/widget).
func TestAnAppApprovalIsSharedOnlyForTheSameRepository(t *testing.T) {
	f := newDecideFixture(t)
	f.srv.ext.Connectors = connectors.WithVirtual(f.store, fakeApp{}, func(context.Context) bool { return true })
	sfx := strconv.FormatInt(time.Now().UnixNano(), 36)
	tok := func(n string) string { return "app-" + n + "-token-" + sfx }
	f.agentWith(t, 1, "widget-a-"+sfx, "example-org/widget", "", tok("wa"))
	f.agentWith(t, 1, "widget-b-"+sfx, "Example-Org/Widget", "", tok("wb"))
	f.agentWith(t, 1, "other-a-"+sfx, "example-org/other", "", tok("oa"))
	f.agentWith(t, 1, "other-b-"+sfx, "example-org/other", "", tok("ob"))
	f.agentWith(t, 1, "norepo-"+sfx, "", "", tok("nr"))

	_, waitingOther := f.ask(t, tok("oa"), "github")
	_, first := f.ask(t, tok("wa"), "github")
	f.approveOn(t, "tab", first)
	sg := f.standingFrom(t, first)
	if sg.source != connectors.SourceApp || sg.repo != "example-org/widget" {
		t.Fatalf("the App standing grant: %+v", sg)
	}
	if st, id := f.ask(t, tok("wb"), "github"); st != "approved" || f.standingOf(t, id) != sg.id {
		t.Fatalf("an agent on the same repository: %s (standing %d)", st, f.standingOf(t, id))
	}
	if g := f.grantRow(t, waitingOther); g.status != "pending" {
		t.Fatalf("a waiting agent on ANOTHER repository was %s by the approval; want pending", g.status)
	}
	for _, n := range []string{"ob", "nr"} {
		if st, id := f.ask(t, tok(n), "github"); st != "pending" {
			t.Fatalf("agent %s asking after the approval is %s; want pending (another repository, or none)", n, st)
		} else if f.prompts(id) != 1 {
			t.Fatalf("agent %s's request did not prompt the owner", n)
		}
	}
}

// TestACredentialSourceChangeAsksAgainThenShares: the owner shared an approval
// of a STORED `github` token; then the GitHub App is configured. Both agents'
// grants re-open (0033) and stay pending — the stored-token approval does not
// approve the App, nor does it approve a new agent asking under the App.
// Approving one agent under the App shares THAT approval: the other agent,
// on the same repository, is approved in the same step from the new (app)
// standing grant.
func TestACredentialSourceChangeAsksAgainThenShares(t *testing.T) {
	f := newDecideFixture(t)
	p := connectors.GitHubPreset
	if _, err := f.store.CreateConnector(f.ctx, connectors.NewConnector{Name: "github", Platform: p.Key, Hosts: p.Hosts,
		Scheme: p.Scheme, Username: p.Username, Secret: connTestSecret}); err != nil {
		t.Fatal(err)
	}
	sfx := strconv.FormatInt(time.Now().UnixNano(), 36)
	ta, tb := "src-a-token-"+sfx, "src-b-token-"+sfx
	f.agentWith(t, 1, "src-a-"+sfx, "example-org/widget", "", ta)
	f.agentWith(t, 1, "src-b-"+sfx, "example-org/widget", "", tb)
	_, ga := f.ask(t, ta, "github")
	f.approveOn(t, "tab", ga)
	stored := f.standingFrom(t, ga)
	st, gb := f.ask(t, tb, "github")
	if st != "approved" || f.standingOf(t, gb) != stored.id {
		t.Fatalf("control: under the stored token the second agent is %s", st)
	}

	f.srv.ext.Connectors = connectors.WithVirtual(f.store, fakeApp{}, func(context.Context) bool { return true })
	for _, tok := range []string{ta, tb} {
		if text, _, _ := f.tool(t, tok, "connector_status", map[string]any{"name": "github"}); !strings.Contains(text, `"grant":"pending"`) {
			t.Fatalf("after the App was configured an agent's github grant reads %s; want pending (re-opened, not re-approved)", text)
		}
	}
	tc := "src-c-token-" + sfx
	f.agentWith(t, 1, "src-c-"+sfx, "example-org/widget", "", tc)
	if st, _ := f.ask(t, tc, "github"); st != "pending" {
		t.Fatalf("a new agent asking under the App is %s from the stored-token approval; want pending", st)
	}
	f.approveOn(t, "tab", ga)
	app := f.standingFrom(t, ga)
	if app.source != connectors.SourceApp || app.repo != "example-org/widget" || app.id == stored.id {
		t.Fatalf("the App approval's standing grant: %+v", app)
	}
	if g := f.grantRow(t, gb); g.status != "approved" || g.source != connectors.SourceApp || f.standingOf(t, gb) != app.id {
		t.Fatalf("the other agent after the App approval: %+v (standing %d)", g, f.standingOf(t, gb))
	}
}

// TestAStandingGrantDoesNotReachAnotherOwner: user 1 approved `svc` and so
// holds a standing grant for it; a member, who has their own connector named
// `svc`, has a worker ask for it — the worker's request stays pending for the
// MEMBER to decide. The CONTROL is user 1's other agent, approved at once.
// (Row-level security is what keeps it here — the member's principal cannot
// read user 1's standing grant; the store's own owner predicate is pinned by
// internal/connectors' TestAStandingGrantNeverCrossesOwners.)
func TestAStandingGrantDoesNotReachAnotherOwner(t *testing.T) {
	f := newDecideFixture(t)
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM connector_grants WHERE owner_id = $1`, `DELETE FROM connector_standing_grants WHERE owner_id = $1`,
			`DELETE FROM connectors WHERE owner_id = $1`} {
			_, _ = f.pool.Exec(sys, q, f.member)
		}
	})
	if _, err := f.store.CreateConnector(asUser(f.member), connectors.NewConnector{Name: "svc", Hosts: []string{"api.example.com"},
		Scheme: connectors.SchemeBearer, Secret: connTestSecret}); err != nil {
		t.Fatal(err)
	}
	_, first := f.ask(t, f.opTok, "svc")
	f.approveOn(t, "tab", first)
	if st, _ := f.ask(t, f.workerTok, "svc"); st != "pending" {
		t.Fatalf("another owner's agent is %s by user 1's approval; want pending", st)
	}
	if st, _ := f.ask(t, f.op2Tok, "svc"); st != "approved" {
		t.Fatalf("control: user 1's other agent is %s; want approved", st)
	}
}

// TestRevokingTheSharedApprovalStopsItAndRevokesWhatItGave: the Connectors tab
// shows the shared approval (and marks the grant it gave); its Revoke ends it
// AND every grant it gave — the first approval and the automatic one — so a
// revoke is an off-switch. After it, another agent asking waits for the owner
// again, and so does an agent whose grant it revoked.
func TestRevokingTheSharedApprovalStopsItAndRevokesWhatItGave(t *testing.T) {
	f := newDecideFixture(t)
	_, first := f.ask(t, f.opTok, "svc")
	f.approveOn(t, "tab", first)
	sg := f.standingFrom(t, first)
	_, second := f.ask(t, f.op2Tok, "svc")
	page := f.call(t, http.MethodGet, "/ui/connectors", nil, f.operator()).Body.String()
	ids := strconv.FormatInt(sg.id, 10)
	if !strings.Contains(page, `data-shared="`+ids+`"`) || !strings.Contains(page, `data-revoke-shared="`+ids+`"`) ||
		!strings.Contains(page, `data-grant-standing="`+ids+`"`) || !strings.Contains(page, `data-shared-agents="1"`) {
		t.Fatalf("the Connectors tab does not show the shared approval and the grant it gave:\n%s", page)
	}
	w := f.call(t, http.MethodDelete, "/connector-standing-grants/"+ids, nil, f.operator())
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "The shared approval of svc was revoked, with 2 agent grant(s) it gave.") {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	for _, id := range []int64{first, second} {
		if g := f.grantRow(t, id); g.status != "revoked" || g.via != "" {
			t.Fatalf("grant %d after the shared approval's revoke: %+v", id, g)
		}
	}
	if strings.Contains(w.Body.String(), `data-shared="`+ids+`"`) {
		t.Fatalf("the revoked shared approval is still shown")
	}
	op3Tok := "attn-op3-token-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	f.agentFor(t, 1, "attnop3-"+strconv.FormatInt(time.Now().UnixNano(), 36), "", op3Tok)
	for name, tok := range map[string]string{"a new agent": op3Tok, "the revoked agent": f.op2Tok} {
		if st, _ := f.ask(t, tok, "svc"); st != "pending" {
			t.Fatalf("%s asking after the revoke is %s; want pending", name, st)
		}
	}
	if w := f.call(t, http.MethodDelete, "/connector-standing-grants/"+ids, nil, f.operator()); !strings.Contains(w.Body.String(), "already ended") {
		t.Fatalf("revoking it again: %d %s", w.Code, w.Body)
	}
}

// TestADenialIsNotShared: the owner denies one agent's request; another agent
// asking for the same connector is pending — no standing grant exists.
func TestADenialIsNotShared(t *testing.T) {
	for _, door := range []string{"tab", "mcp"} {
		t.Run(door, func(t *testing.T) {
			f := newDecideFixture(t)
			_, first := f.ask(t, f.opTok, "svc")
			_, waiting := f.ask(t, f.op2Tok, "svc")
			if door == "mcp" {
				if text, isErr := f.decide(t, f.ws, focusSID, first, "deny"); isErr {
					t.Fatal(text)
				}
			} else if w := f.call(t, http.MethodPost, "/ui/connector-requests/"+strconv.FormatInt(first, 10)+"/deny", nil, f.operator()); w.Code != http.StatusOK {
				t.Fatal(w.Code)
			}
			if g := f.grantRow(t, waiting); g.status != "pending" {
				t.Fatalf("a request waiting beside a denied one is %s; want pending", g.status)
			}
			if n := f.standingCount(t); n != 0 {
				t.Fatalf("a denial created %d standing grant(s)", n)
			}
			op3Tok := "attn-op3-token-" + strconv.FormatInt(time.Now().UnixNano(), 36)
			f.agentFor(t, 1, "attnop3-"+strconv.FormatInt(time.Now().UnixNano(), 36), "", op3Tok)
			if st, _ := f.ask(t, op3Tok, "svc"); st != "pending" {
				t.Fatalf("an agent asking after a denial is %s; want pending", st)
			}
		})
	}
}

// TestAPoolWorkerIsNeverApprovedFromAStandingGrant: a worker-pool member holds
// no GitHub credential (jobs_grants.go), and a standing grant does not change
// that: with the owner's shared approval of a connector reaching github.com,
// the pool worker's request stays pending (and an approval of it is refused, as
// before). The CONTROL is an agent in no pool, approved at once.
func TestAPoolWorkerIsNeverApprovedFromAStandingGrant(t *testing.T) {
	f := newDecideFixture(t)
	if _, err := f.store.CreateConnector(f.ctx, connectors.NewConnector{Name: "gh-token", Hosts: []string{"github.com"},
		Scheme: connectors.SchemeBearer, Secret: connTestSecret}); err != nil {
		t.Fatal(err)
	}
	sfx := strconv.FormatInt(time.Now().UnixNano(), 36)
	f.agentWith(t, 1, "rev-"+sfx, "example-org/widget", "reviewers", "pool-token-"+sfx)
	_, first := f.ask(t, f.opTok, "gh-token")
	f.approveOn(t, "tab", first)
	if st, _ := f.ask(t, "pool-token-"+sfx, "gh-token"); st != "pending" {
		t.Fatalf("a pool worker's GitHub request is %s from a standing grant; want pending", st)
	}
	if st, _ := f.ask(t, f.op2Tok, "gh-token"); st != "approved" {
		t.Fatalf("control: an agent in no pool is %s; want approved", st)
	}
}

// TestTheProxysOwnRequestUsesTheStandingGrant: an agent that never asked calls
// the proxy, whose access check files the request itself (CheckAccess) — the
// door ccd's git routing goes through. With a standing grant, that request is
// approved in the same check and the call is usable: the proxy is wired to the
// same rule (connectors.Proxy.AutoApprove) as the request route.
func TestTheProxysOwnRequestUsesTheStandingGrant(t *testing.T) {
	f := newDecideFixture(t)
	_, first := f.ask(t, f.opTok, "svc")
	f.approveOn(t, "tab", first)
	acc, err := f.srv.connectProxy.CheckAccess(asUser(1), callerOf(f.op2), "svc", "")
	if err != nil || !acc.Usable || !acc.Filed || acc.Grant.StandingGrantID != f.standingFrom(t, first).id {
		t.Fatalf("the proxy's check for an agent that never asked = %+v %v; want filed and usable from the standing grant", acc, err)
	}
	if n := f.prompts(acc.Grant.ID); n != 0 {
		t.Fatalf("the proxy's automatically approved request prompted the owner %d time(s)", n)
	}
}
