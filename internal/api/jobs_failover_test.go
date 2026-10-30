package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccfailover"
	"github.com/ZacxDev/muster/internal/jobs"
	"github.com/ZacxDev/muster/internal/officers"
)

// ---------------------------------------------------------------------------
// THE ACCOUNT FAILOVER, FROM THE JOB RUNNER'S SIDE (failover.go failoverJob):
// a job whose turn hit its worker's account limit is re-queued on the SAME
// worker after the failover moved it to another account, and runs there; at
// most once per account; and not at all when nothing moved.
//
// The worker's turns are played by accountGateway, whose answer depends on the
// account the agent row says it is on at the turn — so "the job ran on the new
// account" is observed, not assumed.
// ---------------------------------------------------------------------------

// Requeue is the PG store's conditional re-queue (jobs.PGStore.Requeue), in
// memory: running, no failover from f.From yet.
func (m *memJobs) Requeue(_ context.Context, id int64, f jobs.Failover) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.rows[id]
	if j.Status != jobs.StatusRunning || j.Tried(f.From) || len(j.Failovers) >= jobs.MaxFailovers {
		return false, nil
	}
	j.Status = jobs.StatusQueued
	j.Failovers = append(j.Failovers, f)
	m.rows[id] = j
	return true, nil
}

var _ jobs.Requeuer = (*memJobs)(nil)

// failoverAgents is jobAgents with ONE mutable row (keen-wren, id 7).
type failoverAgents struct {
	jobAgents
	rowMu sync.Mutex
	row   agents.Agent
}

func (s *failoverAgents) current() agents.Agent { s.rowMu.Lock(); defer s.rowMu.Unlock(); return s.row }

func (s *failoverAgents) Get(_ context.Context, id int64) (agents.Agent, error) {
	if a := s.current(); a.ID == id {
		return a, nil
	}
	return agents.Agent{}, pgx.ErrNoRows
}

func (s *failoverAgents) GetByName(_ context.Context, name string) (agents.Agent, error) {
	if a := s.current(); a.Name == name {
		return a, nil
	}
	return agents.Agent{}, pgx.ErrNoRows
}

func (s *failoverAgents) List(context.Context) ([]agents.Agent, error) {
	return []agents.Agent{s.current()}, nil
}

// fakeFailover plays ccfailover.Service: moves the row from an account to its
// target in `targets`, once per (agent, from) like the real one.
type fakeFailover struct {
	ags     *failoverAgents
	mu      sync.Mutex
	targets map[string]string
	calls   []string
	awaits  int
}

func (f *fakeFailover) AccountFailed(_ context.Context, agentID int64, account, failure, _ string) ccfailover.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, account+":"+failure)
	res := ccfailover.Result{From: account, AgentName: f.ags.current().Name}
	f.ags.rowMu.Lock()
	defer f.ags.rowMu.Unlock()
	if f.ags.row.CCAccount != account {
		res.Moved, res.To = true, f.ags.row.CCAccount
		return res
	}
	to, ok := f.targets[account]
	if !ok {
		res.Why = "no other Claude account is usable"
		return res
	}
	f.ags.row.CCAccount = to
	res.Moved, res.Rebound, res.To = true, true, to
	return res
}

func (f *fakeFailover) AwaitRebound(context.Context, ccfailover.Result) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.awaits++
	return nil
}
func (f *fakeFailover) Events(context.Context, int64, int) ([]ccfailover.Event, error) {
	return nil, nil
}
func (f *fakeFailover) UseHooks(ccfailover.Policy, ccfailover.Prepare) {}
func (f *fakeFailover) Allowed(agents.Agent) (bool, string)            { return true, "" }

// accountGateway answers a turn by the account the agent is on NOW (the row the
// runner re-read): limited accounts fail with failure, others answer reply.
type accountGateway struct {
	jobGateway
	ags     *failoverAgents
	limited map[string]bool
	failure string
	reply   string
	mu      sync.Mutex
	onAcct  []string
}

func (g *accountGateway) Chat(ctx context.Context, a agents.Agent, key, msg string, emit func(string)) (string, error) {
	g.mu.Lock()
	g.onAcct = append(g.onAcct, a.CCAccount)
	g.mu.Unlock()
	route, _ := agents.TurnRouteFrom(ctx)
	_ = route
	if g.limited[a.CCAccount] {
		return "", &agents.RuntimeError{Status: 429, Type: g.failure, Message: "You've hit your limit · resets 11pm (UTC)"}
	}
	return g.reply, nil
}

func (g *accountGateway) turns() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.onAcct...)
}

type failoverRig struct {
	*jobFixture
	ags *failoverAgents
	fo  *fakeFailover
	gw  *accountGateway
}

