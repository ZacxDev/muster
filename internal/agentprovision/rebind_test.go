package agentprovision

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// THE ACCOUNT FAILOVER'S LIFECYCLE HALF (rebind.go): the move is conditional
// on the account being left, the spec is re-applied with the NEW account's
// token, and a Stop cannot interleave with it.

const (
	rebindTokNorth = "sk-ant-oat01-FAKE-rebind-north-5c21"
	rebindTokSouth = "sk-ant-oat01-FAKE-rebind-south-8e07"
)

// rebindStore is a recordingStore whose row can be moved (RebindCCAccount,
// conditionally on from, as the PG store does) and is guarded for the
// concurrent Stop test.
type rebindStore struct {
	*recordingStore
	mu     sync.Mutex
	moves  []string
	status []string
}

func (s *rebindStore) Get(ctx context.Context, id int64) (agents.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordingStore.Get(ctx, id)
}

func (s *rebindStore) UpdateStatus(ctx context.Context, id int64, status, out, msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordingStore.agent.Status = status
	s.status = append(s.status, status)
	return s.recordingStore.UpdateStatus(ctx, id, status, out, msg)
}

func (s *rebindStore) ClearKickoffDelivery(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordingStore.ClearKickoffDelivery(ctx, id)
}

func (s *rebindStore) RebindCCAccount(_ context.Context, id int64, from, to string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recordingStore.agent.CCAccount != from {
		return false, nil
	}
	s.recordingStore.agent.CCAccount = to
	s.moves = append(s.moves, from+"->"+to)
	return true, nil
}

// gatedDriver is a specCapture whose Update can be held, so a test can start a
// Stop while a rebind's re-apply is in flight.
type gatedDriver struct {
	specCapture
	mu      sync.Mutex
	order   []string
	gate    chan struct{}
	entered chan struct{}
}

func (d *gatedDriver) note(s string) { d.mu.Lock(); d.order = append(d.order, s); d.mu.Unlock() }

func (d *gatedDriver) Update(ctx context.Context, s provision.Spec) error {
	if d.entered != nil {
		close(d.entered)
		d.entered = nil
	}
	if d.gate != nil {
		<-d.gate
	}
	d.note("update:" + tokenIn(s))
	return d.specCapture.Update(ctx, s)
}

func (d *gatedDriver) Scale(ctx context.Context, r provision.Ref, n int) error {
	d.note("scale")
	return d.specCapture.Scale(ctx, r, n)
}

func rebindFixture(t *testing.T) (*Adapter, *rebindStore, *gatedDriver) {
	t.Helper()
	row := fixtureAgent()
	row.Kind, row.CCAccount, row.HooksToken, row.Repo, row.RepoBranch, row.Model = agents.KindClaudeCode, "north", "fixture-hooks-rebind", "", "", ""
	row.GatewaySecret = "fixture-turn-credential-rebind"
	row.Status, row.KickedOff = agents.StatusRunning, true
	store := &rebindStore{recordingStore: &recordingStore{agent: row}}
	driver := &gatedDriver{specCapture: specCapture{Noop: provision.MustNewNoop()}}
	cfg := fixtureSpecConfig()
	cfg.ClaudeCode = &agentspec.ClaudeCodeConfig{Image: "ghcr.io/example-org/claude-code-agent:1",
		Accounts: map[string]string{"north": rebindTokNorth, "south": rebindTokSouth}}
	a, err := New(Config{Driver: driver, Store: store, Spec: cfg, KickoffDeliverable: true})
	if err != nil {
		t.Fatal(err)
	}
	return a, store, driver
}

