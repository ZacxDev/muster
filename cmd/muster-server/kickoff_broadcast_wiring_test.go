package main

import (
	"context"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentkickoff"
	"github.com/ZacxDev/muster/internal/agents"
)

// stuckRowStore lists ONE owned, owed row that dwelt past the stuck bound, so the
// deliverer's first tick takes the stuck verdict (the noop plane has no instance
// for it) and writes it.
type stuckRowStore struct {
	agents.Store
	row agents.Agent

	mu     sync.Mutex
	marked int
}

func (s *stuckRowStore) List(context.Context) ([]agents.Agent, error) {
	return []agents.Agent{s.row}, nil
}

func (s *stuckRowStore) MarkKickoffStuck(context.Context, int64, time.Time, string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marked++
	return true, nil
}

// TestTheDelivererBuiltForTheServerBroadcastsItsCardChanges (PR #37 round 1, F4):
// the Agents list re-renders on sse:agent.changed (internal/ui's agentsListTrigger —
// it did NOT until the UI-polish change, so this test's broadcast reached the
// Tasks board only), so buildKickoffDeliverer must
// hand the deliverer the broadcast it is given. Behavioural: a real deliverer from
// the real noop plane ticks once, writes a stuck verdict, and the callback runs.
func TestTheDelivererBuiltForTheServerBroadcastsItsCardChanges(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	cfg := gatewayTestConfig(provisionerNoop, gatewayHooksSHA256)
	store := &stuckRowStore{row: agents.Agent{
		ID: 8821, Name: "stale-gadwall", Namespace: agents.NamespaceFor(cfg.AgentNamespacePrefix, "stale-gadwall"),
		Status: agents.StatusProvisioning, PendingNote: "audit the retry budget", HooksToken: "tok-gadwall-91",
		UpdatedAt: time.Now().Add(-agents.ProvisioningStuckTimeout - time.Hour),
	}}
	prov, gw, err := buildAgentPlane(cfg, store, logger)
	if err != nil || prov == nil || gw == nil {
		t.Fatalf("buildAgentPlane: adapter=%v gateway=%v err=%v", prov != nil, gw != nil, err)
	}
	var mu sync.Mutex
	var changed []string
	d, err := buildKickoffDeliverer(cfg, store, prov, gw, func(name string) {
		mu.Lock()
		defer mu.Unlock()
		changed = append(changed, name)
	}, nil, nil, nil, logger)
	if err != nil || d == nil {
		t.Fatalf("buildKickoffDeliverer: deliverer=%v err=%v", d != nil, err)
	}
	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	d.Wait()

	store.mu.Lock()
	marked := store.marked
	store.mu.Unlock()
	if marked != 1 {
		t.Fatalf("instrument check FAILED: the stuck verdict was written %d time(s), want 1", marked)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(changed) != 1 || changed[0] != "stale-gadwall" {
		t.Errorf("the deliverer built here broadcast %v after a card-changing write, want "+
			"[stale-gadwall]: an open Agents list would keep the stale card until a reload", changed)
	}
}

// TestBuildAppPassesTheServersBroadcastToTheDeliverer pins the one call site.
//
// ⚠ IT IS A SPELLED GUARD: it reads main.go's text, so it cannot tell that the call
// runs, only that it is written. The behaviour of the function it calls is the test
// above; what this adds is that buildApp passes srv.BroadcastAgentChanged rather
// than nil, which nothing else in the suite would notice.
func TestBuildAppPassesTheServersBroadcastToTheDeliverer(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)
	if n := strings.Count(text, "buildKickoffDeliverer("); n != 1 {
		t.Fatalf("instrument check FAILED: main.go calls buildKickoffDeliverer %d time(s), want 1", n)
	}
	// srv.OfficerTurnContext (#497) and kickoffOutcomes(srv): a fresh officer
	// pool's kickoff starts a new conversation, and the server's outcome tracker
	// hears every first turn's course (internal/api agent_outcome.go).
	const want = "buildKickoffDeliverer(cfg, ext.Agents, prov, gw, srv.BroadcastAgentChanged, srv.OfficerTurnContext, kickoffOutcomes(srv), srv.KickoffTurnTools, logger)"
	if !strings.Contains(text, want) {
		t.Errorf("main.go does not contain %q: the deliverer's background card changes would "+
			"reach no open Agents list, or a fresh_session_per_item officer's kickoff would not "+
			"start a new conversation (srv.OfficerTurnContext), or a gateway agent's first turn would carry "+
			"no muster tool (srv.KickoffTurnTools)", want)
	}
}

// TestKickoffConfigCarriesEveryHook: what buildKickoffDeliverer is handed
// reaches the deliverer's configuration — above all the first turn's tools
// (TurnTools), without which a dispatched gateway agent is offered no muster
// tool, agent_report_done included, in the one turn it does its work in. The
// main.go guard above pins that buildApp PASSES srv.KickoffTurnTools; this pins
// that it is not dropped on the way in. Each hook is a distinct value, so a
// swapped or dropped field is told apart from a set one.
func TestKickoffConfigCarriesEveryHook(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	cfg := gatewayTestConfig(provisionerNoop, gatewayHooksSHA256)
	toolsAsked := 0
	tools := func(ctx context.Context, _ agents.Agent) (context.Context, agentkickoff.TurnTools, bool) {
		toolsAsked++
		return ctx, agentkickoff.TurnTools{}, false
	}
	turnAsked := 0
	turnCtx := func(ctx context.Context, _ agents.Agent) context.Context { turnAsked++; return ctx }
	c := kickoffConfig(cfg, nil, nil, nil, nil, turnCtx, nil, tools, logger)
	if c.TurnTools == nil {
		t.Fatal("kickoffConfig dropped TurnTools: every gateway agent's first turn would go out with no muster tool")
	}
	if _, _, _ = c.TurnTools(context.Background(), agents.Agent{}); toolsAsked != 1 {
		t.Errorf("Config.TurnTools is not the function handed in (asked %d time(s))", toolsAsked)
	}
	if c.TurnContext == nil {
		t.Fatal("kickoffConfig dropped TurnContext")
	}
	if _ = c.TurnContext(context.Background(), agents.Agent{}); turnAsked != 1 || toolsAsked != 1 {
		t.Errorf("Config.TurnContext is not the function handed in (turn %d, tools %d)", turnAsked, toolsAsked)
	}
	if c.NamespacePrefix != cfg.AgentNamespacePrefix || c.Logger != logger {
		t.Errorf("prefix %q logger-set=%t; want %q and the logger handed in", c.NamespacePrefix, c.Logger == logger, cfg.AgentNamespacePrefix)
	}
}
