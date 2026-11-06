package officers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/metrics"
)

// ---------------------------------------------------------------------------
// THE WAKE LOOP
//
// For each declared officer, every PollInterval, one pass (polling HQ is the
// only wake signal: there is no webhook receiver):
//
//  1. read HQ: the officer's open items and its claimed items;
//  2. read the fleet: which of the officer's instance agents exist and are up;
//  3. DECIDE (Decide, a pure function of those two reads, the clock and one
//     remembered timestamp) which instances to start and which to stop;
//  4. release any live claim held by an instance that is DOWN, so another
//     instance can take the item at once instead of waiting out the lease;
//  5. act.
//
// 🔴 STATE COMES FROM HQ AND THE FLEET, NOT FROM MEMORY. The one thing kept
// between passes is when the officer was first seen idle (idleSince). A
// restarted muster forgets it and starts the idle window again, which can
// only DELAY a scale-down, never cause one early or cause a start: every
// scale-up decision is a function of the two reads alone, so a restarted
// process makes the same scale-up decisions the old one would have.
//
// 🔴 HQ DOWN CHANGES NOTHING. A pass whose HQ read fails starts nothing,
// stops nothing and releases nothing, and the next pass for that officer is
// pushed out exponentially (Backoff) up to MaxBackoff. A pass whose fleet read
// fails (the provisioner is blind) is treated the same way: acting on "no
// instances are up" when the truth is "I cannot see" would start duplicates.
// ---------------------------------------------------------------------------

// InstanceState is one instance agent of an officer as the fleet sees it.
type InstanceState struct {
	// N is the instance number, 1..MaxInstances.
	N int
	// Name is the agent name (InstanceName(officer, N)).
	Name string
	// Exists reports whether muster has an agent row for it.
	Exists bool
	// Up reports whether its workload is running or on its way up
	// (provisioning, pending). A stopped, absent or never-started instance is
	// not up. An instance in error counts as up: it holds its slot.
	Up bool
	// Error reports the instance in error (crash-looping, a failed apply). It
	// holds its slot, so the loop logs and counts it rather than starting it.
	Error bool
	// PromptPending reports a prompt (a kickoff or a re-prompt) owed to the
	// instance and not yet delivered. Such an instance is not re-prompted.
	PromptPending bool
}

// Fleet is how the loop observes and moves an officer's instances.
// internal/api implements it over the agents store and the provisioner.
type Fleet interface {
	// Instances returns the state of instances 1..o.MaxInstances. An error
	// means the fleet could not be SEEN; the pass then changes nothing.
	Instances(ctx context.Context, o Officer) ([]InstanceState, error)
	// Start brings instance n up — creating its agent when it does not exist
	// — with kickoff as its first message.
	Start(ctx context.Context, o Officer, n int, kickoff string) error
	// Stop scales instance n to zero, keeping its agent and volume.
	Stop(ctx context.Context, o Officer, n int) error
	// Reprompt gives up instance n a new turn with text, delivered when its
	// session can take one (internal/api: the kickoff deliverer, which never
	// types into a turn already running).
	Reprompt(ctx context.Context, o Officer, n int, text string) error
}

// HQ is the part of the HQ API the loop uses. *Client satisfies it (per
// officer); ClientHQ adapts a client factory to it.
type HQ interface {
	AllItems(ctx context.Context, o Officer, status string) ([]Item, error)
	Release(ctx context.Context, o Officer, item ItemID, instance string) error
	// Complete completes item as instance (its holder): the wall-time cap's
	// escalation (Decision.Overrun).
	Complete(ctx context.Context, o Officer, item ItemID, instance string, done Completion) error
	// Heartbeat renews instance's lease on item: a capped item whose
	// escalation HQ refused is kept held, so it is never re-claimed.
	Heartbeat(ctx context.Context, o Officer, item ItemID, instance string, leaseSeconds int) error
}

// ClientHQ is HQ over one Client per officer, built by NewClient.
type ClientHQ struct {
	mu      sync.Mutex
	clients map[string]*Client
	build   func(Officer) *Client
}

// NewClientHQ returns an HQ that talks to each officer's own HQ with its own
// token. build nil uses NewClient(o, nil).
func NewClientHQ(build func(Officer) *Client) *ClientHQ {
	if build == nil {
		build = func(o Officer) *Client { return NewClient(o, nil) }
	}
	return &ClientHQ{clients: map[string]*Client{}, build: build}
}

// Client returns the cached client for o.
func (h *ClientHQ) Client(o Officer) *Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.clients[o.ID]
	if !ok {
		c = h.build(o)
		h.clients[o.ID] = c
	}
	return c
}

// AllItems implements HQ.
func (h *ClientHQ) AllItems(ctx context.Context, o Officer, status string) ([]Item, error) {
	return h.Client(o).AllItems(ctx, status)
}

// Release implements HQ.
func (h *ClientHQ) Release(ctx context.Context, o Officer, item ItemID, instance string) error {
	_, err := h.Client(o).Release(ctx, item, instance)
	return err
}

// Complete implements HQ.
func (h *ClientHQ) Complete(ctx context.Context, o Officer, item ItemID, instance string, done Completion) error {
	_, err := h.Client(o).Complete(ctx, item, instance, done)
	return err
}

// Heartbeat implements HQ.
func (h *ClientHQ) Heartbeat(ctx context.Context, o Officer, item ItemID, instance string, leaseSeconds int) error {
	_, err := h.Client(o).Heartbeat(ctx, item, instance, leaseSeconds)
	return err
}

// Observation is what one pass read.
type Observation struct {
	Open      []Item
	Claimed   []Item
	Instances []InstanceState
	// Capped: the pool is a judgement pool that has spent its day's session
	// cap (JudgementLedger.Usage); then only Counted items — those it
	// already claimed today — are work for it.
	Capped  bool
	Counted map[ItemID]bool
	// Used is how many judgement sessions the pool has used today
	// (JudgementLedger.Usage); 0 for an uncapped pool or an unreadable ledger.
	Used int
}

