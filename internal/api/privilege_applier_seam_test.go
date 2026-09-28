package api

import (
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentprivilege"
	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// THE SEAM BETWEEN PrivilegeApplier AND ITS FIRST REAL IMPLEMENTATION.
//
// 🔴 IT IS THE THIRD FILE OF THIS SHAPE AND THE REASON IS THE SAME EACH TIME:
// internal/agentprivilege does not name api.PrivilegeApplier in a compile-time
// assertion, because this package declares the interface consumer-side precisely
// so that dependency does not exist. The check therefore lives here.
//
// 🔴 AND IT IS AN ISOLATION-SEAM CHECK, WHICH IS THE ONE THING NEITHER SIDE'S OWN
// SUITE CAN BE. internal/agentprivilege's suite is mutation-swept against a real
// kubernetes driver and a fake clientset; this package's readiness table is
// exhaustive over the predicate's arms. Both were green for a combination that
// never existed — the table used a STUB applier, which satisfies the interface by
// construction and therefore cannot tell whether the real type does. The method
// set is the part that can only be wrong together: a drift makes
// cmd/muster-server not compile, and nothing but an assignment can see that.
// ---------------------------------------------------------------------------

// TestThePrivilegeApplierSatisfiesTheConsumerInterface is the seam check.
//
// ⚠ IT ALSO ASSERTS WHAT THE APPLIER MUST *NOT* SATISFY, for the reason the
// lifecycle adapter's equivalent does: the three tiers are three interfaces
// behind three nils so each can refuse honestly. An applier that also satisfied
// Provisioner would let a deployment arm privilege-apply and silently acquire a
// lifecycle tier it never configured — every agent route would stop refusing at
// api.requireLifecycleProvisioner over an object with no store and no spec.
func TestThePrivilegeApplierSatisfiesTheConsumerInterface(t *testing.T) {
	applier, err := agentprivilege.New(agentprivilege.Config{Driver: provision.MustNewNoop()})
	if err != nil {
		t.Fatalf("building the applier: %v", err)
	}

	// The assignment IS the assertion: a method-set drift is a compile error here,
	// which is the same failure cmd/muster-server would get and the only one that
	// proves the wiring is possible.
	var _ PrivilegeApplier = applier

	// ⚠ AND IT IS ALSO CHECKED AT RUNTIME OVER THE INTERFACE VALUE, because the
	// line above is erased by the compiler and a reader cannot tell from the test's
	// OUTPUT that it ran. A dynamic assertion produces a verdict.
	var ext Extensions
	ext.PrivilegeApply = applier
	if _, ok := ext.PrivilegeApply.(PrivilegeApplier); !ok {
		t.Fatal("the applier in Extensions.PrivilegeApply does not satisfy PrivilegeApplier")
	}

	if _, ok := ext.PrivilegeApply.(Provisioner); ok {
		t.Error("*agentprivilege.Applier satisfies api.Provisioner. The three tiers are three " +
			"interfaces behind three nils so each can refuse honestly; if this one covers " +
			"lifecycle too, arming privilege-apply silently stops every agent route refusing " +
			"at api.requireLifecycleProvisioner, over an object with no agents store.")
	}
	if _, ok := ext.PrivilegeApply.(Gateway); ok {
		t.Error("*agentprivilege.Applier satisfies api.Gateway; the two chat routes would stop " +
			"refusing over an object that cannot resolve an address or run a turn")
	}
	if _, ok := ext.PrivilegeApply.(ProfileReapplier); ok {
		t.Error("*agentprivilege.Applier satisfies api.ProfileReapplier. That interface is " +
			"type-ASSERTED on the Provisioner, not on this field, so satisfying it here buys " +
			"nothing and hides which object the grant path would roll.")
	}
}

// TestAWiredProvisionerWithTheREALPrivilegeApplierIsReady is the behavioural half
// of the seam, and it is the case
// TestAWiredProvisionerWithNoPrivilegeApplierIsNotReady structurally cannot make.
//
// 🔴 THAT TABLE'S "applier wired" ARM USES stubPrivilegeApplier, WHICH SATISFIES
// THE INTERFACE BY DEFINITION. So it proves the PREDICATE has the arm, and
// nothing at all about whether a real applier can occupy it. Before
// internal/agentprivilege existed there was no other option; there is now, and
// the distinction matters because the readiness answer is what decides whether a
// pod serves 99 routes or none.
//
// ⚠ IT IS A SEPARATE TEST RATHER THAN A SEVENTH ROW IN THAT TABLE, on purpose:
// the table is about the three-conjunct predicate's arms and its three controls,
// and a row that also reached across a package boundary would make a failure
// there ambiguous between "the predicate changed" and "the adapter's method set
// drifted".
func TestAWiredProvisionerWithTheREALPrivilegeApplierIsReady(t *testing.T) {
	adapter, err := agentprovision.New(agentprovision.Config{
		Driver: provision.MustNewNoop(),
		Store:  stubAgentsStore{},
	})
	if err != nil {
		t.Fatalf("building the lifecycle adapter: %v", err)
	}
	applier, err := agentprivilege.New(agentprivilege.Config{Driver: provision.MustNewNoop()})
	if err != nil {
		t.Fatalf("building the applier: %v", err)
	}

	ready := func(ext Extensions) (int, string) {
		s := New(nil, AuthConfig{}, log.New(os.Stderr, "", 0))
		s.UseExtensions(ext)
		rec := httptest.NewRecorder()
		s.handleReady(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec.Code, rec.Body.String()
	}

	// 🔴 NEGATIVE CONTROL FIRST, AND ITS VERDICT IS REPORTED. Without it a /readyz
	// that answered 200 for everything — a broken handler, a defects() that lost
	// its second entry — would make the assertion below pass while measuring
	// nothing. This is the same combination minus the applier, so exactly one thing
	// differs between the two reads.
	code, body := ready(Extensions{Provisioner: adapter, Privilege: stubPrivilegeStore{}})
	if code == http.StatusOK {
		t.Fatalf("negative control FAILED: /readyz answered 200 for a wired provisioner and a "+
			"wired privilege store with NO applier, which api.Extensions.defects refuses. The "+
			"assertion below cannot distinguish a working applier from a readiness check that "+
			"says yes to everything.\nbody: %s", body)
	}
	if !strings.Contains(body, "MUSTER_AGENT_PRIVILEGE_APPLY") {
		t.Errorf("the refusal does not name the variable that arms the applier, so an operator "+
			"reading it has no action to take and the cheapest way out is to delete the "+
			"readiness probe.\nbody: %s", body)
	}
	t.Logf("negative control: /readyz answered %d without an applier", code)

	code, body = ready(Extensions{
		Provisioner:    adapter,
		Privilege:      stubPrivilegeStore{},
		PrivilegeApply: applier,
	})
	if code != http.StatusOK {
		t.Fatalf("/readyz answered %d with a REAL *agentprivilege.Applier wired. The readiness "+
			"refusal that made this combination undeployable is supposed to be satisfiable by "+
			"the implementation this module now ships.\nbody: %s", code, body)
	}
	// ⚠ ASSERT THE DEFECT'S ABSENCE BY ITS OWN IDENTIFYING STRING, NOT ONLY BY THE
	// STATUS CODE. A 200 from handleReady is also what a lost second defects()
	// entry produces, and the two must not be the same observation.
	if strings.Contains(body, "PrivilegeApply") {
		t.Errorf("/readyz answered 200 and still mentions PrivilegeApply.\nbody: %s", body)
	}
}
