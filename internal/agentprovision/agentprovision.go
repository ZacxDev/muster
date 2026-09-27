// Package agentprovision adapts a [provision.Provisioner] DRIVER to the
// LIFECYCLE interface the agent HTTP handlers depend on (api.Provisioner):
// seven methods over an agent's int64 database id, rather than over a
// [provision.Ref] and a [provision.Spec].
//
// 🔴 IT IS THE LIFECYCLE HALF ONLY, AND THAT IS WHY IT CAN EXIST AT ALL. The
// consumer interface used to carry Chat and ChatWithTools as well, behind one
// nil and one wrapper, so nothing could wire provisioning without also claiming
// a model gateway this module has no implementation of. api.Provisioner and
// api.Gateway are now separate interfaces with separate nils and separate
// wrappers; this type satisfies the first and deliberately not the second, so a
// deployment gets working lifecycle routes and an honest 503 on the two chat
// ones. See internal/api/ext.go on api.Gateway.
//
// 🔴 WHAT THIS PACKAGE MUST NEVER DO, STATED BEFORE ANYTHING ELSE: it must
// never call agents.Store.SetKickedOff. That column answers "was the kickoff
// message HANDED TO A READY GATEWAY" — see agents.Agent.KickedOff and
// agents.Store.SetKickedOff — and this package has no gateway, so setting it
// would be a claim about a delivery that did not happen. That is the same class
// of lie the provisioner wrappers exist to prevent (a 200 over a pod that does
// not exist), one field down. It is not left to a reader's care:
// TestTheAdapterNeverClaimsAKickoffItCannotDeliver asserts it over the whole
// method set.
//
// WHAT HAPPENS TO A PENDING KICKOFF INSTEAD: the instance is created, the note
// stays in agents.pending_note where it was written, and the non-delivery is
// RECORDED on the row via agents.Store.SetKickoffError — the field whose own doc
// calls it "evidence, not a verdict".
//
// 🔴 AND NOTHING RENDERS THAT FIELD TO A HUMAN. THIS PARAGRAPH PREVIOUSLY CLAIMED
// IT DID, AND THE CLAIM WAS MEASURED FALSE — it is recorded here rather than
// quietly corrected, because it is the most load-bearing sentence in this doc.
// It read: "rendered into the agent's error line by agents.KickoffErrorSuffix. So
// the operator sees WHY the first turn never ran rather than a card that sits in
// `provisioning` with nothing written anywhere." Three things are wrong with that:
//
//   - agents.KickoffErrorSuffix has NO caller anywhere in this module. Its only
//     references are agents/reconcile_test.go and prose; the reconcile loop that
//     would call it is explicitly OWED in reconcile.go's own header.
//   - Agent.KickoffError reaches exactly ONE surface — GET /api/agents, which is
//     hook-token-gated MACHINE tier (internal/api/machine_agents.go). No HTML view
//     carries an agent error field at all.
//   - the card does NOT "sit in provisioning". agents.ComputeStatus refines a live
//     ready instance to `running`, so the card reads HEALTHY while the first turn
//     never happened. That is worse than the state the sentence claimed to fix.
//
// So: the field is where a MACHINE or an operator with a token can read it, and
// the boot banner names it — which is honest, because the banner names a field and
// not a screen. Nothing on any page says it. Do not restate the rendering claim
// without a caller to cite.
//
// ⚠ THAT IS A RECORD, NOT A FIX, AND THE GAP IT RECORDS HAS TWO OWNERS. The
// delivery itself needs a gateway (plan step 22c); the ESCALATION of an
// undelivered kickoff to a red card already exists as a pure decision table in
// agents.DecideReconcile (ActionRetryKickoff, then ActionError past
// ProvisioningStuckTimeout) and needs the reconcile loop that table's own header
// declares OWED. Neither is in this package's scope and neither is silently
// assumed: until they land, a dispatch with kickoff=true produces an instance that
// reads HEALTHY on every page and was never told what to do.
package agentprovision

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// DefaultOpTimeout bounds one lifecycle operation.
//
// 🔴 IT EXISTS BECAUSE FOUR OF THE SEVEN METHODS TAKE NO context.Context. The
// consumer interface's Dispatch/Start/Stop/Destroy are called from handler
// goroutines that hand over nothing, so without a budget here a wedged backend
// call holds a goroutine for the life of the process — and the handler has
// already returned, so nothing is left to cancel it. The driver operations under
// it APPLY objects rather than wait for readiness (see the kubernetes driver's
// Create), so this is generous rather than tight on purpose: it is a leak guard,
// not a latency policy.
const DefaultOpTimeout = 5 * time.Minute