// Overrun is a live claim past its pool's wall-time cap (Officer.ItemCap).
type Overrun struct {
	Item Item
	// N is the holder's instance number; Up whether it is up (then stopped).
	N  int
	Up bool
	// Held is how long the item has been held: now less HQ's claimed_at.
	Held time.Duration
}

// Decision is what one pass decided.
type Decision struct {
	// Start, Stop and Reprompt are instance numbers, ascending.
	Start    []int
	Stop     []int
	Reprompt []int
	// StopReason is why Stop stops ("" = the officer was idle for its window).
	StopReason string
	// Release is live claims held by an instance that is not up.
	Release []Item
	// Overrun is live claims held past the pool's wall-time cap: each holder
	// is stopped (when up) and the item completed escalated. An overrun claim
	// is never in Release, Held or Stop.
	Overrun []Overrun
	// Unclocked is live claims of a capped pool that HQ gave no claimed_at:
	// with no clock they are not capped (logged once per officer).
	Unclocked []ItemID
	// Idle reports the officer idle at this pass: nothing claimable, no live
	// claim held by any of its instances.
	Idle bool
	// Claimable is how many items an instance could claim now.
	Claimable int
	// CapWaiting is how many items would be Claimable but for the day's
	// judgement session cap (obs.Capped): work held back by the cap.
	CapWaiting int
	// Held is how many live claims its instances hold.
	Held int
}

// Decide is the loop's whole policy, as a pure function, for ONE POOL of
// instances: o is an untiered officer, or one pool of a tiered one
// (Officer.Pools), and obs.Instances that pool's instances.
//
//   - Claimable = open items, plus claimed items whose lease has expired (the
//     contract makes those claimable by anyone) — less any item HQ marks
//     Blocked: its claim is refused, so it is not work, and an instance woken
//     or kept up for it would claim nothing. A tiered pool counts only the
//     items of its own tier as HQ reads them (ItemTier): its instances' claims
//     name the tier, so another tier's item is not work for them either (HQ
//     answers 409 wrong_tier).
//   - A judgement pool that has spent its day's session cap (obs.Capped)
//     counts as Claimable only the items it already claimed today
//     (obs.Counted): officer_claim refuses it any other.
//   - Held = claimed items with a live lease whose holder is one of this
//     pool's instances — whatever the item's tier: a claim belongs to the pool
//     that holds it.
//   - Busy (Claimable > 0 or Held > 0): want min(MaxInstances, held-by-up +
//     Claimable) instances up; start the lowest-numbered down instances to
//     reach it. Never stop anything while busy.
//   - Idle (neither): once idle has lasted IdleAfter (now - idleSince), stop
//     every up instance. idleSince zero means "idle only now", so nothing is
//     stopped on the first idle pass.
//   - A live claim held by an instance that is NOT up is released, busy or
//     idle: its holder cannot heartbeat it, and releasing returns the item to
//     the queue now rather than when the lease lapses.
//   - A live claim held for the pool's wall-time cap (ItemCap, measured
//     from HQ's claimed_at) is an Overrun instead, up or not: its holder is
//     stopped and the item escalated. A claim without claimed_at has no
//     clock and is not capped (Unclocked). Released, a judgement item would
//     be claimed again and run another cap. The holder is not counted busy, so it is
//     neither re-prompted nor kept up for that claim.
//   - 🔴 AN INSTANCE NUMBER HOLDING A CAPPED CLAIM IS NEVER STARTED, for as
//     long as HQ shows it holding one (an escalation HQ refuses keeps the
//     item held in that NAME): a new session under the name would claim
//     other work, and the next pass would stop it for the old item's cap,
//     killing that work every few polls without bound. Other work starts the
//     next free number, or waits. A holder that is up still counts as up on
//     the pass that stops it, so a failed stop never puts more than
//     MaxInstances up; the next pass, seeing it down, starts another number.
func Decide(o Officer, obs Observation, idleSince, now time.Time, promptedAt map[int]time.Time) Decision {
	byName := map[string]InstanceState{}
	for _, in := range obs.Instances {
		byName[in.Name] = in
	}
	ours := func(holder string) (InstanceState, bool) {
		if p, n, ok := (&Registry{list: []Officer{o}}).ForAgentName(holder); ok && p.Tier == o.Tier {
			if in, ok := byName[holder]; ok {
				return in, true
			}
			return InstanceState{N: n, Name: holder}, true
		}
		return InstanceState{}, false
	}
	var d Decision
	// claimable: the pool could claim it now. A capped judgement pool may
	// claim only the items it already counted today (officer_claim refuses
	// any other), so no other item is work for it.
	workable := func(it Item) bool { return !it.Blocked && o.worksItem(it) }
	claimable := func(it Item) bool {
		return workable(it) && (!obs.Capped || obs.Counted[it.ID])
	}
	count := func(it Item) {
		switch {
		case claimable(it):
			d.Claimable++
		case workable(it):
			d.CapWaiting++
		}
	}
	for _, it := range obs.Open {
		count(it)
	}
	heldByUp := map[string]bool{}
	overrun := map[string]bool{}
	// capHolder: the instance numbers holding a capped claim. Never started.
	capHolder := map[int]bool{}
	limit := o.ItemCap()
	for _, it := range obs.Claimed {
		if !it.LeaseLive(now) {
			count(it)
			continue
		}
		in, mine := ours(it.ClaimedBy)
		if !mine {
			continue
		}
		if limit > 0 && it.ClaimedAt == nil {
			d.Unclocked = append(d.Unclocked, it.ID)
		}
		if limit > 0 && it.ClaimedAt != nil && now.Sub(*it.ClaimedAt) >= limit {
			d.Overrun = append(d.Overrun, Overrun{Item: it, N: in.N, Up: in.Up, Held: now.Sub(*it.ClaimedAt)})
			overrun[in.Name] = true
			capHolder[in.N] = true
			continue
		}
		d.Held++
		if in.Up {
			heldByUp[in.Name] = true
		} else {
			d.Release = append(d.Release, it)
		}
	}
	up := 0
	for _, in := range obs.Instances {
		if in.Up {
			up++
		}
	}
	if d.Claimable > 0 || d.Held > 0 {
		want := len(heldByUp) + d.Claimable
		if want > o.MaxInstances {
			want = o.MaxInstances
		}
		for _, n := range downInstances(o, obs.Instances) {
			if up+len(d.Start) >= want {
				break
			}
			if !capHolder[n] {
				d.Start = append(d.Start, n)
			}
		}
		skip := map[string]bool{}
		for n := range heldByUp {
			skip[n] = true
		}
		for n := range overrun {
			skip[n] = true
		}
		d.Reprompt = reprompts(obs.Instances, skip, d.Claimable-len(d.Start), promptedAt, now)
		return d
	}
	d.Idle = true
	if up > 0 && !idleSince.IsZero() && now.Sub(idleSince) >= o.IdleAfter {
		for _, in := range obs.Instances {
			if in.Up && !overrun[in.Name] {
				d.Stop = append(d.Stop, in.N)
			}
		}
		sort.Ints(d.Stop)
	}
	return d
}

