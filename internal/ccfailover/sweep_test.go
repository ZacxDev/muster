package ccfailover

import (
	"bytes"
	"context"
	"errors"
	"log"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccpool"
)

// THE ACCOUNT SWEEP: an account failure moves every OTHER idle agent of the
// owner's on that account, not only the agent whose turn failed — over the
// REAL ccpool.Pool, a multi-row agents store and a CONDITIONAL rebinder (like
// agents.PGStore.RebindCCAccount).

type rowsAgents struct {
	mu   sync.Mutex
	rows map[int64]agents.Agent
}

func (s *rowsAgents) Get(_ context.Context, id int64) (agents.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.rows[id]
	if !ok {
		return agents.Agent{}, errors.New("no such agent")
	}
	return a, nil
}

func (s *rowsAgents) List(context.Context) ([]agents.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []agents.Agent
	for _, a := range s.rows {
		out = append(out, a)
	}
	return out, nil
}

func (s *rowsAgents) accounts() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for _, a := range s.rows {
		out[a.Name] = a.CCAccount
	}
	return out
}

// noList hides List: an agents store that cannot list.
type noList struct{ a *rowsAgents }

func (n noList) Get(ctx context.Context, id int64) (agents.Agent, error) { return n.a.Get(ctx, id) }

type rowsRebinder struct {
	ags   *rowsAgents
	mu    sync.Mutex
	moved []string
}

func (r *rowsRebinder) Rebind(_ context.Context, id int64, from, to string, _ func(context.Context, agents.Agent) error) (agentprovision.RebindResult, error) {
	r.ags.mu.Lock()
	a := r.ags.rows[id]
	if a.CCAccount != from {
		r.ags.mu.Unlock()
		return agentprovision.RebindResult{Current: a.CCAccount, Skipped: "moved first"}, nil
	}
	a.CCAccount = to
	r.ags.rows[id] = a
	r.ags.mu.Unlock()
	r.mu.Lock()
	r.moved = append(r.moved, a.Name)
	r.mu.Unlock()
	return agentprovision.RebindResult{Rebound: true, Current: to, PrevInstance: "pod-" + a.Name}, nil
}

func (r *rowsRebinder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.moved...)
	sort.Strings(out)
	return out
}

// holdLog is a Hold: busy names the agents with a job; it records each hold
// taken and each release.
type holdLog struct {
	busy map[string]bool
	mu   sync.Mutex
	held []string
	rel  []string
}

func (h *holdLog) hold(_ context.Context, a agents.Agent) (func(), bool, string) {
	if h.busy[a.Name] {
		return nil, false, "1 job(s) queued or running"
	}
	h.mu.Lock()
	h.held = append(h.held, a.Name)
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		h.rel = append(h.rel, a.Name)
		h.mu.Unlock()
	}, true, ""
}

const (
	failingID = int64(100)
	sweepOwn  = int64(1)
)

type sweepRig struct {
	svc   *Service
	pool  *ccpool.Pool
	ags   *rowsAgents
	rb    *rowsRebinder
	h     *holdLog
	logs  *bytes.Buffer
	spawn []func()
}

// newSweepRig: north (rate-limited now) and south (usable). On north: the
// agent whose turn failed, an idle one, a busy one, a pinned one, a pinned one
// that opted in, a stopped one, a gateway-kind one, and another OWNER's; on
// south, one more.
func newSweepRig(t *testing.T, enabled, syncSpawn bool) *sweepRig {
	t.Helper()
	row := func(id int64, name, acct string) agents.Agent {
		return agents.Agent{ID: id, OwnerID: sweepOwn, Name: name, Kind: agents.KindClaudeCode, CCAccount: acct, Status: agents.StatusRunning}
	}
	pinned := row(4, "d-pinned", "north")
	pinned.CCAccountPinned = true
	optIn := row(5, "e-pinned-optin", "north")
	optIn.CCAccountPinned, optIn.CCFailover = true, true
	stopped := row(6, "f-stopped", "north")
	stopped.Status = agents.StatusStopped
	gw := row(7, "g-gateway", "north")
	gw.Kind = agents.KindGateway
	other := row(8, "h-other-owner", "north")
	other.OwnerID = 2
	ags := &rowsAgents{rows: map[int64]agents.Agent{
		failingID: row(failingID, "a-failing", "north"),
		2:         row(2, "b-idle", "north"),
		3:         row(3, "c-busy", "north"),
		4:         pinned,
		5:         optIn,
		6:         stopped,
		7:         gw,
		8:         other,
		9:         row(9, "i-south", "south"),
	}}
	src, err := ccpool.NewStatic(map[string]string{"north": testTokens["north"], "south": testTokens["south"]})
	if err != nil {
		t.Fatal(err)
	}
	marks := &memMarks{m: map[string]ccpool.Mark{}}
	pool, err := ccpool.New(src, marks)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.MarkFailure(context.Background(), sweepOwn, "north", ccpool.FailureRateLimited, "You've hit your limit"); err != nil {
		t.Fatal(err)
	}
	r := &sweepRig{pool: pool, ags: ags, rb: &rowsRebinder{ags: ags}, h: &holdLog{busy: map[string]bool{"c-busy": true}}, logs: &bytes.Buffer{}}
	cfg := Config{Enabled: enabled, Pool: pool, Agents: ags, Rebinder: r.rb, Logger: log.New(r.logs, "", 0)}
	if syncSpawn {
		cfg.Spawn = func(f func()) { f() }
	} else {
		cfg.Spawn = func(f func()) { r.spawn = append(r.spawn, f) }
	}
	if r.svc, err = New(cfg); err != nil {
		t.Fatal(err)
	}
	r.svc.UseHold(r.h.hold)
	return r
}

