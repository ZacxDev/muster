package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/plugins"
)

// Round 1 of #428's audit: the attention offer names a target, a pool
// worker stays counted while it starts, and a ledger write that fails is
// retried and then said.

// --- 1. the attention offer: one option per target -----------------------------

// TestTheAttentionOfferNamesATargetAndTheOwnersClickBindsIt: the click path.
// task_route_attention offers agent_dispatch once per target that exists now
// (each label names its target); the owner picks the pod/gateway one on
// /attention; that answer reaches the chief on a turn the owner did not type,
// so owner_asked cannot be passed — and agent_dispatch on the task, with no
// owner_asked and EXACTLY the target the owner chose, starts that agent. Any
// other target on that task is refused with the owner's choice named.
func TestTheAttentionOfferNamesATargetAndTheOwnersClickBindsIt(t *testing.T) {
	f := newReleaseFixture(t)
	n := f.task(t, "click path")
	text := f.call(f.chief, taskRouteAttentionTool, map[string]any{"task_id": n.ID, "note": "where should it run?"})
	var out struct {
		ID      int64    `json:"id"`
		Offered []string `json:"actions_offered"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.ID <= 0 {
		t.Fatalf("task_route_attention: %s", text)
	}
	ask, err := f.srv.ext.Attention.Get(f.asOwner, out.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"agent_dispatch on pod/gateway: start a new agent on it", "agent_dispatch on pod/claude-code: start a new agent on it"}
	if strings.Join(ask.Options, "|") != strings.Join(want, "|") {
		t.Fatalf("the ask's options %q\nwant one per target %q", ask.Options, want)
	}
	// The owner clicks the gateway option on /attention.
	if _, err := f.srv.ext.Attention.Answer(f.asOwner, out.ID, want[0]); err != nil {
		t.Fatal(err)
	}
	// The answer is delivered as a turn the owner did not type: owner_asked
	// is refused there (R1 stays shut) …
	f.say(t, f.chief, "Your owner answered: "+want[0])
	f.mustRefuse(t, f.callRaw(f.chief, agentDispatchTool, map[string]any{"target": "pod/gateway", "task_id": n.ID, "owner_asked": true}),
		ownerAskedNoTaskNoOwnerTurn+ownerAskedUngatedTail, n.ID)
	// … another target than the owner's choice is refused, naming it …
	f.mustRefuse(t, f.callRaw(f.chief, agentDispatchTool, map[string]any{"target": "pod/claude-code", "task_id": n.ID}),
		`your owner chose pod/gateway for this task`, n.ID)
	// … and exactly the chosen one starts.
	got := f.startedOne(t, f.callRaw(f.chief, agentDispatchTool, map[string]any{"target": "pod/gateway", "task_id": n.ID}), n.ID)
	if got.Kind != agents.KindGateway {
		t.Errorf("agent kind %q, want the gateway the owner chose", got.Kind)
	}
}

// --- 2. a pool worker with no prompt stays counted while it starts --------------

// adapterLikeProvisioner plays agentprovision.Adapter's status writes: a save
// (Dispatch, kickoff false) stores `stopped`; Start stores `provisioning`. Its
// Start reads the chief's live count TWICE, at the two ends of the window
// between the agent's row and its ledger record (agentRecordGate orders them):
// once BEFORE agent_dispatch records the agent (the start counts as a pending
// reservation), once AFTER (it counts as a recorded live agent). No sleep: each
// read waits on the other side's signal.
type adapterLikeProvisioner struct {
	recordingProvisioner
	store     *agents.PGStore
	ledger    plugins.DispatchStore
	gate      *agentRecordGate
	mu        sync.Mutex
	liveAtRun []int
	saves     int
}

func (p *adapterLikeProvisioner) Dispatch(ctx context.Context, id int64, kickoff bool) error {
	if !kickoff {
		p.mu.Lock()
		p.saves++
		p.mu.Unlock()
		if err := p.store.UpdateStatus(ctx, id, agents.StatusStopped, "", ""); err != nil {
			return err
		}
	}
	return p.recordingProvisioner.Dispatch(ctx, id, kickoff)
}

func (p *adapterLikeProvisioner) readLive(ctx context.Context) error {
	b, err := p.ledger.ChiefDispatchBudget(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.liveAtRun = append(p.liveAtRun, b.Live)
	p.mu.Unlock()
	return nil
}

func (p *adapterLikeProvisioner) Start(ctx context.Context, id int64) error {
	if err := p.readLive(ctx); err != nil { // before the agent is recorded
		return err
	}
	close(p.gate.readBefore)
	if !p.gate.wait(p.gate.recorded) {
		return errors.New("the agent was never recorded")
	}
	if err := p.readLive(ctx); err != nil { // after it is recorded
		return err
	}
	if err := p.store.UpdateStatus(ctx, id, agents.StatusProvisioning, "", ""); err != nil {
		return err
	}
	return p.recordingProvisioner.Start(ctx, id)
}

// agentRecordGate is the ledger with RecordChiefDispatchAgent held until the
// provisioner's first read (readBefore), and announcing its first successful
// commit (recorded) — closed once, so a retried record cannot close it twice.
type agentRecordGate struct {
	plugins.DispatchStore
	readBefore, recorded chan struct{}
	recordedOnce         sync.Once
}

// wait reports whether ch closed; the bound is a deadlock detector (a run
// that never reaches the other side fails), not a timing assumption.
func (g *agentRecordGate) wait(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(20 * time.Second):
		return false
	}
}

func (g *agentRecordGate) RecordChiefDispatchAgent(ctx context.Context, object string, id int64) error {
	if !g.wait(g.readBefore) {
		return errors.New("the provisioner never read the live count")
	}
	err := g.DispatchStore.RecordChiefDispatchAgent(ctx, object, id)
	if err == nil {
		g.recordedOnce.Do(func() { close(g.recorded) })
	}
	return err
}

// TestAPoolWorkerStaysLiveWhileItStarts: the pool worker is never saved
// `stopped` on its way up, so the live count the caps compare includes it on
// BOTH sides of its ledger record while its Start runs — a concurrent dispatch
// cannot pass the cap in that window. (Saved `stopped` first, the read after
// the record is one short: neither a live agent nor a pending reservation.)
//
// ⚠ IT PINS ONLY THE START PATH'S ORDERING. Its reads are serialised against
// the record by the gate, so it cannot see the budget's own snapshot race (a
// read straddling the record's commit); that race is covered solely by
// TestTheBudgetNeverMissesAStartWhoseAgentIsBeingRecorded
// (internal/plugins/chief_budget_snapshot_test.go).
func TestAPoolWorkerStaysLiveWhileItStarts(t *testing.T) {
	f := newReleaseFixture(t)
	gate := &agentRecordGate{DispatchStore: f.plugins, readBefore: make(chan struct{}), recorded: make(chan struct{})}
	prov := &adapterLikeProvisioner{store: agents.NewPG(f.pool), ledger: f.plugins, gate: gate}
	f.srv.ext.Provisioner, f.srv.ext.ChiefDispatch = prov, gate
	f.ownerTurn(t, f.chief, f.owner, "Start a worker in pool reviewers.")
	live0 := f.budget(t).Live
	text := f.callRaw(f.chief, agentDispatchTool, map[string]any{"target": "pod/claude-code", "worker_pool": "reviewers",
		"request_key": "pool-live-1", "owner_asked": true})
	if strings.Contains(text, `"error"`) {
		t.Fatalf("pool worker: %s", text)
	}
	if ids := prov.startedIDs(1); len(ids) != 1 {
		t.Fatalf("starts %v, want one", ids)
	}
	prov.mu.Lock()
	defer prov.mu.Unlock()
	if prov.saves != 0 {
		t.Errorf("the pool worker was saved stopped %d time(s) on its way up", prov.saves)
	}
	if len(prov.liveAtRun) != 2 || prov.liveAtRun[0] != live0+1 || prov.liveAtRun[1] != live0+1 {
		t.Errorf("live count during Start, before and after the agent's record %v, want [%d %d]: the starting worker dropped out of the cap",
			prov.liveAtRun, live0+1, live0+1)
	}
}

// --- 3. a ledger write that fails is retried once, then said ---------------------

// flakyLedger fails RecordChiefDispatchFailed / RecordChiefDispatchAgent the
// first failMarker / failAgent times.
type flakyLedger struct {
	plugins.DispatchStore
	mu                    sync.Mutex
	failMarker, failAgent int
}

var errLedgerDown = errors.New("the ledger is down")

func (l *flakyLedger) RecordChiefDispatchFailed(ctx context.Context, object string, id int64) error {
	l.mu.Lock()
	fail := l.failMarker > 0
	l.failMarker--
	l.mu.Unlock()
	if fail {
		return errLedgerDown
	}
	return l.DispatchStore.RecordChiefDispatchFailed(ctx, object, id)
}

func (l *flakyLedger) RecordChiefDispatchAgent(ctx context.Context, object string, id int64) error {
	l.mu.Lock()
	fail := l.failAgent > 0
	l.failAgent--
	l.mu.Unlock()
	if fail {
		return errLedgerDown
	}
	return l.DispatchStore.RecordChiefDispatchAgent(ctx, object, id)
}

// TestALedgerWriteIsRetriedOnceThenSaid: one failure of either write is
// retried and leaves the ledger right (a failed start's replay answers
// started: false; a started agent's replay names it). Two failures are said to
// the chief: the outcome a retry would read is unknown, never a silent
// "starting".
func TestALedgerWriteIsRetriedOnceThenSaid(t *testing.T) {
	cases := []struct {
		name          string
		failMarker    int
		failAgent     int
		createFails   bool
		wantError     string
		replayCarries string
	}{
		{"marker fails once", 1, 0, true, "the agent could not be started; use task_route_attention", `"started":false`},
		{"marker fails twice", 2, 0, true, "muster could not record that this start failed", ""},
		{"agent record fails once", 0, 1, false, "", `"agent_id"`},
		{"agent record fails twice", 0, 2, false, "but muster could not record it as this start's agent", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newReleaseFixture(t)
			f.srv.ext.ChiefDispatch = &flakyLedger{DispatchStore: f.plugins, failMarker: c.failMarker, failAgent: c.failAgent}
			if c.createFails {
				f.srv.ext.Agents = failingCreate{agents.NewPG(f.pool)}
			}
			f.ownerTurn(t, f.chief, f.owner, "Start it.")
			args := noTask("flaky-1", "flaky work")
			text := f.callRaw(f.chief, agentDispatchTool, args)
			if c.wantError == "" {
				if strings.Contains(text, `"error"`) {
					t.Fatalf("first call: %s; want a start", text)
				}
			} else if got := errorText(t, text); !strings.Contains(got, c.wantError) {
				t.Fatalf("first call: error %q\nwant one carrying %q", got, c.wantError)
			}
			if c.replayCarries != "" {
				again := f.callRaw(f.chief, agentDispatchTool, args)
				if !strings.Contains(again, `"replayed":true`) || !strings.Contains(again, c.replayCarries) {
					t.Errorf("replay %s; want it to carry %s", again, c.replayCarries)
				}
			}
		})
	}
}
