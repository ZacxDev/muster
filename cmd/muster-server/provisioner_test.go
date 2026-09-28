package main

import (
	"log"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/ZacxDev/muster/internal/agents"
	k8sdriver "github.com/ZacxDev/muster/internal/provision/k8s"
)

// stubStore is an agents.Store that exists only to be non-nil.
//
// ⚠ IT EMBEDS THE INTERFACE RATHER THAN IMPLEMENTING IT, so any method a test
// actually reaches is a nil-interface panic. That is the loud outcome: these tests
// are about WIRING, and a stub that answered zero values would let a behavioural
// assertion slip in here and pass over a fake.
type stubStore struct{ agents.Store }

// provisionerTestConfig is a config that passes validateProvisioner for the named
// driver, so each test below varies ONE thing.
func provisionerTestConfig(driver string) config {
	return config{
		Database:             "postgres://unused",
		AgentProvisioner:     driver,
		AgentImageRepo:       "registry.example.test/muster/agent-runtime",
		AgentAPIURL:          "http://muster.example.test:8105",
		AgentNamespacePrefix: agents.NamespacePrefix,
	}
}

// TestBuildingTheKubernetesProvisionerUsesTheRealDriver is the behavioural half of
// the linkage ledger's departure.
//
// 🔴 LINKAGE SAYS "REACHABLE", NOT "USED", AND THE LEDGER'S OWN HEADER SAYS SO: an
// import can be a blank `_` that executes nothing. internal/provision/k8s left the
// not-linked ledger because provisioner.go imports it — this is what makes that a
// claim about a CONSTRUCTED driver rather than about an import line.
//
// 🔴 IT BUILDS THE DRIVER FROM A FAKE CLIENTSET RATHER THAN SKIPPING WITHOUT A
// CLUSTER, and that is the whole reason k8s.Config.Client is an interface. A test
// that needed a real cluster would be a t.Skip on CI — which this project's own
// notes call worse than no test, because a suite that ran nothing still reports
// green.
func TestBuildingTheKubernetesProvisionerUsesTheRealDriver(t *testing.T) {
	logger := log.New(&strings.Builder{}, "", 0)
	dc := k8sDriverConfig(provisionerTestConfig(provisionerK8s), logger)
	dc.Client = fake.NewClientset()

	driver, err := k8sdriver.New(dc)
	if err != nil {
		t.Fatalf("k8sdriver.New over the mapped config: %v", err)
	}
	// 🔴 THE DRIVER'S OWN NAME, NOT A TYPE ASSERTION. provision.Provisioner.Driver
	// exists so an operator knows which set of capability losses applies; asserting
	// on it here means a future change that swapped the Kubernetes driver for a
	// stand-in with the same method set reddens, which a `*k8sdriver.Driver` type
	// check would too — but the name is also what the boot banner reports, so this
	// assertion and the banner cannot disagree.
	if got := driver.Driver(); got != "kubernetes" {
		t.Errorf("Driver() = %q, want %q", got, "kubernetes")
	}

	// NEGATIVE CONTROL: the noop branch must produce a DIFFERENT driver. Without
	// it, an assertion that a driver reports "kubernetes" would pass over a
	// buildDriver that ignored its argument and always built the same thing.
	noop, err := buildDriver(provisionerTestConfig(provisionerNoop), logger)
	if err != nil {
		t.Fatalf("buildDriver(noop): %v", err)
	}
	if got := noop.Driver(); got == driver.Driver() {
		t.Errorf("the noop branch built a driver reporting %q, the same as the kubernetes "+
			"branch — buildDriver is not branching on its argument", got)
	}

	// 🔴 AND buildDriver'S kubernetes BRANCH MUST ROUTE TO buildK8sDriver, which
	// the assertions above cannot see. Found by mutation: replacing
	// `case provisionerK8s: return buildK8sDriver(...)` with `return
	// provision.NewNoop()` SURVIVED, because this test constructs the driver from
	// k8sDriverConfig DIRECTLY and never asks buildDriver for one. Outside a pod
	// the real branch REFUSES, so a nil error here means it routed somewhere else.
	if _, err := buildDriver(provisionerTestConfig(provisionerK8s), logger); err == nil {
		t.Error("buildDriver(kubernetes) succeeded outside a cluster, so its kubernetes case " +
			"is not reaching buildK8sDriver — something else is answering for it.")
	}
}

