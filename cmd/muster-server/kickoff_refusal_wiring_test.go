package main

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// THE SEAM BETWEEN THE GATEWAY THIS BINARY RESOLVES AND THE REFUSAL THE ADAPTER
// PERFORMS.
//
// 🔴 BOTH SIDES OF THIS SEAM ARE ALREADY TESTED IN ISOLATION AND THAT IS EXACTLY
// WHY IT NEEDS A TEST. internal/agentprovision pins the refusal against a bool it
// is handed; gateway_test.go pins that naming a runtime produces a gateway and not
// naming one produces nil. Neither builds the combined state, so the wire between
// them — buildAgentPlane passing `gw != nil` into the adapter — is owned by no
// test at all. A plane that always passed true, or always false, satisfies every
// existing assertion in both packages.
//
// ⚠ THE ASSERTIONS ARE BEHAVIOURAL, NOT STRUCTURAL. Config.KickoffDeliverable is
// unexported and reading it by reflection would type-check past a wiring that set
// it from the wrong thing. These tests DISPATCH through the constructed adapter and
// read what happened to the store, which is the only claim that cannot be satisfied
// by a correctly-shaped mistake.
// ---------------------------------------------------------------------------

// dispatchRecorder is an agents.Store that answers the two reads the lifecycle
// adapter makes on a dispatch and records every write.
//
// ⚠ IT EMBEDS agents.Store, so any method NOT implemented here is a nil-interface
// panic rather than a zero value — the same choice stubStore makes, for the same
// reason: a dispatch that started calling something new must fail loudly instead of
// passing over a stub.
type dispatchRecorder struct {
	agents.Store

	mu    sync.Mutex
	agent agents.Agent
	calls []string
}

func (s *dispatchRecorder) record(f string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, f)
}

func (s *dispatchRecorder) transcript() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.calls, " -> ")
}

func (s *dispatchRecorder) Get(context.Context, int64) (agents.Agent, error) {
	s.record("Get")
	return s.agent, nil
}

func (s *dispatchRecorder) UpdateStatus(_ context.Context, _ int64, status, _, errMsg string) error {
	s.record("UpdateStatus(" + status + "):" + errMsg)
	return nil
}

func (s *dispatchRecorder) SetHooksToken(_ context.Context, _ int64, _ string) error {
	s.record("SetHooksToken")
	return nil
}

func (s *dispatchRecorder) SetKickoffError(_ context.Context, _ int64, _ string) error {
	s.record("SetKickoffError")
	return nil
}

func newDispatchRecorder() *dispatchRecorder {
	return &dispatchRecorder{agent: agents.Agent{
		ID:          7712,
		Name:        "wiring-kestrel",
		Namespace:   agents.NamespaceFor(agents.NamespacePrefix, "wiring-kestrel"),
		PendingNote: "read the plan and report what is missing",
		Status:      agents.StatusProvisioning,
	}}
}

