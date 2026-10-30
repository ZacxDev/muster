package ccfailover

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/provision"
)

// THE FAILOVER'S DECISION, over the REAL ccpool.Pool (marks, cooldown, current
// token) and fakes of the agents store, the lifecycle adapter and the event
// store. The fake rebinder moves the row UNCONDITIONALLY: the idempotency on
// (agent, from) under test here is the service's own, not the adapter's
// (agentprovision's rebind_test pins that one).

// Token values are distinct from every account name, so a line that carried
// one is not mistaken for a line that names the account.
var testTokens = map[string]string{
	"north": "sk-ant-oat01-FAKE-failover-north-71c2",
	"south": "sk-ant-oat01-FAKE-failover-south-0b9e",
	"east":  "sk-ant-oat01-FAKE-failover-east-44d1",
}

type memMarks struct {
	mu sync.Mutex
	m  map[string]ccpool.Mark
}

func (s *memMarks) Marks(context.Context, int64) (map[string]ccpool.Mark, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]ccpool.Mark{}
	for k, v := range s.m {
		out[k] = v
	}
	return out, nil
}
func (s *memMarks) LiveCounts(context.Context, int64) (map[string]int, error) { return nil, nil }
func (s *memMarks) MarkRateLimited(_ context.Context, _ int64, account, detail string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.m[account]
	m.Account, m.RateLimitedAt, m.RateLimitedDetail = account, at, detail
	s.m[account] = m
	return nil
}
func (s *memMarks) MarkAuthFailed(_ context.Context, _ int64, account, fp, detail string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.m[account]
	m.Account, m.AuthFailedAt, m.AuthFailedDetail, m.AuthFailedToken = account, at, detail, fp
	s.m[account] = m
	return nil
}

type fakeAgents struct {
	mu  sync.Mutex
	row agents.Agent
}

func (f *fakeAgents) Get(_ context.Context, id int64) (agents.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id != f.row.ID {
		return agents.Agent{}, errors.New("no such agent")
	}
	return f.row, nil
}

type fakeRebinder struct {
	ags   *fakeAgents
	mu    sync.Mutex
	calls []string
	delay time.Duration
}

func (r *fakeRebinder) Rebind(ctx context.Context, id int64, from, to string, prepare func(context.Context, agents.Agent) error) (agentprovision.RebindResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, from+"->"+to)
	r.mu.Unlock()
	time.Sleep(r.delay)
	r.ags.mu.Lock()
	r.ags.row.CCAccount = to
	row := r.ags.row
	r.ags.mu.Unlock()
	if prepare != nil {
		_ = prepare(ctx, row)
	}
	return agentprovision.RebindResult{Rebound: true, Current: to, PrevInstance: "pod-old"}, nil
}

func (r *fakeRebinder) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.calls) }

type memEvents struct {
	mu  sync.Mutex
	evs []Event
}

func (m *memEvents) Record(_ context.Context, e Event) (Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.ID = int64(len(m.evs) + 1)
	m.evs = append(m.evs, e)
	return e, nil
}
func (m *memEvents) ForAgent(_ context.Context, id int64, _ int) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Event
	for i := len(m.evs) - 1; i >= 0; i-- {
		if m.evs[i].AgentID == id {
			out = append(out, m.evs[i])
		}
	}
	return out, nil
}

type fixture struct {
	svc   *Service
	pool  *ccpool.Pool
	marks *memMarks
	ags   *fakeAgents
	rb    *fakeRebinder
	evs   *memEvents
	logs  *bytes.Buffer
}

const agentID = int64(5317)

