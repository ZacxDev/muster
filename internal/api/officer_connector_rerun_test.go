package api

import (
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// Re-running an officer's items blocked on a connector grant
// (officer_connector_rerun.go): the refusal records the held item under the
// grant's id; approving THAT grant asks HQ to re-run it — once, within the
// window, at most RerunMaxPerApproval — and an HQ without the route is logged
// and passed over. Ids, names and the subject are made up to equal no
// constant.
// ---------------------------------------------------------------------------

const (
	rrOff     = "shepherd:example-infra"
	rrTok     = "fixture-officer-token-rerun-3c71"
	rrKind    = "shepherd.opened"
	rrGitHub  = int64(1907) // the github grant
	rrDriver  = int64(2311) // the app-driver grant, same agent
	rrDriverC = "app-driver"
)

// rerunConnStub is the connector store the seam needs: grants by id, by
// (agent, connector), decided in place; the audit is discarded.
type rerunConnStub struct {
	connectors.Store
	mu     sync.Mutex
	grants map[int64]*connectors.Grant
}

func (s *rerunConnStub) find(agentID int64, connector string) *connectors.Grant {
	for _, g := range s.grants {
		if g.AgentID == agentID && g.Connector == connector {
			return g
		}
	}
	return nil
}

func (s *rerunConnStub) LatestGrant(_ context.Context, agentID int64, connector string) (connectors.Grant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := s.find(agentID, connector); g != nil {
		return *g, nil
	}
	return connectors.Grant{}, connectors.ErrNotFound
}

func (s *rerunConnStub) RequestGrant(_ context.Context, agentID int64, connector, _ string) (connectors.Grant, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := s.find(agentID, connector); g != nil {
		return *g, false, nil
	}
	return connectors.Grant{}, false, connectors.ErrNotFound
}

func (s *rerunConnStub) GetGrant(_ context.Context, id int64) (connectors.Grant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g, ok := s.grants[id]; ok {
		return *g, nil
	}
	return connectors.Grant{}, connectors.ErrNotFound
}

func (s *rerunConnStub) GetConnector(_ context.Context, name string) (connectors.Connector, error) {
	return connectors.Connector{Name: name, Hosts: []string{"api.example.test"}, Scheme: connectors.SchemeBearer}, nil
}

func (s *rerunConnStub) DecideGrantFrom(_ context.Context, id int64, approve bool, by, source, repo string, _ connectors.DecisionSource) (connectors.Grant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.grants[id]
	if !ok {
		return connectors.Grant{}, connectors.ErrNotFound
	}
	if g.Status != connectors.StatusPending {
		return *g, connectors.ErrNotPending
	}
	g.Status = connectors.StatusDenied
	if approve {
		g.Status = connectors.StatusApproved
	}
	g.DecidedBy, g.Source, g.Repo = by, source, repo
	return *g, nil
}

func (s *rerunConnStub) Audit(context.Context, connectors.AuditEntry) error { return nil }

// reopen puts grant id back to pending (as a credential-source change does).
func (s *rerunConnStub) reopen(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants[id].Status = connectors.StatusPending
}

type rerunRig struct {
	t     *testing.T
	s     *Server
	hq    *hqtest.Server
	conn  *rerunConnStub
	judge agents.Agent
	logs  *syncBuf
	mu    sync.Mutex
	now   time.Time
}

func newRerunRig(t *testing.T) *rerunRig {
	t.Helper()
	r := &rerunRig{t: t, now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC), logs: &syncBuf{}}
	r.hq = hqtest.New(map[string]string{rrTok: rrOff})
	t.Cleanup(r.hq.Close)
	r.hq.Now = r.clock
	store := &officerAgents{}
	prov := &officerProv{store: store, phase: map[string]provision.Phase{}}
	r.judge = agents.Agent{ID: 743, Name: officers.InstanceName(rrOff, 1), Kind: agents.KindClaudeCode, Status: agents.StatusRunning, OwnerID: 1, Namespace: "ns"}
	r.conn = &rerunConnStub{grants: map[int64]*connectors.Grant{
		rrGitHub: {ID: rrGitHub, AgentID: r.judge.ID, AgentName: r.judge.Name, Connector: officers.GitHubConnector, Status: connectors.StatusPending},
		rrDriver: {ID: rrDriver, AgentID: r.judge.ID, AgentName: r.judge.Name, Connector: rrDriverC, Status: connectors.StatusPending},
	}}
	r.s = New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(r.logs, "", 0))
	r.s.UseExtensions(Extensions{Agents: store, Provisioner: prov, Gateway: &repromptChat{}, SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"},
		Connectors: r.conn})
	list, err := officers.Parse(`[{"id":"`+rrOff+`","hq_url":"`+r.hq.BaseURL()+`","connectors":["`+rrDriverC+`"]}]`, func(k string) string {
		return map[string]string{officers.TokenEnvPrefix + officers.TokenEnvSuffix(rrOff): rrTok}[k]
	}, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.EnableOfficers(OfficersConfig{Officers: list, InstallSession: discardSession, NoShipResume: true, Now: r.clock}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.rows = append(store.rows, r.judge)
	store.mu.Unlock()
	return r
}

func (r *rerunRig) clock() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now }
func (r *rerunRig) advance(d time.Duration) {
	r.mu.Lock()
	r.now = r.now.Add(d)
	r.mu.Unlock()
}