// TestEveryDeploymentRefusesAKickoffWhileNothingCanDeliverOne is the seam guard.
//
// 🔴 ITS SECOND HALF IS THE REGRESSION TEST FOR THE DEFECT THIS FILE WAS WRONG
// ABOUT, AND THE TEST USED TO ASSERT THE DEFECT. It was named
// TestAGatewaylessDeploymentRefusesAKickoffAndAGatewayedOneDoesNot, and its
// "a gateway is named" half asserted that the identical dispatch WENT THROUGH —
// calling that the positive control that separates "refuses when it cannot deliver"
// from "refuses always". That was measured false on the live deployment: naming a
// gateway does NOT make a kickoff deliverable, because nothing in this module calls
// a gateway on the dispatch path, so the dispatch it let through created a
// Deployment, a ServiceAccount, a namespace and a Secret holding a minted token,
// cloned the repository in, handed over a model credential — and recorded that the
// first turn never happened, behind a card agents.ComputeStatus refines to
// `running`. The old assertion's own failure message called the refusal "removing a
// working feature from every deployment that configured one"; the feature it was
// protecting was the half-provision.
//
// 🔴 SO THE PAIR NO LONGER DISCRIMINATES AND THIS TEST DOES NOT PRETEND IT DOES.
// With agentprovision.KickoffDeliveryWired false, the predicate is constant across
// every configuration, and NO behavioural test here can tell a wire from a constant
// — because today it IS a constant. Saying so is the honest description; the two
// things that keep it from going stale are elsewhere, and both are mechanical:
//
//   - TestTheDeliverabilityPredicateNeedsBOTHConjuncts drives all four rows of
//     kickoffDeliverable, so "a gateway is required" stays pinned whatever the
//     constant currently is.
//   - internal/modulegate's TestNothingDeliversAKickoffAndThisModuleSaysSo fails
//     when a deliverer lands and the constant is still false, which is what forces
//     the flip that makes this pair discriminating again.
//
// ⚠ BOTH HALVES BELOW STILL RUN, AND THE GATEWAYED ONE IS THE ONE THAT MOVED. The
// gatewayless half is unchanged and would pass against the old code too; the
// gatewayed half fails against it. Keeping both is what makes "every deployment"
// a claim about the set rather than about one configuration.
func TestEveryDeploymentRefusesAKickoffWhileNothingCanDeliverOne(t *testing.T) {
	logger := log.New(&strings.Builder{}, "", 0)

	// MUSTER_AGENT_GATEWAY unset — the deployed configuration this refusal is for.
	t.Run("no gateway named", func(t *testing.T) {
		store := newDispatchRecorder()
		prov, gw, _, err := buildAgentPlane(provisionerTestConfig(provisionerNoop), store, logger)
		if err != nil {
			t.Fatalf("buildAgentPlane(noop, no gateway): %v", err)
		}
		// INSTRUMENT CHECK: this half is only meaningful if the gateway really is nil.
		if gw != nil {
			t.Fatalf("instrument check FAILED: a gateway was built for a config naming none, "+
				"so this case is not the no-gateway deployment: %T", gw)
		}
		if prov == nil {
			t.Fatal("instrument check FAILED: no lifecycle adapter was built, so nothing " +
				"below is exercised")
		}

		err = prov.Dispatch(store.agent.ID, true)
		if err == nil {
			t.Fatalf("a kickoff dispatch was ACCEPTED on a deployment with no gateway, so the "+
				"adapter was told a kickoff is deliverable when buildGateway returned nil.\n"+
				"  transcript: %s", store.transcript())
		}
		if !errors.Is(err, agentprovision.ErrKickoffUndeliverable) {
			t.Errorf("the dispatch failed for some reason OTHER than the undeliverable-kickoff "+
				"refusal, so this test is green for the wrong cause: %v", err)
		}
		if store.called("SetHooksToken") {
			t.Errorf("the refused dispatch still minted a token, so it reached the create "+
				"path.\n  transcript: %s", store.transcript())
		}
		if !strings.Contains(store.transcript(), "UpdateStatus("+agents.StatusError+")") {
			t.Errorf("the refusal was not recorded on the row as %s.\n  transcript: %s",
				agents.StatusError, store.transcript())
		}
		// The recorded reason must name the variable THIS BINARY reads.
		if !strings.Contains(store.transcript(), envAgentGateway) {
			t.Errorf("the recorded refusal does not name %s, so the operator is told "+
				"something was refused and not what to set.\n  transcript: %s",
				envAgentGateway, store.transcript())
		}
	})

	// 🔴 THE DEPLOYED CONFIGURATION, AND THE HALF THAT MOVED. MUSTER_AGENT_GATEWAY IS
	// set on the deployment this guard was written for, so this is the case an
	// operator's Dispatch click actually takes. A gateway is BUILT here — the
	// instrument check below insists on it — and the dispatch must still be refused,
	// because a gateway with nothing calling it delivers nothing.
	t.Run("a gateway is named and a kickoff is STILL undeliverable", func(t *testing.T) {
		store := newDispatchRecorder()
		prov, gw, _, err := buildAgentPlane(
			gatewayTestConfig(provisionerNoop, gatewayHooksSHA256), store, logger)
		if err != nil {
			t.Fatalf("buildAgentPlane(noop, %s): %v", gatewayHooksSHA256, err)
		}
		// INSTRUMENT CHECK: without a gateway this is the other subtest, not this one.
		if gw == nil {
			t.Fatalf("instrument check FAILED: no gateway was built for %s=%s, so this case is "+
				"not the gateway-configured deployment it claims to be",
				envAgentGateway, gatewayHooksSHA256)
		}
		if prov == nil {
			t.Fatal("instrument check FAILED: no lifecycle adapter was built, so nothing " +
				"below is exercised")
		}

		err = prov.Dispatch(store.agent.ID, true)
		if err == nil {
			t.Fatalf("a kickoff dispatch was ACCEPTED on a deployment that has a gateway and "+
				"no call site for it.\n  transcript: %s\n"+
				"    This is the shipped defect: `gw != nil` was the whole predicate, so a "+
				"configured gateway was read as a deliverable kickoff and the adapter created "+
				"the instance, minted its token and then recorded that the first turn never "+
				"happened. Restore the second conjunct "+
				"(agentprovision.KickoffDeliveryWired) in buildAgentPlane.", store.transcript())
		}
		if !errors.Is(err, agentprovision.ErrKickoffUndeliverable) {
			t.Errorf("the dispatch failed for some reason OTHER than the undeliverable-kickoff "+
				"refusal, so this test is green for the wrong cause: %v", err)
		}
		// 🔴 THE TWO COSTS THE REFUSAL EXISTS TO NOT PAY, ASSERTED SEPARATELY FROM THE
		// ERROR. A refusal that still minted the credential and built the instance
		// would satisfy an assertion phrased only over the returned error, and would be
		// the same defect with a louder log.
		if store.called("SetHooksToken") {
			t.Errorf("the refused dispatch still minted a token, so it reached the create "+
				"path.\n  transcript: %s", store.transcript())
		}
		if !strings.Contains(store.transcript(), "UpdateStatus("+agents.StatusError+")") {
			t.Errorf("the refusal was not recorded on the row as %s.\n  transcript: %s",
				agents.StatusError, store.transcript())
		}
	})
}