// reprompts picks the up instances to prompt again for claimable work an
// instance being started does not already cover.
//
// 🔴 WITHOUT IT AN UP INSTANCE IS NEVER PROMPTED AGAIN. An instance works the
// queue, holds nothing when it is done, and its turn ends; it stays up until
// the officer idles out. A new item then wakes nobody — no start is due (it is
// up), and the officer is not idle (an item is open), so it never scales to
// zero either. An idle instance is one that is up, not in error, holds no live
// claim, and has no prompt pending (the last one was delivered).
//
// Rate limit: an instance prompted within RepromptAfter is not prompted again,
// and it — like a pending prompt — counts as already covering one claimable
// item, so a fresh item gets one prompt, not one per instance. An instance
// busy without a claim (deciding which item to take) is thus prompted at most
// every RepromptAfter, and the deliverer refuses to type into a running turn
// anyway (ccd's 409 busy), so a prompt never interrupts one.
func reprompts(insts []InstanceState, heldByUp map[string]bool, uncovered int, promptedAt map[int]time.Time, now time.Time) []int {
	sorted := append([]InstanceState(nil), insts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].N < sorted[j].N })
	var due []int
	for _, in := range sorted {
		if !in.Up || in.Error || heldByUp[in.Name] {
			continue
		}
		at, prompted := promptedAt[in.N]
		if in.PromptPending || (prompted && now.Sub(at) < RepromptAfter) {
			uncovered-- // already on its way to the queue
			continue
		}
		due = append(due, in.N)
	}
	if uncovered <= 0 {
		return nil
	}
	if len(due) > uncovered {
		due = due[:uncovered]
	}
	return due
}

// downInstances is 1..MaxInstances minus the up ones, ascending. An instance
// the fleet did not report is down.
func downInstances(o Officer, insts []InstanceState) []int {
	upN := map[int]bool{}
	for _, in := range insts {
		if in.Up {
			upN[in.N] = true
		}
	}
	var out []int
	for n := 1; n <= o.MaxInstances; n++ {
		if !upN[n] {
			out = append(out, n)
		}
	}
	return out
}

// Backoff bounds.
const (
	MaxBackoff = 10 * time.Minute
)

// Backoff is how long to wait after the failures'th consecutive failed pass:
// PollInterval doubled per failure, capped at MaxBackoff.
func Backoff(o Officer, failures int) time.Duration {
	d := o.PollInterval
	for i := 0; i < failures && d < MaxBackoff; i++ {
		d *= 2
	}
	if d > MaxBackoff {
		d = MaxBackoff
	}
	return d
}

// officerState is the little the loop remembers per officer.
type officerState struct {
	// idleSince is when each pool was first seen idle, by Officer.Tier ("" for
	// an untiered officer's one pool).
	idleSince map[string]time.Time
	failures  int
	nextAt    time.Time
	// promptedAt is when each instance was last started or re-prompted, by
	// instance agent name (the two pools' numbers overlap).
	promptedAt map[string]time.Time
	// errored is the instances last seen in error (logged on change), by
	// instance agent name.
	errored map[string]erroredSlot
	// unclockedLogged: a capped pool's claim without HQ's claimed_at has been
	// logged (once per officer per process).
	unclockedLogged bool
	// capRefused is each capped item whose escalation HQ is refusing: the
	// last refusal logged (logged on change, not every pass) and the episode
	// Config.CapHold was told of. An entry is one withheld instance number
	// (muster_officer_cap_refusal_withheld); it is dropped, and the episode
	// ended, when the escalation lands, HQ answers 409, or the item is no
	// longer over its cap.
	capRefused map[ItemID]refusedCap
	// fault is the CONFIRMED permanent HQ refusal (the status, e.g. "404"; ""
	// = none). It is set once a refusal has been seen on FaultStrikes
	// consecutive passes and cleared ONLY by a successful answer — a 5xx in
	// between leaves it, so the gauge and the "answers again" line stay true.
	fault string
	// strikes counts consecutive passes that saw a permanent refusal; a
	// successful answer and an outage (5xx, network) both reset it.
	strikes int
	// capReached: the last pass succeeded and saw the judgement pool at its
	// daily session cap with claimable judgement items waiting
	// (muster_officer_judgement_cap_reached); a failed pass clears it. capLoggedDay is the UTC day that
	// was last logged, so the line is written once per (officer, UTC day).
	capReached   bool
	capLoggedDay string
}

