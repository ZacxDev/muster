package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccfailover"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/jobs"
)

// ---------------------------------------------------------------------------
// A POOL WHOSE EVERY WORKER SITS ON A RATE-LIMITED ACCOUNT, WHILE ANOTHER
// ACCOUNT IS USABLE (the 2026-11-07 incident). Two accounts, sara and devin;
// pool civ-hq-reviewers has two workers, keen-fox and mellow-claw, both on
// devin; devin is marked cooling by a turn of ANOTHER agent, clever-heron,
// which the failover moves to sara. Before the fix every submission to the
// pool was refused 429 until devin's reset, because a refused submission runs
// no turn and so no failover ever fired for the pool's workers.
//
// These run the REAL ccpool.Pool (marks, cooldown) and the REAL
// ccfailover.Service behind the job API; only the lifecycle adapter (the
// rebind's row move — conditional, like agents.PGStore.RebindCCAccount) and
// ccd (a turn on devin answers rate_limited) are played.
// ---------------------------------------------------------------------------

const incidentPool = "civ-hq-reviewers"

// incidentTokens are distinct from every account and agent name.
var incidentTokens = map[string]string{
	"sara":  "sk-ant-oat01-FAKE-incident-sara-5d1e",
	"devin": "sk-ant-oat01-FAKE-incident-devin-a07c",
}

// poolMarks is ccpool.Store in memory.
type poolMarks struct {
	mu sync.Mutex
	m  map[string]ccpool.Mark
}

func (s *poolMarks) Marks(context.Context, int64) (map[string]ccpool.Mark, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]ccpool.Mark{}
	for k, v := range s.m {
		out[k] = v
	}
	return out, nil
}
func (s *poolMarks) LiveCounts(context.Context, int64) (map[string]int, error) { return nil, nil }
func (s *poolMarks) MarkRateLimited(_ context.Context, _ int64, account, detail string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.m[account]
	m.Account, m.RateLimitedAt, m.RateLimitedDetail = account, at, detail
	s.m[account] = m
	return nil
}
func (s *poolMarks) MarkAuthFailed(_ context.Context, _ int64, account, fp, detail string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.m[account]
	m.Account, m.AuthFailedAt, m.AuthFailedDetail, m.AuthFailedToken = account, at, detail, fp
	s.m[account] = m
	return nil
}

// incidentAgents is the agents store: mutable rows, everything else jobAgents'.
type incidentAgents struct {
	jobAgents
	rowMu sync.Mutex
	rows  map[int64]agents.Agent
}

func (s *incidentAgents) Get(_ context.Context, id int64) (agents.Agent, error) {
	s.rowMu.Lock()
	defer s.rowMu.Unlock()
	if a, ok := s.rows[id]; ok {
		return a, nil
	}
	return agents.Agent{}, pgx.ErrNoRows
}

func (s *incidentAgents) GetByName(_ context.Context, name string) (agents.Agent, error) {
	s.rowMu.Lock()
	defer s.rowMu.Unlock()
	for _, a := range s.rows {
		if a.Name == name {
			return a, nil
		}
	}
	return agents.Agent{}, pgx.ErrNoRows
}

func (s *incidentAgents) List(context.Context) ([]agents.Agent, error) {
	s.rowMu.Lock()
	defer s.rowMu.Unlock()
	out := make([]agents.Agent, 0, len(s.rows))
	for _, a := range s.rows {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *incidentAgents) account(name string) string {
	a, _ := s.GetByName(context.Background(), name)
	return a.CCAccount
}

// incidentRebinder plays agentprovision.Adapter.Rebind: the conditional move.
type incidentRebinder struct {
	ags   *incidentAgents
	mu    sync.Mutex
	moved []string
}

func (r *incidentRebinder) Rebind(_ context.Context, id int64, from, to string, _ func(context.Context, agents.Agent) error) (agentprovision.RebindResult, error) {
	r.ags.rowMu.Lock()
	a, ok := r.ags.rows[id]
	if !ok {
		r.ags.rowMu.Unlock()
		return agentprovision.RebindResult{Skipped: "gone"}, errors.New("no such agent")
	}
	if a.CCAccount != from {
		r.ags.rowMu.Unlock()
		return agentprovision.RebindResult{Current: a.CCAccount, Skipped: "moved first"}, nil
	}
	a.CCAccount = to
	r.ags.rows[id] = a
	r.ags.rowMu.Unlock()
	r.mu.Lock()
	r.moved = append(r.moved, a.Name+":"+from+"->"+to)
	r.mu.Unlock()
	return agentprovision.RebindResult{Rebound: true, Current: to, PrevInstance: "pod-" + a.Name}, nil
}

func (r *incidentRebinder) moves() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.moved...)
	sort.Strings(out)
	return out
}

