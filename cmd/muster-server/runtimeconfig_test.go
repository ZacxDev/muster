package main

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
)

// ---------------------------------------------------------------------------
// THE RUNTIME-CONFIG BUNDLE AT THE BINARY'S OWN SEAM.
//
// 🔴 internal/agentspec'S GUARDS PROVE A COMPLETE BUNDLE IS INSTALLED. THEY
// CANNOT PROVE THIS BINARY SUPPLIES ONE. That is the seam, and it is exactly the
// kind that has shipped broken here before: doc_seams.go entry 1's first blocker
// was a port both sides tested and nobody wired, found only when a test resolved
// an endpoint through the real driver over this binary's own mapping. So every
// test below goes through agentSpecConfig rather than constructing an
// agentspec.Config by hand.
// ---------------------------------------------------------------------------

// TestANamedRuntimeWithNoBundleIsRefusedAtBoot pins the required-together half.
//
// 🔴 THE ALTERNATIVE IS NOT A DEGRADED FEATURE, IT IS A PROVISIONER THAT COMES UP
// HEALTHY AND CREATES PODS THAT NEVER SERVE. Measured against the agent runtime
// image: with no configuration file its gateway exits 78 with `Missing config`,
// the pod crashloops, and — before this change's probes — reported 0/1 for ever
// with nothing timing it out. Boot is the only place that is attributable.
//
// ⚠ EACH HALF OF THE PAIR IS REMOVED SEPARATELY, because a refusal keyed on "both
// unset" would accept the two configurations an operator is most likely to reach:
// a ConfigMap with one key renamed, and a Deployment env block with one entry
// forgotten.
func TestANamedRuntimeWithNoBundleIsRefusedAtBoot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config)
	}{
		{"neither key", func(c *config) { c.AgentRuntimeConfig = ""; c.AgentRuntimeInstall = "" }},
		{"template only", func(c *config) { c.AgentRuntimeInstall = "" }},
		{"script only", func(c *config) { c.AgentRuntimeConfig = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := gatewayTestConfig(provisionerNoop, gatewayHooksSHA256)
			tc.mutate(&cfg)
			err := cfg.validateProvisioner()
			if err == nil {
				t.Fatalf("validateProvisioner accepted %s=%s with an incomplete runtime-config "+
					"bundle. Every instance provisioned on that deployment would run the image's "+
					"own entrypoint with no configuration, which is measured to exit 78 and "+
					"crashloop.", envAgentGateway, gatewayHooksSHA256)
			}
			// It must name BOTH keys: an operator in this state set one of them, and a
			// message naming only that one sends them to change what is already right.
			for _, want := range []string{envAgentRuntimeConfig, envAgentRuntimeInstall, envAgentGateway} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %s.\n  got: %v", want, err)
				}
			}
			// 🔴 AND IT MUST SAY WHICH ONE IS MISSING, OR AN OPERATOR WITH ONE KEY SET
			// CANNOT TELL WHETHER THE OTHER WAS THE PROBLEM. presence() is what renders
			// that without printing a multi-line configuration file into a log line.
			if !strings.Contains(err.Error(), "unset") {
				t.Errorf("the refusal names both keys but reports neither's STATE, so it reads "+
					"the same whichever one is missing.\n  got: %v", err)
			}
		})
	}

	// POSITIVE CONTROL: the complete configuration must PASS, or every case above
	// is satisfied by a validate that refuses everything.
	if err := gatewayTestConfig(provisionerNoop, gatewayHooksSHA256).validateProvisioner(); err != nil {
		t.Fatalf("positive control FAILED: the complete configuration was refused: %v", err)
	}
	// CONTROL: a deployment with NO runtime named must not be asked for a bundle —
	// otherwise every existing deployment stops booting on this release.
	if err := provisionerTestConfig(provisionerK8s).validateProvisioner(); err != nil {
		t.Fatalf("control FAILED: a deployment with no gateway was refused for having no "+
			"runtime-config bundle, which breaks every release that shipped before this one: %v", err)
	}
}

