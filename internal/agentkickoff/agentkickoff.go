// Package agentkickoff DELIVERS an agent's first turn: it hands the note a
// dispatch was created with (agents.Agent.PendingNote) to the agent's model
// gateway once the agent's instance is ready, and records what happened on the row.
//
// 🔴 IT IS A BACKGROUND RECONCILE LOOP, NOT A STEP IN THE DISPATCH REQUEST, AND
// THE CODE THAT ALREADY EXISTED DECIDED THAT. agents.DecideReconcile is a
// per-tick decision table over (row, live instance, now): "a ready instance whose
// kickoff was never sent → retry while young, error once it has dwelt past
// ProvisioningStuckTimeout". That is the shape of a loop, and the alternatives are
// worse on their own terms:
//
//   - Dispatch runs inside a handler goroutine with a 5-minute operation budget
//     (agentprovision.DefaultOpTimeout) and returns when the Deployment is APPLIED,
//     not when the pod is ready. Delivering there means either blocking that
//     goroutine on image pull + repository clone + runtime boot, or giving up on a
//     slow-but-healthy pod. A loop simply looks again next tick.
//   - A first turn that fails BEFORE it is sent (no instance yet, no token) is
//     retried by the loop for free, and the table's dwell bound is what eventually
//     turns "never delivered" into a visible `error` — a mechanism a request-scoped
//     call has no equivalent of.
//   - Start of a saved agent owes the same first turn (agentprovision.Adapter.Start
//     says so) and has no request that waits for readiness either. One deliverer
//     serves both doors.
//
// 🔴 SINGLE-FLIGHT IS THE PER-AGENT CLAIM, NOT A LEADER LEASE. Two replicas
// ticking against one database both see `ready && !KickedOff`; each must win
// agents.Store.ClaimKickoff (a conditional UPDATE — a compare-and-set on the row)
// before doing anything, and the winner RE-READS the row under the claim, so a
// replica that lost the race or arrived after a delivery does nothing. No lease is
// needed and none exists in this module.
//
// 🔴 KickedOff IS STAMPED *BEFORE* THE MODEL TURN, SO A PAID TURN IS NEVER RUN
// TWICE. Once the stamp lands, agents.DecideReconcile stops returning
// ActionRetryKickoff for the row and this package never touches it again — even if
// the turn then fails, times out, or this process dies mid-turn. The cost is the
// mirror image and it is accepted: a turn that fails AFTER the stamp is recorded in
// agents.kickoff_error and is NOT retried automatically. Everything that can fail
// without spending money (no token, a lost claim, no chat session) is checked
// before the stamp, and is retried.
//
// 🔴 IT ACTS ONLY ON ROWS THIS DEPLOYMENT OWNS, AND NEVER ON A KICKED-OFF ROW. The
// agents table can hold rows another system wrote (a namespace the configured
// prefix did not produce); escalating those to `error` would be this process
// asserting a verdict about pods it cannot see. And a kicked-off row's recovery
// (agents.ActionResendKickoff / ActionErrorKickoffLost) is a re-delivery decision
// this package deliberately does not take — see [owesFirstTurn].
//
// 🔴 AN EMPTY REPLY IS A FAILED DELIVERY. The agent runtime's /v1/responses is
// known to discard its own successful retry for reasoning models and answer 200
// with no text; recording that as delivered would report an agent that was told
// its task when nothing says it was. It lands in agents.kickoff_error.
package agentkickoff

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// DefaultInterval is how often [Deliverer.Run] looks for an owed first turn.
// A dispatched pod takes tens of seconds to become ready, so this adds at most
// that much latency to a first turn while costing two list reads per tick.
const DefaultInterval = 10 * time.Second

// DefaultTurnTimeout bounds one delivered first turn, end to end. The gateway's
// own HTTP client bounds each REQUEST; this bounds the turn this package owns.
const DefaultTurnTimeout = 15 * time.Minute

// claimMargin is how much longer than a turn the per-agent claim lives, so a
// healthy turn always finishes inside its own claim.
const claimMargin = 2 * time.Minute

// bookkeepingTimeout bounds each store write that RECORDS an outcome. It is a
// fresh budget so a turn that consumed its whole deadline can still be recorded.
const bookkeepingTimeout = 15 * time.Second

// EmptyReplyReason is written to agents.kickoff_error when the first turn came
// back with no text.
const EmptyReplyReason = "kickoff turn returned an EMPTY reply, so it is recorded as NOT " +
	"delivered: the gateway answered without error and with no text. The known cause is a " +
	"reasoning model behind the agent runtime's /v1/responses, which discards its own " +
	"successful retry for that model class; pin a non-reasoning model for this agent and " +
	"send the task through its chat."