func newFixture(t *testing.T, enabled bool, accounts ...string) *fixture {
	t.Helper()
	acc := map[string]string{}
	for _, a := range accounts {
		acc[a] = testTokens[a]
	}
	src, err := ccpool.NewStatic(acc)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{marks: &memMarks{m: map[string]ccpool.Mark{}}, evs: &memEvents{}, logs: &bytes.Buffer{}}
	if f.pool, err = ccpool.New(src, f.marks); err != nil {
		t.Fatal(err)
	}
	f.ags = &fakeAgents{row: agents.Agent{ID: agentID, Name: "keen-wren", Kind: agents.KindClaudeCode, CCAccount: "north", OwnerID: 1, Status: agents.StatusRunning}}
	f.rb = &fakeRebinder{ags: f.ags}
	if f.svc, err = New(Config{Enabled: enabled, Pool: f.pool, Agents: f.ags, Rebinder: f.rb, Store: f.evs,
		Logger: log.New(f.logs, "", 0)}); err != nil {
		t.Fatal(err)
	}
	return f
}

// fail is what the gateway does: mark the account, then tell the failover.
func (f *fixture) fail(t *testing.T, account, failure, detail string) Result {
	t.Helper()
	if err := f.pool.MarkFailure(context.Background(), 1, account, failure, detail); err != nil {
		t.Fatal(err)
	}
	return f.svc.AccountFailed(context.Background(), agentID, account, failure, detail)
}

// TestARateLimitedAutoAgentIsReboundToTheUsableAccount: the core case — an
// unpinned agent on north, north rate-limited, south free → moved to south once,
// with the event (and its reset time) recorded and one log line.
func TestARateLimitedAutoAgentIsReboundToTheUsableAccount(t *testing.T) {
	f := newFixture(t, true, "north", "south")
	res := f.fail(t, "north", ccpool.FailureRateLimited, "You've hit your limit · resets 11:45pm (UTC)")
	if !res.Rebound || !res.Moved || res.To != "south" || res.PrevInstance != "pod-old" {
		t.Fatalf("result = %+v, want rebound onto south", res)
	}
	if got := f.rb.calls; len(got) != 1 || got[0] != "north->south" {
		t.Fatalf("rebinds = %v, want one north->south", got)
	}
	if len(f.evs.evs) != 1 {
		t.Fatalf("%d events recorded, want 1", len(f.evs.evs))
	}
	ev := f.evs.evs[0]
	if ev.From != "north" || ev.To != "south" || ev.Reason != ccpool.FailureRateLimited || ev.AgentID != agentID || ev.ResetsAt.IsZero() {
		t.Fatalf("event = %+v, want north->south rate_limited with a reset time", ev)
	}
	if n := strings.Count(f.logs.String(), "account_failover from north to south, reason rate_limited, resets "); n != 1 {
		t.Fatalf("%d rebind log line(s), want 1:\n%s", n, f.logs.String())
	}
}

// TestNoRebindWhenEveryOtherAccountIsCooling: south is cooling too, so the
// agent stays on north — today's behaviour — and nothing is restarted.
func TestNoRebindWhenEveryOtherAccountIsCooling(t *testing.T) {
	f := newFixture(t, true, "north", "south")
	if err := f.pool.MarkFailure(context.Background(), 1, "south", ccpool.FailureRateLimited, "limit"); err != nil {
		t.Fatal(err)
	}
	res := f.fail(t, "north", ccpool.FailureRateLimited, "limit")
	if res.Moved || res.Rebound || res.Why == "" {
		t.Fatalf("result = %+v, want not moved with a reason", res)
	}
	if f.rb.count() != 0 || len(f.evs.evs) != 0 || f.ags.row.CCAccount != "north" {
		t.Fatalf("rebinds %v, events %d, account %q; want none, none, north", f.rb.calls, len(f.evs.evs), f.ags.row.CCAccount)
	}
}