// ccPoolKinds answers the pool's account states from the real pool.
type ccPoolKinds struct {
	AgentKinds
	pool *ccpool.Pool
}

func (k ccPoolKinds) ClaudeAccountStates(ctx context.Context, owner int64) (map[string]ccpool.AccountState, error) {
	return k.pool.States(ctx, owner)
}

// ActiveCounts is the PG store's count (jobs.PGStore.ActiveCounts), in memory:
// every owner's queued and running jobs per agent.
func (m *memJobs) ActiveCounts(_ context.Context, ids []int64) (map[int64]jobs.ActiveCount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	out := map[int64]jobs.ActiveCount{}
	for _, j := range m.rows {
		if !want[j.AgentID] {
			continue
		}
		c := out[j.AgentID]
		switch j.Status {
		case jobs.StatusQueued:
			c.Queued++
		case jobs.StatusRunning:
			c.Running++
		default:
			continue
		}
		out[j.AgentID] = c
	}
	return out, nil
}

type incidentRig struct {
	*jobFixture
	ags  *incidentAgents
	pool *ccpool.Pool
	svc  *ccfailover.Service
	rb   *incidentRebinder
	gw   *accountGateway
}

// The agents of the incident. clever-heron is not in the pool.
const (
	cleverHeronID = int64(30)
	keenFoxID     = int64(31)
	mellowClawID  = int64(32)
)

func newIncidentRig(t *testing.T) *incidentRig {
	t.Helper()
	row := func(id int64, name, pool string) agents.Agent {
		return agents.Agent{ID: id, OwnerID: 1, Name: name, Kind: agents.KindClaudeCode, CCAccount: "devin",
			Repo: "acme/widgets", Status: agents.StatusRunning, KickedOff: true, WorkerPool: pool}
	}
	ags := &incidentAgents{rows: map[int64]agents.Agent{
		cleverHeronID: row(cleverHeronID, "clever-heron", ""),
		keenFoxID:     row(keenFoxID, "keen-fox", incidentPool),
		mellowClawID:  row(mellowClawID, "mellow-claw", incidentPool),
	}}
	src, err := ccpool.NewStatic(incidentTokens)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := ccpool.New(src, &poolMarks{m: map[string]ccpool.Mark{}})
	if err != nil {
		t.Fatal(err)
	}
	rb := &incidentRebinder{ags: ags}
	svc, err := ccfailover.New(ccfailover.Config{Enabled: true, Pool: pool, Agents: ags, Rebinder: rb,
		Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	gw := &accountGateway{limited: map[string]bool{"devin": true}, failure: ccpool.FailureRateLimited, reply: "LGTM"}
	f := &jobFixture{store: newMemJobs(), ags: &ags.jobAgents, ws: &jobWorkspace{files: map[string]string{}}}
	f.s = New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken, ServiceToken: testServiceToken}, log.New(io.Discard, "", 0))
	f.s.UseExtensions(Extensions{
		Notes:           stubNotesStore{},
		Agents:          ags,
		SessionLiveness: stubLiveness{},
		Gateway:         gw,
		Jobs:            f.store,
		JobWorkspace:    f.ws,
		AccountFailover: svc,
		Kinds:           ccPoolKinds{pool: pool},
	})
	f.ts = httptest.NewServer(f.s.Handler())
	t.Cleanup(f.ts.Close)
	return &incidentRig{jobFixture: f, ags: ags, pool: pool, svc: svc, rb: rb, gw: gw}
}

// markLimited marks account rate-limited as ccd's typed failure does (the
// gateway's MarkFailure), with a message stating no reset muster reads, so the
// cooldown is the default hour from now.
func (r *incidentRig) markLimited(t *testing.T, account string) {
	t.Helper()
	if err := r.pool.MarkFailure(context.Background(), 1, account, ccpool.FailureRateLimited, "You've hit your limit"); err != nil {
		t.Fatal(err)
	}
}