// InstanceLister is what the deliverer needs from a provisioner: the live
// instances, so it can tell a ready recipient from one that is not.
// agentprovision.Adapter.Instances satisfies it.
type InstanceLister interface {
	Instances(ctx context.Context) ([]provision.Instance, error)
}

// Gateway is the one call a first turn needs. It is the method
// POST /api/agents/{name}/messages calls, so the kickoff travels the same path an
// operator's message does. agentgateway.Gateway satisfies it.
type Gateway interface {
	Chat(ctx context.Context, a agents.Agent, sessionKey, message string, emit func(string)) (string, error)
}

// Config is everything the deliverer needs.
type Config struct {
	// Store is the agents store. REQUIRED.
	Store agents.Store
	// Instances lists live instances. REQUIRED.
	Instances InstanceLister
	// Gateway runs the turn. REQUIRED.
	Gateway Gateway
	// NamespacePrefix is the deployment's CONFIGURED agent namespace prefix — the
	// same value the row-writing path and the driver use. A row is this
	// deployment's iff its stored namespace is agents.NamespaceFor(prefix, name).
	NamespacePrefix string
	// Owner names this process in a kickoff claim. Defaults to hostname/random.
	Owner string
	// Logger is optional.
	Logger *log.Logger
	// Now is the clock DecideReconcile is evaluated against. Defaults to time.Now.
	Now func() time.Time
	// TurnTimeout defaults to [DefaultTurnTimeout].
	TurnTimeout time.Duration
}

// Deliverer delivers owed first turns. Build it with [New].
type Deliverer struct {
	store    agents.Store
	insts    InstanceLister
	gw       Gateway
	prefix   string
	owner    string
	log      *log.Logger
	now      func() time.Time
	turn     time.Duration
	mu       sync.Mutex
	inFlight map[int64]bool
	wg       sync.WaitGroup
}

// New validates the configuration and builds the deliverer.
func New(cfg Config) (*Deliverer, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("agentkickoff: Config.Store is required")
	case cfg.Instances == nil:
		return nil, errors.New("agentkickoff: Config.Instances is required (without live " +
			"instances no recipient can be judged ready)")
	case cfg.Gateway == nil:
		return nil, errors.New("agentkickoff: Config.Gateway is required (a deliverer with no " +
			"gateway delivers nothing, which is the defect this package exists to close)")
	}
	d := &Deliverer{
		store:    cfg.Store,
		insts:    cfg.Instances,
		gw:       cfg.Gateway,
		prefix:   agents.ResolveNamespacePrefix(cfg.NamespacePrefix),
		owner:    cfg.Owner,
		log:      cfg.Logger,
		now:      cfg.Now,
		turn:     cfg.TurnTimeout,
		inFlight: map[int64]bool{},
	}
	if d.owner == "" {
		d.owner = defaultOwner()
	}
	if d.log == nil {
		d.log = log.New(io.Discard, "", 0)
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.turn <= 0 {
		d.turn = DefaultTurnTimeout
	}
	return d, nil
}

// defaultOwner is hostname/random: the hostname says which pod, the suffix
// separates a restarted container from its predecessor.
func defaultOwner() string {
	host, _ := os.Hostname()
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	return host + "/" + hex.EncodeToString(buf)
}

