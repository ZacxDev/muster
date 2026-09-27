package main

import (
	"fmt"
	"log"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
	k8sdriver "github.com/ZacxDev/muster/internal/provision/k8s"
)

// ---------------------------------------------------------------------------
// THE PROVISIONER SEAM'S CLOSING HALF.
//
// 🔴 THIS FILE IS WHAT MAKES internal/provision/k8s REACHABLE FROM A PROCESS,
// AND THAT WAS THE WHOLE MECHANICAL POINT. Before it, the Kubernetes driver —
// 2,565 non-test lines, fully tested against a fake clientset — was on the
// not-linked ledger in internal/modulegate/linkage_test.go: it compiled, its
// tests passed, and `go list -deps ./cmd/...` did not resolve it, so no process
// could execute a line of it. That ledger is ASSERTED, not an allowlist: it fails
// when the set shrinks as well as when it grows, so the two entries this change
// removes are the signal, not paperwork.
//
// 🔴 AND "IMPORTED" IS NOT "USED" — the ledger's own header says so, because an
// import can be a blank `_` that executes nothing. The driver is CONSTRUCTED
// here, from a real in-cluster client, behind a configuration value that names
// it; TestBuildingTheKubernetesProvisionerUsesTheRealDriver pins that the
// wired-up adapter reports the kubernetes driver's own name rather than any
// stand-in.
//
// ✅ CHAT WAS THE OPEN HALF AND IT IS WIRED NOW, BY A SECOND TYPE RATHER THAN BY
// WIDENING THE FIRST. The paragraph here used to read "WHAT IS STILL OPEN AFTER
// THIS FILE: chat. The adapter satisfies api.Provisioner (lifecycle) and NOT
// api.Gateway". The adapter still does not, and must not: internal/agentgateway
// is a separate type over the SAME driver, assigned to api.Extensions.Gateway
// below. The split's purpose was that each half can be wired independently, and
// the way to use that is two implementations — not one type growing two methods
// and losing the ability to refuse honestly.
//
// 🔴 BOTH HALVES SHARE ONE DRIVER INSTANCE, AND THAT IS LOAD-BEARING RATHER THAN
// THRIFTY. The gateway asks the driver where an agent is reachable; the lifecycle
// adapter asks the same driver to create it there. Two driver instances built from
// the same configuration would agree today and diverge the moment anything about a
// driver is stateful or per-instance — and the failure would be a chat turn that
// resolves no endpoint for a pod that demonstrably exists.
//
// 🔴 TWO PREREQUISITES FOR `kubernetes` THAT LIVE OUTSIDE THIS REPOSITORY, NAMED
// HERE BECAUSE NOTHING ELSE IN IT CAN CHECK THEM:
//
//  1. RBAC. This module ships no Kubernetes manifest of any kind, so nothing here
//     grants muster's own ServiceAccount the Deployment / Service / Secret /
//     ConfigMap / ServiceAccount / Namespace permissions internal/provision/k8s
//     needs. Past the readiness defect in doc_seams.go entry 2, every dispatch
//     403s — and a 403 from the apiserver reads as a driver defect rather than a
//     missing Role. CLOSING CONDITION: a merged change in the deployment's own
//     repository adding that Role/RoleBinding, verified by one real dispatch.
//  2. ROLLBACK IS NOT SYMMETRIC. Once this has been enabled and instances exist,
//     rolling the image back to a build WITHOUT internal/agentprovision makes every
//     lifecycle route answer 503 again while the rows AND the instances remain: the
//     instances become unmanageable from muster (no stop, no destroy, no logs) and
//     have to be torn down with cluster tooling. Destroy every instance BEFORE
//     rolling back, or accept a manual teardown.
//
// ⚠ EVERY KUBERNETES ASSERTION IN THIS PACKAGE'S TESTS IS AGAINST
// k8s.io/client-go/kubernetes/fake. That is what makes them runnable, and it means
// no test here — or anywhere in this module — has ever driven the driver against a
// real apiserver. Plan step 22d is that test.
// ---------------------------------------------------------------------------