// refusedCap is one refused cap escalation the loop remembers.
type refusedCap struct {
	msg  string
	pool Officer
	hold CapHold
}

// CapHold is one episode of a refused wall-time cap escalation (Config.
// CapHold): HQ refuses to take capped item Item, so muster keeps it held as
// its stopped holder and never starts that instance number (N) again until
// the escalation lands. The pool is withheld that instance meanwhile; at
// max_instances 1, the whole pool.
type CapHold struct {
	Item ItemID
	// N is the holder's instance number, withheld from the pool.
	N int
	// Refusal is HQ's refusal, as muster got it, when the episode began.
	Refusal string
	// Waiting is the pool's claimable items when the episode began.
	Waiting int
	// Ended: the episode is over (the escalation landed, HQ answered 409, or
	// the item is no longer held over its cap).
	Ended bool
}

// Reconciler runs the wake loop.
type Reconciler struct {
	reg   *Registry
	hq    HQ
	fleet Fleet
	log   *log.Logger
	now   func() time.Time
	// observed is Config.Observed (nil: none).
	observed func(o Officer, claimed []Item, at time.Time)
	// judgement is Config.Judgement (nil: no pool is capped).
	judgement *JudgementLedger
	// capHold is Config.CapHold (nil: none).
	capHold func(o Officer, h CapHold)

	mu    sync.Mutex
	state map[string]*officerState
	done  chan struct{}
}

// Config is NewReconciler's input.
type Config struct {
	Registry *Registry
	HQ       HQ
	Fleet    Fleet
	Logger   *log.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Observed, when set, is told every pass's claimed items for o, read no
	// earlier than at (the transcript forwarder's Sync). It runs only after
	// a SUCCESSFUL read — a pass that saw nothing tells it nothing, so an HQ
	// outage never reads as "no item is held". It must not block.
	Observed func(o Officer, claimed []Item, at time.Time)
	// Judgement is the judgement-session ledger officer_claim keeps
	// (sessions.go): a judgement pool whose day's cap is spent is not woken
	// for new work; a ledger that cannot be read leaves that pool as capped
	// for the pass (logged) and the other pool unaffected. nil caps nothing.
	Judgement *JudgementLedger
	// CapHold, when set, is told once when an episode of a refused cap
	// escalation begins (a capped item HQ will not take, whose instance
	// number is withheld from its pool) and once when it ends (h.Ended), with
	// o the item's pool. It is how a withheld pool reaches a human: nothing
	// else says the judgement pool is waiting. Called outside the loop's
	// lock, from the pass; it must not block. A restarted muster begins a
	// new episode for a refusal still in force.
	CapHold func(o Officer, h CapHold)
}