// Run ticks every interval until ctx is cancelled, then waits for in-flight turns.
func (d *Deliverer) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	defer d.Wait()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := d.Tick(ctx); err != nil && ctx.Err() == nil {
			d.log.Printf("agentkickoff: tick: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Wait blocks until every delivery started by [Deliverer.Tick] has finished.
func (d *Deliverer) Wait() { d.wg.Wait() }

// owned reports whether a row was written by THIS deployment: its stored
// namespace is exactly the one the configured prefix produces for its name.
func (d *Deliverer) owned(a agents.Agent) bool {
	return a.Namespace == agents.NamespaceFor(d.prefix, a.Name)
}

// owesFirstTurn is the deliverer's scope: a row this deployment owns, holding a
// note, that has NEVER been kicked off.
//
// 🔴 `!a.KickedOff` IS A HARD EXCLUSION, NOT A DETAIL OF agents.KickoffOwed. A
// kicked-off row has already had a (possibly paid) turn handed to a gateway; the
// only decisions the table makes for it are re-delivery after a lost recipient and
// the error that ends that budget, and this package does not take them. Deciding to
// re-send a task is a different, costlier decision than paying a debt nobody has
// paid, and it is not taken here by omission.
func (d *Deliverer) owesFirstTurn(a agents.Agent) bool {
	return d.owned(a) && !a.KickedOff && a.PendingNote != ""
}

// Tick makes one pass: it decides, for every owed row, what agents.DecideReconcile
// says, acts on it, and starts each delivery in the background (see
// [Deliverer.Wait]).
//
// 🔴 A FAILED INSTANCE LIST ENDS THE TICK WITH NO ACTION. An unobserved backend is
// not an empty one: deciding against a nil instance would read every young row as
// absent and every old one as stuck, and error rows on no information.
func (d *Deliverer) Tick(ctx context.Context) error {
	rows, err := d.store.List(ctx)
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}
	var owed []agents.Agent
	for _, a := range rows {
		if d.owesFirstTurn(a) {
			owed = append(owed, a)
		}
	}
	if len(owed) == 0 {
		return nil
	}
	insts, err := d.insts.Instances(ctx)
	if err != nil {
		return fmt.Errorf("list instances (no action taken this tick): %w", err)
	}
	idx := agents.InstanceIndex(insts)
	now := d.now()
	for _, a := range owed {
		if d.isInFlight(a.ID) {
			continue
		}
		inst := idx[a.Name]
		action, _ := agents.DecideReconcile(a, inst, now)
		switch action {
		case agents.ActionRetryKickoff:
			d.startDelivery(ctx, a, *inst)
		case agents.ActionError:
			d.failStuck(a)
		default:
			// ActionNone / ActionPersistStatus: nothing owed THIS tick. Persisting a
			// recomputed status is deliberately not taken: views refine the stored
			// status live (agents.ComputeStatus), and a persist from an instance
			// snapshot taken before the dispatch's Create would race it.
		}
	}
	return nil
}

func (d *Deliverer) isInFlight(id int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.inFlight[id]
}

func (d *Deliverer) startDelivery(ctx context.Context, a agents.Agent, inst provision.Instance) {
	d.mu.Lock()
	if d.inFlight[a.ID] {
		d.mu.Unlock()
		return
	}
	d.inFlight[a.ID] = true
	d.mu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() {
			d.mu.Lock()
			delete(d.inFlight, a.ID)
			d.mu.Unlock()
		}()
		d.deliver(ctx, a, inst)
	}()
}

// failStuck marks an owed row `error` once the table says its first turn will not
// arrive: it dwelt past ProvisioningStuckTimeout without a ready instance accepting
// the turn. The last recorded send failure is appended, so error_message carries the
// primary evidence (the card itself shows a red status dot; the text is machine tier).
func (d *Deliverer) failStuck(a agents.Agent) {
	msg := fmt.Sprintf("kickoff never delivered: the agent dwelt more than %s without a ready "+
		"instance accepting its first turn", agents.ProvisioningStuckTimeout) +
		agents.KickoffErrorSuffix(a)
	ctx, cancel := bookkeeping()
	defer cancel()
	if err := d.store.UpdateStatus(ctx, a.ID, agents.StatusError, "", msg); err != nil {
		d.log.Printf("agentkickoff: agent %d (%s): could not record %q: %v", a.ID, a.Name, msg, err)
		return
	}
	d.log.Printf("agentkickoff: agent %d (%s): %s", a.ID, a.Name, msg)
}

