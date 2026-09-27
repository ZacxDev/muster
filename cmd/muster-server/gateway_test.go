package main

import (
	"log"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// THE CHAT TIER'S WIRING, WHICH IS A SECOND AXIS ON THE SAME CALL.
//
// 🔴 EVERY TEST HERE IS ABOUT A COMBINATION, NOT ABOUT A VALUE. The two knobs are
// independent by design — lifecycle without chat is a deployment, and was the only
// one available for a whole step — so the states worth pinning are the CROSSINGS:
// chat on with lifecycle off (refused at boot), chat off with lifecycle on (the
// previous release's behaviour, which must not change), and both on.
// ---------------------------------------------------------------------------

// gatewayTestConfig is a config that passes validate for the named driver and
// runtime, so each test varies one thing.
func gatewayTestConfig(driver, runtime string) config {
	c := provisionerTestConfig(driver)
	c.AgentGateway = runtime
	if runtime != gatewayNone {
		c.AgentGatewayModel = "runtime-sentinel"
	}
	return c
}

// TestNamingARuntimeWiresAGatewayAndNotNamingOneWiresNothing pins both ends of the
// knob.
//
// 🔴 THE OFF END IS THE ONE THAT MATTERS, because it is what every existing
// deployment is: a released muster sets MUSTER_AGENT_PROVISIONER and nothing else,
// and an image bump that started sending agents chat turns would be a behaviour
// change nobody asked for. A nil gateway keeps both chat routes refusing at
// api.requireGatewayProvisioner exactly as before.
func TestNamingARuntimeWiresAGatewayAndNotNamingOneWiresNothing(t *testing.T) {
	logger := log.New(&strings.Builder{}, "", 0)

	prov, gw, err := buildAgentPlane(gatewayTestConfig(provisionerNoop, gatewayHooksSHA256), stubStore{}, logger)
	if err != nil {
		t.Fatalf("buildAgentPlane(noop, %s): %v", gatewayHooksSHA256, err)
	}
	if gw == nil {
		t.Fatal("naming a runtime produced no gateway, so both chat routes would keep refusing " +
			"and a dispatched agent's kickoff would still be undeliverable")
	}
	if got := gw.Runtime(); got != gatewayHooksSHA256 {
		t.Errorf("gw.Runtime() = %q, want %q — the banner reports this, so the wiring and the "+
			"banner cannot be allowed to disagree", got, gatewayHooksSHA256)
	}
	// CONTROL: the lifecycle half is unaffected by naming a runtime. Both are built
	// from one call now, and a change that wired chat by breaking lifecycle would
	// otherwise be invisible here.
	if prov == nil {
		t.Error("naming a runtime cost us the lifecycle provisioner")
	}

	off, offGw, err := buildAgentPlane(provisionerTestConfig(provisionerNoop), stubStore{}, logger)
	if err != nil {
		t.Fatalf("buildAgentPlane(noop, unset gateway): %v", err)
	}
	if offGw != nil {
		t.Errorf("an unset %s produced a %q gateway. The default must leave "+
			"api.Extensions.Gateway nil: a deployment that named only a provisioner gets exactly "+
			"the behaviour it had before this knob existed.", envAgentGateway, offGw.Runtime())
	}
	if off == nil {
		t.Error("the lifecycle provisioner is nil with the gateway unset, which is the " +
			"combination the previous release shipped")
	}

	// 🔴 THE ZERO-VALUE CONFIG IS ITS OWN CASE, for the reason
	// TestTheNoopProvisionerWiresAnAdapterAndNoneWiresNothing records: buildApp
	// accepts configs that never went through loadConfig, so a resolver default that
	// was wrong would be invisible to the case above, which passes its value
	// explicitly.
	_, zeroGw, err := buildAgentPlane(config{}, stubStore{}, logger)
	if err != nil {
		t.Fatalf("buildAgentPlane over a zero-value config: %v", err)
	}
	if zeroGw != nil {
		t.Errorf("a zero-value config produced a %q gateway. An unset %s must resolve to %q.",
			zeroGw.Runtime(), envAgentGateway, gatewayNone)
	}
}

// TestAGatewayWithoutADriverIsRefusedAtBoot pins the cross-check between the two
// knobs.
//
// 🔴 THE ALTERNATIVE IS A SERVER THAT BOOTS, PRINTS "CHAT: WIRED", AND CANNOT REACH
// ANY AGENT. This binary's only source of an instance's address is the provisioning
// driver, so a named runtime with no named driver is a chat tier that resolves
// nothing — and it would fail inside a request handler, once per turn, as an error
// about an endpoint rather than about configuration.
//
// ⚠ THE REFUSAL MUST NAME BOTH VARIABLES. An operator in this state set one of them
// deliberately; a message naming only the other sends them to change the wrong one.
func TestAGatewayWithoutADriverIsRefusedAtBoot(t *testing.T) {
	cfg := gatewayTestConfig(provisionerNone, gatewayHooksSHA256)
	err := cfg.validateProvisioner()
	if err == nil {
		t.Fatal("validateProvisioner accepted a named runtime with no named driver. The server " +
			"would boot, announce CHAT: WIRED, and answer every turn with an endpoint " +
			"resolution error from inside a handler.")
	}
	for _, want := range []string{envAgentGateway, envAgentProvisioner} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s.\n  got: %v", want, err)
		}
	}

	// POSITIVE CONTROL: the same configuration with a driver named must PASS, or the
	// assertion above is satisfied by any validation failure at all.
	if err := gatewayTestConfig(provisionerNoop, gatewayHooksSHA256).validateProvisioner(); err != nil {
		t.Fatalf("positive control FAILED: a named runtime WITH a named driver was refused: %v", err)
	}
	// CONTROL: the no-driver state is fine as long as no runtime is named, which is
	// the default every existing deployment is in.
	if err := gatewayTestConfig(provisionerNone, gatewayNone).validateProvisioner(); err != nil {
		t.Fatalf("control FAILED: neither knob set was refused: %v", err)
	}
}

