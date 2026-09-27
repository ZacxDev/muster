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
// calls it "evidence, not a verdict", rendered into the agent's error line by
// agents.KickoffErrorSuffix. So the operator sees WHY the first turn never ran
// rather than a card that sits in `provisioning` with nothing written anywhere.
//
// ⚠ THAT IS A RECORD, NOT A FIX, AND THE GAP IT RECORDS HAS TWO OWNERS. The
// delivery itself needs a gateway (plan step 22c); the ESCALATION of an
// undelivered kickoff to a red card already exists as a pure decision table in
// agents.DecideReconcile (ActionRetryKickoff, then ActionError past
// ProvisioningStuckTimeout) and needs the reconcile loop that table's own header
// declares OWED. Neither is in this package's scope and neither is silently
// assumed: until they land, a dispatch with kickoff=true produces a running
// instance that was never told what to do, and the kickoff-error field is the
// only place that says so.
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

// Capabilities returns the backend's declared capabilities, so a caller that has
// only the adapter can still branch on what the driver can do.
func (a *Adapter) Capabilities() provision.Capabilities { return a.driver.Capabilities() }

// ---------------------------------------------------------------------------
// LIFECYCLE
// ---------------------------------------------------------------------------

// Dispatch provisions an instance for the agent.
//
// ⚠ THE kickoff ARGUMENT DECIDES THE STORED STATUS AND NOTHING ELSE HERE, which
// is the one place this adapter's behaviour and the upstream implementation's
// deliberately part company — upstream launched a delivery goroutine. See the
// package doc for what replaces it.
//
//	kickoff=true  -> `provisioning` (a first turn is owed) + the non-delivery
//	                 recorded on the row.
//	kickoff=false -> `stopped` (nothing is owed), matching upstream: the stored
//	                 status is a statement about what this service is WAITING
//	                 FOR, and a live instance refines it through
//	                 agents.ComputeStatus on the next read either way.
func (a *Adapter) Dispatch(agentID int64, kickoff bool) error {
	ctx, cancel := a.opCtx()
	defer cancel()

	ag, err := a.store.Get(ctx, agentID)
	if err != nil {
		return fmt.Errorf("agentprovision: dispatch agent %d: load row: %w", agentID, err)
	}
	if ag, err = a.create(ctx, ag); err != nil {
		return err
	}
	if !kickoff {
		return a.setStatus(ctx, ag.ID, agents.StatusStopped)
	}
	if err := a.setStatus(ctx, ag.ID, agents.StatusProvisioning); err != nil {
		return err
	}
	return a.recordUndeliveredKickoff(ctx, ag)
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
			return a.fail(ctx, ag, "scale up", err)
		}
		if ag, err = a.create(ctx, ag); err != nil {
			return err
		}
	}
	if err := a.setStatus(ctx, ag.ID, agents.StatusProvisioning); err != nil {
		return err
	}
	if !ag.KickedOff {
		// A first turn is still owed, whether this is a first start or a restart
		// of something that never got its note.
		return a.recordUndeliveredKickoff(ctx, ag)
	}
	// Already kicked off: nothing is owed, so the row stops saying it is waiting.
	// A live read refines this through agents.ComputeStatus.
	return a.setStatus(ctx, ag.ID, agents.StatusRunning)
}

// Stop scales the instance to zero replicas, keeping its declaration.
func (a *Adapter) Stop(agentID int64) error {
	ctx, cancel := a.opCtx()
	defer cancel()

	ag, err := a.store.Get(ctx, agentID)
	if err != nil {
		return fmt.Errorf("agentprovision: stop agent %d: load row: %w", agentID, err)
	}
	if err := a.driver.Scale(ctx, refOf(ag), 0); err != nil {
		return fmt.Errorf("agentprovision: stop agent %d (%s): %w", ag.ID, ag.Name, err)
	}
	// 🔴 THE DELIVERY PROVENANCE IS DROPPED, AND agents.Store's own doc on this
	// method is the argument: a deliberate stop means the next start brings up an
	// instance with a new id, which is indistinguishable from an eviction — so
	// leaving the old recipient stamped would make agents.kickoffLost read this
	// agent as having lost its message and re-send the original task. Best-effort
	// on purpose: failing the stop over a provenance write would be a worse
	// answer than a stop that succeeded with stale provenance.
	if err := a.store.ClearKickoffDelivery(ctx, ag.ID); err != nil {
		a.log.Printf("agentprovision: stop %s: clear kickoff delivery: %v", ag.Name, err)
	}
	return a.setStatus(ctx, ag.ID, agents.StatusStopped)
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
		return fmt.Errorf("agentprovision: destroy agent %d (%s): the stored row was KEPT "+
			"because the instance was not removed: %w", ag.ID, ag.Name, err)
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
		return ag, a.fail(ctx, ag, "build spec", err)
	}
	if err := a.driver.Create(ctx, spec); err != nil {
		// provision.ErrDivergentSpec is named in the message because it is the
		// one Create failure a reader can act on WITHOUT touching the cluster:
		// the instance exists and does not match, and the way to change it is
		// Update (ReapplyProfiles), not another Create.
		if errors.Is(err, provision.ErrDivergentSpec) {
			return ag, a.fail(ctx, ag, "create instance (it already exists with a different "+
				"spec; reconciling an existing instance is ReapplyProfiles, not Dispatch)", err)
		}
		return ag, a.fail(ctx, ag, "create instance", err)
	}
	return ag, nil
}

// ensureHooksToken gives the agent its own per-agent token if it has none.
//
// ⚠ IT IS A SECRET AND IS NEVER LOGGED, not even truncated. agentspec.Build puts
// it in Spec.Secrets rather than Spec.Env for the same reason.
func (a *Adapter) ensureHooksToken(ctx context.Context, ag agents.Agent) (agents.Agent, error) {
	if ag.HooksToken != "" {
		return ag, nil
	}
	buf := make([]byte, hooksTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return ag, a.fail(ctx, ag, "mint hooks token", err)
	}
	token := hex.EncodeToString(buf)
	if err := a.store.SetHooksToken(ctx, ag.ID, token); err != nil {
		return ag, a.fail(ctx, ag, "store hooks token", err)
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

// recordUndeliveredKickoff writes the non-delivery where a human reads it.
//
// 🔴 IT WRITES THE KICKOFF *ERROR* AND NEVER THE KICKOFF *FLAG*. The two fields
// are one letter apart in a call site and opposite in meaning: kickoff_error is
// evidence that a send did not happen, KickedOff is a claim that one did. See
// the package doc.
func (a *Adapter) recordUndeliveredKickoff(ctx context.Context, ag agents.Agent) error {
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
func (a *Adapter) setStatus(ctx context.Context, id int64, status string) error {
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
func (a *Adapter) fail(ctx context.Context, ag agents.Agent, what string, cause error) error {
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
