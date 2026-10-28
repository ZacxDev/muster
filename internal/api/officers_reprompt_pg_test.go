package api

import (
	"bytes"
	"context"
	"errors"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agentkickoff"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/provision"
)

// repromptChat is an api.Gateway recording each Chat (the re-prompt's send).
type repromptChat struct {
	stubGateway
	mu   sync.Mutex
	sent []string
}

func (g *repromptChat) Chat(_ context.Context, a agents.Agent, _, message string, _ func(string)) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sent = append(g.sent, a.Name+": "+message)
	return "on it", nil
}

func (g *repromptChat) count() int { g.mu.Lock(); defer g.mu.Unlock(); return len(g.sent) }

// kickoffGW is the kickoff deliverer's gateway: it records every first-turn send.
type kickoffGW struct {
	mu    sync.Mutex
	sends int
}

func (g *kickoffGW) Resolve(context.Context, agents.Agent) (agentgateway.Target, error) {
	return agentgateway.Target{}, nil
}

func (g *kickoffGW) Send(context.Context, agentgateway.Target, string, string, func(string)) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sends++
	return "ok", nil
}

func (g *kickoffGW) Checkout(context.Context, agentgateway.Target) (agents.CheckoutStatus, error) {
	return agents.CheckoutStatus{}, nil
}

// readyProv reports one ready, running instance by name.
type readyProv struct {
	stubProvisioner
	name string
}

func (p readyProv) Instances(context.Context) ([]provision.Instance, error) {
	return []provision.Instance{{Ref: provision.Ref{Name: p.name}, InstanceID: p.name + "-pod", Phase: provision.PhaseRunning, Ready: true, Replicas: 1}}, nil
}

// TestARepromptDuringARunningFirstTurnNeverStrandsTheInstance is the round-2
// audit's case, against the REAL store and the REAL kickoff deliverer.
//
// The instance's first turn is still running (the deliverer holds its session
// claim, as it does for a whole turn — up to ~37 minutes) when the loop sees it
// up with no live claim and re-prompts it. More than ProvisioningStuckTimeout
// (15 min) of that turn then passes. A re-prompt that changed the kickoff state
// (kicked_off=false, a fresh updated_at) is read by the deliverer's dwell check
// as a first turn that never landed and marks the instance `error` — which then
// holds its slot for ever. The fix sends a re-prompt as its OWN turn under the
// session claim and touches no kickoff state, so:
//
//   - while the first turn runs, the re-prompt answers ErrInstanceBusy and
//     changes nothing;
//   - 16 minutes on, the deliverer's tick leaves the row alone (not error) and
//     re-sends nothing;
//   - once the first turn ends (its claim released), the re-prompt is sent
//     exactly once, and its claim is released after.
func TestARepromptDuringARunningFirstTurnNeverStrandsTheInstance(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 60*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pg := agents.NewPG(pool)

	prefix := "offr-" + time.Now().Format("150405.000") + "-"
	o := officers.Officer{ID: offB, HQURL: "http://hq.invalid/api/v1", MaxInstances: 1, IdleAfter: time.Hour, PollInterval: time.Minute, Owner: 1}.WithToken(offBTok)
	name := officers.InstanceName(o.ID, 1)
	row, err := pg.Create(ctx, agents.Agent{Name: name, Namespace: prefix + name, Status: agents.StatusRunning,
		Kind: agents.KindClaudeCode, CCAccount: "acct-one", PendingNote: officers.KickoffText(o, 1), HooksToken: "tok-" + prefix})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(dbtest.AsOperator(context.Background()), `DELETE FROM agents WHERE id=$1`, row.ID)
	})
	// The first turn was stamped and handed over, and is STILL RUNNING: the
	// deliverer holds the session claim for the turn's whole length.
	if err := pg.SetKickedOff(ctx, row.ID, true); err != nil {
		t.Fatal(err)
	}
	const firstTurn = "agentkickoff/first-turn-holder"
	if won, err := pg.ClaimKickoff(ctx, row.ID, firstTurn, 40*time.Minute); err != nil || !won {
		t.Fatalf("hold the first turn's claim: %v %v", won, err)
	}

	chat := &repromptChat{}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: pg, Provisioner: readyProv{name: name}, Gateway: chat,
		SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}, AgentNamespacePrefix: prefix})
	if _, err := s.EnableOfficers(OfficersConfig{Officers: []officers.Officer{o}, InstallSession: discardSession}); err != nil {
		t.Fatal(err)
	}
	fleet := officerFleet{s: s}

	// The loop re-prompts it mid-turn.
	repErr := fleet.Reprompt(ctx, o, 1, officers.RepromptText(o, 1))

	if !errors.Is(repErr, officers.ErrInstanceBusy) {
		t.Errorf("a re-prompt while the first turn runs = %v, want ErrInstanceBusy", repErr)
	}

	// The first turn runs on for more than the dwell bound, then ENDS (its
	// claim released) — and the deliverer ticks. This is the audit's boundary:
	// a re-armed row older than ProvisioningStuckTimeout with a free claim.
	if _, err := pool.Exec(ctx, `UPDATE agents SET updated_at = now() - $2::interval WHERE id=$1`,
		row.ID, (agents.ProvisioningStuckTimeout + time.Minute).String()); err != nil {
		t.Fatalf("age the row: %v", err)
	}
	if err := pg.ReleaseKickoffClaim(ctx, row.ID, firstTurn); err != nil {
		t.Fatal(err)
	}
	kgw := &kickoffGW{}
	d, err := agentkickoff.New(agentkickoff.Config{Store: pg, Instances: readyProv{name: name}, Gateway: kgw,
		NamespacePrefix: prefix, Owner: "offr-deliverer/1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	d.Wait()
	got, _ := pg.Get(ctx, row.ID)
	if got.Status == agents.StatusError {
		t.Fatalf("the instance reached `error` (%q) after a first turn longer than %s that it was re-prompted during — "+
			"it now holds its slot for ever and the queue is stranded", got.ErrorMessage, agents.ProvisioningStuckTimeout)
	}
	if !got.KickedOff {
		t.Fatal("the re-prompt changed the kickoff state")
	}
	if chat.count() != 0 {
		t.Fatalf("a re-prompt was sent into the running turn (%d)", chat.count())
	}

	// The next pass re-prompts; it is delivered once, as its own turn.
	if err := fleet.Reprompt(ctx, o, 1, officers.RepromptText(o, 1)); err != nil {
		t.Fatalf("re-prompt after the turn ended: %v", err)
	}
	s.WaitForOfficerReprompts()
	if chat.count() != 1 {
		t.Fatalf("re-prompt sends after the turn ended = %d, want 1", chat.count())
	}
	kgw.mu.Lock()
	ks := kgw.sends
	kgw.mu.Unlock()
	if ks != 0 {
		t.Fatalf("the re-prompt went through the kickoff deliverer (%d first-turn sends)", ks)
	}
	if after, _ := pg.Get(ctx, row.ID); after.Status == agents.StatusError || !after.KickedOff {
		t.Fatalf("after the re-prompt: status %q kicked_off %v", after.Status, after.KickedOff)
	}
	// Its session claim was released: another holder can take it.
	if won, err := pg.ClaimKickoff(ctx, row.ID, "someone-else/1", time.Minute); err != nil || !won {
		t.Fatalf("the re-prompt's session claim was not released: %v %v", won, err)
	}
}
