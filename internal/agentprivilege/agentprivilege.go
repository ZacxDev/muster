// Package agentprivilege adapts a [provision.Provisioner] DRIVER to the
// PRIVILEGE-APPLY interface the privilege handlers depend on
// (api.PrivilegeApplier): two methods over an agent's NAME and a
// [privilege.Profile], rather than over a [provision.Ref] and a
// [provision.Policy].
//
// It is the third adapter over the same driver, and the naming says so
// deliberately: internal/agentprovision is the LIFECYCLE half,
// internal/agentgateway is the CHAT half, and this is the PRIVILEGE half. Each
// is a separate type behind a separate nil, so a deployment can wire one and
// refuse honestly on the others — which is the argument agentprovision's own doc
// makes at length about not letting one type grow a second responsibility and
// lose the ability to refuse.
//
// 🔴 IT IMPLEMENTS NO RBAC OF ITS OWN, AND THAT IS THE MOST LOAD-BEARING FACT
// ABOUT IT. Every Kubernetes object a grant needs is already rendered by
// internal/provision/k8s's Grant/Revoke: the deterministic (instance, policy)
// object name with the digest that makes the pair recoverable, the
// create-only bindings whose RoleRef is immutable, the ownership predicate that
// refuses to write over — or delete — an object muster did not create, and the
// STRICT decode that refuses a rules payload this build does not fully
// understand. A second applier written beside them would be wrong at every one
// of those points, in the same direction, and its `granted` chip would read as
// coverage. So this package TRANSLATES and DELEGATES; it does not re-derive.
//
// 🔴 AND IT GOES THROUGH provision.Grant / provision.Revoke, NEVER THROUGH A
// PolicyGranter TYPE ASSERTION OF ITS OWN. Those two functions are that
// package's own stated security control: they refuse a driver that cannot apply
// policy at all, and a driver that implements the interface while reporting
// Capabilities.Policy false — which is how a driver says "configured without
// the `escalate`/`bind` verbs this needs". Asserting the interface here would
// skip the capability half, and the observable would be a grant reported applied
// by a driver that had already declared it could not.
//
// ⚠ WHY IT IS NOT IN internal/provision, WHICH api.PrivilegeApplier'S OWN
// COMMENT NAMES AS THE IMPLEMENTOR. That comment predates the two adapters above
// and points at the package that holds the client, which is the right instinct
// and the wrong package: internal/provision is the driver CONTRACT, and putting
// this there would make the contract import internal/privilege — a PERSISTENCE
// domain, with a Store interface and a Postgres implementation behind it. The
// dependency would run from the thing every driver implements towards the thing
// one HTTP handler stores, and no driver could then be written without it. The
// comment is corrected rather than obeyed.
//
// 🔴 THE PAYLOAD THIS BUILDS IS THE KUBERNETES DRIVER'S, AND A DRIVER THAT
// CANNOT READ IT REFUSES RATHER THAN PARTIALLY APPLYING. provision.Policy.Rules
// is opaque by design; the shape here is [k8s.Rules], reached through that
// package's exported type rather than by hand-assembling JSON, so the encoder
// and the strict decoder cannot drift. Against the noop driver — which
// implements no PolicyGranter — provision.Grant refuses before any of this is
// read, with a reason naming the driver. That refusal is the honest answer and
// the reason this package does not need to know which driver it was given.
package agentprivilege

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/ZacxDev/muster/internal/privilege"
	"github.com/ZacxDev/muster/internal/provision"
	k8sdriver "github.com/ZacxDev/muster/internal/provision/k8s"
)

// Config is everything the applier needs. New says which field is missing
// rather than producing an applier that fails at the first grant.
type Config struct {
	// Driver is the provisioning backend whose policy tier applies the grant.
	// REQUIRED.
	//
	// 🔴 IT MUST BE THE SAME DRIVER INSTANCE THE LIFECYCLE ADAPTER HOLDS, for the
	// reason cmd/muster-server/provisioner.go gives about the gateway: the
	// lifecycle adapter creates the instance's ServiceAccount, and this tier binds
	// RBAC to it. Two drivers built from one configuration agree today and
	// diverge the moment anything about a driver is per-instance — and the failure
	// would be a grant refused as "not managed by this driver" over a
	// ServiceAccount muster demonstrably created.
	Driver provision.Provisioner

	// ⚠ THERE IS DELIBERATELY NO Logger. Every error either method can produce is
	// RETURNED, and both call sites in internal/api already log what they get —
	// so a logger here would only ever duplicate a line, and a field nothing
	// writes to reads as a facility a reader will go looking for. The two adapters
	// beside this one carry one because they run work in background goroutines
	// whose errors reach nobody; this one does not.
}