// NewReconciler returns a loop over cfg's officers.
func NewReconciler(cfg Config) (*Reconciler, error) {
	if cfg.Registry == nil || cfg.HQ == nil || cfg.Fleet == nil {
		return nil, errors.New("officers: a reconciler needs a registry, an HQ and a fleet")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Reconciler{reg: cfg.Registry, hq: cfg.HQ, fleet: cfg.Fleet, log: cfg.Logger, now: cfg.Now, observed: cfg.Observed,
		judgement: cfg.Judgement, capHold: cfg.CapHold, state: map[string]*officerState{}, done: make(chan struct{})}, nil
}

// erroredSlot is how an instance in error is named in the log.
type erroredSlot struct{ officer, instance string }

func (r *Reconciler) st(id string) *officerState {
	s, ok := r.state[id]
	if !ok {
		s = &officerState{idleSince: map[string]time.Time{}, promptedAt: map[string]time.Time{}, errored: map[string]erroredSlot{},
			capRefused: map[ItemID]refusedCap{}}
		r.state[id] = s
	}
	return s
}

// PassResult is one pass's outcome, for logs and tests.
type PassResult struct {
	// Decision is the pass's decision for an untiered officer, or for a
	// tiered officer's judgement pool.
	Decision Decision
	// Routine is a tiered officer's routine pool's decision (nil: untiered).
	Routine *Decision
	// Skipped is set when the pass read nothing because the officer is in
	// backoff.
	Skipped bool
	// Err is the read failure that made the pass change nothing.
	Err error
	// ActErrs are failures acting on the decision (each logged).
	ActErrs []error
}

// Check runs one pass for officer id NOW, whatever its schedule, unless it is
// backing off and force is false.
func (r *Reconciler) Check(ctx context.Context, id string, force bool) PassResult {
	o, err := r.reg.Get(id)
	if err != nil {
		return PassResult{Err: err}
	}
	now := r.now()
	r.mu.Lock()
	s := r.st(id)
	if !force && s.failures > 0 && now.Before(s.nextAt) {
		r.mu.Unlock()
		return PassResult{Skipped: true}
	}
	idleSince := make(map[string]time.Time, len(s.idleSince))
	for k, v := range s.idleSince {
		idleSince[k] = v
	}
	r.mu.Unlock()

	pools := o.Pools()
	obs, err := r.observe(ctx, pools)
	if err != nil {
		r.mu.Lock()
		// A pass that saw nothing cannot say the pool is held back by its
		// session cap: muster_officer_judgement_cap_reached is the last
		// SUCCESSFUL pass's state, so a failed pass (and a fault scale-down,
		// which follows one) clears it rather than republish a stale value.
		s.capReached = false
		s.failures++
		s.nextAt = now.Add(Backoff(o, s.failures))
		f, next := s.failures, s.nextAt
		refusal := permanentRefusal(err)
		changed := false
		if refusal != "" {
			s.strikes++
			if s.strikes >= FaultStrikes && s.fault != refusal {
				s.fault, changed = refusal, true
			}
		} else {
			// An outage (5xx, network) BREAKS a run of refusals: HQ mid-change
			// is not HQ configured wrong. It does not clear a CONFIRMED fault —
			// only a successful answer does.
			s.strikes = 0
		}
		confirmed := refusal != "" && s.fault == refusal
		strikes := s.strikes
		r.mu.Unlock()
		r.publish()
		if !confirmed {
			note := ""
			if refusal != "" {
				note = fmt.Sprintf(" (HQ refusal %s seen on %d of %d consecutive passes before it counts as a configuration fault)", refusal, strikes, FaultStrikes)
			}
			r.log.Printf("officers: %s: %v; changing nothing%s, next check in %s (failure %d)", o.ID, err, note, next.Sub(now).Round(time.Second), f)
			return PassResult{Err: err}
		}
		// 🔴 A PERMANENT REFUSAL IS CONFIGURATION, NOT AN OUTAGE — once it has
		// held for FaultStrikes consecutive passes (a single 404 or 401 is an HQ
		// deploy or a token cache mid-rollout, and one-strike scaled every
		// instance to zero mid-turn on it). Holding pods and Claude accounts for
		// ever behind a revoked token is the wrong way to wait, so the officer is
		// scaled to zero; it wakes normally once HQ answers again. Logged once
		// per change, counted on the gauge.
		if changed {
			r.log.Printf("officers: %s: HQ refuses this officer (HQ answered %s on %d consecutive passes): a CONFIGURATION fault, not an outage — check %s%s and that HQ serves the officer API for %s. "+
				"Scaling its instances to zero; retrying with backoff (next in %s)", o.ID, refusal, strikes, TokenEnvPrefix, TokenEnvSuffix(o.ID), o.ID, next.Sub(now).Round(time.Second))
		}
		return r.scaleDownForFault(ctx, o, err, refusal)
	}
	r.noteErrors(o, pools, obs)
	if r.observed != nil {
		r.observed(o, obs[0].Claimed, now)
	}
	r.mu.Lock()
	ds := make([]Decision, len(pools))
	for i, p := range pools {
		prompted := map[int]time.Time{}
		for n := 1; n <= p.MaxInstances; n++ {
			if at, ok := s.promptedAt[p.InstanceName(n)]; ok {
				prompted[n] = at
			}
		}
		ds[i] = Decide(p, obs[i], idleSince[p.Tier], now, prompted)
	}
	if s.fault != "" {
		r.log.Printf("officers: %s: HQ answers again (was a configuration fault: HQ answered %s)", o.ID, s.fault)
		s.fault = ""
	}
	s.strikes = 0
	s.failures = 0
	s.nextAt = now.Add(o.PollInterval)
	over := map[ItemID]bool{}
	for _, d := range ds {
		for _, ov := range d.Overrun {
			over[ov.Item.ID] = true
		}
		if len(d.Unclocked) > 0 && !s.unclockedLogged {
			s.unclockedLogged = true
			r.log.Printf("officers: %s: HQ gave item %s no claimed_at, so max_item_minutes cannot time it: such items are NOT capped (logged once)", o.ID, d.Unclocked[0])
		}
	}
	var capLines []string
	for i, p := range pools {
		limit := p.SessionCap()
		if limit == 0 {
			continue
		}
		// obs.Used >= limit tells the cap from an unreadable ledger, which
		// also reads as Capped (fail closed) but with Used 0.
		reached := obs[i].Capped && obs[i].Used >= limit && ds[i].CapWaiting > 0
		s.capReached = reached
		if day := utcDay(now); reached && r.st("").capLoggedDay != day { // MUTANT (round 1 F3): one memory per loop
			r.st("").capLoggedDay = day
			reset := CapResetAt(now)
			capLines = append(capLines, fmt.Sprintf("officers: %s: %s: %d claimable judgement item(s) wait in HQ and are not claimed until the cap resets at %s "+
				"(tiers.judgement.max_sessions_per_day; logged once per UTC day)", p.label(), CapReason(obs[i].Used, limit, reset), ds[i].CapWaiting, reset.Format(time.RFC3339)))
		}
	}
	var ended []refusedCap
	for id, c := range s.capRefused {
		if !over[id] {
			delete(s.capRefused, id)
			ended = append(ended, c)
		}
	}
	for i, p := range pools {
		d := ds[i]
		switch {
		case !d.Idle:
			delete(s.idleSince, p.Tier)
		case s.idleSince[p.Tier].IsZero():
			s.idleSince[p.Tier] = now
		}
		if len(d.Stop) > 0 {
			// The pool is going to zero; a later wake starts a fresh window.
			delete(s.idleSince, p.Tier)
		}
	}
	r.mu.Unlock()
	r.publish()
	for _, l := range capLines {
		r.log.Print(l)
	}
	for _, c := range ended {
		r.endCapHold(c)
	}
	return r.actAll(ctx, pools, ds, nil)
}

// actAll acts on each pool's decision, in Pools order, and gathers them into
// one PassResult: Decision is the first pool's (the untiered officer's, or the
// judgement pool's), Routine the routine pool's.
func (r *Reconciler) actAll(ctx context.Context, pools []Officer, ds []Decision, cause error) PassResult {
	res := PassResult{Err: cause}
	for i, p := range pools {
		res.ActErrs = append(res.ActErrs, r.act(ctx, p, ds[i])...)
	}
	res.Decision = ds[0]
	if len(ds) > 1 {
		d := ds[1]
		res.Routine = &d
	}
	return res
}

// scaleDownForFault stops every up instance of o (HQ refuses it permanently).
// A fleet that cannot be seen changes nothing, as everywhere else.
func (r *Reconciler) scaleDownForFault(ctx context.Context, o Officer, cause error, status string) PassResult {
	pools := o.Pools()
	ds := make([]Decision, len(pools))
	for i, p := range pools {
		insts, err := r.fleet.Instances(ctx, p)
		if err != nil {
			return PassResult{Err: cause}
		}
		ds[i] = Decision{StopReason: "HQ configuration fault: " + status}
		for _, in := range insts {
			if in.Up {
				ds[i].Stop = append(ds[i].Stop, in.N)
			}
		}
		sort.Ints(ds[i].Stop)
	}
	return r.actAll(ctx, pools, ds, cause)
}

// noteErrors logs each instance entering or leaving error, once per change,
// across all of o's pools (obs is parallel to pools).
func (r *Reconciler) noteErrors(o Officer, pools []Officer, obs []Observation) {
	r.mu.Lock()
	s := r.st(o.ID)
	var entered, left []string
	now := map[string]erroredSlot{}
	for i, p := range pools {
		for _, in := range obs[i].Instances {
			if in.Error {
				now[in.Name] = erroredSlot{officer: p.label(), instance: p.InstanceLabel(in.N)}
				if _, was := s.errored[in.Name]; !was {
					entered = append(entered, in.Name)
				}
			}
		}
	}
	gone := map[string]erroredSlot{}
	for name, slot := range s.errored {
		if _, still := now[name]; !still {
			left = append(left, name)
			gone[name] = slot
		}
	}
	s.errored = now
	r.mu.Unlock()
	sort.Strings(entered)
	sort.Strings(left)
	for _, name := range entered {
		e := now[name]
		r.log.Printf("officers: %s: instance %s is in error — it holds its slot and is NOT restarted, so the officer runs below target "+
			"until it recovers or an operator acts (agent %s)", e.officer, e.instance, name)
	}
	for _, name := range left {
		e := gone[name]
		r.log.Printf("officers: %s: instance %s left error", e.officer, e.instance)
	}
	r.publish()
}

// publish sets the officer gauges from every officer's remembered state.
func (r *Reconciler) publish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	slots, faults, withheld, capReached := 0, 0, 0, 0
	for _, s := range r.state {
		if s.capReached {
			capReached++
		}
		slots += len(s.errored)
		if s.fault != "" {
			faults++
		}
		withheld += len(s.capRefused)
	}
	metrics.OfficerErrorSlots.Set(float64(slots))
	metrics.OfficerHQFaults.Set(float64(faults))
	metrics.OfficerCapRefusalWithheld.Set(float64(withheld))
	metrics.OfficerJudgementCapReached.Set(float64(capReached))
	published.Lock()
	published.slots, published.faults = float64(slots), float64(faults)
	published.Unlock()
}