// deliver runs one first turn. The order is the design: everything that can fail
// without spending money happens BEFORE KickedOff is stamped, and is therefore
// retried by the next tick; the turn itself happens only after the stamp, so it is
// never run twice.
func (d *Deliverer) deliver(parent context.Context, a agents.Agent, inst provision.Instance) {
	ctx, cancel := context.WithTimeout(parent, d.turn)
	defer cancel()

	// A token-less row derives a well-formed WRONG bearer (agentgateway.reach), so
	// it is refused here, before the stamp, and retried until the dwell bound.
	if a.HooksToken == "" {
		d.recordError(a, "kickoff not sent: the agent has no hooks token yet, so no gateway "+
			"credential can be derived (it is minted when the instance is created)")
		return
	}

	won, err := d.store.ClaimKickoff(ctx, a.ID, d.owner, d.turn+claimMargin)
	if err != nil || !won {
		// Another process holds it, or the claim could not be verified. Either way
		// this process must not run the turn — and must not release a claim it
		// does not hold.
		if err != nil {
			d.log.Printf("agentkickoff: agent %d (%s): claim: %v", a.ID, a.Name, err)
		}
		return
	}
	defer func() {
		rctx, rcancel := bookkeeping()
		defer rcancel()
		if err := d.store.ReleaseKickoffClaim(rctx, a.ID, d.owner); err != nil {
			d.log.Printf("agentkickoff: agent %d (%s): release claim: %v", a.ID, a.Name, err)
		}
	}()

	// 🔴 RE-READ UNDER THE CLAIM. The row above came from a list taken before the
	// claim; another replica may have delivered in between. The stamp is what makes
	// that visible, so the fresh row is the only one this decision may use.
	fresh, err := d.store.Get(ctx, a.ID)
	if err != nil {
		d.log.Printf("agentkickoff: agent %d (%s): re-read under claim: %v", a.ID, a.Name, err)
		return
	}
	if !d.owesFirstTurn(fresh) || fresh.Status == agents.StatusError || fresh.HooksToken == "" {
		return
	}

	// Defence in depth on the budget: a row that has somehow been delivered to
	// MaxKickoffAttempts times while still reading never-kicked-off is not paid
	// again.
	if fresh.KickoffAttempts >= agents.MaxKickoffAttempts {
		d.failBudget(fresh)
		return
	}

	// The transcript session is opened BEFORE the stamp: failing to open one costs
	// nothing and is retried, whereas after the stamp it would strand the turn.
	sess, err := d.store.LatestOrCreateSession(ctx, fresh.ID, fresh.Name)
	if err != nil {
		d.recordError(fresh, "kickoff not sent: could not open a chat session: "+err.Error())
		return
	}

	// ---- the point of no return: from here the turn is never re-run. ----
	if err := d.store.SetKickedOff(ctx, fresh.ID, true); err != nil {
		d.recordError(fresh, "kickoff not sent: could not stamp kicked_off: "+err.Error())
		return
	}
	if err := d.store.RecordKickoffDelivery(ctx, fresh.ID, inst.InstanceID, inst.Restarts); err != nil {
		// Not fatal: an unrecorded recipient disables restart detection for this
		// agent (agents.kickoffLost's fail-safe direction); the turn still runs.
		d.log.Printf("agentkickoff: agent %d (%s): record delivery provenance: %v",
			fresh.ID, fresh.Name, err)
	}

	_, _ = d.store.AddChatMessage(ctx, agents.ChatMessage{
		AgentID: fresh.ID, SessionID: sess.ID, Role: "user", Content: fresh.PendingNote,
	})
	reply, err := d.gw.Chat(ctx, fresh, sess.SessionKey, fresh.PendingNote, nil)
	if err != nil {
		d.recordError(fresh, "kickoff turn failed after it was handed to the gateway (not "+
			"retried, so it is never paid twice): "+err.Error())
		return
	}
	if strings.TrimSpace(reply) == "" {
		d.recordError(fresh, EmptyReplyReason)
		return
	}
	actx, acancel := bookkeeping()
	defer acancel()
	_, _ = d.store.AddChatMessage(actx, agents.ChatMessage{
		AgentID: fresh.ID, SessionID: sess.ID, Role: "assistant", Content: reply,
	})
	d.log.Printf("agentkickoff: agent %d (%s): first turn delivered to %s (%d-byte reply)",
		fresh.ID, fresh.Name, inst.InstanceID, len(reply))
}

// failBudget marks a row whose delivery budget is spent `error`.
func (d *Deliverer) failBudget(a agents.Agent) {
	msg := fmt.Sprintf("kickoff not sent: %d deliveries already recorded (budget %d)",
		a.KickoffAttempts, agents.MaxKickoffAttempts) + agents.KickoffErrorSuffix(a)
	ctx, cancel := bookkeeping()
	defer cancel()
	if err := d.store.UpdateStatus(ctx, a.ID, agents.StatusError, "", msg); err != nil {
		d.log.Printf("agentkickoff: agent %d (%s): could not record %q: %v", a.ID, a.Name, msg, err)
	}
}

// recordError writes a send failure as evidence (agents.Store.SetKickoffError —
// it does not move the dwell clock) and logs it.
func (d *Deliverer) recordError(a agents.Agent, msg string) {
	d.log.Printf("agentkickoff: agent %d (%s): %s", a.ID, a.Name, msg)
	ctx, cancel := bookkeeping()
	defer cancel()
	if err := d.store.SetKickoffError(ctx, a.ID, msg); err != nil {
		d.log.Printf("agentkickoff: agent %d (%s): could not record kickoff error: %v",
			a.ID, a.Name, err)
	}
}

func bookkeeping() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), bookkeepingTimeout)
}
