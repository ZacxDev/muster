package main

import (
	"log"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/provision"
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

// TestTheChatWiredBannerDoesNotClaimReachability pins the correction an audit forced.
//
// 🔴 THE LINE SHIPPED CLAIMING THE CHAT ROUTES WERE LIVE "over the same driver as
// lifecycle", AND THAT WAS FALSE FOR EVERY AGENT THIS BINARY PROVISIONS. agentspec.Build
// renders a spec with no port and no endpoint, so the driver resolves no address and a
// chat turn fails PER TURN — strictly worse than the 503 it replaced, which named its
// own cause. The routes really do stop refusing; that is the only part that was true.
//
// 🔴 IT PINS THE WHOLE NORMALISED LINE, AND THE SUBSTRING VERSION OF THIS TEST WAS
// MEASURED WALKABLE. That version required four phrases and forbade two, and its own
// comment claimed "IT PINS THE CLAIM, NOT A KEYWORD" — an audit then wrote a banner
// carrying all four required phrases, neither forbidden one, and the sentence "chat is
// FULLY USABLE against any agent, including one this binary provisioned … the old
// warning is obsolete; ignore them", and THIS TEST PASSED. A guard that reads as
// coverage while providing none is worse than no guard, because it stops the next
// person looking. When the artifact under test IS PROSE, only the whole string is a
// machine-readable claim.
//
// ⚠ THE COST IS REAL AND IS ACCEPTED: any cosmetic reword of that banner line fails
// this test and wantLine must be updated with it. That is the price of the property.
// The forbidden list stays alongside — a pinned line is walkable one way, by
// regenerating the golden, and re-pinning is a reflex; the negatives survive a
// careless re-pin because they encode INTENT rather than text.
func TestTheChatWiredBannerDoesNotClaimReachability(t *testing.T) {
	onOut, _ := bannerBothDirections(t)
	line := lineNaming(onOut, envAgentGateway)
	if line == "" {
		t.Fatalf("no banner line names %s at all.\nbanner:\n%s", envAgentGateway, onOut)
	}

	// Built from the same constants the banner formats, so a renamed variable or a
	// changed scheme spelling moves BOTH sides and this stays a claim about the
	// SENTENCE rather than about the values interpolated into it.
	wantLine := "agent provisioning CHAT: WIRED " + envAgentGateway + "=" + gatewayHooksSHA256 +
		" (" + envAgentGatewayModel + "=runtime-sentinel) — the two chat routes no longer " +
		"refuse at api.requireGatewayProvisioner. 🔴 WIRED IS NOT REACHABLE: agentspec.Build " +
		"declares no port and no endpoint, so this driver resolves no address for an agent " +
		"this binary provisioned and every such turn fails with " + provision.ErrNoEndpoint.Error() +
		". Chat is usable only against an instance provisioned elsewhere, with an address " +
		"this process can resolve. See cmd/muster-server/doc_seams.go entry 1"

	if line != wantLine {
		t.Errorf("the CHAT: WIRED line changed.\n  got:  %s\n  want: %s\n"+
			"  This line is PINNED WHOLE on purpose: it carries a correction an audit forced —\n"+
			"  the routes stop refusing, and a turn against an agent THIS binary provisioned\n"+
			"  still resolves no address. A reword that keeps that meaning is fine; update\n"+
			"  wantLine with it. A reword that drops it restores a banner stating a falsehood,\n"+
			"  and a substring version of this test was MEASURED to pass over exactly that.",
			line, wantLine)
	}

	// 🔴 THE NEGATIVES ARE NOT REDUNDANT WITH THE PIN. They are what survives a lazy
	// re-pin: regenerating wantLine from a bad line is one paste, and these two phrases
	// are the retracted claim itself, in both spellings it has appeared in.
	for _, forbidden := range []string{"chat routes are live", "over the same driver as lifecycle"} {
		if strings.Contains(strings.ToLower(line), strings.ToLower(forbidden)) {
			t.Errorf("the CHAT: WIRED line has regained the retracted claim %q.\n  line: %s",
				forbidden, line)
		}
	}
}

// TestTheRetractedReachabilityClaimIsGoneTREEWIDE is the sweep half, and it exists
// because the retraction reached two of its three sites.
//
// 🔴 A RETRACTION IS A TREE-WIDE SWEEP, NOT AN EDIT WHERE YOU WERE LOOKING. The round
// that corrected the banner and the linkage note left "The two CHAT ROUTES are live"
// in doc_seams.go — the one document the corrected banner POINTS A READER AT. The
// sweep that missed it was a lowercase fixed-string grep; the case-insensitive one
// hit. So this guard is case-insensitive by construction, and it covers the sources a
// reader arrives at, not just the file that was reported.
//
// ⚠ IT ALLOWS THE PHRASE INSIDE THIS FILE'S OWN FORBIDDEN LIST AND COMMENTS, which is
// the one place it must appear. That exemption is narrow and named rather than a
// path-prefix skip, because an exemption wide enough to be convenient is how the next
// occurrence hides.
func TestTheRetractedReachabilityClaimIsGoneTREEWIDE(t *testing.T) {
	// The claim, as a relationship rather than one spelling: "chat routes" followed
	// closely by a reachability word.
	re := regexp.MustCompile(`(?i)chat[ _-]?routes?[^.\n]{0,60}(live|usable|reachable)`)

	roots := []string{"main.go", "doc_seams.go", "config.go", "provisioner.go"}
	hits := 0
	for _, f := range roots {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v — this guard is scoped to a NAMED list, so a missing "+
				"file is a broken guard rather than a clean sweep", f, err)
		}
		for _, m := range re.FindAllString(string(body), -1) {
			hits++
			t.Errorf("%s still asserts the retracted reachability claim: %q\n"+
				"  The chat routes stop REFUSING; they are not reachable for an agent this\n"+
				"  binary provisions. See doc_seams.go entry 1 for both blockers.", f, m)
		}
	}

	// 🔴 POSITIVE CONTROL: the pattern must be able to MATCH, or a zero above means
	// only that the regexp is wrong. This is the exact sentence the sweep missed.
	const missed = "NOTHING CALLS THE GATEWAY ON THE DISPATCH PATH. The two CHAT ROUTES are live;"
	if !re.MatchString(missed) {
		t.Fatalf("positive control FAILED: the pattern does not match the very sentence this "+
			"guard was written for (%q), so its %d hit(s) over the sources say nothing", missed, hits)
	}
}