// bookkeepingTimeout bounds the store write that RECORDS what an operation did.
// See bookkeepingCtx for why it is a separate budget rather than the operation's.
const bookkeepingTimeout = 15 * time.Second

// hooksTokenBytes is the entropy of a minted per-agent token, in bytes. 32 bytes
// renders as the 64 hex characters the deployment's other tokens use.
const hooksTokenBytes = 32

// UndeliveredKickoffReason is written to agents.kickoff_error when a dispatch or
// start asked for a kickoff this adapter cannot deliver.
//
// 🔴 IT NAMES THE BUILD, NOT THE AGENT, BECAUSE THE CONDITION IS A PROPERTY OF
// THE BUILD. An operator reading it on a card must not go looking at the agent's
// pod, its model or its credentials — none of them is why the turn did not run.
// Same reasoning as api.ProvisionerUnwiredField being a field rather than a
// sentence: the reader has to be able to tell a permanent property of the
// deployment from a transient failure of this agent.
const UndeliveredKickoffReason = "kickoff NOT delivered: this build wires a lifecycle-only agent " +
	"provisioner (internal/agentprovision) and no api.Gateway, so nothing can hand the pending " +
	"note to the instance's model gateway. The instance WAS created and the note is still in " +
	"agents.pending_note. This is a declared seam, not a failure of this agent: see " +
	"cmd/muster-server/doc_seams.go entry 1."

// Config is everything the adapter needs. Every field without a stated default
// is required, and New says which one is missing rather than producing an
// adapter that fails at the first dispatch.
type Config struct {
	// Driver is the provisioning backend. REQUIRED.
	Driver provision.Provisioner
	// Store resolves an agent id to its row and is where every status write
	// lands. REQUIRED.
	Store agents.Store
	// Spec is the deployment-wide half of every built spec — image repository,
	// the base URL instances phone home to, model, resources. REQUIRED, and
	// agentspec.Build refuses the two fields that have no defensible default.
	Spec agentspec.Config
	// Logger is optional.
	Logger *log.Logger
	// OpTimeout defaults to [DefaultOpTimeout].
	//
	// ⚠ NOTHING SETS IT TODAY — not buildProvisioner, not any test — and it was
	// kept over a round-0 deletion candidate for one reason: the alternative is a
	// package whose only timeout is a constant, which cannot be shortened by a
	// test that needs to observe the expiry. If no such test exists by the time the
	// reconcile loop lands, collapse this to the constant.
	OpTimeout time.Duration
}

// Adapter implements the consumer-side lifecycle interface over a driver.
//
// ⚠ IT DOES NOT NAME THAT INTERFACE IN A COMPILE-TIME ASSERTION, and the
// omission is deliberate: internal/api declares it consumer-side precisely so
// this direction of the dependency does not exist. The binding is checked where
// it is used — cmd/muster-server assigns an *Adapter to
// api.Extensions.Provisioner, which is a compile error if the method set drifts
// — and internal/api's own TestTheLifecycleAdapterSatisfiesTheConsumerInterface
// pins it from the side that owns the interface.
type Adapter struct {
	driver  provision.Provisioner
	store   agents.Store
	spec    agentspec.Config
	log     *log.Logger
	timeout time.Duration
}

// New validates the configuration and builds the adapter.
func New(cfg Config) (*Adapter, error) {
	if cfg.Driver == nil {
		return nil, errors.New("agentprovision: Config.Driver is required (an adapter over no " +
			"driver would answer every lifecycle call successfully and create nothing, which is " +
			"the exact observable the provisioner wrappers exist to prevent)")
	}
	if cfg.Store == nil {
		return nil, errors.New("agentprovision: Config.Store is required (every method resolves " +
			"an agent id through it, and every status this adapter reports is written to it)")
	}
	a := &Adapter{
		driver:  cfg.Driver,
		store:   cfg.Store,
		spec:    cfg.Spec,
		log:     cfg.Logger,
		timeout: cfg.OpTimeout,
	}
	if a.log == nil {
		a.log = log.New(io.Discard, "", 0)
	}
	if a.timeout <= 0 {
		a.timeout = DefaultOpTimeout
	}
	return a, nil
}