// observe reads HQ ONCE for the officer — every tier's items — and the fleet
// once per pool, returning one Observation per pool (parallel to pools), each
// with all the items and that pool's instances: Decide does the tier split.
// Any failed read fails the whole pass, as for an untiered officer.
func (r *Reconciler) observe(ctx context.Context, pools []Officer) ([]Observation, error) {
	o := pools[0]
	open, err := r.hq.AllItems(ctx, o, StatusOpen)
	if err != nil {
		return nil, err
	}
	claimed, err := r.hq.AllItems(ctx, o, StatusClaimed)
	if err != nil {
		return nil, err
	}
	out := make([]Observation, len(pools))
	for i, p := range pools {
		insts, err := r.fleet.Instances(ctx, p)
		if err != nil {
			return nil, err
		}
		used, counted, err := r.judgement.Usage(ctx, p, r.now())
		capped := used > 0 && used >= p.SessionCap()
		if err != nil {
			// Only the CAPPED pool pays for an unreadable ledger: it is
			// treated as capped (no new work, fail closed), and the other
			// pool's pass goes on.
			r.log.Printf("officers: %s: %v — not waking it for new work this pass", p.label(), err)
			used, capped, counted = 0, true, nil
		}
		out[i] = Observation{Open: open, Claimed: claimed, Instances: insts, Capped: capped, Counted: counted, Used: used}
	}
	return out, nil
}

// act moves pool o's instances as d says.
func (r *Reconciler) act(ctx context.Context, o Officer, d Decision) []error {
	var errs []error
	for _, ov := range d.Overrun {
		errs = append(errs, r.stopOverrun(ctx, o, ov, d.Claimable)...)
	}
	for _, it := range d.Release {
		if err := r.hq.Release(ctx, o, it.ID, it.ClaimedBy); err != nil {
			errs = append(errs, err)
			r.log.Printf("officers: %s: release item %s held by stopped instance %s: %v", o.label(), it.ID, it.ClaimedBy, err)
			continue
		}
		r.log.Printf("officers: %s: released item %s: its holder %s is not running", o.label(), it.ID, it.ClaimedBy)
	}
	for _, n := range d.Start {
		if err := r.fleet.Start(ctx, o, n, KickoffText(o, n)); err != nil {
			errs = append(errs, err)
			r.log.Printf("officers: %s: start instance %s: %v", o.label(), o.InstanceLabel(n), err)
			continue
		}
		r.prompted(o, n)
		r.log.Printf("officers: %s: started instance %s (%d claimable, %d held)", o.label(), o.InstanceLabel(n), d.Claimable, d.Held)
	}
	for _, n := range d.Reprompt {
		err := r.fleet.Reprompt(ctx, o, n, RepromptText(o, n))
		if errors.Is(err, ErrInstanceBusy) {
			// A turn is running in its session; it may well be working the
			// queue. Not a prompt and not a failure: the next pass tries again.
			continue
		}
		if err != nil {
			errs = append(errs, err)
			r.log.Printf("officers: %s: re-prompt instance %s: %v", o.label(), o.InstanceLabel(n), err)
			continue
		}
		r.prompted(o, n)
		r.log.Printf("officers: %s: re-prompted idle instance %s for new work (%d claimable)", o.label(), o.InstanceLabel(n), d.Claimable)
	}
	for _, n := range d.Stop {
		if err := r.fleet.Stop(ctx, o, n); err != nil {
			errs = append(errs, err)
			r.log.Printf("officers: %s: scale instance %s to zero: %v", o.label(), o.InstanceLabel(n), err)
			continue
		}
		reason := d.StopReason
		if reason == "" {
			reason = "idle " + o.IdleAfter.String()
		}
		r.log.Printf("officers: %s: scaled instance %s to zero (%s)", o.label(), o.InstanceLabel(n), reason)
	}
	return errs
}

