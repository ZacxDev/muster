package api

import (
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// THE SEAM BETWEEN THE CONSUMER INTERFACE AND ITS FIRST REAL IMPLEMENTATION.
//
// 🔴 IT IS TESTED FROM *THIS* SIDE, WHICH IS THE SIDE THAT OWNS THE INTERFACE.
// internal/agentprovision deliberately does NOT name api.Provisioner in a
// compile-time assertion: this package declares the interface consumer-side
// precisely so the adapter's package does not import it, and an assertion over
// there would create the dependency the split exists to avoid. So the check lives
// here, where the import direction is already the right way round.
//
// 🔴 THIS IS AN "ISOLATION SEAM" CHECK AND THAT IS THE WHOLE REASON IT EXISTS.
// Both sides are separately tested to death — the adapter's package has a
// mutation-swept suite, this package's wrappers have a three-shape matrix — and
// neither suite ever builds the COMBINED state. The two things that can only be
// wrong together are the method set (a drift makes the wiring not compile, which
// only a test that performs the assignment can see) and the OPTIONAL type
// assertion, which leaves no trace at all when it fails.
// ---------------------------------------------------------------------------

// TestTheLifecycleAdapterSatisfiesTheConsumerInterface is the seam check.
//
// ⚠ IT ALSO ASSERTS WHAT THE ADAPTER MUST *NOT* SATISFY. api.Gateway is the chat
// half, and the whole argument for splitting the two interfaces was that a
// lifecycle adapter can implement the seven honestly and the two not at all. If
// this adapter ever satisfied Gateway, the chat routes would stop refusing at
// api.requireGatewayProvisioner and start answering 200 over a gateway that does
// not exist — which is the observable cmd/muster-server/doc_seams.go entry 1
// rejects, and nothing else would notice, because a satisfied interface is
// silent.
func TestTheLifecycleAdapterSatisfiesTheConsumerInterface(t *testing.T) {
	adapter, err := agentprovision.New(agentprovision.Config{
		Driver: provision.MustNewNoop(),
		Store:  stubAgentsStore{},
	})
	if err != nil {
		t.Fatalf("building the adapter: %v", err)
	}

	// The assignment IS the assertion for the lifecycle half: a method-set drift
	// on either side is a compile error here.
	var _ Provisioner = adapter

	// 🔴 THE OPTIONAL ONE IS A TYPE ASSERTION, AND doc_seams.go ENTRY 3 IS ABOUT
	// EXACTLY THIS. The grant path and the model-change path both type-assert the
	// Provisioner for ProfileReapplier; an adapter without the method compiles,
	// wires, and takes the else branch for ever with no nil anywhere for a reader
	// to notice. "It compiles" is not this test — a method added to the wrong type
	// compiles just as well — so the assertion is performed.
	if _, ok := any(adapter).(ProfileReapplier); !ok {
		t.Errorf("*agentprovision.Adapter does NOT satisfy api.ProfileReapplier.\n" +
			"    Two paths type-assert for it — reapplyEnvAsync (privilege.go) and the " +
			"model-change roll (agents.go) — and a failed assertion is SILENT: both " +
			"silently become no-ops, so a model change or a re-granted profile never " +
			"reaches the running instance and nothing anywhere says so.\n" +
			"    If dropping it is deliberate, invert this assertion and say why here; do " +
			"not delete it. See cmd/muster-server/doc_seams.go entry 3.")
	}

	if _, ok := any(adapter).(Gateway); ok {
		t.Error("*agentprovision.Adapter satisfies api.Gateway, which it must not.\n" +
			"    Gateway is the CHAT half. The adapter is built over " +
			"provision.Provisioner, a lifecycle contract with no notion of a chat turn, " +
			"so it cannot honestly answer one — and satisfying the interface is what " +
			"stops api.requireGatewayProvisioner refusing. The two chat routes would " +
			"answer 200 over a gateway that does not exist.\n" +
			"    If a real gateway is being added, wire it into Extensions.Gateway as its " +
			"own value; do not widen this type.")
	}
}

// TestAWiredProvisionerWithNoPrivilegeApplierIsNotReady is the readiness half of
// cmd/muster-server/doc_seams.go entry 2 falling due.
//
// 🔴 ENTRY 2 NAMED THE MOMENT ITS OWN ARGUMENT DIES, AND THIS IS THAT MOMENT. A
// privilege store that RECORDS grants nobody applies was defensible only while no
// agent pod could exist to hold one — "there is no surface on which a user is told
// a privilege is live when it is not, because there is no live agent to hold it."
// A wired Provisioner creates real instances, so the grant chip starts claiming a
// permission the instance's ServiceAccount does not have.
//
// 🔴 IT DRIVES /readyz RATHER THAN CALLING defects(), for the reason
// TestUncomposedNotesStoreIsNotReady states: a defect list nothing reads is not a
// guard. The failure it defends against is silent by construction — every page
// renders and nothing errors — so the readiness answer is the only observable that
// separates the two states.
//
// ⚠ THE THREE CONTROLS ARE THE POINT OF THE TABLE, not padding. Without them this
// would be "any missing dependency is a defect", which would make a
// provisioner-less deployment permanently not-ready and a provisioner-only one
// undeployable.
func TestAWiredProvisionerWithNoPrivilegeApplierIsNotReady(t *testing.T) {
	adapter, err := agentprovision.New(agentprovision.Config{
		Driver: provision.MustNewNoop(),
		Store:  stubAgentsStore{},
	})
	if err != nil {
		t.Fatalf("building the adapter: %v", err)
	}

	cases := []struct {
		name      string
		ext       Extensions
		wantReady bool
	}{
		{
			name:      "provisioner wired, privilege store wired, no applier",
			ext:       Extensions{Provisioner: adapter, Privilege: stubPrivilegeStore{}},
			wantReady: false,
		},
		{
			name: "provisioner wired, privilege store wired, applier wired",
			ext: Extensions{
				Provisioner:    adapter,
				Privilege:      stubPrivilegeStore{},
				PrivilegeApply: stubPrivilegeApplier{},
			},
			wantReady: true,
		},
		{
			// CONTROL: no privilege store means no grant to render, so nothing can
			// claim a permission. This is the combination a deployment that keeps
			// privilege in another service is in, and it must stay deployable.
			name:      "provisioner wired, no privilege store",
			ext:       Extensions{Provisioner: adapter},
			wantReady: true,
		},
		{
			// CONTROL: the state every previous revision of the server binary was
			// in. A grant recorded with no pod that could hold it lies about
			// nothing, which is entry 2's whole original argument.
			name:      "privilege store wired, no provisioner",
			ext:       Extensions{Privilege: stubPrivilegeStore{}},
			wantReady: true,
		},
		{
			// CONTROL: neither. Nothing about agents is configured at all.
			name:      "neither wired",
			ext:       Extensions{},
			wantReady: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(nil, AuthConfig{}, log.New(os.Stderr, "", 0))
			s.UseExtensions(tc.ext)
			rec := httptest.NewRecorder()
			s.handleReady(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			gotReady := rec.Code == http.StatusOK
			if gotReady != tc.wantReady {
				t.Fatalf("/readyz answered %d (ready=%v), want ready=%v.\nbody: %s",
					rec.Code, gotReady, tc.wantReady, rec.Body.String())
			}
			if !tc.wantReady {
				// The refusal must NAME the fields, or an operator reading it cannot
				// act on it — the same requirement the notes/liveness defect carries.
				for _, want := range []string{"PrivilegeApply", "Provisioner"} {
					if !strings.Contains(rec.Body.String(), want) {
						t.Errorf("the refusal does not name %q.\nbody: %s", want, rec.Body.String())
					}
				}
			}
		})
	}
}