func newFailoverRig(t *testing.T, targets map[string]string, limited map[string]bool, failure string) *failoverRig {
	t.Helper()
	ags := &failoverAgents{row: agents.Agent{ID: 7, Name: "keen-wren", Kind: agents.KindClaudeCode, CCAccount: "north",
		Repo: "acme/widgets", Status: agents.StatusRunning}}
	fo := &fakeFailover{ags: ags, targets: targets}
	gw := &accountGateway{ags: ags, limited: limited, failure: failure, reply: "LGTM"}
	f := &jobFixture{store: newMemJobs(), ags: &ags.jobAgents, ws: &jobWorkspace{files: map[string]string{}}}
	f.s = New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken, ServiceToken: testServiceToken}, log.New(io.Discard, "", 0))
	f.s.UseExtensions(Extensions{
		Notes:           stubNotesStore{},
		Agents:          ags,
		SessionLiveness: stubLiveness{},
		Gateway:         gw,
		Jobs:            f.store,
		JobWorkspace:    f.ws,
		AccountFailover: fo,
	})
	f.ts = httptest.NewServer(f.s.Handler())
	t.Cleanup(f.ts.Close)
	return &failoverRig{jobFixture: f, ags: ags, fo: fo, gw: gw}
}

// TestARateLimitedJobIsRequeuedOnItsWorkersNewAccountAndRuns: north is limited,
// south is free; the job's first turn fails on north, the failover moves the
// worker to south, and the job — re-queued, not failed — runs again on the
// same worker, now on south, and succeeds. The re-queue is on the job.
func TestARateLimitedJobIsRequeuedOnItsWorkersNewAccountAndRuns(t *testing.T) {
	for _, failure := range []string{"rate_limited", "auth_failed"} {
		t.Run(failure, func(t *testing.T) {
			r := newFailoverRig(t, map[string]string{"north": "south"}, map[string]bool{"north": true}, failure)
			id := r.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review"})
			out := r.await(t, id)
			if out["status"] != jobs.StatusSucceeded || out["result"] != "LGTM" {
				t.Fatalf("job = %v/%v (%v), want succeeded/LGTM after the failover", out["status"], out["result"], out["error"])
			}
			if out["cc_account"] != "south" || out["agent"] != "keen-wren" {
				t.Fatalf("agent/cc_account = %v/%v, want keen-wren/south (same worker, new account)", out["agent"], out["cc_account"])
			}
			if got := strings.Join(r.gw.turns(), ","); got != "north,south" {
				t.Fatalf("turns ran on %q, want north,south", got)
			}
			fos, _ := out["account_failovers"].([]any)
			if len(fos) != 1 {
				t.Fatalf("account_failovers = %v, want one", out["account_failovers"])
			}
			fo, _ := fos[0].(map[string]any)
			if fo["from"] != "north" || fo["to"] != "south" || fo["reason"] != failure {
				t.Fatalf("failover = %v, want north -> south, %s", fo, failure)
			}
			if r.fo.awaits != 1 {
				t.Fatalf("waited for the new pod %d time(s), want 1", r.fo.awaits)
			}
		})
	}
}