// TestTheKubernetesBranchRefusesOutsideAClusterRatherThanFallingBack pins the
// refusal, which is the safety property.
//
// 🔴 A KUBECONFIG FALLBACK WOULD PROVISION INTO WHICHEVER CLUSTER THE AMBIENT
// CONTEXT NAMES. That is the failure where a developer's test dispatch creates
// pods in production, and it is a failure of OMISSION — nothing would log, because
// from the binary's point of view it worked.
//
// ⚠ IT ASSERTS THE MESSAGE NAMES THE VARIABLE THAT CAUSED IT. An error that said
// only "unable to load in-cluster configuration" sends the reader to look at
// Kubernetes; the variable is what they actually set.
func TestTheKubernetesBranchRefusesOutsideAClusterRatherThanFallingBack(t *testing.T) {
	// This test process is not a pod: rest.InClusterConfig has no service-account
	// token to read.
	_, _, _, err := buildAgentPlane(provisionerTestConfig(provisionerK8s), stubStore{}, log.New(&strings.Builder{}, "", 0))
	if err == nil {
		t.Fatal("buildAgentPlane succeeded for the kubernetes driver outside a cluster, " +
			"which means it fell back to some other credential source. A fallback " +
			"provisions into whichever cluster the ambient context names.")
	}
	if !strings.Contains(err.Error(), envAgentProvisioner) {
		t.Errorf("the refusal does not name %s, so a reader is sent to debug Kubernetes "+
			"rather than the variable they set.\n  got: %v", envAgentProvisioner, err)
	}
}

// TestTheNoopProvisionerWiresAnAdapterAndNoneWiresNothing pins both ends of the
// knob, because the default is the one that must not change behaviour.
//
// 🔴 (nil, nil) FOR `none` IS A SUPPORTED RESULT, NOT AN ERROR CASE. Every
// previous revision of this binary had no provisioner at all, and a default that
// provisioned would change what an unchanged deployment does on the next image
// bump — where the thing it changes is "create pods in a cluster".
func TestTheNoopProvisionerWiresAnAdapterAndNoneWiresNothing(t *testing.T) {
	logger := log.New(&strings.Builder{}, "", 0)

	adapter, _, _, err := buildAgentPlane(provisionerTestConfig(provisionerNoop), stubStore{}, logger)
	if err != nil {
		t.Fatalf("buildAgentPlane(noop): %v", err)
	}
	if adapter == nil {
		t.Fatal("buildAgentPlane(noop) returned nil, so the lifecycle routes would keep refusing")
	}
	if got := adapter.Driver(); got != "noop" {
		t.Errorf("adapter.Driver() = %q, want %q", got, "noop")
	}

	none, _, _, err := buildAgentPlane(provisionerTestConfig(provisionerNone), stubStore{}, logger)
	if err != nil {
		t.Fatalf("buildAgentPlane(none): %v", err)
	}
	if none != nil {
		t.Error("buildAgentPlane(none) returned a provisioner. The default must leave " +
			"api.Extensions.Provisioner nil so every lifecycle route keeps refusing at the " +
			"door exactly as it did before this knob existed.")
	}

	// 🔴 THE ZERO-VALUE CONFIG IS ITS OWN CASE, AND IT WAS MISSING. Found by
	// mutation: changing config.agentProvisioner's empty-string default from
	// provisionerNone to provisionerNoop SURVIVED, because the case above passes
	// "none" EXPLICITLY and never exercises the resolver. buildApp accepts configs
	// that never went through loadConfig — every wiring test builds one — so this is
	// the shape a zero value actually reaches the wiring in, and a default that
	// provisioned would create pods for a deployment that asked for nothing.
	zero, _, _, err := buildAgentPlane(config{}, stubStore{}, logger)
	if err != nil {
		t.Fatalf("buildAgentPlane over a zero-value config: %v", err)
	}
	if zero != nil {
		t.Errorf("a zero-value config produced a %q provisioner. An unset "+
			"%s must resolve to %q: a default that provisions changes what an unchanged "+
			"deployment does on its next image bump, and what it changes is \"create pods "+
			"in a cluster\".", zero.Driver(), envAgentProvisioner, provisionerNone)
	}

	// A provisioner with no store could resolve no agent id, and would fail from a
	// background goroutine whose only trace is a log line.
	if _, _, _, err := buildAgentPlane(provisionerTestConfig(provisionerNoop), nil, logger); err == nil {
		t.Error("buildAgentPlane accepted a nil agents store")
	}
}