// Driver returns the backend's short name, for the boot banner and for error
// messages that have to say which set of capability losses applies.
func (a *Adapter) Driver() string { return a.driver.Driver() }

// ⚠ THERE IS DELIBERATELY NO Capabilities() PASSTHROUGH. One existed and was
// deleted: round 0 of this PR's audit measured ZERO callers anywhere in the module,
// and its own doc argued it was "so a caller that has only the adapter can still
// branch" — a caller that does not exist. Anything holding the driver can call
// driver.Capabilities() directly. Driver() stays because
// TestBuildingTheKubernetesProvisionerUsesTheRealDriver reads it to tell the real
// driver from a stand-in, and the boot banner prints it.

// ---------------------------------------------------------------------------
// LIFECYCLE
// ---------------------------------------------------------------------------

// Dispatch provisions an instance for the agent when kickoff is true, and
// creates NOTHING when it is false.
//
// 🔴 kickoff=false CREATES NO INSTANCE, AND AN EARLIER REVISION OF THIS METHOD
// CREATED ONE. That revision mirrored the upstream implementation, which installs
// its release either way and then stores `stopped` — and carrying that across
// turned a consumer-side comment into a falsehood and bypassed a guard. The caller
// is internal/api's createAndDispatchAgent, where `kickoff` is
// `action == "dispatch"`: the OTHER action is the UI's "Save for later" button,
// and the gate check four lines above it refuses a dispatch of a
// `gate:<reason>`-tagged task while allowing a save, giving as its reason that a
// save "provisions nothing". That was TRUE for as long as this module had no
// provisioner at all. Wiring one made it false in the worst direction: a save on a
// task explicitly gated from being worked created a pod, cloned the repository into
// it, and handed it a model credential — one per click, with the gate's own check
// passing.
//
// So the two arguments now differ in WHAT IS BUILT, not only in what is stored:
//
//	kickoff=true  -> create the instance, store `provisioning` (a first turn is
//	                 owed), and record the non-delivery. See the package doc.
//	kickoff=false -> create nothing, store `stopped`. The status is now TRUE
//	                 rather than a statement about what this service is waiting
//	                 for, which is also what makes the model-change roll in
//	                 internal/api correct for such an agent: it branches on the
//	                 STORED status, so a `stopped` row over a running instance
//	                 made it skip a roll the instance needed.
//
// ⚠ Start IS WHAT PROVISIONS A SAVED AGENT, and it already does: its
// provision.ErrNotFound branch creates the instance the save deliberately did not.
// So "save, then start later" works without this method creating anything.
func (a *Adapter) Dispatch(agentID int64, kickoff bool) error {
	ctx, cancel := a.opCtx()
	defer cancel()

	ag, err := a.store.Get(ctx, agentID)
	if err != nil {
		return fmt.Errorf("agentprovision: dispatch agent %d: load row: %w", agentID, err)
	}
	if !kickoff {
		// 🔴 NO DRIVER CALL AT ALL ON THIS PATH. It is not a route answering 200
		// and doing nothing — the caller asked for a saved agent and gets exactly
		// that: a row, a status that is true, and no cluster objects. The token is
		// deliberately NOT minted either: an agent that was never provisioned has
		// no instance holding a credential, and minting one now would be a secret
		// written for nothing.
		return a.setStatus(ag.ID, agents.StatusStopped)
	}
	if ag, err = a.create(ctx, ag); err != nil {
		return err
	}
	if err := a.setStatus(ag.ID, agents.StatusProvisioning); err != nil {
		return err
	}
	return a.recordUndeliveredKickoff(ag)
}

// Start scales a stopped instance back up, creating it first when the backend
// does not have it.
//
// 🔴 THE MISSING-INSTANCE BRANCH KEYS ON provision.ErrNotFound, NOT ON "Scale
// FAILED". Upstream fell back to a full re-provision on ANY scale error, which
// treats an unreachable backend (provision.ErrBlind — the driver knows nothing)
// as evidence that nothing is there, and answers it by creating. The sentinels
// exist to keep those apart: "the backend says there is nothing" is a reason to
// create, "I cannot see the backend" is a reason to stop and say so.
func (a *Adapter) Start(agentID int64) error {
	ctx, cancel := a.opCtx()
	defer cancel()

	ag, err := a.store.Get(ctx, agentID)
	if err != nil {
		return fmt.Errorf("agentprovision: start agent %d: load row: %w", agentID, err)
	}
	if err := a.driver.Scale(ctx, refOf(ag), 1); err != nil {
		if !errors.Is(err, provision.ErrNotFound) {
			return a.fail(ag, "scale up", err)
		}
		if ag, err = a.create(ctx, ag); err != nil {
			return err
		}
	}
	if err := a.setStatus(ag.ID, agents.StatusProvisioning); err != nil {
		return err
	}
	if !ag.KickedOff {
		// A first turn is still owed, whether this is a first start or a restart
		// of something that never got its note.
		return a.recordUndeliveredKickoff(ag)
	}
	// Already kicked off: nothing is owed, so the row stops saying it is waiting.
	// A live read refines this through agents.ComputeStatus.
	return a.setStatus(ag.ID, agents.StatusRunning)
}