// Applier implements the consumer-side privilege-apply interface over a driver.
//
// ⚠ IT DOES NOT NAME THAT INTERFACE IN A COMPILE-TIME ASSERTION, for the same
// reason *agentprovision.Adapter does not: internal/api declares it
// consumer-side precisely so this direction of the dependency does not exist.
// The binding is checked where it is used — cmd/muster-server assigns an
// *Applier to api.Extensions.PrivilegeApply, which is a compile error if the
// method set drifts — and internal/api's own
// TestThePrivilegeApplierSatisfiesTheConsumerInterface pins it from the side
// that owns the interface.
type Applier struct {
	driver provision.Provisioner
}

// New validates the configuration and builds the applier.
func New(cfg Config) (*Applier, error) {
	if cfg.Driver == nil {
		return nil, errors.New("agentprivilege: Config.Driver is required (an applier over no " +
			"driver would nil-deref on the first grant, inside a handler or a dispatch " +
			"goroutine — and until it did, api.Extensions.defects would read the non-nil " +
			"interface as a wired applier and let /readyz report ready)")
	}
	return &Applier{driver: cfg.Driver}, nil
}

// Driver returns the backend's short name, for the boot banner and for error
// messages that have to say which set of capability losses applies.
func (a *Applier) Driver() string { return a.driver.Driver() }

// ApplyGrant applies profile's Kubernetes RBAC to the agent's identity.
//
// 🔴 namespace IS NOT A PLACEMENT INPUT, AND PASSING A WRONG ONE CANNOT MOVE AN
// OBJECT. Every caller in internal/api passes agents.Agent.Namespace — the value
// the HTTP handler WROTE on the row, from agents.NamespaceFor — while the
// namespace an instance actually lives in is the DRIVER's answer, from its own
// NamespacePerInstance/NamespacePrefix/Namespace configuration. Those are two
// subsystems that must agree and whose disagreement is invisible;
// agents.NamespacePrefix's own doc says so, and
// TestTheStoredNamespacePrefixIsWhatTheDriverIsConfiguredWith is where the pair
// is pinned. Honouring the argument here would make a stale row DECIDE where
// cluster RBAC lands — binding a Role in a namespace the pod is not in, and
// reporting the grant applied. So the driver decides, always, and
// TestTheCallersNamespaceArgumentCannotRelocateTheRBAC pins it.
//
// ⚠ IT IS IDEMPOTENT BECAUSE THE DRIVER'S Grant IS: provision.PolicyGranter's
// contract requires granting the same policy twice to be one grant, and the
// kubernetes driver satisfies it by upserting the roles and create-only-ing the
// bindings under a deterministic name. Nothing is re-derived here.
func (a *Applier) ApplyGrant(ctx context.Context, agentName, namespace string, profile privilege.Profile) error {
	pol, err := policyFor(profile)
	if err != nil {
		return fmt.Errorf("agentprivilege: apply %q to %q: %w", profile.Name, agentName, err)
	}
	if err := provision.Grant(ctx, a.driver, refFor(agentName), pol); err != nil {
		return fmt.Errorf("agentprivilege: apply %q to %q: %w", profile.Name, agentName, err)
	}
	return nil
}