// TestAnAccountFailureMovesEveryIdleAgentOnTheAccount: a-failing's turn fails
// rate_limited on north. It moves (as before), and so does every other agent
// failover may move that is idle: b-idle and the opted-in e-pinned-optin. Left
// on north: the busy c-busy (its own next failed turn moves it), the pinned
// d-pinned, the stopped f-stopped, the gateway-kind g-gateway, and another
// owner's agent; i-south was never on north. Each moved agent was held and
// released; the busy one was never held.
func TestAnAccountFailureMovesEveryIdleAgentOnTheAccount(t *testing.T) {
	r := newSweepRig(t, true, true)
	res := r.svc.AccountFailed(context.Background(), failingID, "north", ccpool.FailureRateLimited, "You've hit your limit")
	if !res.Moved || res.To != "south" {
		t.Fatalf("the failing agent's own failover = %+v, want moved to south", res)
	}
	if got := strings.Join(r.rb.names(), ","); got != "a-failing,b-idle,e-pinned-optin" {
		t.Fatalf("moved %q, want a-failing,b-idle,e-pinned-optin", got)
	}
	want := map[string]string{"a-failing": "south", "b-idle": "south", "c-busy": "north", "d-pinned": "north",
		"e-pinned-optin": "south", "f-stopped": "north", "g-gateway": "north", "h-other-owner": "north", "i-south": "south"}
	got := r.ags.accounts()
	for name, acct := range want {
		if got[name] != acct {
			t.Errorf("%s is on %q, want %q", name, got[name], acct)
		}
	}
	r.h.mu.Lock()
	held, rel := strings.Join(r.h.held, ","), strings.Join(r.h.rel, ",")
	r.h.mu.Unlock()
	if held != "b-idle,e-pinned-optin" || rel != held {
		t.Fatalf("held %q, released %q; want b-idle,e-pinned-optin both", held, rel)
	}
	if !strings.Contains(r.logs.String(), "left 1 busy one(s) to move at their next failed turn [c-busy (1 job(s) queued or running)]") {
		t.Fatalf("the sweep's log line does not name the busy agent left behind:\n%s", r.logs.String())
	}
}