// held adds an item of subject the judge holds a live claim on.
func (r *rerunRig) held(subject string) string {
	id := r.hq.AddItem(rrOff, rrKind, subject)
	r.hq.SetClaim(id, r.judge.Name, r.clock().Add(10*time.Minute))
	return id
}

// refuse runs the access check every connector door runs, for connector,
// and asserts it refused (the grant is pending).
func (r *rerunRig) refuse(connector string) {
	r.t.Helper()
	acc, err := r.s.connectProxy.CheckAccess(context.Background(), r.s.callerOf(r.judge), connector, "")
	if err != nil || acc.Usable || acc.Grant.Status != connectors.StatusPending {
		r.t.Fatalf("access to %s = %+v, %v; want refused, pending", connector, acc, err)
	}
	r.s.WaitForConnectorBlocks() // the record is written in the background
}

// blocks is how many unsettled blocks grant has.
func (r *rerunRig) blocks(grant int64) int {
	r.t.Helper()
	bs, err := r.s.officerRT.connectorBlocks().BlocksForGrant(context.Background(), rrOff, grant, time.Time{}, 100)
	if err != nil {
		r.t.Fatal(err)
	}
	return len(bs)
}

// claimedReads counts the HQ reads of the claimed list (the refusal's read).
func (r *rerunRig) claimedReads() int {
	n := 0
	for _, q := range r.hq.Requests() {
		if q.Method == "GET" && strings.HasSuffix(q.Path, "/items") && strings.Contains(q.Query, "status=claimed") {
			n++
		}
	}
	return n
}

func (r *rerunRig) approve(id int64) {
	r.t.Helper()
	if _, _, err := r.s.decideConnectorGrant(context.Background(), id, true, connectors.DecisionSource{}); err != nil {
		r.t.Fatalf("approve grant %d: %v", id, err)
	}
	r.s.WaitForConnectorBlocks()
}

// TestApprovingAGrantReRunsTheItemsItBlocked: the scenario. An instance
// holding an item is refused github while grant rrGitHub is pending; the item
// finishes blind; approving rrGitHub asks HQ to re-run exactly that item with
// key connector-grant:<id> (never another instance's item held at that
// moment), and HQ enqueues a new open item of its subject.
func TestApprovingAGrantReRunsTheItemsItBlocked(t *testing.T) {
	r := newRerunRig(t)
	item := r.held("example-org/example-infra#3615")
	// another instance's item, held at the same moment, is not this refusal's
	other := r.hq.AddItem(rrOff, rrKind, "example-org/example-infra#3616")
	r.hq.SetClaim(other, officers.InstanceName(rrOff, 2), r.clock().Add(10*time.Minute))
	r.refuse(officers.GitHubConnector)
	r.hq.Escalate(item)
	r.hq.Escalate(other)

	r.approve(rrGitHub)
	got := r.hq.Reruns()
	if len(got) != 1 || got[0].Item != item || got[0].Key != "connector-grant:"+strconv.FormatInt(rrGitHub, 10) || got[0].Officer != rrOff ||
		!strings.Contains(got[0].Reason, "grant "+strconv.FormatInt(rrGitHub, 10)) {
		t.Fatalf("re-run requests %+v; want one for item %s with key connector-grant:%d", got, item, rrGitHub)
	}
	var reran []hqtest.Item
	for _, it := range r.hq.Items(rrOff) {
		if it.ID != item && it.SubjectKey == "example-org/example-infra#3615" {
			reran = append(reran, it)
		}
	}
	if len(reran) != 1 || reran[0].Status != "open" || !strings.Contains(string(reran[0].Payload), `"item_id":`+item) {
		t.Fatalf("HQ items of the subject besides the source: %+v; want ONE open re-run naming item %s", reran, item)
	}
}

