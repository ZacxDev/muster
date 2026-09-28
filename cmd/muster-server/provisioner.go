package main

import (
	"fmt"
	"log"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agentprivilege"
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
//     needs. Every dispatch then 403s — and a 403 from the apiserver reads as a
//     driver defect rather than a missing Role. CLOSING CONDITION: a merged change
//     in the deployment's own repository adding that Role/RoleBinding, verified by
//     one real dispatch.
//     🔴 MUSTER_AGENT_PRIVILEGE_APPLY NEEDS *MORE* THAN THAT SET, AND THIS
//     PARAGRAPH USED TO NAME THE SMALLER HALF OF IT. It said the tier needs the
//     `escalate` verb on `clusterroles` and `bind` for the binding, AND NOTHING
//     ELSE — so a cluster administrator who granted exactly that got a 403 on the
//     first grant. `escalate` and `bind` are ADDITIONAL authorisation checks the
//     apiserver layers on top of an ordinary write, never substitutes for one: the
//     policy path also makes ordinary create/get/update/delete/list calls on all
//     four rbac resource types, and it needs `escalate`/`bind` on `roles` as well
//     as on `clusterroles`, because a profile carrying only namespaceRules writes a
//     Role and a RoleBinding and touches no cluster-scoped object at all.
//     🔴 THE EXACT SET IS ENUMERATED ONCE, AS DATA, IN
//     k8s.PolicyRBACPrerequisite — 18 of its 22 (resource, verb) pairs DERIVED
//     from the call sites in internal/provision/k8s/policy.go and guarded against
//     them by TestTheRBACPrerequisiteMatchesThePolicyCallSites, which fails when
//     that set grows OR shrinks. ⚠ THE OTHER FOUR ARE ASSERTED, NOT DERIVED, AND
//     CALLING THE WHOLE SET DERIVED ERASES THAT: `escalate` and `bind` on `roles`
//     and on `clusterroles` have no call site BY CONSTRUCTION — they are
//     authorisation checks the apiserver layers on top of an ordinary write, not
//     API calls muster makes — so the DERIVATION EXCLUDES them
//     (k8s.PolicyEscalationVerbs is where that exclusion is named rather than
//     hardcoded in the test). THAT IS NOT "UNGUARDED, DELETE FREELY": the same
//     test pins all four EXPLICITLY and pins their ABSENCE on `clusterrolebindings`
//     and `rolebindings`, so dropping one from the enumeration reddens. What
//     nothing in this module can redden is whether they are RIGHT; they are a claim
//     about apiserver behaviour, and only a grant against a real apiserver tests it.
//     It is not restated here, and it must not be: the
//     incomplete version above existed in FOUR files simultaneously, which is why
//     it was wrong in four places at once.
//     A missing permission now surfaces to the caller rather than being swallowed
//     — internal/api logs it and grantProfile returns it before recording the
//     grant — but the cheaper answer while the permissions are missing is to leave
//     this variable unset, which keeps the tier unbuilt. (k8s.Config.PolicyDisabled
//     is the driver's own spelling of the same refusal and is deliberately NOT
//     exposed as a second variable: with this one unset there is no caller of
//     provision.Grant in the binary at all.)
//     ⚠ THE API GROUP IS NAMED IN PROSE RATHER THAN IN ITS FULL DOTTED FORM, and
//     that is a leak-gate accommodation rather than vagueness: the full spelling
//     begins with the word `authorization` followed by 20+ dotted characters, which
//     is exactly tests/leakscan.py's `Authorization: <token>` pattern, and it
//     refused this file. The gate is right that the shape is credential-like.
//     PolicyRBACPrerequisite spells the group through the generated constant for
//     the same reason.
//  2. ROLLBACK IS NOT SYMMETRIC — FOR EITHER KNOB, AND THE SECOND ONE IS WORSE.
//     Once MUSTER_AGENT_PROVISIONER has been enabled and instances exist, rolling
//     the image back to a build WITHOUT internal/agentprovision makes every
//     lifecycle route answer 503 again while the rows AND the instances remain: the
//     instances become unmanageable from muster (no stop, no destroy, no logs) and
//     have to be torn down with cluster tooling. Destroy every instance BEFORE
//     rolling back, or accept a manual teardown.
//     🔴 UNSETTING MUSTER_AGENT_PRIVILEGE_APPLY IS NOT A ROLLBACK, IT IS AN
//     OUTAGE, AND NOTHING SAID SO UNTIL THIS PARAGRAPH. On a deployment that has a
//     database and a provisioner — which is the only kind that can arm this tier —
//     taking the variable away re-enters api.Extensions.defects' second entry:
//     /readyz answers 503, the pod is pulled from its Service, and the whole
//     server goes dark, not just the grant path. The only escape that keeps the pod
//     serving is to unset MUSTER_AGENT_PROVISIONER in the SAME change — and then
//     this variable has to come off too, because config.validateProvisioner
//     refuses an armed applier with no provisioner at boot — which costs the
//     lifecycle tier above. "Leave the privilege store unset" is not a third
//     escape: nothing gates that store on its own variable, so it means running
//     with no database, which also drops notes, agents, runbooks and GitHub.
//     🔴 THE CHEAPEST DISARM IS NOT A VARIABLE AT ALL, AND THIS PARAGRAPH USED TO
//     OMIT IT: DELETE THE ClusterRoleBinding THAT GRANTS MUSTER'S OWN
//     ServiceAccount THE RBAC WRITE SET FROM (1). Nothing in this module checks
//     those permissions before attempting a write — PolicyRBACPrerequisite's own
//     doc says so and says why — and api.Extensions.defects branches on NIL-NESS
//     only, so with the binding gone the applier is still wired, the three
//     conjuncts are unchanged, /readyz still passes, the pod stays in its Service
//     and NO route goes dark. Every grant then fails at apply time with the
//     apiserver's own 403, returned to the caller rather than swallowed. It is a
//     one-file, zero-outage revert, and cheap precisely because the binding is
//     new: the deployment that runs this today has no serviceAccountName and no
//     ClusterRoleBinding of any kind, so arming the tier adds one manifest and
//     disarming it deletes that same one.
//     ⚠ IT DISARMS, IT DOES NOT UNDO. Grants already applied keep their live RBAC
//     — nothing revoked those objects — and revoking them afterwards ALSO 403s,
//     because teardown needs the `delete` verbs from the same set. So this is the
//     first move when the tier has to stop escalating NOW; the two-variable change
//     or an image rollback to a build with no provisioner at all is what retires
//     the tier afterwards. Plan it in that order.
//     🔴 AND IT BREAKS AGENT DESTROY, WHICH IS THE THIRD THING THIS PARAGRAPH
//     OMITTED AND THE ONE THAT INVERTS THE ADVICE UNDER PRESSURE: k8s
//     Driver.Destroy's revokeAllPolicies opens with a LIST of clusterrolebindings,
//     so with the binding gone every destroy returns `destroy … did not complete`
//     at its policy step while the Deployment, Service and — where the driver owns
//     one — the namespace are torn down anyway, since none of those needs an rbac
//     permission — leaving precisely what Destroy's own comment calls policy objects
//     outliving what points at them, "a security bug waiting for a namesake", and
//     what internal/metrics.AgentRBACTeardown's doc spells out as "an orphaned
//     ClusterRoleBinding silently re-grants itself to the next agent that draws
//     the same name — a privilege escalation that no code path performs and no
//     audit of the grant table can see" — so pair this disarm with hand-removing
//     the orphaned ClusterRoles and bindings, or destroy no agents until the
//     binding is back.
//
// ⚠ EVERY KUBERNETES ASSERTION IN THIS PACKAGE'S TESTS IS AGAINST
// k8s.io/client-go/kubernetes/fake. That is what makes them runnable, and it means
// no test here — or anywhere in this module — has ever driven the driver against a
// real apiserver. Plan step 22d is that test.
// ---------------------------------------------------------------------------

// buildAgentPlane builds all THREE tiers of the agent seam over ONE driver: the
// lifecycle provisioner named by cfg, the chat gateway named by cfg, and the
// privilege applier cfg arms. Any of them may be nil.
//
// ⚠ A nil RESULT IS A SUPPORTED DEPLOYMENT AND NOT AN ERROR CASE. api.Extensions
// tolerates a nil Provisioner and a nil Gateway by design — those routes refuse at
// the door with api.ProvisionerUnwiredField — so "no provisioner" and "lifecycle
// without chat" are configurations, not failures. The caller must not treat either
// nil as something to fall back from.
//
// 🔴 THE THIRD TIER IS THE ONE WHOSE nil IS NOT MERELY A REDUCED FEATURE SET. With
// a privilege store present, `Provisioner != nil && PrivilegeApply == nil` is a
// READINESS DEFECT (api.Extensions.defects), so this nil pulls the pod from the
// Service rather than dimming a tab. That is deliberate and it is why the tier is
// off by default: see config.AgentPrivilegeApply.
//
// 🔴 IT RETURNS CONCRETE POINTER TYPES, NOT INTERFACES, AND main.go's nil-check
// DEPENDS ON THAT. Assigning a typed nil pointer to an interface field yields a
// non-nil interface holding a nil pointer, which sails past api's wrappers into a
// nil-pointer method call in a goroutine. The concrete return is what makes the
// check at the assignment site possible. For the privilege tier the consequence is
// the readiness one above INVERTED: a typed nil there makes defects() go quiet and
// /readyz report ready over an applier that nil-derefs on the first grant.
func buildAgentPlane(cfg config, store agents.Store, logger *log.Logger) (*agentprovision.Adapter, *agentgateway.Gateway, *agentprivilege.Applier, error) {
	named := cfg.agentProvisioner()
	if named == provisionerNone {
		// The gateway needs a driver to resolve an address, and so does the
		// privilege applier, so there is nothing to build here either.
		// config.validateProvisioner refuses both combinations — "gateway named,
		// provisioner none" and "privilege apply armed, provisioner none" — at boot
		// rather than letting them arrive here as silent nils.
		return nil, nil, nil, nil
	}
	if store == nil {
		// Reachable only with no database: the stores are built inside the
		// `cfg.Database != ""` branch. An adapter with no store would resolve no
		// agent id, so every lifecycle call would fail at its first line — and it
		// would do so from a background goroutine, where the only trace is a log
		// line nobody is reading.
		return nil, nil, nil, fmt.Errorf("%s=%s needs an agents store, and there is none because %s is "+
			"unset: an agent provisioner resolves every request through the database",
			envAgentProvisioner, named, envDatabase)
	}

	driver, err := buildDriver(cfg, logger)
	if err != nil {
		return nil, nil, nil, err
	}
	prov, err := agentprovision.New(agentprovision.Config{
		Driver: driver,
		Store:  store,
		Spec:   agentSpecConfig(cfg),
		Logger: logger,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	gw, err := buildGateway(cfg, driver)
	if err != nil {
		return nil, nil, nil, err
	}
	priv, err := buildPrivilegeApplier(cfg, driver)
	if err != nil {
		return nil, nil, nil, err
	}
	return prov, gw, priv, nil
}

// buildPrivilegeApplier builds the privilege tier over an already-constructed
// driver, or (nil, nil) when the configuration has not armed it.
//
// 🔴 IT TAKES THE DRIVER RATHER THAN BUILDING ONE, AND THAT IS THE SAME
// LOAD-BEARING REASON THE GATEWAY DOES. The lifecycle tier creates the instance's
// ServiceAccount; this tier binds cluster RBAC to it. A second driver built from
// the same configuration agrees today and diverges the moment anything about a
// driver is per-instance — and the failure would be a grant refused as "not
// managed by this driver" over a ServiceAccount muster demonstrably created,
// which reads as a defect in the ownership predicate rather than as two clients.
//
// ⚠ IT IS NOT REFUSED FOR THE noop DRIVER, AND THE FIRST DRAFT OF THIS FUNCTION
// REFUSED IT. The noop driver implements no provision.PolicyGranter, so
// provision.Grant refuses every grant with a reason that names the driver —
// which is the honest, per-grant answer and leaves the privilege UI developable
// without a cluster. A boot refusal would have rejected a working development
// configuration; and the readiness check's own "⚠ IT OVER-TRIGGERS FOR A
// PROVISIONER THAT CREATES NOTHING" note is why arming it there is coherent
// rather than a lie: the grant fails loudly instead of being recorded silently.
func buildPrivilegeApplier(cfg config, driver provision.Provisioner) (*agentprivilege.Applier, error) {
	if !cfg.AgentPrivilegeApply {
		return nil, nil
	}
	return agentprivilege.New(agentprivilege.Config{Driver: driver})
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
func buildGateway(cfg config, driver provision.Provisioner) (*agentgateway.Gateway, error) {
	switch cfg.agentGateway() {
	case gatewayNone:
		return nil, nil
	case gatewayHooksSHA256:
		return agentgateway.New(agentgateway.Config{
			Driver:  driver,
			Runtime: agentgateway.HooksSHA256(),
			Model:   cfg.AgentGatewayModel,
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
