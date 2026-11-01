package officers

import (
	"context"
	"errors"
	"fmt"
	"log"
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

// Observation is what one pass read.
type Observation struct {
	Open      []Item
	Claimed   []Item
	Instances []InstanceState
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
	// Idle reports the officer idle at this pass: nothing claimable, no live
	// claim held by any of its instances.
	Idle bool
	// Claimable is how many items an instance could claim now.
	Claimable int
	// Held is how many live claims its instances hold.
	Held int
}

// Decide is the loop's whole policy, as a pure function.
//
//   - Claimable = open items, plus claimed items whose lease has expired (the
//     contract makes those claimable by anyone) — less any item HQ marks
//     Blocked: its claim is refused, so it is not work, and an instance woken
//     or kept up for it would claim nothing.
//   - Held = claimed items with a live lease whose holder is one of this
//     officer's instances.
//   - Busy (Claimable > 0 or Held > 0): want min(MaxInstances, held-by-up +
//     Claimable) instances up; start the lowest-numbered down instances to
//     reach it. Never stop anything while busy.
//   - Idle (neither): once idle has lasted IdleAfter (now - idleSince), stop
//     every up instance. idleSince zero means "idle only now", so nothing is
//     stopped on the first idle pass.
//   - A live claim held by an instance that is NOT up is released, busy or
//     idle: its holder cannot heartbeat it, and releasing returns the item to
//     the queue now rather than when the lease lapses.
func Decide(o Officer, obs Observation, idleSince, now time.Time, promptedAt map[int]time.Time) Decision {
	byName := map[string]InstanceState{}
	for _, in := range obs.Instances {
		byName[in.Name] = in
	}
	ours := func(holder string) (InstanceState, bool) {
		if _, n, ok := (&Registry{list: []Officer{o}}).ForAgentName(holder); ok {
			if in, ok := byName[holder]; ok {
				return in, true
			}
			return InstanceState{N: n, Name: holder}, true
		}
		return InstanceState{}, false
	}
	var d Decision
	for _, it := range obs.Open {
		if !it.Blocked {
			d.Claimable++
		}
	}
	heldByUp := map[string]bool{}
	for _, it := range obs.Claimed {
		if !it.LeaseLive(now) {
			if !it.Blocked {
				d.Claimable++
			}
			continue
		}
		in, mine := ours(it.ClaimedBy)
		if !mine {
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
		down := downInstances(o, obs.Instances)
		for i := 0; up+len(d.Start) < want && i < len(down); i++ {
			d.Start = append(d.Start, down[i])
		}
		d.Reprompt = reprompts(obs.Instances, heldByUp, d.Claimable-len(d.Start), promptedAt, now)
		return d
	}
	d.Idle = true
	if up > 0 && !idleSince.IsZero() && now.Sub(idleSince) >= o.IdleAfter {
		for _, in := range obs.Instances {
			if in.Up {
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
	idleSince time.Time
	failures  int
	nextAt    time.Time
	// promptedAt is when each instance was last started or re-prompted.
	promptedAt map[int]time.Time
	// errored is the instances last seen in error (logged on change).
	errored map[int]bool
	// fault is the CONFIRMED permanent HQ refusal (the status, e.g. "404"; ""
	// = none). It is set once a refusal has been seen on FaultStrikes
	// consecutive passes and cleared ONLY by a successful answer — a 5xx in
	// between leaves it, so the gauge and the "answers again" line stay true.
	fault string
	// strikes counts consecutive passes that saw a permanent refusal; a
	// successful answer and an outage (5xx, network) both reset it.
	strikes int
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
		state: map[string]*officerState{}, done: make(chan struct{})}, nil
}

func (r *Reconciler) st(id string) *officerState {
	s, ok := r.state[id]
	if !ok {
		s = &officerState{promptedAt: map[int]time.Time{}, errored: map[int]bool{}}
		r.state[id] = s
	}
	return s
}

// PassResult is one pass's outcome, for logs and tests.
type PassResult struct {
	Decision Decision
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
	idleSince := s.idleSince
	r.mu.Unlock()

	obs, err := r.observe(ctx, o)
	if err != nil {
		r.mu.Lock()
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
	r.noteErrors(o, obs.Instances)
	if r.observed != nil {
		r.observed(o, obs.Claimed, now)
	}
	r.mu.Lock()
	prompted := make(map[int]time.Time, len(s.promptedAt))
	for n, at := range s.promptedAt {
		prompted[n] = at
	}
	r.mu.Unlock()
	d := Decide(o, obs, idleSince, now, prompted)
	r.mu.Lock()
	if s.fault != "" {
		r.log.Printf("officers: %s: HQ answers again (was a configuration fault: HQ answered %s)", o.ID, s.fault)
		s.fault = ""
	}
	s.strikes = 0
	s.failures = 0
	s.nextAt = now.Add(o.PollInterval)
	switch {
	case !d.Idle:
		s.idleSince = time.Time{}
	case s.idleSince.IsZero():
		s.idleSince = now
	}
	if len(d.Stop) > 0 {
		// The officer is going to zero; a later wake starts a fresh window.
		s.idleSince = time.Time{}
	}
	r.mu.Unlock()
	r.publish()
	return PassResult{Decision: d, ActErrs: r.act(ctx, o, d)}
}

// scaleDownForFault stops every up instance of o (HQ refuses it permanently).
// A fleet that cannot be seen changes nothing, as everywhere else.
func (r *Reconciler) scaleDownForFault(ctx context.Context, o Officer, cause error, status string) PassResult {
	insts, err := r.fleet.Instances(ctx, o)
	if err != nil {
		return PassResult{Err: cause}
	}
	d := Decision{StopReason: "HQ configuration fault: " + status}
	for _, in := range insts {
		if in.Up {
			d.Stop = append(d.Stop, in.N)
		}
	}
	sort.Ints(d.Stop)
	return PassResult{Err: cause, Decision: d, ActErrs: r.act(ctx, o, d)}
}

// noteErrors logs each instance entering or leaving error, once per change.
func (r *Reconciler) noteErrors(o Officer, insts []InstanceState) {
	r.mu.Lock()
	s := r.st(o.ID)
	var entered, left []int
	now := map[int]bool{}
	for _, in := range insts {
		if in.Error {
			now[in.N] = true
			if !s.errored[in.N] {
				entered = append(entered, in.N)
			}
		}
	}
	for n := range s.errored {
		if !now[n] {
			left = append(left, n)
		}
	}
	s.errored = now
	r.mu.Unlock()
	sort.Ints(entered)
	sort.Ints(left)
	for _, n := range entered {
		r.log.Printf("officers: %s: instance %d is in error — it holds its slot and is NOT restarted, so the officer runs below target "+
			"until it recovers or an operator acts (agent %s)", o.ID, n, InstanceName(o.ID, n))
	}
	for _, n := range left {
		r.log.Printf("officers: %s: instance %d left error", o.ID, n)
	}
	r.publish()
}

// publish sets the officer gauges from every officer's remembered state.
func (r *Reconciler) publish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	slots, faults := 0, 0
	for _, s := range r.state {
		slots += len(s.errored)
		if s.fault != "" {
			faults++
		}
	}
	metrics.OfficerErrorSlots.Set(float64(slots))
	metrics.OfficerHQFaults.Set(float64(faults))
	published.Lock()
	published.slots, published.faults = float64(slots), float64(faults)
	published.Unlock()
}

func (r *Reconciler) observe(ctx context.Context, o Officer) (Observation, error) {
	open, err := r.hq.AllItems(ctx, o, StatusOpen)
	if err != nil {
		return Observation{}, err
	}
	claimed, err := r.hq.AllItems(ctx, o, StatusClaimed)
	if err != nil {
		return Observation{}, err
	}
	insts, err := r.fleet.Instances(ctx, o)
	if err != nil {
		return Observation{}, err
	}
	return Observation{Open: open, Claimed: claimed, Instances: insts}, nil
}

func (r *Reconciler) act(ctx context.Context, o Officer, d Decision) []error {
	var errs []error
	for _, it := range d.Release {
		if err := r.hq.Release(ctx, o, it.ID, it.ClaimedBy); err != nil {
			errs = append(errs, err)
			r.log.Printf("officers: %s: release item %s held by stopped instance %s: %v", o.ID, it.ID, it.ClaimedBy, err)
			continue
		}
		r.log.Printf("officers: %s: released item %s: its holder %s is not running", o.ID, it.ID, it.ClaimedBy)
	}
	for _, n := range d.Start {
		if err := r.fleet.Start(ctx, o, n, KickoffText(o, n)); err != nil {
			errs = append(errs, err)
			r.log.Printf("officers: %s: start instance %d: %v", o.ID, n, err)
			continue
		}
		r.prompted(o, n)
		r.log.Printf("officers: %s: started instance %d (%d claimable, %d held)", o.ID, n, d.Claimable, d.Held)
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
			r.log.Printf("officers: %s: re-prompt instance %d: %v", o.ID, n, err)
			continue
		}
		r.prompted(o, n)
		r.log.Printf("officers: %s: re-prompted idle instance %d for new work (%d claimable)", o.ID, n, d.Claimable)
	}
	for _, n := range d.Stop {
		if err := r.fleet.Stop(ctx, o, n); err != nil {
			errs = append(errs, err)
			r.log.Printf("officers: %s: scale instance %d to zero: %v", o.ID, n, err)
			continue
		}
		reason := d.StopReason
		if reason == "" {
			reason = "idle " + o.IdleAfter.String()
		}
		r.log.Printf("officers: %s: scaled instance %d to zero (%s)", o.ID, n, reason)
	}
	return errs
}

func (r *Reconciler) prompted(o Officer, n int) {
	r.mu.Lock()
	r.st(o.ID).promptedAt[n] = r.now()
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
	return fmt.Sprintf("New work for officer %s (you are instance %d, %s): call %s and work the queue as before — %s, %s while you work, %s when done. "+
		"Call %s again only if you need your charter.", o.ID, n, InstanceName(o.ID, n), ToolItems, ToolClaim, ToolHeartbeat, ToolComplete, ToolBrief)
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