// TestAnotherConnectorsApprovalReRunsNothing: an item blocked on the github
// grant is not re-run when the SAME agent's app-driver grant is approved.
func TestAnotherConnectorsApprovalReRunsNothing(t *testing.T) {
	r := newRerunRig(t)
	item := r.held("example-org/example-infra#3643")
	r.refuse(officers.GitHubConnector)
	r.hq.Escalate(item)
	r.approve(rrDriver)
	if got := r.hq.Reruns(); len(got) != 0 {
		t.Fatalf("approving the app-driver grant re-ran %+v; want nothing (the item was blocked on the github grant)", got)
	}
	// positive control: the github grant's approval does re-run it
	r.approve(rrGitHub)
	if got := r.hq.Reruns(); len(got) != 1 || got[0].Item != item {
		t.Fatalf("control: approving the github grant re-ran %+v; want item %s", got, item)
	}
}

// TestABlockOlderThanTheWindowIsNotReRun: a block recorded more than
// RerunWindow before the approval is not re-run; one just inside it is.
func TestABlockOlderThanTheWindowIsNotReRun(t *testing.T) {
	r := newRerunRig(t)
	old := r.held("example-org/example-infra#3535")
	r.refuse(officers.GitHubConnector)
	r.hq.Escalate(old)
	r.advance(officers.RerunWindow - 36*time.Hour)
	recent := r.held("example-org/example-infra#3661")
	r.refuse(officers.GitHubConnector)
	r.hq.Escalate(recent)
	r.advance(37 * time.Hour) // old: window + 1h ago; recent: 37h ago
	r.approve(rrGitHub)
	got := r.hq.Reruns()
	if len(got) != 1 || got[0].Item != recent {
		t.Fatalf("re-run requests %+v; want only item %s (item %s was blocked past the %s window)", got, recent, old, officers.RerunWindow)
	}
}

// TestASecondApprovalDoesNotReRunTwice: the grant re-opened and approved
// again re-runs nothing more — muster marked the block, and never re-sends it.
func TestASecondApprovalDoesNotReRunTwice(t *testing.T) {
	r := newRerunRig(t)
	item := r.held("example-org/example-infra#3536")
	r.refuse(officers.GitHubConnector)
	r.hq.Escalate(item)
	r.approve(rrGitHub)
	r.conn.reopen(rrGitHub)
	r.approve(rrGitHub)
	if got := r.hq.Reruns(); len(got) != 1 {
		t.Fatalf("%d re-run requests after two approvals, want 1: %+v", len(got), got)
	}
}

// TestReRunsArePerApprovalCapped: an instance refused while holding more
// items than RerunMaxPerApproval gets that many re-run, no more.
func TestReRunsArePerApprovalCapped(t *testing.T) {
	r := newRerunRig(t)
	n := officers.RerunMaxPerApproval + 3
	for i := 0; i < n; i++ {
		r.held("example-org/example-infra#" + strconv.Itoa(5100+i))
	}
	r.refuse(officers.GitHubConnector)
	for _, it := range r.hq.Items(rrOff) {
		r.hq.Escalate(it.ID)
	}
	r.approve(rrGitHub)
	if got := r.hq.Reruns(); len(got) != officers.RerunMaxPerApproval {
		t.Fatalf("%d re-run requests for %d blocked items, want the cap %d", len(got), n, officers.RerunMaxPerApproval)
	}
	if !strings.Contains(r.logs.String(), "re-ran the newest "+strconv.Itoa(officers.RerunMaxPerApproval)) {
		t.Fatalf("the cap was not logged:\n%s", r.logs.String())
	}
}

// TestAnHQWithoutTheRerunRouteIsLoggedAndPassedOver: HQ answers 404 (an HQ
// older than the route): the approval succeeds, each item is logged and
// skipped, and the block stays unmarked — an HQ that gains the route re-runs
// it on the grant's next approval.
func TestAnHQWithoutTheRerunRouteIsLoggedAndPassedOver(t *testing.T) {
	r := newRerunRig(t)
	a, b := r.held("example-org/example-infra#701"), r.held("example-org/example-infra#702")
	r.refuse(officers.GitHubConnector)
	r.hq.Escalate(a)
	r.hq.Escalate(b)
	r.hq.RerunAbsent = true
	r.approve(rrGitHub)
	if n := strings.Count(r.logs.String(), "HQ answered 404 to re-running"); n != 2 {
		t.Fatalf("%d 404 lines, want 2 (one per item, the second tried after the first):\n%s", n, r.logs.String())
	}
	r.hq.RerunAbsent = false
	r.conn.reopen(rrGitHub)
	r.approve(rrGitHub)
	if got := r.hq.Reruns(); len(got) != 2 {
		t.Fatalf("after HQ gained the route, %d re-run requests, want 2 (the unmarked blocks)", len(got))
	}
}