// RemoveGrant removes the RBAC this service applied for (agent, profile).
//
// ⚠ IT TAKES A PROFILE NAME RATHER THAN A PROFILE, AND THAT IS WHY IT CAN RUN
// AFTER THE PROFILE IS GONE. The driver finds the objects from the
// (instance, policy) name pair, so a revoke needs no rules — which matters
// because internal/api's revoke path reads the profile only to re-apply env, and
// a deleted profile cascades its grants away.
//
// 🔴 A DRIVER THAT CANNOT GRANT RETURNS nil HERE RATHER THAN AN ERROR, AND THAT
// ASYMMETRY IS provision.Revoke'S, NOT THIS PACKAGE'S: a driver that cannot
// grant cannot have granted, so absent is already the state the caller asked
// for. Refusing would leave an operator with no way to clean up after a driver
// swap. It is restated here because the pair of methods reads as inconsistent
// otherwise, and the next reader's instinct is to "fix" it.
func (a *Applier) RemoveGrant(ctx context.Context, agentName, namespace, profileName string) error {
	if err := provision.Revoke(ctx, a.driver, refFor(agentName), profileName); err != nil {
		return fmt.Errorf("agentprivilege: remove %q from %q: %w", profileName, agentName, err)
	}
	return nil
}

// refFor builds the driver reference for an agent known only by name.
//
// ⚠ Ref.ID IS LEFT ZERO ON PURPOSE AND THAT IS WITHIN CONTRACT.
// provision.Ref.ID's own doc says nothing may depend on it being non-zero and
// nothing may look it up; it is a correlation id a driver may surface in a
// label. The consumer interface hands this package a NAME and no id, and
// inventing one would be worse than omitting it. agents.RefOf is deliberately
// not reachable from here — it takes a row, and this side of the seam has none.
func refFor(agentName string) provision.Ref {
	return provision.Ref{Name: agentName}
}

// policyFor turns a stored profile into the driver-interpreted policy.
//
// 🔴 IT DOES NOT CHECK THAT THE PROFILE CARRIES RULES, AND THE OMISSION IS THE
// POINT. internal/provision/k8s's Grant already refuses a rule-less policy with
// provision.ErrUnsupported and a displayable reason, and internal/provision/k8s's
// own policy.go records what happens when a refusal is open-coded at two layers:
// "two refusals for one rule, disagreeing about which layer owns it", which is
// exactly the defect the provision.Grant chokepoint exists to prevent. A caller
// that wants to skip an env-only profile has privilege.Spec.HasRBAC, and both
// call sites in internal/api use it.
func policyFor(p privilege.Profile) (provision.Policy, error) {
	raw, err := json.Marshal(k8sdriver.Rules{
		ClusterRules:   toRBACRules(p.Spec.ClusterRules),
		NamespaceRules: toRBACRules(p.Spec.NamespaceRules),
	})
	if err != nil {
		// Not reachable for these types — every field is a string slice — but a
		// silent empty payload here would be a policy the driver reads as "no
		// rules" and refuses, which sends the reader to the profile rather than to
		// the encoder.
		return provision.Policy{}, fmt.Errorf("encoding the profile's rules: %w", err)
	}
	return provision.Policy{Name: p.Name, Rules: raw}, nil
}

// toRBACRules translates the stored rule shape into the apiserver's.
//
// ⚠ nil IN, nil OUT, NOT AN EMPTY SLICE. k8s.Rules marshals both of its fields
// `omitempty`, and an empty non-nil slice is omitted just the same — but
// provision.Policy.HasRules reads the ENCODED payload, so a profile with neither
// kind of rule has to produce `{}` for the driver's refusal to be the one that
// fires. Returning `[]rbacv1.PolicyRule{}` here would still encode to `{}`
// today; it is written this way so it cannot stop doing so if either field loses
// its omitempty.
//
// 🔴 THE FIELDS ARE COPIED ONE FOR ONE AND NOT BY EMBEDDING. privilege.PolicyRule
// is the SUBSET of rbacv1.PolicyRule this domain stores — it has no
// NonResourceURLs — so a conversion that assumed the two types were
// interchangeable would compile only until one of them gained a field. Pinned by
// TestTheRuleTranslationIsFieldForField.
func toRBACRules(in []privilege.PolicyRule) []rbacv1.PolicyRule {
	if len(in) == 0 {
		return nil
	}
	out := make([]rbacv1.PolicyRule, 0, len(in))
	for _, r := range in {
		out = append(out, rbacv1.PolicyRule{
			APIGroups:     r.APIGroups,
			Resources:     r.Resources,
			Verbs:         r.Verbs,
			ResourceNames: r.ResourceNames,
		})
	}
	return out
}