// TestABundleWithNoNamedRuntimeIsRefusedAtBoot pins the other direction.
//
// 🔴 IT IS A DIFFERENT FAILURE, NOT A MIRROR. With no scheme named this binary has
// no credential derivation, so the bundle would be installed and the gateway
// credential written EMPTY — a well-formed configuration the runtime accepts and
// then refuses every turn against with a 401, attributed to the credential rather
// than to an unset variable. It is the same armed-switch shape validateProvisioner
// already refuses for the privilege tier.
func TestABundleWithNoNamedRuntimeIsRefusedAtBoot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config)
	}{
		{"both keys", func(c *config) {
			c.AgentRuntimeConfig = fixtureRuntimeConfigTemplate
			c.AgentRuntimeInstall = fixtureRuntimeInstallScript
		}},
		{"template only", func(c *config) { c.AgentRuntimeConfig = fixtureRuntimeConfigTemplate }},
		{"script only", func(c *config) { c.AgentRuntimeInstall = fixtureRuntimeInstallScript }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := provisionerTestConfig(provisionerK8s)
			tc.mutate(&cfg)
			err := cfg.validateProvisioner()
			if err == nil {
				t.Fatalf("validateProvisioner accepted a runtime-config bundle with %s=%s. The "+
					"bundle would be installed with an EMPTY gateway credential and every turn "+
					"would 401.", envAgentGateway, gatewayNone)
			}
			if !strings.Contains(err.Error(), envAgentGateway) {
				t.Errorf("the refusal does not name %s, which is the variable to set.\n  got: %v",
					envAgentGateway, err)
			}
			// The remedy has to be legal: naming the scheme is what makes a derivation
			// available, so the message must say which value that is.
			if !strings.Contains(err.Error(), gatewayHooksSHA256) {
				t.Errorf("the refusal does not name a legal runtime (%q), so the operator has to "+
					"read the source to act on it.\n  got: %v", gatewayHooksSHA256, err)
			}
		})
	}

	// CONTROL: no bundle and no runtime is the default every existing deployment
	// runs, and must pass.
	if err := provisionerTestConfig(provisionerK8s).validateProvisioner(); err != nil {
		t.Fatalf("control FAILED: neither set was refused: %v", err)
	}
}