// TestTheDeliverabilityPredicateNeedsBOTHConjuncts drives every row of
// kickoffDeliverable.
//
// 🔴 IT IS WHAT THE SEAM TEST ABOVE CAN NO LONGER BE. `gw != nil &&
// agentprovision.KickoffDeliveryWired` is constant-false today, so three of these
// four rows are unreachable through buildAgentPlane and the one reachable row cannot
// tell "both conjuncts are required" from "this expression is false". Driving the
// predicate directly pins the requirement independently of the constant's current
// value — including after it flips, which is when the gateway conjunct starts
// carrying weight again and is exactly when nobody will be re-reading this.
//
// ⚠ WHAT IT DOES *NOT* PIN: that the call site passes the real constant rather than
// a literal. That is unobservable from here by construction — the argument is a
// bool — and it is named in kickoffDeliverable's own doc instead.
func TestTheDeliverabilityPredicateNeedsBOTHConjuncts(t *testing.T) {
	driver, err := provision.NewNoop()
	if err != nil {
		t.Fatalf("building the recording driver for the fixture: %v", err)
	}
	built, err := agentgateway.New(agentgateway.Config{
		Driver:  driver,
		Runtime: agentgateway.HooksSHA256(),
		Model:   "fixture-model",
	})
	if err != nil {
		t.Fatalf("building a gateway for the fixture: %v", err)
	}
	// INSTRUMENT CHECK: a nil "built" gateway would make the two gateway rows
	// indistinguishable from the two nil ones, and every row would pass for the
	// wrong reason.
	if built == nil {
		t.Fatal("instrument check FAILED: agentgateway.New returned nil with no error, so " +
			"the rows below that claim to hold a gateway do not")
	}

	for _, c := range []struct {
		label string
		gw    *agentgateway.Gateway
		wired bool
		want  bool
	}{
		{label: "no gateway, no call site", gw: nil, wired: false, want: false},
		{label: "no gateway, a call site", gw: nil, wired: true, want: false},
		{label: "a gateway, no call site — THE DEPLOYED ONE", gw: built, wired: false, want: false},
		{label: "a gateway AND a call site", gw: built, wired: true, want: true},
	} {
		if got := kickoffDeliverable(c.gw, c.wired); got != c.want {
			t.Errorf("kickoffDeliverable(gw!=nil=%t, wired=%t) = %t, want %t — %s\n"+
				"    Delivering a kickoff needs a gateway to deliver it THROUGH and a call "+
				"site to deliver it FROM. A predicate that drops either conjunct is wrong in "+
				"one of two directions: dropping the gateway tells a gatewayless deployment "+
				"it can deliver, and dropping the call site is the shipped defect — a "+
				"configured gateway read as a delivered first turn.",
				c.gw != nil, c.wired, got, c.want, c.label)
		}
	}
}

func (s *dispatchRecorder) called(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if strings.HasPrefix(c, name) {
			return true
		}
	}
	return false
}

// TestTheKickoffRefusalNamesTheBinarysOwnGatewayVariable pins the one duplicated
// string in this change.
//
// 🔴 internal/agentprovision CANNOT IMPORT package main, so the environment
// variable's name is spelled as a literal there and as envAgentGateway here. A
// duplicated name is a name that goes stale in one place, and the stale half would
// be the one an operator reads: a refusal naming a variable that no longer exists
// is worse than a refusal naming none, because it sends them to set something with
// no effect and conclude the feature is broken.
//
// ⚠ IT ASSERTS THE REMEDY'S *VALUE* TOO, for the same reason and with the same
// mechanism: agentgateway.SchemeHooksSHA256 is what config accepts, and a refusal
// telling the operator to set a value validateProvisioner rejects at boot would
// turn one refusal into two.
func TestTheKickoffRefusalNamesTheBinarysOwnGatewayVariable(t *testing.T) {
	for _, want := range []string{envAgentGateway, envAgentGatewayModel, gatewayHooksSHA256} {
		if !strings.Contains(agentprovision.KickoffRefusalReason, want) {
			t.Errorf("agentprovision.KickoffRefusalReason does not contain %q.\n"+
				"    Either this binary's constant was renamed and the refusal text was not, "+
				"or the refusal never named it. Fix the TEXT, not this test — the text is "+
				"what the operator reads.\n  reason: %s", want, agentprovision.KickoffRefusalReason)
		}
	}

	// CONTROL: the assertion above must be able to fail. A value this binary does NOT
	// recognise must be absent, or the test would pass over a reason string that
	// happened to contain every token anyone looked for.
	if strings.Contains(agentprovision.KickoffRefusalReason, "MUSTER_AGENT_GATEWAY_RUNTIME") {
		t.Error("the refusal names MUSTER_AGENT_GATEWAY_RUNTIME, which this binary does not " +
			"read — the operator would set a variable with no effect")
	}
}