// TestAPinnedAgentIsNotReboundUnlessItOptedIn: a pin (on the row, or through
// the policy hook — an officer's cc_account) keeps the agent where it is; the
// opt-in (row or hook) lets it move.
func TestAPinnedAgentIsNotReboundUnlessItOptedIn(t *testing.T) {
	for _, tc := range []struct {
		name               string
		rowPinned, rowOpt  bool
		hookPinned, hookOp bool
		wantMove           bool
	}{
		{"unpinned", false, false, false, false, true},
		{"pinned on the row", true, false, false, false, false},
		{"pinned and opted in on the row", true, true, false, false, true},
		{"pinned by the policy (officer)", false, false, true, false, false},
		{"pinned and opted in by the policy", false, false, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, true, "north", "south")
			f.ags.row.CCAccountPinned, f.ags.row.CCFailover = tc.rowPinned, tc.rowOpt
			f.svc.UseHooks(func(agents.Agent) (bool, bool) { return tc.hookPinned, tc.hookOp }, nil)
			res := f.fail(t, "north", ccpool.FailureRateLimited, "limit")
			if res.Moved != tc.wantMove || (f.rb.count() == 1) != tc.wantMove {
				t.Fatalf("moved=%v rebinds=%v (%s), want moved=%v", res.Moved, f.rb.calls, res.Why, tc.wantMove)
			}
		})
	}
}

// TestAnAuthFailedAccountIsLeftToo: auth_failed for the current token moves
// the agent as rate_limited does, with no reset time.
func TestAnAuthFailedAccountIsLeftToo(t *testing.T) {
	f := newFixture(t, true, "north", "south")
	res := f.fail(t, "north", ccpool.FailureAuthFailed, "Invalid API key · Please run /login")
	if !res.Rebound || res.To != "south" {
		t.Fatalf("result = %+v, want rebound onto south", res)
	}
	if ev := f.evs.evs[0]; ev.Reason != ccpool.FailureAuthFailed || !ev.ResetsAt.IsZero() {
		t.Fatalf("event = %+v, want auth_failed with no reset", ev)
	}
}

// TestTwoConcurrentFailuresRebindOnce: two turns on the agent fail on north at
// once; exactly one rebind happens, and both callers learn the agent is on
// south (the job runner relies on the second answer).
func TestTwoConcurrentFailuresRebindOnce(t *testing.T) {
	f := newFixture(t, true, "north", "south", "east")
	f.rb.delay = 30 * time.Millisecond
	if err := f.pool.MarkFailure(context.Background(), 1, "north", ccpool.FailureRateLimited, "limit"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]Result, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = f.svc.AccountFailed(context.Background(), agentID, "north", ccpool.FailureRateLimited, "limit")
		}(i)
	}
	wg.Wait()
	if n := f.rb.count(); n != 1 {
		t.Fatalf("%d rebinds (%v), want exactly 1", n, f.rb.calls)
	}
	rebound := 0
	for _, r := range results {
		if !r.Moved || r.To != f.ags.row.CCAccount {
			t.Fatalf("result %+v, want moved onto the agent's account %q", r, f.ags.row.CCAccount)
		}
		if r.Rebound {
			rebound++
		}
		if r.PrevInstance != "pod-old" {
			t.Fatalf("result %+v lost the replaced instance", r)
		}
	}
	if rebound != 1 || len(f.evs.evs) != 1 {
		t.Fatalf("%d caller(s) report the rebind, %d event(s); want 1 and 1", rebound, len(f.evs.evs))
	}
}

// TestTheTargetFollowsThePoolsPreferenceOrder across three other accounts:
// never rate-limited first, else rate-limited longest ago.
func TestTheTargetFollowsThePoolsPreferenceOrder(t *testing.T) {
	f := newFixture(t, true, "north", "south", "east")
	old := time.Now().Add(-6 * time.Hour)
	f.marks.m["south"] = ccpool.Mark{Account: "south", RateLimitedAt: old, RateLimitedDetail: "limit"}
	if res := f.fail(t, "north", ccpool.FailureRateLimited, "limit"); res.To != "east" {
		t.Fatalf("to = %q (%s), want east (never rate-limited)", res.To, res.Why)
	}
	// Now east hits its limit: south (limited 6h ago, cooldown 1h over) is
	// usable, north is cooling.
	if res := f.fail(t, "east", ccpool.FailureRateLimited, "limit"); res.To != "south" {
		t.Fatalf("to = %q (%s), want south", res.To, res.Why)
	}
}