// TestAnUnknownRuntimeNameIsRefusedAtBoot pins the legality check.
//
// 🔴 WITHOUT IT A TYPO IS INDISTINGUISHABLE FROM "off". buildGateway's default arm
// would be reached, and a nil gateway means both chat routes refuse — so
// a one-character typo in the scheme name would present as a deployment that
// simply never turned chat on, with a banner that agrees.
func TestAnUnknownRuntimeNameIsRefusedAtBoot(t *testing.T) {
	err := gatewayTestConfig(provisionerNoop, "hooks-sha255").validateProvisioner()
	if err == nil {
		t.Fatal("validateProvisioner accepted an unknown runtime name, so a typo presents as " +
			"\"chat was never enabled\" rather than as a configuration error")
	}
	if !strings.Contains(err.Error(), envAgentGateway) {
		t.Errorf("the refusal does not name %s.\n  got: %v", envAgentGateway, err)
	}
	// It must name what IS legal, or the operator has to read the source to fix it.
	for _, want := range gatewayChoices {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name the legal value %q.\n  got: %v", want, err)
		}
	}
}

// TestAMissingSentinelIsRefusedAtBoot pins the second half of what naming a runtime
// commits a deployment to.
//
// 🔴 THE SENTINEL HAS NO DEFENSIBLE DEFAULT AND ITS ABSENCE IS INVISIBLE UNTIL A
// TURN. Both gateway endpoints require the wire's `model` field; an empty one is an
// HTTP 400 from inside a chat handler, per turn, with a message about a model —
// which reads as a model-configuration problem rather than as an unset variable.
// Boot is the only place that failure can be attributed correctly.
//
// ⚠ IT IS A SEPARATE REFUSAL FROM THE DRIVER CROSS-CHECK, not a second clause of
// it, because the two are independently reachable: a deployment can name a runtime
// AND a driver and still omit the sentinel.
func TestAMissingSentinelIsRefusedAtBoot(t *testing.T) {
	cfg := gatewayTestConfig(provisionerNoop, gatewayHooksSHA256)
	cfg.AgentGatewayModel = ""
	err := cfg.validateProvisioner()
	if err == nil {
		t.Fatal("validateProvisioner accepted a named runtime with no sentinel. Every chat turn " +
			"would come back 400 from inside a handler, naming the model field.")
	}
	if !strings.Contains(err.Error(), envAgentGatewayModel) {
		t.Errorf("the refusal does not name %s, so the operator cannot act on it.\n  got: %v",
			envAgentGatewayModel, err)
	}

	// POSITIVE CONTROL: the same config WITH a sentinel must pass, or the assertion
	// above is satisfied by any validation failure at all.
	if err := gatewayTestConfig(provisionerNoop, gatewayHooksSHA256).validateProvisioner(); err != nil {
		t.Fatalf("positive control FAILED: a named runtime with a sentinel was refused: %v", err)
	}
	// CONTROL: with no runtime named the sentinel is irrelevant and must not be
	// required — otherwise every existing deployment stops booting.
	noGateway := provisionerTestConfig(provisionerNoop)
	noGateway.AgentGatewayModel = ""
	if err := noGateway.validateProvisioner(); err != nil {
		t.Fatalf("control FAILED: a deployment with no gateway was refused for having no "+
			"sentinel: %v", err)
	}
}