// TestAWhitespaceOnlyBundleKeyReadsAsUnset pins the trim, through loadConfig.
//
// 🔴 IT IS THE REACHABLE SPELLING OF "I FORGOT TO FILL THIS IN". A ConfigMap key
// written as a YAML block scalar with no body projects as a single newline, which
// is a one-byte script: without the trim it is SET, boot passes, and the container
// runs `sh -eu` over an empty file — which exits 0 without exec'ing anything, so
// the pod COMPLETES successfully and the Deployment restarts it for ever with no
// error in any log.
func TestAWhitespaceOnlyBundleKeyReadsAsUnset(t *testing.T) {
	env := map[string]string{
		envDatabase:            "postgres://unused",
		envAgentProvisioner:    provisionerNoop,
		envAgentImageRepo:      "registry.example.test/muster/agent-runtime",
		envAgentAPIURL:         "http://muster.example.test:8105",
		envAgentGateway:        gatewayHooksSHA256,
		envAgentGatewayModel:   "runtime-sentinel",
		envAgentRuntimeConfig:  fixtureRuntimeConfigTemplate,
		envAgentRuntimeInstall: "\n   \n\t\n",
	}
	cfg, err := loadConfig(func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.AgentRuntimeInstall != "" {
		t.Errorf("a whitespace-only install key loaded as %q rather than empty, so boot would "+
			"accept it and the container would run an empty script: `sh -eu` over no commands "+
			"exits 0 without exec'ing the runtime, and the pod completes instead of serving",
			cfg.AgentRuntimeInstall)
	}
	if err := cfg.validate(); err == nil {
		t.Error("validate accepted a whitespace-only install script")
	}

	// POSITIVE CONTROL: the same environment with a real script must load it and
	// pass, or the assertions above are about a loader that drops both values.
	env[envAgentRuntimeInstall] = fixtureRuntimeInstallScript
	ok, err := loadConfig(func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("positive control FAILED: loadConfig: %v", err)
	}
	if ok.AgentRuntimeInstall != strings.TrimSpace(fixtureRuntimeInstallScript) {
		t.Errorf("positive control FAILED: the install script loaded as %q, want %q",
			ok.AgentRuntimeInstall, strings.TrimSpace(fixtureRuntimeInstallScript))
	}
	if err := ok.validate(); err != nil {
		t.Fatalf("positive control FAILED: the complete environment was refused: %v", err)
	}
}

// TestTheBundleReachesTheSpecThroughThisBinarysOwnMapping is the seam guard that
// internal/agentspec structurally cannot write.
//
// 🔴 THE CREDENTIAL IS CHECKED AGAINST THE SAME Runtime THE GATEWAY HOLDS, which
// is the whole point of design (B). muster derives the bearer once and ships the
// RESULT, so the instance's configured credential and the one muster sends on a
// turn are the same bytes by construction rather than by two implementations of a
// formula agreeing. This asserts that the value in the spec is
// agentgateway.HooksSHA256's output over the ROW's token — not merely that some
// 64-character string was shipped.
//
// ⚠ WHAT IT STILL DOES NOT PROVE, SO A GREEN RUN IS NOT OVERREAD: that a real
// runtime accepts it. The operator's install script is what writes this value into
// the runtime's configuration file, that script is in a ConfigMap this repository
// never sees, and no test here runs a container. doc_seams.go entry 1 carries the
// live closing condition.
func TestTheBundleReachesTheSpecThroughThisBinarysOwnMapping(t *testing.T) {
	cfg := gatewayTestConfig(provisionerK8s, gatewayHooksSHA256)
	specCfg := mustAgentSpecConfig(t, cfg)

	if !specCfg.RuntimeConfig.Configured() {
		t.Fatalf("agentSpecConfig produced an UNCONFIGURED bundle from a configuration that "+
			"names %s and sets both keys. Every instance would run the image's own entrypoint "+
			"with no configuration.", envAgentGateway)
	}
	if got := string(specCfg.RuntimeConfig.Template); got != cfg.AgentRuntimeConfig {
		t.Errorf("the template reaching the spec is not the one configured.\n  got:  %q\n  want: %q",
			got, cfg.AgentRuntimeConfig)
	}
	if got := string(specCfg.RuntimeConfig.Install); got != cfg.AgentRuntimeInstall {
		t.Errorf("the install script reaching the spec is not the one configured.\n  got:  %q\n  want: %q",
			got, cfg.AgentRuntimeInstall)
	}

	a := reachabilityAgent()
	spec, err := agentspec.Build(a, specCfg, agentspec.Options{})
	if err != nil {
		t.Fatalf("agentspec.Build over this binary's own spec config: %v", err)
	}
	var bearer string
	for _, s := range spec.Secrets {
		if s.Name == agentspec.EnvGatewayBearer {
			bearer = s.Value
		}
	}
	// Pinned against the SCHEME's own function, which is also what buildGateway
	// hands the gateway — so a mapping that reached for a different derivation
	// moves this assertion rather than being invisible.
	want := agentgateway.HooksSHA256().Bearer(a.HooksToken)
	if bearer != want {
		t.Errorf("%s = %q, want %q — the derivation agentgateway.HooksSHA256 performs over this "+
			"agent's own token. A mismatch here is a 401 on every turn, attributed to the "+
			"credential rather than to the mapping.", agentspec.EnvGatewayBearer, bearer, want)
	}
	// CONTROL: the derived value must differ from the raw token, which travels
	// separately under agentspec.EnvGatewayToken. Without this a pass-through
	// derivation satisfies the assertion above whenever `want` is computed the same
	// wrong way.
	if bearer == a.HooksToken {
		t.Errorf("%s carries the row's token verbatim", agentspec.EnvGatewayBearer)
	}

	// 🔴 AND THE OFF STATE MUST COME FROM THE SAME MAPPING. A deployment with no
	// runtime named must produce no bundle and no bearer — the state every release
	// before this one shipped.
	off := mustAgentSpecConfig(t, provisionerTestConfig(provisionerK8s))
	if off.RuntimeConfig.Configured() {
		t.Error("a configuration with no runtime named produced a CONFIGURED bundle")
	}
	if off.RuntimeConfig.DeriveBearer != nil {
		t.Error("a configuration with no runtime named still carries a bearer derivation, " +
			"which is a claim about the image that no variable made")
	}
}

// TestAProvisionedAgentIsProbedSoACrashloopIsVisible is the closing condition's
// in-process half.
//
// 🔴 RED AT origin/main, AND THE SYMPTOM THERE IS THE MEASURED ONE. The kubernetes
// driver rendered no probe of any kind, so a provisioned agent whose gateway exits
// 78 reported 0/1 indefinitely with nothing in the cluster timing it out. Both
// assertions below fail at the parent commit.
//
// 🔴 CLOSING CONDITION A REVIEWER CAN RUN WITHOUT A CLUSTER:
//
//	go test ./cmd/muster-server/ -run TestAProvisionedAgentIsProbedSoACrashloopIsVisible -v
//
// ⚠ AND THE HALF THAT NEEDS A CLUSTER, NAMED HONESTLY. That the probe PASSES —
// that the agent's gateway really answers the path on the port — is not provable
// against a fake clientset: nothing is listening and no kubelet runs. doc_seams.go
// entry 1 carries those commands.
func TestAProvisionedAgentIsProbedSoACrashloopIsVisible(t *testing.T) {
	cfg := gatewayTestConfig(provisionerK8s, gatewayHooksSHA256)
	d, cs := reachabilityDriver(t, cfg)
	ctx := context.Background()

	a := reachabilityAgent()
	spec, err := agentspec.Build(a, mustAgentSpecConfig(t, cfg), agentspec.Options{})
	if err != nil {
		t.Fatalf("agentspec.Build: %v", err)
	}
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ns := agents.ResolveNamespacePrefix(cfg.AgentNamespacePrefix) + a.Name
	dep, err := cs.AppsV1().Deployments(ns).Get(ctx, a.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get the Deployment this binary's own spec created in %q: %v", ns, err)
	}
	c := dep.Spec.Template.Spec.Containers[0]

	if c.StartupProbe == nil {
		t.Fatal("the Deployment muster creates carries NO startupProbe. A provisioned agent " +
			"whose gateway exits 78 then reports 0/1 for ever and nothing times it out — which " +
			"is the measured defect, not a hypothetical.")
	}
	if c.LivenessProbe == nil {
		t.Fatal("the Deployment muster creates carries NO livenessProbe")
	}
	// 🔴 THE PROBE'S PORT MUST BE THE PORT THE Service PUBLISHES, OR THE AGENT IS
	// EITHER UNREACHABLE OR UNPROBED AND ONLY ONE OF THOSE IS VISIBLE. Both come
	// from the same declared port, and this is the only place both are in one
	// process: internal/agentspec has no cluster client and internal/provision/k8s
	// has no agent row.
	svc, err := cs.CoreV1().Services(ns).Get(ctx, a.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get the Service: %v", err)
	}
	if got, want := c.StartupProbe.HTTPGet.Port.IntValue(), int(svc.Spec.Ports[0].Port); got != want {
		t.Errorf("the startup probe targets port %d while the Service publishes %d: the probe "+
			"and the address disagree, and only the address failure is visible", got, want)
	}
	if got := c.StartupProbe.HTTPGet.Path; got != agentspec.DefaultGatewayHealthPath {
		t.Errorf("the startup probe path = %q, want %q", got, agentspec.DefaultGatewayHealthPath)
	}
}
