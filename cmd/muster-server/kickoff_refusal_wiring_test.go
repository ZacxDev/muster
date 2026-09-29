package main

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
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

// TestAGatewaylessDeploymentRefusesAKickoffAndAGatewayedOneDoesNot is the seam
// guard, and it asserts BOTH directions because a one-directional test cannot tell
// a wire from a constant.
//
// 🔴 THE PAIR IS THE TEST. Only the gateway configuration differs between the two
// halves — same driver, same store shape, same call — so a plane that hardcoded
// either polarity fails exactly one of them. Asserting only the refusal would pass
// over a build that refused every dispatch on every deployment, which is the
// failure mode that removes a working feature rather than a broken one.
func TestAGatewaylessDeploymentRefusesAKickoffAndAGatewayedOneDoesNot(t *testing.T) {
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

	// POSITIVE CONTROL: name a runtime and the identical dispatch must go through.
	t.Run("a gateway is named", func(t *testing.T) {
		store := newDispatchRecorder()
		prov, gw, _, err := buildAgentPlane(
			gatewayTestConfig(provisionerNoop, gatewayHooksSHA256), store, logger)
		if err != nil {
			t.Fatalf("buildAgentPlane(noop, %s): %v", gatewayHooksSHA256, err)
		}
		if gw == nil {
			t.Fatalf("instrument check FAILED: no gateway was built for %s=%s, so this is not "+
				"the positive control it claims to be", envAgentGateway, gatewayHooksSHA256)
		}

		if err := prov.Dispatch(store.agent.ID, true); err != nil {
			t.Fatalf("a kickoff dispatch was REFUSED on a deployment WITH a gateway: %v\n"+
				"  transcript: %s\n"+
				"    The refusal is firing unconditionally, which removes a working feature "+
				"from every deployment that configured one.", err, store.transcript())
		}
		if !store.called("SetHooksToken") {
			t.Errorf("the accepted dispatch never reached the create path.\n  transcript: %s",
				store.transcript())
		}
		if strings.Contains(store.transcript(), "UpdateStatus("+agents.StatusError+")") {
			t.Errorf("the row was marked %s on a dispatch that was not refused.\n"+
				"  transcript: %s", agents.StatusError, store.transcript())
		}
	})
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