// TestFailoverOffMovesNothing: MUSTER_AGENT_CC_FAILOVER=off.
func TestFailoverOffMovesNothing(t *testing.T) {
	f := newFixture(t, false, "north", "south")
	res := f.fail(t, "north", ccpool.FailureRateLimited, "limit")
	if res.Moved || f.rb.count() != 0 || !strings.Contains(res.Why, "MUSTER_AGENT_CC_FAILOVER") {
		t.Fatalf("result = %+v, rebinds %v; want nothing moved, the switch named", res, f.rb.calls)
	}
}

// TestOtherFailuresMoveNothing: busy, capacity and friends say nothing about
// the account.
func TestOtherFailuresMoveNothing(t *testing.T) {
	f := newFixture(t, true, "north", "south")
	for _, typ := range []string{"busy", "capacity", "turn_failed", ""} {
		if res := f.svc.AccountFailed(context.Background(), agentID, "north", typ, "x"); res.Moved {
			t.Fatalf("%q moved the agent: %+v", typ, res)
		}
	}
	if f.rb.count() != 0 {
		t.Fatalf("rebinds %v, want none", f.rb.calls)
	}
}

// TestTheEventAndLogLineCarryNoToken: the recorded event, its JSON-able fields
// and every log line name accounts, never a setup-token.
func TestTheEventAndLogLineCarryNoToken(t *testing.T) {
	f := newFixture(t, true, "north", "south")
	f.fail(t, "north", ccpool.FailureRateLimited, "limit · resets 11pm (UTC)")
	// The positive control: the line that must be there is there.
	if !strings.Contains(f.logs.String(), "account_failover from north to south") {
		t.Fatalf("no rebind line logged:\n%s", f.logs.String())
	}
	dump := f.logs.String() + f.evs.evs[0].String() + strings.Join([]string{f.evs.evs[0].From, f.evs.evs[0].To, f.evs.evs[0].AgentName}, " ")
	for name, tok := range testTokens {
		if strings.Contains(dump, tok) || strings.Contains(dump, ccpool.Fingerprint(tok)) {
			t.Fatalf("account %s's token (or its fingerprint) reached the event or the log:\n%s", name, dump)
		}
	}
}

// TestAwaitReboundWaitsForTheReplacementInstance: a Ready instance that is
// still the OLD pod does not count; its replacement does.
func TestAwaitReboundWaitsForTheReplacementInstance(t *testing.T) {
	var mu sync.Mutex
	list := []provision.Instance{{Ref: provision.Ref{Name: "keen-wren"}, InstanceID: "pod-old", Ready: true}}
	f := newFixture(t, true, "north", "south")
	svc, err := New(Config{Enabled: true, Pool: f.pool, Agents: f.ags, Rebinder: f.rb, ReadyPoll: 5 * time.Millisecond, ReadyWait: time.Second,
		Instances: func(context.Context) ([]provision.Instance, error) {
			mu.Lock()
			defer mu.Unlock()
			return append([]provision.Instance(nil), list...), nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	r := Result{AgentName: "keen-wren", From: "north", To: "south", Moved: true, PrevInstance: "pod-old"}
	done := make(chan error, 1)
	go func() { done <- svc.AwaitRebound(context.Background(), r) }()
	select {
	case err := <-done:
		t.Fatalf("returned (%v) while only the old pod was ready", err)
	case <-time.After(40 * time.Millisecond):
	}
	mu.Lock()
	list = []provision.Instance{{Ref: provision.Ref{Name: "keen-wren"}, InstanceID: "pod-new", Ready: true}}
	mu.Unlock()
	if err := <-done; err != nil {
		t.Fatalf("await: %v", err)
	}
}