// buildAgentPlane builds both halves of the agent seam over ONE driver: the
// lifecycle provisioner named by cfg, and the chat gateway named by cfg. Either
// or both may be nil.
//
// ⚠ A nil RESULT IS A SUPPORTED DEPLOYMENT AND NOT AN ERROR CASE. api.Extensions
// tolerates a nil Provisioner and a nil Gateway by design — those routes refuse at
// the door with api.ProvisionerUnwiredField — so "no provisioner" and "lifecycle
// without chat" are configurations, not failures. The caller must not treat either
// nil as something to fall back from.
//
// 🔴 IT RETURNS CONCRETE POINTER TYPES, NOT INTERFACES, AND main.go's nil-check
// DEPENDS ON THAT. Assigning a typed nil pointer to an interface field yields a
// non-nil interface holding a nil pointer, which sails past api's wrappers into a
// nil-pointer method call in a goroutine. The concrete return is what makes the
// check at the assignment site possible.
func buildAgentPlane(cfg config, store agents.Store, logger *log.Logger) (*agentprovision.Adapter, *agentgateway.Gateway, error) {
	named := cfg.agentProvisioner()
	if named == provisionerNone {
		// The gateway needs a driver to resolve an address, so there is nothing to
		// build here either. config.validateProvisioner refuses the combination
		// "gateway named, provisioner none" at boot rather than letting it arrive
		// here as a silent nil gateway.
		return nil, nil, nil
	}
	if store == nil {
		// Reachable only with no database: the stores are built inside the
		// `cfg.Database != ""` branch. An adapter with no store would resolve no
		// agent id, so every lifecycle call would fail at its first line — and it
		// would do so from a background goroutine, where the only trace is a log
		// line nobody is reading.
		return nil, nil, fmt.Errorf("%s=%s needs an agents store, and there is none because %s is "+
			"unset: an agent provisioner resolves every request through the database",
			envAgentProvisioner, named, envDatabase)
	}

	driver, err := buildDriver(cfg, logger)
	if err != nil {
		return nil, nil, err
	}
	prov, err := agentprovision.New(agentprovision.Config{
		Driver: driver,
		Store:  store,
		Spec:   agentSpecConfig(cfg),
		Logger: logger,
	})
	if err != nil {
		return nil, nil, err
	}
	gw, err := buildGateway(cfg, driver, logger)
	if err != nil {
		return nil, nil, err
	}
	return prov, gw, nil
}

// buildGateway builds the agent chat gateway named by cfg over an
// already-constructed driver, or (nil, nil) when the configuration names none.
//
// 🔴 THE RUNTIME IS NAMED BY CONFIGURATION AND HAS NO DEFAULT, WHICH IS THE WHOLE
// REASON THIS IS NOT FOLDED INTO THE PROVISIONER'S SWITCH. Two of the three facts
// a chat turn needs — the bearer derivation and the model sentinel — belong to the
// agent IMAGE, not to the provisioning backend, and they are independent of it: the
// kubernetes driver can run any image, and the noop driver can record a spec for
// one. A deployment that names a driver is saying where instances live; naming a
// runtime is saying what protocol the thing inside speaks.
func buildGateway(cfg config, driver provision.Provisioner, logger *log.Logger) (*agentgateway.Gateway, error) {
	switch cfg.agentGateway() {
	case gatewayNone:
		return nil, nil
	case gatewayHooksSHA256:
		return agentgateway.New(agentgateway.Config{
			Driver:  driver,
			Runtime: agentgateway.HooksSHA256(),
			Model:   cfg.AgentGatewayModel,
			Logger:  logger,
		})
	default:
		// Unreachable: config.validateProvisioner refuses anything else at boot. A
		// silent nil here would present as a working server whose chat routes all
		// answered 503 with no reason an operator could find.
		return nil, fmt.Errorf("unhandled %s %q (config.validateProvisioner should have refused "+
			"it at boot; this is a wiring bug, not a configuration one)",
			envAgentGateway, cfg.agentGateway())
	}
}

// buildDriver builds the provisioning backend itself.
func buildDriver(cfg config, logger *log.Logger) (provision.Provisioner, error) {
	switch cfg.agentProvisioner() {
	case provisionerNoop:
		// The recording driver. It is a real driver with declared capabilities
		// that its own CheckSpec enforces and a Destroy that removes state — see
		// its type doc on why it is not the "declared but inert" fake its
		// predecessor was — so it is a legitimate deployment for developing
		// everything above the provisioner without a cluster.
		//
		// 🔴 IT IS NOT WHAT CLEARS THE LEDGER, and that distinction is the one
		// this whole step was told not to fudge. internal/provision already
		// linked (internal/api imports it); the entry that had to move is
		// internal/provision/k8s, and only the branch below reaches it. A noop
		// wired to satisfy a ledger would be exactly the "fake consumer" the
		// ledger's own entry forbids.
		return provision.NewNoop()
	case provisionerK8s:
		return buildK8sDriver(cfg, logger)
	default:
		// Unreachable: config.validateProvisioner refuses anything else at boot.
		// A silent nil here would present as a working server whose every
		// dispatch panicked in a goroutine.
		return nil, fmt.Errorf("unhandled %s %q (config.validateProvisioner should have refused "+
			"it at boot; this is a wiring bug, not a configuration one)",
			envAgentProvisioner, cfg.agentProvisioner())
	}
}