// TestAnExplicitRequestRecordsTheBlockToo: an instance that asks for a
// connector (connector_request) while holding an item, its grant left
// pending, has that item re-run on the approval — the request door records
// as the access check does.
func TestAnExplicitRequestRecordsTheBlockToo(t *testing.T) {
	r := newRerunRig(t)
	item := r.held("example-org/example-infra#919")
	v, _, err := r.s.AgentRequestConnector(context.Background(), r.judge, rrDriverC, "need the builder browser")
	if err != nil || v.Grant != connectors.StatusPending || v.GrantID != rrDriver {
		t.Fatalf("request = %+v, %v; want pending grant %d", v, err, rrDriver)
	}
	r.s.WaitForConnectorBlocks()
	r.hq.Escalate(item)
	r.approve(rrDriver)
	if got := r.hq.Reruns(); len(got) != 1 || got[0].Item != item {
		t.Fatalf("re-run requests %+v; want item %s", got, item)
	}
}

// TestADeniedGrantRecordsNoBlock: a refusal on a DENIED grant records
// nothing (no approval of it follows); the same refusal while pending does.
func TestADeniedGrantRecordsNoBlock(t *testing.T) {
	r := newRerunRig(t)
	r.held("example-org/example-infra#1204")
	r.conn.mu.Lock()
	r.conn.grants[rrGitHub].Status = connectors.StatusDenied
	r.conn.mu.Unlock()
	if acc, err := r.s.connectProxy.CheckAccess(context.Background(), r.s.callerOf(r.judge), officers.GitHubConnector, ""); err != nil || acc.Usable {
		t.Fatalf("access on a denied grant = %+v, %v; want refused", acc, err)
	}
	r.s.WaitForConnectorBlocks()
	if n := r.blocks(rrGitHub); n != 0 {
		t.Fatalf("%d blocks recorded on a denied grant, want 0", n)
	}
	r.conn.reopen(rrGitHub) // control: pending, the same refusal records it
	r.refuse(officers.GitHubConnector)
	if n := r.blocks(rrGitHub); n != 1 {
		t.Fatalf("control: %d blocks on the pending grant, want 1", n)
	}
}

// TestOnlyEscalatedItemsAreReRun: the operator's rule ("only escalated ones",
// 2026-11-17). Of three items held at one refusal, the one that finished
// escalated is re-run; the one that finished done and the one still being
// worked are not — and all three are settled, so a later approval of the
// grant looks at none of them again.
func TestOnlyEscalatedItemsAreReRun(t *testing.T) {
	r := newRerunRig(t)
	esc, done, live := r.held("example-org/example-infra#801"), r.held("example-org/example-infra#802"), r.held("example-org/example-infra#803")
	r.refuse(officers.GitHubConnector)
	r.hq.Escalate(esc)
	r.hq.Complete(done)
	r.approve(rrGitHub)
	if got := r.hq.Reruns(); len(got) != 1 || got[0].Item != esc {
		t.Fatalf("re-run requests %+v; want only the escalated item %s (not done %s, not live %s)", got, esc, done, live)
	}
	if n := r.blocks(rrGitHub); n != 0 {
		t.Fatalf("%d blocks left unsettled, want 0: the done and the live item are skipped for good", n)
	}
	// the live one finishes escalated later: settled, so a second approval
	// still re-runs nothing more
	r.hq.Escalate(live)
	r.conn.reopen(rrGitHub)
	r.approve(rrGitHub)
	if got := r.hq.Reruns(); len(got) != 1 {
		t.Fatalf("after a second approval, %d re-run requests, want 1 (skips are never retried)", len(got))
	}
}

// TestARefusalIsDebouncedPerAgentAndGrant: a refused instance retrying within
// connectorBlockDebounce reads HQ's claimed list once; past it, the next
// refusal reads it again and records what it holds then.
func TestARefusalIsDebouncedPerAgentAndGrant(t *testing.T) {
	r := newRerunRig(t)
	r.held("example-org/example-infra#901")
	r.refuse(officers.GitHubConnector)
	base := r.claimedReads()
	if base == 0 || r.blocks(rrGitHub) != 1 {
		t.Fatalf("first refusal: %d claimed reads, %d blocks; want >0 and 1", base, r.blocks(rrGitHub))
	}
	r.held("example-org/example-infra#902")
	for i := 0; i < 5; i++ {
		r.refuse(officers.GitHubConnector)
	}
	if n := r.claimedReads(); n != base {
		t.Fatalf("five retries within the debounce read the claimed list %d more time(s), want 0", n-base)
	}
	if n := r.blocks(rrGitHub); n != 1 {
		t.Fatalf("%d blocks within the debounce, want 1", n)
	}
	r.advance(61 * time.Second)
	r.refuse(officers.GitHubConnector)
	if n := r.blocks(rrGitHub); n != 2 {
		t.Fatalf("past the debounce, %d blocks, want 2 (the item held then recorded)", n)
	}
	// another grant of the same agent is not debounced by this one
	r.refuse(rrDriverC)
	if n := r.blocks(rrDriver); n != 2 {
		t.Fatalf("the other grant: %d blocks, want 2", n)
	}
}