// TestAJobWhoseAccountCouldNotFailOverFailsAsBefore: no other account usable
// (every one cooling), so nothing moved: the job fails rate_limited on north
// after ONE turn, with no failover recorded — today's behaviour.
func TestAJobWhoseAccountCouldNotFailOverFailsAsBefore(t *testing.T) {
	r := newFailoverRig(t, map[string]string{}, map[string]bool{"north": true}, "rate_limited")
	out := r.await(t, r.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review"}))
	if out["status"] != jobs.StatusFailed || out["reason"] != "rate_limited" || out["cc_account"] != "north" {
		t.Fatalf("job = %v/%v on %v, want failed/rate_limited on north", out["status"], out["reason"], out["cc_account"])
	}
	if _, ok := out["account_failovers"]; ok {
		t.Fatalf("a job that was not re-queued carries account_failovers: %v", out["account_failovers"])
	}
	if got := r.gw.turns(); len(got) != 1 {
		t.Fatalf("turns = %v, want 1", got)
	}
}

// TestAJobIsRequeuedAtMostOncePerAccount: every account is limited and the
// failover keeps bouncing the worker north -> south -> north; the job is
// re-queued off north once and off south once, and its third failure — back on
// north, which it already left — ends it. It never loops.
func TestAJobIsRequeuedAtMostOncePerAccount(t *testing.T) {
	r := newFailoverRig(t, map[string]string{"north": "south", "south": "north"}, map[string]bool{"north": true, "south": true}, "rate_limited")
	out := r.await(t, r.submit(t, map[string]any{"agent": "keen-wren", "prompt": "review"}))
	if out["status"] != jobs.StatusFailed || out["reason"] != "rate_limited" {
		t.Fatalf("job = %v/%v, want failed/rate_limited", out["status"], out["reason"])
	}
	if got := strings.Join(r.gw.turns(), ","); got != "north,south,north" {
		t.Fatalf("turns ran on %q, want north,south,north", got)
	}
	if fos, _ := out["account_failovers"].([]any); len(fos) != 2 {
		t.Fatalf("account_failovers = %v, want two (off north, off south)", out["account_failovers"])
	}
}

// ---------------------------------------------------------------------------
// THE OFFICER HALF: an instance's item is retried after its account moved.
// ---------------------------------------------------------------------------

// TestAnOfficerInstanceFailoverReArmsItsFirstTurnAndReleasesItsClaim: the
// pre-apply hook re-arms instance 1's first turn with the officer kickoff and
// releases the item it held, so the restarted instance (or another) takes it
// again at once.
func TestAnOfficerInstanceFailoverReArmsItsFirstTurnAndReleasesItsClaim(t *testing.T) {
	r := newOfficerRig(t)
	one := r.addInstance(r.a, 1, 811)
	other := r.addInstance(r.a, 2, 812)
	item := r.hq.AddItem(offA, "release.evaluate", "svc:v10")
	held := r.hq.AddItem(offA, "release.evaluate", "svc:v11")
	ctx := context.Background()
	if out := r.s.dispatchAgentTool(ctx, one, officers.ToolClaim, `{"item_id":"`+item+`"}`); isToolErr(out) {
		t.Fatalf("claim: %s", out)
	}
	// The control: another instance's claim is NOT released.
	if out := r.s.dispatchAgentTool(ctx, other, officers.ToolClaim, `{"item_id":"`+held+`"}`); isToolErr(out) {
		t.Fatalf("claim: %s", out)
	}
	if err := r.s.failoverPrepare(ctx, one); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	r.store.mu.Lock()
	note, rearmed := r.store.rearmed[one.ID]
	_, otherRearmed := r.store.rearmed[other.ID]
	r.store.mu.Unlock()
	if !rearmed || note != officers.KickoffText(r.a, 1) || otherRearmed {
		t.Fatalf("re-armed: instance 1 %v (%q), instance 2 %v; want only instance 1, with the officer kickoff", rearmed, note, otherRearmed)
	}
	if it := r.hq.Item(item); it.Status != "open" {
		t.Fatalf("the item instance 1 held is %q, want open again", it.Status)
	}
	if it := r.hq.Item(held); it.Status != "claimed" {
		t.Fatalf("instance 2's item is %q, want still claimed", it.Status)
	}
	// A non-officer agent: nothing.
	if err := r.s.failoverPrepare(ctx, r.agent(t, officerOrdinaryID)); err != nil {
		t.Fatalf("prepare of an ordinary agent: %v", err)
	}
	r.store.mu.Lock()
	_, ordinary := r.store.rearmed[officerOrdinaryID]
	r.store.mu.Unlock()
	if ordinary {
		t.Fatal("an ordinary agent's first turn was re-armed by a failover")
	}
}

// TestAnOfficersPinIsReadFromItsConfiguration: an officer that names
// cc_account pins its instances (policy pinned), cc_failover opts them back in,
// and an officer without cc_account pins nothing.
func TestAnOfficersPinIsReadFromItsConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name              string
		account           string
		optIn             bool
		wantPinned, wantO bool
	}{
		{"unpinned", "", false, false, false},
		{"pinned", "north", false, true, false},
		{"pinned, opted in", "north", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
			s.UseExtensions(Extensions{Agents: &officerAgents{}, Provisioner: &officerProv{store: &officerAgents{}}, Gateway: &repromptChat{},
				SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}})
			o := officers.Officer{ID: offA, HQURL: "http://hq.example.test", MaxInstances: 1, IdleAfter: time.Hour, PollInterval: time.Minute,
				Owner: 1, CCAccount: tc.account, CCFailover: tc.optIn}.WithToken(offATok)
			if _, err := s.EnableOfficers(OfficersConfig{Officers: []officers.Officer{o}, InstallSession: discardSession}); err != nil {
				t.Fatal(err)
			}
			p, opt := s.failoverPolicy(agents.Agent{Name: officers.InstanceName(offA, 1)})
			if p != tc.wantPinned || opt != tc.wantO {
				t.Fatalf("policy = pinned %v, opt-in %v; want %v, %v", p, opt, tc.wantPinned, tc.wantO)
			}
			if p, opt := s.failoverPolicy(agents.Agent{Name: "brave-otter"}); p || opt {
				t.Fatal("a non-officer agent read as pinned by the officer policy")
			}
		})
	}
}

var _ = errors.New