// Stop scales the instance to zero replicas, keeping its declaration.
func (a *Adapter) Stop(agentID int64) error {
	ctx, cancel := a.opCtx()
	defer cancel()

	ag, err := a.store.Get(ctx, agentID)
	if err != nil {
		return fmt.Errorf("agentprovision: stop agent %d: load row: %w", agentID, err)
	}
	// 🔴 provision.ErrNotFound IS ALREADY-STOPPED, NOT A FAILURE, AND TREATING IT
	// AS ONE LEFT THE ROW PERMANENTLY WRONG. An instance that was evicted or
	// deleted out of band is gone, which is what a stop is FOR; refusing meant the
	// stored status stayed `running` with no way to correct it from the UI, and the
	// model-change roll kept trying to reconcile an instance that does not exist.
	// Destroy already draws this distinction — see its own doc.
	if err := a.driver.Scale(ctx, refOf(ag), 0); err != nil && !errors.Is(err, provision.ErrNotFound) {
		return fmt.Errorf("agentprovision: stop agent %d (%s): %w", ag.ID, ag.Name, err)
	}
	// 🔴 THE DELIVERY PROVENANCE IS DROPPED, AND agents.Store's own doc on this
	// method is the argument: a deliberate stop means the next start brings up an
	// instance with a new id, which is indistinguishable from an eviction — so
	// leaving the old recipient stamped would make agents.kickoffLost read this
	// agent as having lost its message and re-send the original task. Best-effort
	// on purpose: failing the stop over a provenance write would be a worse
	// answer than a stop that succeeded with stale provenance.
	if err := a.clearKickoffDelivery(ag); err != nil {
		a.log.Printf("agentprovision: stop %s: clear kickoff delivery: %v", ag.Name, err)
	}
	return a.setStatus(ag.ID, agents.StatusStopped)
}

// Destroy removes the instance and then the stored row.
//
// 🔴 THE ROW IS DELETED ONLY WHEN THE BACKEND HOLDS NOTHING, AND THIS IS WHERE
// THIS ADAPTER IS STRICTER THAN THE IMPLEMENTATION IT REPLACES. Upstream tore
// down best-effort and deleted the row unconditionally, which was safe there
// because its teardown could only fail transiently. The driver contract has two
// failures that are NOT transient-and-harmless, and both of them mean nothing
// was removed:
//
//   - provision.ErrNotManaged — something under this instance's name is not
//     muster's, so Destroy refused BEFORE deleting anything and the contract
//     calls the refusal terminal. Deleting the row here would leave whatever is
//     running with no record in this service at all: unlistable, unstoppable,
//     and invisible to the very refusal that was trying to protect it.
//   - provision.ErrBlind — the backend could not be reached, so the driver knows
//     nothing. Deleting the row on no information is the same loss with a
//     different cause.
//
// A nil error and provision.ErrNotFound both mean the backend holds nothing, and
// only those two reach the delete.
func (a *Adapter) Destroy(agentID int64) error {
	ctx, cancel := a.opCtx()
	defer cancel()

	ag, err := a.store.Get(ctx, agentID)
	if err != nil {
		return fmt.Errorf("agentprovision: destroy agent %d: load row: %w", agentID, err)
	}
	err = a.driver.Destroy(ctx, refOf(ag))
	if err != nil && !errors.Is(err, provision.ErrNotFound) {
		// 🔴 THE REASON GOES ON THE ROW, NOT ONLY INTO A RETURNED ERROR, AND AN
		// EARLIER REVISION RETURNED IT AND WROTE NOTHING. Nobody reads the return:
		// handleAgentDelete runs this in a background goroutine that logs, and it
		// has ALREADY answered an empty 200, so htmx removed the card before this
		// line ran. With no row write the card simply REAPPEARS on the next render
		// with no explanation anywhere a user looks — and the row we correctly kept
		// is the only evidence the instance is still out there. Recording `error`
		// plus the cause is what turns "the card came back" into a red card naming
		// an ownership refusal or an unreachable backend.
		return a.fail(ag, "destroy instance (the stored row was KEPT because the "+
			"instance was not removed)", err)
	}
	if err := a.store.Delete(ctx, ag.ID); err != nil {
		return fmt.Errorf("agentprovision: destroy agent %d (%s): instance removed but the row "+
			"was not deleted: %w", ag.ID, ag.Name, err)
	}
	return nil
}