// TestAPoolWhoseWorkersAllSitOnARateLimitedAccountRunsTheJobOnTheUsableOne is
// the incident, end to end: clever-heron's turn marks devin and the failover
// moves clever-heron to sara; then a job submitted to the pool — whose two
// workers are both on devin — is ACCEPTED and runs on sara, instead of the 429
// naming devin's reset for each worker that every submission got for ~45 min.
func TestAPoolWhoseWorkersAllSitOnARateLimitedAccountRunsTheJobOnTheUsableOne(t *testing.T) {
	r := newIncidentRig(t)
	ctx := context.Background()
	r.markLimited(t, "devin")
	if res := r.svc.AccountFailed(ctx, cleverHeronID, "devin", ccpool.FailureRateLimited, "You've hit your limit"); !res.Moved || res.To != "sara" {
		t.Fatalf("clever-heron's failover = %+v, want moved to sara (the incident's first log line)", res)
	}
	code, out := r.call(t, http.MethodPost, "/api/jobs", map[string]any{"worker_pool": incidentPool, "prompt": "review"}, true)
	if code != http.StatusAccepted {
		t.Fatalf("POST /api/jobs to the pool = %d %v, want 202: sara is usable, so the pool must not refuse", code, out)
	}
	done := r.await(t, int64(out["job_id"].(float64)))
	if done["status"] != jobs.StatusSucceeded || done["cc_account"] != "sara" {
		t.Fatalf("job = %v on %v (%v), want succeeded on sara", done["status"], done["cc_account"], done["error"])
	}
	if w := done["agent"]; w != "keen-fox" && w != "mellow-claw" {
		t.Fatalf("job ran on %v, want a worker of %s", w, incidentPool)
	}
	if got := strings.Join(r.gw.turns(), ","); got != "sara" {
		t.Fatalf("turns ran on %q, want exactly one, on sara", got)
	}
}

// pin pins each named agent to its current account, without opting in to
// failover, as a dispatch with an explicit cc_account does.
func (r *incidentRig) pin(names ...string) {
	r.ags.rowMu.Lock()
	defer r.ags.rowMu.Unlock()
	for _, n := range names {
		for id, a := range r.ags.rows {
			if a.Name == n {
				a.CCAccountPinned, a.CCFailover = true, false
				r.ags.rows[id] = a
			}
		}
	}
}

// TestAPoolWhosePinnedWorkersAllSitOnARateLimitedAccountRunsTheJobOnTheUsableOne:
// the incident with every agent PINNED to devin (cc_account given at creation,
// no cc_failover). For a pool submission the pin does not hold the pool
// hostage (operator, 2026-11-07: "pool overrides pin"): one worker is moved to
// sara and the job runs there. Nothing else moves: the pinned clever-heron,
// which is in no pool, stays on devin although its own turn failed there, and
// so does the second worker, which the job did not need.
func TestAPoolWhosePinnedWorkersAllSitOnARateLimitedAccountRunsTheJobOnTheUsableOne(t *testing.T) {
	r := newIncidentRig(t)
	ctx := context.Background()
	r.pin("clever-heron", "keen-fox", "mellow-claw")
	r.markLimited(t, "devin")
	if res := r.svc.AccountFailed(ctx, cleverHeronID, "devin", ccpool.FailureRateLimited, "You've hit your limit"); res.Moved {
		t.Fatalf("the pinned clever-heron was moved to %q by its own failed turn; a pin outside a pool holds", res.To)
	}
	code, out := r.call(t, http.MethodPost, "/api/jobs", map[string]any{"worker_pool": incidentPool, "prompt": "review"}, true)
	if code != http.StatusAccepted {
		t.Fatalf("POST /api/jobs to a pool of pinned workers = %d %v, want 202: sara is usable, and a pool overrides a pin", code, out)
	}
	done := r.await(t, int64(out["job_id"].(float64)))
	if done["status"] != jobs.StatusSucceeded || done["agent"] != "keen-fox" || done["cc_account"] != "sara" {
		t.Fatalf("job = %v on %v/%v (%v), want succeeded on keen-fox/sara", done["status"], done["agent"], done["cc_account"], done["error"])
	}
	for name, want := range map[string]string{"keen-fox": "sara", "mellow-claw": "devin", "clever-heron": "devin"} {
		if got := r.ags.account(name); got != want {
			t.Errorf("%s is on %q, want %q", name, got, want)
		}
	}
}

// TestAPoolSubmissionFailsOverAWorkerWhenNoFailoverFollowedTheMark: devin was
// marked by a path that moved nobody (a server start, another server: here,
// the mark alone), so both workers still sit on it when the job arrives. The
// submission fails one idle worker over — the first by name — and the job
// runs on it, on sara.
func TestAPoolSubmissionFailsOverAWorkerWhenNoFailoverFollowedTheMark(t *testing.T) {
	r := newIncidentRig(t)
	r.markLimited(t, "devin")
	code, out := r.call(t, http.MethodPost, "/api/jobs", map[string]any{"worker_pool": incidentPool, "prompt": "review"}, true)
	if code != http.StatusAccepted {
		t.Fatalf("POST /api/jobs to the pool = %d %v, want 202 after failing a worker over to sara", code, out)
	}
	done := r.await(t, int64(out["job_id"].(float64)))
	if done["status"] != jobs.StatusSucceeded || done["agent"] != "keen-fox" || done["cc_account"] != "sara" {
		t.Fatalf("job = %v on %v/%v (%v), want succeeded on keen-fox/sara", done["status"], done["agent"], done["cc_account"], done["error"])
	}
	if got := r.ags.account("keen-fox"); got != "sara" {
		t.Fatalf("keen-fox is on %q, want sara", got)
	}
}