// buildK8sDriver builds the Kubernetes driver from the pod's own service
// account.
//
// 🔴 IT IS IN-CLUSTER ONLY, AND SAYING SO IS BETTER THAN A KUBECONFIG FALLBACK.
// A fallback to a local kubeconfig would make this binary provision against
// whatever cluster the developer's current context names — which is the failure
// mode where a test dispatch creates pods in production. In-cluster config fails
// with a message naming what is missing, and that message is the correct outcome
// outside a pod.
func buildK8sDriver(cfg config, logger *log.Logger) (provision.Provisioner, error) {
	rc, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("%s=%s needs in-cluster credentials and there are none: %w "+
			"(this binary does not fall back to a local kubeconfig, deliberately — a fallback "+
			"would provision into whichever cluster the ambient context names)",
			envAgentProvisioner, provisionerK8s, err)
	}
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	dc := k8sDriverConfig(cfg, logger)
	dc.Client = client
	dc.RESTConfig = rc
	return k8sdriver.New(dc)
}

// k8sDriverConfig maps this binary's configuration onto the driver's, WITHOUT
// touching a cluster.
//
// 🔴 THE SPLIT FROM buildK8sDriver IS WHAT MAKES THE MAPPING TESTABLE, and the
// mapping is where the mistakes are. Two of the fields below invert or default
// their source, and neither error would announce itself: a wrong
// NamespacePerInstance puts instances somewhere the stored row does not name, and
// a nil WorkspaceStorageClass makes the driver refuse persistence at the first
// dispatch rather than at boot. Acquiring the client is the part that needs a
// pod; deciding what to tell the driver is not, so only the first is left
// unreachable from a test.
func k8sDriverConfig(cfg config, logger *log.Logger) k8sdriver.Config {
	dc := k8sdriver.Config{
		// 🔴 THE FLAG IS INVERTED HERE, ON PURPOSE, AND THIS IS THE ONLY PLACE IT
		// HAPPENS. The driver's knob is NamespacePerInstance (default false =
		// shared); the deployment's is MUSTER_AGENT_NAMESPACE_SHARED (default
		// false = per-instance). See config.AgentNamespaceShared for why the
		// deployment default is the opposite of the driver's.
		NamespacePerInstance: !cfg.AgentNamespaceShared,
		NamespacePrefix:      cfg.AgentNamespacePrefix,
		Namespace:            cfg.AgentNamespace,
		EndpointTemplate:     cfg.AgentEndpointTemplate,
		Logger:               logger,
	}
	if cfg.AgentWorkspacePersist {
		// The pointer's PRESENCE is what raises Capabilities.Persistence; its
		// value being empty means "the cluster's default StorageClass". See
		// config.AgentWorkspacePersist on why an env var cannot express the third
		// state on its own.
		class := cfg.AgentStorageClass
		dc.WorkspaceStorageClass = &class
	}
	return dc
}

// agentSpecConfig is the deployment-wide half of every agent spec.
//
// ⚠ THE RESOURCE FIELDS agentspec.Config CARRIES ARE NOT EXPOSED HERE YET, and
// that is a gap rather than a decision: left empty, each one falls through to the
// driver's own default. It is stated because the alternative — a reader assuming
// the whole of agentspec.Config is reachable from the environment — is how a
// documented knob turns out to be unread, which is the finding config.go's const
// block exists to answer.
func agentSpecConfig(cfg config) agentspec.Config {
	return agentspec.Config{
		ImageRepo:        cfg.AgentImageRepo,
		ImageTag:         cfg.AgentImageTag,
		APIBaseURL:       cfg.AgentAPIURL,
		Model:            cfg.AgentModel,
		OpenRouterAPIKey: cfg.AgentOpenRouterKey,
		WorkspacePersist: cfg.AgentWorkspacePersist,
	}
}