// Instances returns every instance the driver manages, for status
// reconciliation.
//
// ⚠ IT PASSES THE DRIVER'S ERROR THROUGH RATHER THAN DEGRADING TO AN EMPTY
// SLICE. An empty slice is a positive claim that nothing is running, and
// api.Server.instanceIndex distinguishes a nil index ("live state was not
// observed", keep the stored status) from an empty one ("observed, nothing is
// running", every agent is stopped). Swallowing the error here would collapse
// those two and report a whole board stopped on one unreachable backend.
func (a *Adapter) Instances(ctx context.Context) ([]provision.Instance, error) {
	return a.driver.List(ctx)
}

// TailLogs returns the last `lines` lines of the agent's instance.
func (a *Adapter) TailLogs(ctx context.Context, ag agents.Agent, lines int64) (string, error) {
	return a.driver.TailLogs(ctx, refOf(ag), lines)
}

// StreamLogs follows the agent's instance output, calling emit per line.
func (a *Adapter) StreamLogs(ctx context.Context, ag agents.Agent, emit func(string)) error {
	return a.driver.StreamLogs(ctx, refOf(ag), emit)
}

// ReapplyProfiles re-renders the agent's spec from its current row and
// reconciles the live instance to it, which rolls the instance.
//
// 🔴 IT SATISFIES api.ProfileReapplier, WHICH IS A TYPE ASSERTION AND THEREFORE
// THE ONE FORM OF OPTIONAL DEPENDENCY THAT LEAVES NO TRACE WHEN IT IS NOT
// SATISFIED. cmd/muster-server/doc_seams.go entry 3 exists for exactly that: an
// adapter without this method compiles, wires, and takes the else branch for
// ever with no nil anywhere to notice. So it is implemented rather than omitted,
// and internal/api's TestTheLifecycleAdapterSatisfiesTheConsumerInterface
// asserts the assertion succeeds — "it compiles" is not that test, because a
// method added to the wrong type compiles just as well.
//
// ⚠ ROLLING THE INSTANCE RE-DELIVERS A KICKOFF ONCE A GATEWAY EXISTS, and
// agents.kickoffLost's own doc says so and calls it intended: a roll has already
// killed whatever turn was running, so the choice is between re-delivering the
// task and silently doing nothing. It is capped by agents.MaxKickoffAttempts.
// Today nothing here delivers a kickoff at all, so the consequence is latent.
func (a *Adapter) ReapplyProfiles(ctx context.Context, agentID int64) error {
	ag, err := a.store.Get(ctx, agentID)
	if err != nil {
		return fmt.Errorf("agentprovision: reapply agent %d: load row: %w", agentID, err)
	}
	ag, err = a.ensureHooksToken(ctx, ag)
	if err != nil {
		return err
	}
	spec, err := a.buildSpec(ag)
	if err != nil {
		return err
	}
	if err := a.driver.Update(ctx, spec); err != nil {
		return fmt.Errorf("agentprovision: reapply agent %d (%s): %w", ag.ID, ag.Name, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// INTERNALS
// ---------------------------------------------------------------------------

// create mints the agent's token if it has none, builds its spec, and brings the
// instance into existence. It returns the row as updated, so a caller that goes
// on to read ag.HooksToken sees the minted value rather than the empty one.
func (a *Adapter) create(ctx context.Context, ag agents.Agent) (agents.Agent, error) {
	ag, err := a.ensureHooksToken(ctx, ag)
	if err != nil {
		return ag, err
	}
	spec, err := a.buildSpec(ag)
	if err != nil {
		return ag, a.fail(ag, "build spec", err)
	}
	if err := a.driver.Create(ctx, spec); err != nil {
		// provision.ErrDivergentSpec is named in the message because it is the
		// one Create failure a reader can act on WITHOUT touching the cluster:
		// the instance exists and does not match, and the way to change it is
		// Update (ReapplyProfiles), not another Create.
		if errors.Is(err, provision.ErrDivergentSpec) {
			return ag, a.fail(ag, "create instance (it already exists with a different "+
				"spec; reconciling an existing instance is ReapplyProfiles, not Dispatch)", err)
		}
		return ag, a.fail(ag, "create instance", err)
	}
	return ag, nil
}

// ensureHooksToken gives the agent its own per-agent token if it has none.
//
// ⚠ IT IS A SECRET AND IS NEVER LOGGED, not even truncated. agentspec.Build puts
// it in Spec.Secrets rather than Spec.Env for the same reason.
//
// 🔴 OWED, NAMED RATHER THAN FIXED: THE MINT IS NOT ATOMIC. The guard reads a row
// fetched at the top of the calling method, and agents.Store.SetHooksToken is an
// unconditional `UPDATE agents SET hooks_token=$2`. Two operations on the SAME
// agent that both see an empty token both mint, and the second write overwrites the
// credential the first-created instance was handed — after which that instance's
// Spec.Secrets no longer matches, so the next Create returns
// provision.ErrDivergentSpec and the agent cannot authenticate to muster.
//
// ⚠ THE WINDOW IS NARROWER THAN IT LOOKS, WHICH IS WHY IT IS DEFERRED RATHER THAN
// DISMISSED. Two dispatch clicks do NOT collide: internal/api creates a NEW row per
// click, so there is no shared row. The reachable path is a model change or a
// privilege grant (both reach ReapplyProfiles) landing while the dispatch goroutine
// for the same agent is still in flight.
// TestDispatchCreatesTheInstanceAndMintsTheAgentsOwnToken covers the
// single-threaded case only, and says so.
//
// CLOSING CONDITION: agents.Store gains a compare-and-set mint — the SQL clause is
// `WHERE id=$1 AND coalesce(hooks_token,”) = ”` plus a reported row count — and
// this function re-reads the row when it LOSES, so the spec is built from the
// winner's token. It is a one-caller change (this is the only non-test caller of
// SetHooksToken, measured), which is why it is small and why it was not bundled
// into an audit-fix round already carrying a behaviour change.
// WHO CHECKS IT: the reviewer of that PR, against a test that runs two mints
// concurrently and asserts one row value and one winner.
func (a *Adapter) ensureHooksToken(ctx context.Context, ag agents.Agent) (agents.Agent, error) {
	if ag.HooksToken != "" {
		return ag, nil
	}
	buf := make([]byte, hooksTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return ag, a.fail(ag, "mint hooks token", err)
	}
	token := hex.EncodeToString(buf)
	if err := a.store.SetHooksToken(ctx, ag.ID, token); err != nil {
		return ag, a.fail(ag, "store hooks token", err)
	}
	ag.HooksToken = token
	return ag, nil
}

// buildSpec renders one agent row into a driver spec.
//
// 🔴 agentspec.WorkerInstructions IS PASSED, NOT OMITTED. That prose is the only
// thing that tells a dispatched instance the autosave rescue log exists (see
// TestTheInstructionsAndTheDaemonReferToEachOther), so an adapter that built a
// spec without it would ship the daemon and tell nobody to look at its log —
// which is what that test's "move both or neither" is about, one layer up.
func (a *Adapter) buildSpec(ag agents.Agent) (provision.Spec, error) {
	return agentspec.Build(ag, a.spec, agentspec.Options{
		Instructions: agentspec.WorkerInstructions,
	})
}

// clearKickoffDelivery drops the delivery provenance on its own budget. See
// bookkeepingCtx — this is a record of what the stop did, not part of doing it.
func (a *Adapter) clearKickoffDelivery(ag agents.Agent) error {
	ctx, cancel := a.bookkeepingCtx()
	defer cancel()
	return a.store.ClearKickoffDelivery(ctx, ag.ID)
}

// recordUndeliveredKickoff writes the non-delivery where a human reads it.
//
// 🔴 IT WRITES THE KICKOFF *ERROR* AND NEVER THE KICKOFF *FLAG*. The two fields
// are one letter apart in a call site and opposite in meaning: kickoff_error is
// evidence that a send did not happen, KickedOff is a claim that one did. See
// the package doc.
func (a *Adapter) recordUndeliveredKickoff(ag agents.Agent) error {
	ctx, cancel := a.bookkeepingCtx()
	defer cancel()
	a.log.Printf("agentprovision: agent %d (%s) was dispatched with a kickoff this build "+
		"cannot deliver — instance created, note left pending. %s", ag.ID, ag.Name,
		UndeliveredKickoffReason)
	if err := a.store.SetKickoffError(ctx, ag.ID, UndeliveredKickoffReason); err != nil {
		return fmt.Errorf("agentprovision: agent %d (%s): record undelivered kickoff: %w",
			ag.ID, ag.Name, err)
	}
	return nil
}

// setStatus persists a status with no output and no error message.
//
// 🔴 IT TAKES NO ctx AND DERIVES ITS OWN, WHICH IS THE WHOLE POINT — see
// bookkeepingCtx. It used to take the operation's context, and that is how the one
// failure most in need of a durable record got none.
func (a *Adapter) setStatus(id int64, status string) error {
	ctx, cancel := a.bookkeepingCtx()
	defer cancel()
	if err := a.store.UpdateStatus(ctx, id, status, "", ""); err != nil {
		return fmt.Errorf("agentprovision: agent %d: persist status %q: %w", id, status, err)
	}
	return nil
}

// fail records a terminal failure on the agent and returns the error.
//
// ⚠ THE STATUS WRITE'S OWN FAILURE IS LOGGED, NOT RETURNED. The error being
// reported is what the caller needs; replacing it with "could not write the
// status" would lose the cause and report the bookkeeping.
func (a *Adapter) fail(ag agents.Agent, what string, cause error) error {
	ctx, cancel := a.bookkeepingCtx()
	defer cancel()
	msg := what + ": " + cause.Error()
	if err := a.store.UpdateStatus(ctx, ag.ID, agents.StatusError, "", msg); err != nil {
		a.log.Printf("agentprovision: agent %d (%s): could not record failure %q: %v",
			ag.ID, ag.Name, msg, err)
	}
	return fmt.Errorf("agentprovision: agent %d (%s): %s: %w", ag.ID, ag.Name, what, cause)
}

// opCtx is the budget for a method that was handed no context.
func (a *Adapter) opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), a.timeout)
}