// TestARebindMovesTheRowAndReappliesTheSpecWithTheNewAccountsToken: the row
// moves north → south, the re-applied spec carries south's token (never north's),
// and a kicked-off agent ends running.
func TestARebindMovesTheRowAndReappliesTheSpecWithTheNewAccountsToken(t *testing.T) {
	a, store, driver := rebindFixture(t)
	var prepared string
	res, err := a.Rebind(context.Background(), fixtureAgentID, "north", "south", func(_ context.Context, ag agents.Agent) error {
		prepared = ag.CCAccount
		return nil
	})
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if !res.Rebound || res.Current != "south" {
		t.Fatalf("result = %+v, want rebound onto south", res)
	}
	if len(store.moves) != 1 || store.moves[0] != "north->south" {
		t.Fatalf("moves = %v, want one north->south", store.moves)
	}
	if prepared != "south" {
		t.Fatalf("prepare saw account %q, want the new one", prepared)
	}
	if driver.updates != 1 || tokenIn(driver.last) != rebindTokSouth {
		t.Fatalf("%d update(s), last token %q; want one re-apply carrying south's token", driver.updates, tokenIn(driver.last))
	}
	if got := store.recordingStore.agent.Status; got != agents.StatusRunning {
		t.Fatalf("status = %q, want running for a kicked-off agent", got)
	}
}

// TestARebindOffAnAccountTheAgentIsNoLongerOnDoesNothing: the idempotency key
// is (agent, from) — an agent already moved off north is not moved or restarted
// by a second rebind from north.
func TestARebindOffAnAccountTheAgentIsNoLongerOnDoesNothing(t *testing.T) {
	a, store, driver := rebindFixture(t)
	if _, err := a.Rebind(context.Background(), fixtureAgentID, "north", "south", nil); err != nil {
		t.Fatal(err)
	}
	res, err := a.Rebind(context.Background(), fixtureAgentID, "north", "south", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rebound || res.Current != "south" {
		t.Fatalf("second rebind = %+v, want not rebound, current south", res)
	}
	if driver.updates != 1 || len(store.moves) != 1 {
		t.Fatalf("%d update(s), moves %v after two rebinds from north; want one of each", driver.updates, store.moves)
	}
}

// TestARebindOfAStoppedAgentDoesNothing: a rebind never brings back an agent
// that is not live.
func TestARebindOfAStoppedAgentDoesNothing(t *testing.T) {
	a, store, driver := rebindFixture(t)
	store.recordingStore.agent.Status = agents.StatusStopped
	res, err := a.Rebind(context.Background(), fixtureAgentID, "north", "south", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rebound || driver.updates != 0 || len(store.moves) != 0 {
		t.Fatalf("rebind of a stopped agent: %+v, %d update(s), moves %v; want nothing", res, driver.updates, store.moves)
	}
}

// TestAStopDuringARebindWaitsAndEndsStopped: a Stop that arrives while a
// rebind's re-apply is in flight runs after it — the backend sees the
// re-apply, then the scale to zero — and the agent ends stopped, not
// restarted behind the operator's back.
func TestAStopDuringARebindWaitsAndEndsStopped(t *testing.T) {
	a, store, driver := rebindFixture(t)
	driver.gate, driver.entered = make(chan struct{}), make(chan struct{})
	entered := driver.entered
	done := make(chan error, 1)
	go func() {
		_, err := a.Rebind(context.Background(), fixtureAgentID, "north", "south", nil)
		done <- err
	}()
	<-entered
	stopped := make(chan error, 1)
	go func() { stopped <- a.Stop(context.Background(), fixtureAgentID) }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned (%v) while the rebind's re-apply was still in flight", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(driver.gate)
	if err := <-done; err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("stop: %v", err)
	}
	driver.mu.Lock()
	order := append([]string(nil), driver.order...)
	driver.mu.Unlock()
	if len(order) != 2 || order[0] != "update:"+rebindTokSouth || order[1] != "scale" {
		t.Fatalf("backend order = %v, want the re-apply then the scale to zero", order)
	}
	if got := store.recordingStore.agent.Status; got != agents.StatusStopped {
		t.Fatalf("final status = %q, want stopped", got)
	}
}