// stopOverrun ends a claim past its pool's wall-time cap: the holder is
// stopped when up, then the item is completed escalated as the holder, with a
// low-confidence investigation block (OverrunInvestigation: HQ requires one
// on an investigating type, and takes it on any).
//
//   - A failed stop does not hold back the escalation: once the item is
//     complete the holder's next heartbeat is refused (lease_lost).
//   - A 400 with the block is retried once without it: an HQ from before the
//     block refuses it as an unknown field.
//   - 409 (lease_lost: the holder finished it, or the lease lapsed): the item
//     is not ours to hold; logged, nothing more.
//   - Any other refusal (a 400 with and without the block, a 5xx): the item
//     is KEPT HELD — muster heartbeats it as the stopped holder, so its lease
//     never lapses and no instance claims it for another full session. The
//     next pass sees it over the cap again (claimed_at is HQ's, a heartbeat
//     keeps it) and tries the escalation again. HQ's error is logged once per
//     change. Releasing it instead would re-run the session the cap stopped.
//     The holder's number stays withheld from the pool (Decide), so the
//     first such refusal begins a CapHold episode (Config.CapHold: the
//     owner is told the pool is waiting); it ends when the escalation lands
//     or HQ answers 409. waiting is the pool's claimable items, for the
//     episode.
func (r *Reconciler) stopOverrun(ctx context.Context, o Officer, ov Overrun, waiting int) []error {
	var errs []error
	if ov.Up {
		if err := r.fleet.Stop(ctx, o, ov.N); err != nil {
			errs = append(errs, err)
			r.log.Printf("officers: %s: scale instance %s to zero at the wall-time cap: %v", o.label(), o.InstanceLabel(ov.N), err)
		} else {
			r.log.Printf("officers: %s: scaled instance %s to zero (held item %s for %s, max_item_minutes %d)", o.label(), o.InstanceLabel(ov.N),
				ov.Item.ID, ov.Held.Round(time.Minute), int(o.ItemCap()/time.Minute))
		}
	}
	inv := OverrunInvestigation(o)
	done := Completion{Outcome: StatusEscalated, Summary: OverrunSummary(o, ov), Investigation: &inv}
	err := r.hq.Complete(ctx, o, ov.Item.ID, ov.Item.ClaimedBy, done)
	if hqStatus(err) == http.StatusBadRequest {
		done.Investigation = nil
		if err2 := r.hq.Complete(ctx, o, ov.Item.ID, ov.Item.ClaimedBy, done); err2 == nil || hqStatus(err2) == http.StatusConflict {
			err = err2
		} else {
			err = fmt.Errorf("%w; without the investigation block: %v", err, err2)
		}
	}
	if err == nil {
		r.log.Printf("officers: %s: escalated item %s: held by %s past the wall-time cap", o.label(), ov.Item.ID, ov.Item.ClaimedBy)
		r.forgetCapRefusal(o, ov.Item.ID)
		return errs
	}
	errs = append(errs, err)
	if hqStatus(err) == http.StatusConflict {
		r.log.Printf("officers: %s: escalate item %s at the wall-time cap: %v (no longer held by %s)", o.label(), ov.Item.ID, err, ov.Item.ClaimedBy)
		r.forgetCapRefusal(o, ov.Item.ID)
		return errs
	}
	hbErr := r.hq.Heartbeat(ctx, o, ov.Item.ID, ov.Item.ClaimedBy, overrunHoldSeconds(o))
	if hbErr != nil {
		errs = append(errs, hbErr)
	}
	msg := err.Error()
	r.mu.Lock()
	s := r.st(o.ID)
	c, had := s.capRefused[ov.Item.ID]
	changed := c.msg != msg
	if !had {
		c = refusedCap{pool: o, hold: CapHold{Item: ov.Item.ID, N: ov.N, Refusal: msg, Waiting: waiting}}
	}
	c.msg = msg
	s.capRefused[ov.Item.ID] = c
	r.mu.Unlock()
	if !had {
		r.publish()
		r.log.Printf("officers: %s: instance %s is WITHHELD from the pool while HQ refuses the escalation of capped item %s (%d claimable item(s) waiting)",
			o.label(), o.InstanceLabel(ov.N), ov.Item.ID, waiting)
		if r.capHold != nil {
			r.capHold(o, c.hold)
		}
	}
	if changed || hbErr != nil {
		held := "kept it held by " + ov.Item.ClaimedBy + " (heartbeat) so it is not re-claimed; retrying the escalation every pass"
		if hbErr != nil {
			held = fmt.Sprintf("and could not keep it held (heartbeat: %v): it may be re-claimed once its lease lapses", hbErr)
		}
		r.log.Printf("officers: %s: HQ refused the wall-time cap's escalation of item %s: %v; %s", o.label(), ov.Item.ID, err, held)
	}
	return errs
}

// forgetCapRefusal drops capped item id's refused escalation, if any, and
// ends its episode.
func (r *Reconciler) forgetCapRefusal(o Officer, id ItemID) {
	r.mu.Lock()
	s := r.st(o.ID)
	c, had := s.capRefused[id]
	delete(s.capRefused, id)
	r.mu.Unlock()
	if had {
		r.publish()
		r.endCapHold(c)
	}
}