// TestAPoolSubmissionDoesNotFailOverABusyWorker: keen-fox wins on name but has
// a job running, so moving it would restart its pod under that job. The
// submission fails over the idle mellow-claw instead, and keen-fox stays on
// devin (its own next failed turn moves it).
func TestAPoolSubmissionDoesNotFailOverABusyWorker(t *testing.T) {
	r := newIncidentRig(t)
	r.markLimited(t, "devin")
	r.store.mu.Lock()
	r.store.next++
	r.store.rows[r.store.next] = jobs.Job{ID: r.store.next, AgentID: keenFoxID, Agent: "keen-fox", Prompt: "earlier",
		TimeoutSeconds: 60, Status: jobs.StatusRunning, CreatedAt: time.Now()}
	r.store.mu.Unlock()
	code, out := r.call(t, http.MethodPost, "/api/jobs", map[string]any{"worker_pool": incidentPool, "prompt": "review"}, true)
	if code != http.StatusAccepted {
		t.Fatalf("POST /api/jobs to the pool = %d %v, want 202 on mellow-claw", code, out)
	}
	done := r.await(t, int64(out["job_id"].(float64)))
	if done["agent"] != "mellow-claw" || done["cc_account"] != "sara" {
		t.Fatalf("job ran on %v/%v, want mellow-claw/sara", done["agent"], done["cc_account"])
	}
	if got := r.ags.account("keen-fox"); got != "devin" {
		t.Fatalf("the busy keen-fox was moved to %q; want it left on devin", got)
	}
}

// TestAPoolStaysRefusedWhenNoAccountIsUsable: sara is cooling down too, so
// there is nowhere to move anything: the submission is today's 429 naming
// each worker's reset, nothing is recorded and nobody moved.
func TestAPoolStaysRefusedWhenNoAccountIsUsable(t *testing.T) {
	r := newIncidentRig(t)
	ctx := context.Background()
	r.markLimited(t, "sara")
	r.markLimited(t, "devin")
	if res := r.svc.AccountFailed(ctx, cleverHeronID, "devin", ccpool.FailureRateLimited, "You've hit your limit"); res.Moved {
		t.Fatalf("clever-heron moved to %q with every account cooling", res.To)
	}
	until, err := r.pool.CoolingUntil(ctx, 1, "devin")
	if err != nil || until.IsZero() {
		t.Fatalf("devin's cooldown = %v, %v", until, err)
	}
	reset := until.UTC().Format(time.RFC3339)
	code, out := r.call(t, http.MethodPost, "/api/jobs", map[string]any{"worker_pool": incidentPool, "prompt": "review"}, true)
	want := fmt.Sprintf(`no worker in pool %q can take the job: `+
		`keen-fox (its Claude account "devin" is cooling down after a rate limit until %s); `+
		`mellow-claw (its Claude account "devin" is cooling down after a rate limit until %s)`, incidentPool, reset, reset)
	if code != http.StatusTooManyRequests || out["error"] != want {
		t.Fatalf("POST /api/jobs = %d %v\nwant 429 %q", code, out, want)
	}
	if n := r.store.count(); n != 0 {
		t.Fatalf("%d jobs recorded, want 0", n)
	}
	if m := r.rb.moves(); len(m) != 0 {
		t.Fatalf("moves = %v, want none", m)
	}
}

// noCounter hides the job store's ActiveCounts.
type noCounter struct{ jobs.Store }

// TestHoldIdleAgentFailsClosed: an agent is idle only when nothing says
// otherwise — a job of this process holding its lane, a job counted in the
// store, a store that cannot count — and an idle one's lane stays held until
// released.
func TestHoldIdleAgentFailsClosed(t *testing.T) {
	r := newIncidentRig(t)
	ctx := context.Background()
	fox, _ := r.ags.Get(ctx, keenFoxID)
	release, idle, why := r.s.holdIdleAgent(ctx, fox)
	if !idle || release == nil {
		t.Fatalf("an agent with no job: idle %v (%q), want idle", idle, why)
	}
	if _, again, why := r.s.holdIdleAgent(ctx, fox); again || why != "a job is running on it" {
		t.Fatalf("while held: idle %v (%q), want not idle: its lane is held", again, why)
	}
	release()
	r.s.ext.Jobs = noCounter{r.store}
	if _, idle, why := r.s.holdIdleAgent(ctx, fox); idle || why != "the job store cannot count its jobs" {
		t.Fatalf("a store that cannot count: idle %v (%q), want not idle", idle, why)
	}
	if rel, idle, _ := r.s.holdIdleAgent(ctx, fox); idle {
		rel()
	}
	if rel, ok := r.s.jobLanes.tryLock(keenFoxID); !ok {
		t.Fatal("a refused hold left the lane locked")
	} else {
		rel()
	}
}
