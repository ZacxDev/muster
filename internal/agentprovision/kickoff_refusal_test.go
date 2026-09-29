package agentprovision

import (
	"errors"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// ---------------------------------------------------------------------------
// THE REFUSAL, AND THE THREE THINGS IT MUST NOT BREAK.
//
// 🔴 EVERY ASSERTION IN THIS FILE IS BEHAVIOURAL — the transcript of store and
// driver calls, the returned error, the driver's own create counter. None of them
// reads a field of Adapter or compares against KickoffRefusalReason itself, because
// an expectation derived from the implementation under test cannot see the
// implementation being wrong. The strings below are LITERAL for the same reason: a
// test asserting `strings.Contains(err, KickoffRefusalReason)` passes over any
// rewording, including one that dropped the variable name an operator needs.
//
// 🔴 AND THE POSITIVE CONTROL IS A TEST OF ITS OWN, NOT A COMMENT. A refusal that
// fired unconditionally would satisfy the refusal test, the save test and the start
// test — save and start take different branches — so
// TestAKickoffIsNOTRefusedWhenAGatewayIsConfigured is the only thing that separates
// "refuses when it cannot deliver" from "refuses always".
// ---------------------------------------------------------------------------

// literals the refusal must carry, spelled out rather than referenced.
const (
	// wantRefusalVariable is the ONE actionable token: without it the operator knows
	// something was refused and not what to set. cmd/muster-server's
	// TestTheKickoffRefusalNamesTheBinarysOwnGatewayVariable pins it against that
	// binary's own constant, so the two spellings cannot drift apart.
	wantRefusalVariable = "MUSTER_AGENT_GATEWAY"
	// wantRefusalRemedyValue is the value to set it to. A cause with no remedy reads
	// as "this is broken" rather than "this is off".
	wantRefusalRemedyValue = "hooks-sha256"
	// wantRefusalSaveEscape is the operation that still works, named in the refusal so
	// the operator is not left with no next move at all.
	wantRefusalSaveEscape = "Save for later"
)

// TestACreateWithAKickoffIsREFUSEDWhenNothingCanDeliverIt is the regression test for
// the behaviour this change exists to fix.
//
// 🔴 WHAT IT WAS, MEASURED ON THE DEPLOYED BUILD: a dispatch asking for a kickoff on
// a deployment with no gateway CREATED the instance — a pod, a namespace, a
// ServiceAccount, a Secret holding a freshly-minted token, the repository cloned in,
// a model credential handed over — and then recorded that the one thing the caller
// asked for had not happened, in a field no page renders. agents.ComputeStatus then
// refined the live ready instance to `running`, so the card read HEALTHY over an
// agent that was never told what to do. Every click cost another such pod.
//
// 🔴 THE ASSERTIONS ARE ORDERED BY WHAT THEY PROTECT, AND THE FIRST TWO ARE THE
// POINT: nothing was created, and no secret was minted. A refusal that still built
// the instance and merely returned an error would pass a test phrased only over the
// error — and would be the same defect with a louder log.
func TestACreateWithAKickoffIsREFUSEDWhenNothingCanDeliverIt(t *testing.T) {
	a, store, driver := newAdapterWithNoKickoffDelivery(t, nil)

	err := a.Dispatch(fixtureAgentID, true)
	if err == nil {
		t.Fatalf("Dispatch(kickoff=true) with no gateway returned nil, so the caller was told "+
			"the work had begun.\n  transcript: %s", store.transcript())
	}

	// INSTRUMENT CHECK: the adapter really ran and really reached the fake. Without
	// this, every "was not called" assertion below would pass over an adapter that
	// returned at its first line for an unrelated reason.
	if !store.called("Get") {
		t.Fatalf("instrument check FAILED: the adapter never loaded the row, so the "+
			"assertions below are over nothing.\n  transcript: %s", store.transcript())
	}

	// 1. NOTHING WAS BUILT.
	if driver.creates != 0 {
		t.Errorf("the driver was asked to create %d instance(s) for a kickoff that cannot be "+
			"delivered, want 0.\n  transcript: %s\n"+
			"    Creating it is the expensive half of a request whose only point — telling the "+
			"agent what to do — is impossible here.", driver.creates, store.transcript())
	}

	// 2. NO SECRET WAS MINTED. A token written for an instance that does not exist is
	//    a credential with no holder and no expiry.
	if store.called("SetHooksToken") {
		t.Errorf("a hooks token was minted for an agent that was never provisioned.\n"+
			"  transcript: %s", store.transcript())
	}

	// 3. THE ROW SAYS IT FAILED, TERMINALLY. `error` is the one status
	//    agents.ComputeStatus returns early on, so the card cannot be refined back to
	//    `running` by a live instance — which is exactly what used to happen.
	if !strings.Contains(store.transcript(), "UpdateStatus(4291,error,") {
		t.Errorf("the refusal was not recorded as %s on the row, so the operator's only "+
			"evidence is a log line.\n  transcript: %s", agents.StatusError, store.transcript())
	}
	if store.called("UpdateStatus(4291,provisioning") {
		t.Errorf("the row was left saying `provisioning` over an instance that does not exist "+
			"and never will.\n  transcript: %s", store.transcript())
	}

	// 4. THE RECORDED MESSAGE AND THE RETURNED ERROR BOTH CARRY THE CAUSE AND THE
	//    REMEDY. Two surfaces, because they are read by different people: the row by
	//    whoever looks at the agent, the error by whoever reads this pod's log.
	for _, want := range []string{wantRefusalVariable, wantRefusalRemedyValue, wantRefusalSaveEscape} {
		if !strings.Contains(store.transcript(), want) {
			t.Errorf("the message recorded on the row does not contain %q.\n  transcript: %s",
				want, store.transcript())
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the returned error does not contain %q.\n  error: %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), fixtureAgentName) {
		t.Errorf("the returned error does not name the agent it refused: %v", err)
	}

	// 5. IT IS MATCHABLE. The caller that will answer the operator's POST has to tell
	//    a refusal from a driver failure without parsing prose.
	if !errors.Is(err, ErrKickoffUndeliverable) {
		t.Errorf("the refusal does not wrap ErrKickoffUndeliverable, so no caller can "+
			"distinguish it from a backend failure: %v", err)
	}

	// 6. IT IS NOT RECORDED AS A DELIVERY FAILURE. kickoff_error means "the instance
	//    exists and its first message was not delivered" — evidence
	//    agents.DecideReconcile reads as a reason to RETRY. There is no instance here,
	//    so writing it would point a retry at something that was never created.
	if store.called("SetKickoffError") {
		t.Errorf("the refusal was written to kickoff_error, which claims an undelivered "+
			"message against an instance that does not exist.\n  transcript: %s",
			store.transcript())
	}
	// And it must never claim the opposite.
	if store.called("SetKickedOff") {
		t.Errorf("the adapter claimed a kickoff it refused to attempt.\n  transcript: %s",
			store.transcript())
	}
}

// TestAKickoffIsNOTRefusedWhenAGatewayIsConfigured IS THE POSITIVE CONTROL, and
// without it every other test in this file is satisfied by a refusal that fires
// unconditionally.
//
// 🔴 THE REFUSAL MUST BE CONDITIONAL ON THE RESOLVED CONFIGURATION, NOT ON THE
// BUILD. internal/agentgateway is shipped and live-verified; the only reason a
// kickoff cannot be delivered on the deployment that motivated this change is that
// MUSTER_AGENT_GATEWAY is unset. A refusal keyed on anything else — a build tag, the
// package's own identity, a guess about what this binary contains — would break
// every deployment that HAS configured a gateway, and would do so in the direction
// that removes a working feature.
//
// ⚠ IT ASSERTS THE WHOLE PRE-EXISTING SEQUENCE, not merely a nil error. "Dispatch
// did not fail" is also true of a Dispatch that silently did nothing, which is the
// shape this package's own header calls the lie it exists to prevent.
func TestAKickoffIsNOTRefusedWhenAGatewayIsConfigured(t *testing.T) {
	a, store, driver := newAdapter(t, nil)

	if err := a.Dispatch(fixtureAgentID, true); err != nil {
		t.Fatalf("Dispatch(kickoff=true) was refused on a deployment that HAS a gateway, "+
			"which removes a working feature: %v\n  transcript: %s", err, store.transcript())
	}

	if driver.creates != 1 {
		t.Errorf("the driver created %d instance(s) with a gateway configured, want 1.\n"+
			"  transcript: %s", driver.creates, store.transcript())
	}
	if !store.called("SetHooksToken") {
		t.Errorf("no token was minted for an instance that was created.\n  transcript: %s",
			store.transcript())
	}
	if !strings.Contains(store.transcript(), "UpdateStatus(4291,provisioning,") {
		t.Errorf("the row does not say `provisioning` after a dispatch that created an "+
			"instance and owes a first turn.\n  transcript: %s", store.transcript())
	}
	if !store.called("SetKickoffError") {
		t.Errorf("the owed-but-undelivered first turn was not recorded in kickoff_error.\n"+
			"  transcript: %s\n"+
			"    A configured gateway makes the dispatch legal; it does not by itself make "+
			"the note delivered — nothing calls the gateway on this path yet (see "+
			"cmd/muster-server/doc_seams.go entry 1), so the existing record must remain.",
			store.transcript())
	}

	// 🔴 AND THE REFUSAL'S OWN TEXT MUST BE ABSENT. This is the assertion that fails
	// on a refusal that fires in both configurations while still creating the
	// instance — a mutant a nil-error check alone cannot see.
	if strings.Contains(store.transcript(), "REFUSED and NOTHING was provisioned") {
		t.Errorf("the undeliverable-kickoff refusal fired on a deployment WITH a gateway.\n"+
			"  transcript: %s", store.transcript())
	}
	if strings.Contains(store.transcript(), "UpdateStatus(4291,error,") {
		t.Errorf("the row was marked %s on a dispatch that succeeded.\n  transcript: %s",
			agents.StatusError, store.transcript())
	}
}

// TestASaveIsNeverRefusedWhenNoKickoffCanBeDelivered guards the operation the
// refusal must not touch.
//
// 🔴 A SAVE ASKS FOR NOTHING THIS CONFIGURATION CANNOT DO. "Save for later" —
// action=save in the UI, kickoff=false here — deliberately provisions nothing and
// stores `stopped`; there is no kickoff to deliver, so there is nothing to refuse.
// Refusing it would remove a working operation to punish a missing gateway, and it
// is the operation the refusal's own remedy text points the operator at, so the
// remedy would become a lie.
//
// ⚠ IT ALSO PINS THAT THE SAVE PATH STILL CREATES NOTHING, which is a property the
// refusal branch sits directly next to in the source and could plausibly be moved
// above by a later edit.
func TestASaveIsNeverRefusedWhenNoKickoffCanBeDelivered(t *testing.T) {
	a, store, driver := newAdapterWithNoKickoffDelivery(t, nil)

	if err := a.Dispatch(fixtureAgentID, false); err != nil {
		t.Fatalf("Dispatch(kickoff=false) — \"Save for later\" — was refused on a deployment "+
			"with no gateway: %v\n  transcript: %s", err, store.transcript())
	}

	if !strings.Contains(store.transcript(), "UpdateStatus(4291,stopped,") {
		t.Errorf("a save did not store %s.\n  transcript: %s",
			agents.StatusStopped, store.transcript())
	}
	if driver.creates != 0 {
		t.Errorf("a save created %d instance(s), want 0.\n  transcript: %s",
			driver.creates, store.transcript())
	}
	if strings.Contains(store.transcript(), "UpdateStatus(4291,error,") {
		t.Errorf("a save was recorded as a failure.\n  transcript: %s", store.transcript())
	}
	if strings.Contains(store.transcript(), wantRefusalVariable) {
		t.Errorf("the undeliverable-kickoff refusal text was written for a save, which asked "+
			"for no kickoff.\n  transcript: %s", store.transcript())
	}
}

// TestStartStillWorksWhenNoKickoffCanBeDelivered guards the capability a wider
// refusal would have deleted.
//
// 🔴 THE REFUSAL IS ON THE CREATE-WITH-KICKOFF PATH ONLY, AND THIS IS WHERE THAT
// DECISION IS ASSERTED RATHER THAN ARGUED. Dispatch(kickoff=true) is the only call
// that promises "begin this work now"; Start promises "bring this instance up",
// which this build genuinely can do — the pod runs and its logs stream. Refusing
// Start would remove three working operations at once: restarting a stopped agent,
// recovering one whose instance was evicted, and provisioning an agent that was
// deliberately SAVED — the escape route the refusal's own text recommends.
//
// ⚠ AND NOTHING IS HIDDEN BY ALLOWING IT: the owed first turn is still recorded in
// kickoff_error, where it is honest, because an instance now exists for a retry to
// act on. Each case asserts that record, so a Start that quietly stopped reporting
// the non-delivery fails here.
func TestStartStillWorksWhenNoKickoffCanBeDelivered(t *testing.T) {
	t.Run("restart of a live instance", func(t *testing.T) {
		a, store, driver := newAdapterWithNoKickoffDelivery(t, nil)
		seedInstance(t, a, store, driver)

		if err := a.Start(fixtureAgentID); err != nil {
			t.Fatalf("Start was refused on a deployment with no gateway, so a stopped agent "+
				"cannot be restarted: %v\n  transcript: %s", err, store.transcript())
		}
		if len(driver.scaled) == 0 || driver.scaled[len(driver.scaled)-1] != 1 {
			t.Errorf("Start did not scale the instance up: scaled=%v\n  transcript: %s",
				driver.scaled, store.transcript())
		}
		if driver.creates != 0 {
			t.Errorf("Start re-created an instance the backend already has (%d create(s)).\n"+
				"  transcript: %s", driver.creates, store.transcript())
		}
		if !store.called("SetKickoffError") {
			t.Errorf("Start stopped recording the owed first turn in kickoff_error.\n"+
				"  transcript: %s", store.transcript())
		}
		if strings.Contains(store.transcript(), "UpdateStatus(4291,error,") {
			t.Errorf("Start marked the row %s on a deployment with no gateway. That is the "+
				"refusal leaking onto a path that works.\n  transcript: %s",
				agents.StatusError, store.transcript())
		}
	})

	// 🔴 THE SAVED-AGENT CASE IS THE ONE THE REMEDY TEXT DEPENDS ON. The refusal tells
	// the operator to save the agent and start it later; if Start refused to create
	// the instance a save deliberately did not, that advice would dead-end.
	t.Run("first start of a saved agent creates its instance", func(t *testing.T) {
		a, store, driver := newAdapterWithNoKickoffDelivery(t, nil)

		if err := a.Start(fixtureAgentID); err != nil {
			t.Fatalf("Start of a never-provisioned (saved) agent was refused, which dead-ends "+
				"the remedy the refusal recommends: %v\n  transcript: %s", err, store.transcript())
		}
		if driver.creates != 1 {
			t.Errorf("Start of a saved agent created %d instance(s), want 1.\n  transcript: %s",
				driver.creates, store.transcript())
		}
		if !store.called("SetHooksToken") {
			t.Errorf("Start created the instance without minting its token.\n  transcript: %s",
				store.transcript())
		}
		if !store.called("SetKickoffError") {
			t.Errorf("Start created an instance owing a first turn and recorded nothing.\n"+
				"  transcript: %s", store.transcript())
		}
	})

	// An agent that HAS been kicked off owes nothing, so its restart must land on
	// `running` and record no non-delivery — the branch the refusal is furthest from,
	// included so "Start is untouched" is a claim about both of its arms.
	t.Run("restart of an already-kicked-off agent owes nothing", func(t *testing.T) {
		a, store, driver := newAdapterWithNoKickoffDelivery(t, func(s *recordingStore, _ *flakyDriver) {
			s.agent.KickedOff = true
		})
		seedInstance(t, a, store, driver)

		if err := a.Start(fixtureAgentID); err != nil {
			t.Fatalf("Start of an already-kicked-off agent was refused: %v\n  transcript: %s",
				err, store.transcript())
		}
		if !strings.Contains(store.transcript(), "UpdateStatus(4291,running,") {
			t.Errorf("a restart of an agent that owes no first turn did not land on %s.\n"+
				"  transcript: %s", agents.StatusRunning, store.transcript())
		}
		if store.called("SetKickoffError") {
			t.Errorf("a non-delivery was recorded for an agent that was already kicked off.\n"+
				"  transcript: %s", store.transcript())
		}
	})
}

// TestARefusedKickoffReportsAFailedRecordRatherThanSwallowingIt pins the one place
// refuseKickoff deliberately differs from fail().
//
// 🔴 A REFUSAL NOBODY COULD RECORD IS A DIFFERENT OUTCOME FROM A REFUSAL. fail()
// logs its own bookkeeping failure and returns the underlying cause, because there
// the cause is what the caller needs. Here the refusal IS the outcome, so if the row
// could not be marked the caller's log line must say THAT — otherwise the operator
// is left with a row still reading `provisioning` and a log line claiming the
// refusal was recorded.
func TestARefusedKickoffReportsAFailedRecordRatherThanSwallowingIt(t *testing.T) {
	wantErr := errors.New("database is in recovery")
	a, store, driver := newAdapterWithNoKickoffDelivery(t, func(s *recordingStore, _ *flakyDriver) {
		s.updateErr = wantErr
	})

	err := a.Dispatch(fixtureAgentID, true)
	if err == nil {
		t.Fatalf("Dispatch returned nil when it could neither deliver the kickoff nor record "+
			"the refusal.\n  transcript: %s", store.transcript())
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("the error does not carry the store failure that prevented the refusal being "+
			"recorded: %v", err)
	}
	// The refusal still created nothing — the store failure must not fall through to
	// the create path.
	if driver.creates != 0 {
		t.Errorf("an instance was created after the refusal failed to record (%d create(s)).\n"+
			"  transcript: %s", driver.creates, store.transcript())
	}
}