// bookkeepingCtx is a FRESH, SHORT budget for the store write that RECORDS what an
// operation did — never the operation's own context.
//
// 🔴 A RECORD WRITTEN ON THE EXPIRED BUDGET OF THE THING IT IS RECORDING CANNOT
// LAND IN THE CASE THAT NEEDS IT MOST. The failure is exact: opCtx's budget covers
// the driver call AND every store write after it, so when a driver call consumes the
// whole budget, `fail`'s UpdateStatus runs on a dead context, returns `context
// deadline exceeded`, and is only logged. Net result: the cluster holds partial
// objects, the row still says whatever the handler wrote, and the sole record of the
// timeout is a line in this pod's stdout — which is the observable this package's
// header says it exists to remove, reproduced one layer down.
//
// ⚠ IT IS Background-DERIVED, SO IT ALSO SURVIVES SHUTDOWN CANCELLATION, and that
// is the right direction for this one write: a SIGTERM mid-operation is exactly when
// the row most needs to stop claiming something that is no longer true. The
// operation itself is a different question and is NOT solved here — see opCtx.
//
// ⚠ THE HELPERS DERIVE IT THEMSELVES RATHER THAN TAKING IT, so no call site can
// pass the wrong one. That is the structural form of the fix; a rule asking every
// caller to remember which context to hand a status write is a rule that gets
// obeyed at four sites and missed at the fifth.
func (a *Adapter) bookkeepingCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), bookkeepingTimeout)
}

// refOf is the one place an agent row becomes a driver reference.
//
// 🔴 IT KEYS ON Name, NOT ON Namespace, AND THE DIFFERENCE IS A SILENT ONE.
// api.Server.instanceIndex indexes live instances by provision.Instance.Ref.Name
// — agents.InstanceIndex's own doc says Group is empty for a driver with no
// namespacing concept, so a group-keyed index collapses every instance onto "".
// A reference built from the namespace would not fail: it would resolve to
// nothing, and every agent would render stopped while running perfectly.
func refOf(ag agents.Agent) provision.Ref {
	return provision.Ref{Name: ag.Name, ID: ag.ID}
}