// TestAnUnknownProvisionerNameIsStillRefused is the control for the oneOf consolidation.
//
// ⚠ IT IS HERE BECAUSE A REFACTOR MOVED A PREDICATE, NOT BECAUSE THE PROVISIONER CHANGED.
// Folding two identical membership loops into oneOf touched the provisioner's legality
// check, and nothing in this package asserted THAT branch — so the consolidation could
// have broken it silently. Mutation-checked: making oneOf always return true kills this.
func TestAnUnknownProvisionerNameIsStillRefused(t *testing.T) {
	cfg := provisionerTestConfig("kubernets") // one letter, the realistic typo
	err := cfg.validateProvisioner()
	if err == nil {
		t.Fatal("validateProvisioner accepted an unknown driver name, so a typo would fall through " +
			"to buildDriver's unreachable default arm at dispatch time instead of failing at boot")
	}
	if !strings.Contains(err.Error(), envAgentProvisioner) {
		t.Errorf("the refusal does not name %s.\n  got: %v", envAgentProvisioner, err)
	}
	// POSITIVE CONTROL: a legal name must still pass, or the assertion above is
	// satisfied by any refusal at all.
	if err := provisionerTestConfig(provisionerNoop).validateProvisioner(); err != nil {
		t.Fatalf("positive control FAILED: a legal driver name was refused: %v", err)
	}
}