// TestTheStoredNamespacePrefixIsWhatTheDriverIsConfiguredWith is the guard
// internal/agents.NamespacePrefix's own comment asks for.
//
// 🔴 IT PINS A RELATIONSHIP BETWEEN TWO SUBSYSTEMS THAT NEVER TALK, AND A
// DISAGREEMENT BETWEEN THEM IS SILENT. The HTTP handler writes
// agents.NamespaceFor(name) into the row; the driver's own NamespacePrefix decides
// where the instance really goes. Nothing fails if they differ — the card renders
// a namespace that holds nothing, so `kubectl -n <what the card says>` returns
// nothing and reads as "this agent was never provisioned" while the pod runs one
// namespace over.
//
// ⚠ IT CHECKS THE *DEFAULT*, which is the case that can drift. An operator who
// sets MUSTER_AGENT_NAMESPACE_PREFIX has said what they want and owns the
// consequence; the default is what nobody looks at.
func TestTheStoredNamespacePrefixIsWhatTheDriverIsConfiguredWith(t *testing.T) {
	cfg, err := loadConfig(func(name string) string {
		switch name {
		case envAgentProvisioner:
			return provisionerK8s
		case envAgentImageRepo:
			return "registry.example.test/muster/agent-runtime"
		case envAgentAPIURL:
			return "http://muster.example.test:8105"
		case envDatabase:
			return "postgres://unused"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	const name = "harbour-kestrel"
	stored := agents.NamespaceFor(name)
	dc := k8sDriverConfig(cfg, nil)
	driverSide := dc.NamespacePrefix + name

	if stored != driverSide {
		t.Errorf("the row would say namespace %q and the driver would create %q.\n"+
			"    Nothing fails when these differ: the agent card names a namespace that "+
			"holds nothing, which reads as \"never provisioned\" while the instance runs.\n"+
			"    Both sides must come from agents.NamespacePrefix — see its doc comment.",
			stored, driverSide)
	}

	// 🔴 AND THE LAYOUT MUST BE PER-INSTANCE, OR THE PREFIX IS NEVER USED AT ALL.
	// k8s.Config.NamespacePrefix is read only when NamespacePerInstance is true, so
	// a prefix that matches while the layout is shared makes the check above pass
	// over a driver that puts every instance in one namespace the row never names.
	if !dc.NamespacePerInstance {
		t.Errorf("the default layout is SHARED, so NamespacePrefix (%q) is ignored and the "+
			"assertion above is vacuous. %s defaults to false, which must map to "+
			"NamespacePerInstance TRUE.", dc.NamespacePrefix, envAgentNSShared)
	}

	// Control: the prefix is non-empty, or "stored == driverSide" holds trivially
	// for every possible value.
	if dc.NamespacePrefix == "" {
		t.Errorf("instrument check FAILED: the default NamespacePrefix is empty, so the "+
			"comparison above would pass for any name. %s should default to %q",
			envAgentNSPrefix, agents.NamespacePrefix)
	}
}

// TestTheSharedNamespaceLayoutIsRefusedWithoutANamespace pins the one boot refusal
// that is about a combination rather than a missing value.
func TestTheSharedNamespaceLayoutIsRefusedWithoutANamespace(t *testing.T) {
	cfg := provisionerTestConfig(provisionerK8s)
	cfg.AgentNamespaceShared = true
	if err := cfg.validate(); err == nil {
		t.Fatalf("validate accepted %s=1 with no %s: the kubernetes driver requires a "+
			"namespace in that layout, so every dispatch would be refused inside a "+
			"goroutine instead of at boot", envAgentNSShared, envAgentNamespace)
	}
	cfg.AgentNamespace = "muster-agents"
	if err := cfg.validate(); err != nil {
		t.Errorf("validate rejected the complete shared layout: %v", err)
	}
}

// TestAnIllegalProvisionerNameIsRefusedAtBoot pins the enumeration.
//
// ⚠ THE UNHANDLED BRANCH IN buildDriver IS THE OTHER HALF OF THIS, and it returns
// an error naming itself a wiring bug rather than a nil driver — because a nil
// there would present as a working server whose every dispatch panicked in a
// goroutine.
func TestAnIllegalProvisionerNameIsRefusedAtBoot(t *testing.T) {
	cfg := provisionerTestConfig("kubernets") // a plausible typo
	err := cfg.validate()
	if err == nil {
		t.Fatal("validate accepted an unknown provisioner name")
	}
	for _, want := range provisionerChoices {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not list the legal value %q, so the reader is told "+
				"they are wrong without being told what is right.\n  got: %v", want, err)
		}
	}

	// And the wiring branch must refuse rather than return a nil driver, for a
	// value validate would have caught.
	if _, err := buildDriver(cfg, nil); err == nil {
		t.Error("buildDriver returned a driver for an unknown provisioner name")
	}
}