// endCapHold ends c's episode: logged, and told to Config.CapHold.
func (r *Reconciler) endCapHold(c refusedCap) {
	r.log.Printf("officers: %s: instance %s is no longer withheld: capped item %s's escalation landed or the item is no longer held over its cap",
		c.pool.label(), c.pool.InstanceLabel(c.hold.N), c.hold.Item)
	if r.capHold != nil {
		h := c.hold
		h.Ended = true
		r.capHold(c.pool, h)
	}
}

// hqStatus is the HQ status of a refusal (an *HQError), else 0.
func hqStatus(err error) int {
	var he *HQError
	if errors.As(err, &he) {
		return he.Status
	}
	return 0
}

// overrunHoldSeconds is the lease a refused cap escalation's item is kept
// held for: three polls, at least the default lease, at most HQ's maximum.
func overrunHoldSeconds(o Officer) int {
	return ClampLease(max(DefaultLeaseSeconds, int(3*o.PollInterval/time.Second)))
}

// OverrunInvestigation is the investigation block of a claim stopped at the
// cap: low confidence (operator steps only), nothing concluded, re-investigate.
func OverrunInvestigation(o Officer) Investigation {
	n := int(o.ItemCap() / time.Minute)
	return Investigation{
		Hypotheses: []string{fmt.Sprintf("Not reached: the instance was stopped at the wall-time cap of %d minutes before it concluded.", n)},
		Eliminated: []InvestigationRule{},
		RootCause:  fmt.Sprintf("Not reached: stopped by muster at the wall-time cap of %d minutes (tiers.judgement.max_item_minutes); no conclusion.", n),
		Confidence: ConfidenceLow,
		Fix: InvestigationFix{OperatorSteps: []string{
			fmt.Sprintf("Re-investigate this item: the investigation hit the %d-minute wall-time cap and reached no conclusion.", n),
		}},
		NotChecked: []string{},
	}
}

// OverrunSummary is the escalation summary of a claim stopped at the cap.
func OverrunSummary(o Officer, ov Overrun) string {
	return fmt.Sprintf("Stopped by muster at the wall-time cap: instance %s held this item for %s without completing it "+
		"(tiers.judgement.max_item_minutes = %d). The work did not finish and nothing was concluded; a human should pick it up. "+
		"The instance's transcript, if forwarded, shows where it stopped.",
		ov.Item.ClaimedBy, ov.Held.Round(time.Minute), int(o.ItemCap()/time.Minute))
}

func (r *Reconciler) prompted(o Officer, n int) {
	r.mu.Lock()
	r.st(o.ID).promptedAt[o.InstanceName(n)] = r.now()
	r.mu.Unlock()
}

// RunTick is how often Run looks for a due officer.
var RunTick = time.Second

// passTimeout bounds one pass.
const passTimeout = 2 * time.Minute

// Done is closed once Run has returned: the shutdown drain joins the loop on it
// before waiting for re-prompt sends, so no pass can start one meanwhile.
func (r *Reconciler) Done() <-chan struct{} { return r.done }

// Run drives the loop until ctx ends. Call it at most once.
func (r *Reconciler) Run(ctx context.Context) {
	defer close(r.done)
	t := time.NewTicker(RunTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := r.now()
			for _, o := range r.reg.All() {
				r.mu.Lock()
				due := !now.Before(r.st(o.ID).nextAt)
				r.mu.Unlock()
				if due {
					r.pass(ctx, o.ID, false)
				}
			}
		}
	}
}

func (r *Reconciler) pass(ctx context.Context, id string, force bool) {
	pctx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Printf("officers: PANIC in a pass for %s: %v", id, rec)
		}
	}()
	r.Check(pctx, id, force)
}

// FaultStrikes is how many consecutive passes must see a permanent HQ refusal
// before it is treated as a configuration fault (scale to zero).
const FaultStrikes = 2

// ErrInstanceBusy is a re-prompt the fleet did not send because a turn holds
// the instance's session (its session claim is taken). It is not a failure and
// not a prompt: the loop tries again on its next pass.
var ErrInstanceBusy = errors.New("officers: a turn holds the instance's session")

// RepromptAfter is the least time between two prompts of one instance while it
// has claimable work and holds no claim.
const RepromptAfter = 5 * time.Minute

// published is what publish last set on the two gauges, for ErrorSlots and
// HQFaults (the gauges themselves are write-only from here).
var published struct {
	sync.Mutex
	slots, faults float64
}

// ErrorSlots is muster_officer_error_slots' last published value.
func ErrorSlots() float64 { published.Lock(); defer published.Unlock(); return published.slots }

// HQFaults is muster_officer_hq_faults' last published value.
func HQFaults() float64 { published.Lock(); defer published.Unlock(); return published.faults }

// RepromptText is the turn an up, idle instance is given when its officer has
// claimable work. Like KickoffText it carries no credential.
func RepromptText(o Officer, n int) string {
	return fmt.Sprintf("New work for officer %s (you are instance %s, %s): call %s and work the queue as before — %s, %s while you work, %s when done. "+
		"Call %s again only if you need your charter.", o.ID, o.InstanceLabel(n), o.InstanceName(n), ToolItems, ToolClaim, ToolHeartbeat, ToolComplete, ToolBrief)
}

// permanentRefusal names an HQ answer that is a configuration fault rather
// than an outage: 401/403 (the officer token is revoked, wrong or not this
// officer's) and 404 (HQ without the officer API, or no such officer), as the
// status ("404"). "" for anything else.
func permanentRefusal(err error) string {
	var hq *HQError
	if !errors.As(err, &hq) {
		return ""
	}
	switch hq.Status {
	case 401, 403, 404:
		return strconv.Itoa(hq.Status)
	}
	return ""
}