// TestTheSweepMovesNothingWithoutAUsableAccountOrAHold: south cooling too,
// failover off, no hold, or an agents store that cannot list — the sweep moves
// none of the other agents (the failing agent's own outcome is AccountFailed's
// as before).
func TestTheSweepMovesNothingWithoutAUsableAccountOrAHold(t *testing.T) {
	ctx := context.Background()
	t.Run("no usable account", func(t *testing.T) {
		r := newSweepRig(t, true, true)
		if err := r.pool.MarkFailure(ctx, sweepOwn, "south", ccpool.FailureRateLimited, "You've hit your limit"); err != nil {
			t.Fatal(err)
		}
		r.svc.AccountFailed(ctx, failingID, "north", ccpool.FailureRateLimited, "x")
		if got := r.rb.names(); len(got) != 0 {
			t.Fatalf("moved %v with every account cooling", got)
		}
		if out := r.svc.SweepAccount(ctx, sweepOwn, "north", ccpool.FailureRateLimited, "x", failingID); out.Why != "no other Claude account is usable" {
			t.Fatalf("why = %q", out.Why)
		}
	})
	t.Run("failover off", func(t *testing.T) {
		r := newSweepRig(t, false, true)
		r.svc.AccountFailed(ctx, failingID, "north", ccpool.FailureRateLimited, "x")
		if got := r.rb.names(); len(got) != 0 {
			t.Fatalf("moved %v with failover off", got)
		}
		if out := r.svc.SweepAccount(ctx, sweepOwn, "north", ccpool.FailureRateLimited, "x", failingID); out.Why != "account failover is off" {
			t.Fatalf("why = %q", out.Why)
		}
	})
	t.Run("no hold", func(t *testing.T) {
		r := newSweepRig(t, true, false)
		r.svc.UseHold(nil)
		r.svc.AccountFailed(ctx, failingID, "north", ccpool.FailureRateLimited, "x")
		if len(r.spawn) != 0 {
			t.Fatalf("%d sweep(s) started in the background with no hold installed, want none", len(r.spawn))
		}
		// Asked directly, the sweep refuses too.
		out := r.svc.SweepAccount(ctx, sweepOwn, "north", ccpool.FailureRateLimited, "x", failingID)
		if got := strings.Join(r.rb.names(), ","); got != "a-failing" || out.Why != "no job runner to tell an idle agent from a busy one" {
			t.Fatalf("moved %q (why %q) with no hold installed, want only a-failing (fail closed)", got, out.Why)
		}
	})
	t.Run("cannot list", func(t *testing.T) {
		r := newSweepRig(t, true, true)
		svc, err := New(Config{Enabled: true, Pool: r.pool, Agents: noList{r.ags}, Rebinder: r.rb, Spawn: func(f func()) { f() }})
		if err != nil {
			t.Fatal(err)
		}
		svc.UseHold(r.h.hold)
		svc.AccountFailed(ctx, failingID, "north", ccpool.FailureRateLimited, "x")
		if got := strings.Join(r.rb.names(), ","); got != "a-failing" {
			t.Fatalf("moved %q, want only a-failing", got)
		}
	})
	t.Run("not an account failure", func(t *testing.T) {
		r := newSweepRig(t, true, true)
		if out := r.svc.SweepAccount(ctx, sweepOwn, "north", "timeout", "x", failingID); out.Why != "not an account failure" || len(r.rb.names()) != 0 {
			t.Fatalf("a timeout swept: why %q, moved %v", out.Why, r.rb.names())
		}
	})
}

// TestAnAuthFailureSweepsTheAccountToo: auth_failed is an account failure
// like rate_limited.
func TestAnAuthFailureSweepsTheAccountToo(t *testing.T) {
	r := newSweepRig(t, true, true)
	ctx := context.Background()
	r.svc.AccountFailed(ctx, failingID, "north", ccpool.FailureAuthFailed, "invalid token")
	if got := strings.Join(r.rb.names(), ","); got != "a-failing,b-idle,e-pinned-optin" {
		t.Fatalf("moved %q, want a-failing,b-idle,e-pinned-optin", got)
	}
}

// TestOneSweepAtATimePerAccount: a second failure on north while its sweep is
// still running starts no second sweep; once it ends, the next failure starts
// one again.
func TestOneSweepAtATimePerAccount(t *testing.T) {
	r := newSweepRig(t, true, false)
	ctx := context.Background()
	r.svc.AccountFailed(ctx, failingID, "north", ccpool.FailureRateLimited, "x")
	r.svc.AccountFailed(ctx, 3, "north", ccpool.FailureRateLimited, "x")
	if len(r.spawn) != 1 {
		t.Fatalf("%d sweeps started for north while one was running, want 1", len(r.spawn))
	}
	r.spawn[0]()
	r.svc.AccountFailed(ctx, 3, "north", ccpool.FailureRateLimited, "x")
	if len(r.spawn) != 2 {
		t.Fatalf("%d sweeps started after the first ended, want 2", len(r.spawn))
	}
}

// TestRebindingIsTheAgentsLatestMoveForAWhile: a job about to run on a
// just-moved agent asks Rebinding, which answers the move (and the instance it
// replaced) for memoTTL, then nothing.
func TestRebindingIsTheAgentsLatestMoveForAWhile(t *testing.T) {
	r := newSweepRig(t, true, true)
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	r.svc.cfg.Now = func() time.Time { return now }
	if _, ok := r.svc.Rebinding(2); ok {
		t.Fatal("Rebinding answered before any move")
	}
	r.svc.AccountFailed(context.Background(), failingID, "north", ccpool.FailureRateLimited, "x")
	got, ok := r.svc.Rebinding(2)
	if !ok || got.To != "south" || got.AgentName != "b-idle" || got.PrevInstance != "pod-b-idle" {
		t.Fatalf("Rebinding(b-idle) = %+v, %v; want its move to south replacing pod-b-idle", got, ok)
	}
	if _, ok := r.svc.Rebinding(3); ok {
		t.Fatal("Rebinding answered for the busy agent, which was not moved")
	}
	now = now.Add(memoTTL)
	if _, ok := r.svc.Rebinding(2); ok {
		t.Fatal("Rebinding still answered memoTTL after the move")
	}
}
