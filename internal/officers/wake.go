package officers

import (
	"context"
	"errors"
	"log"
	"sort"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// THE WAKE LOOP
//
// For each declared officer, every PollInterval (or at once, on a webhook
// nudge), one pass:
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
	// not up.
	Up bool
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
	// Start and Stop are instance numbers, ascending.
	Start []int
	Stop  []int
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
//     contract makes those claimable by anyone).
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
func Decide(o Officer, obs Observation, idleSince, now time.Time) Decision {
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
	d.Claimable = len(obs.Open)
	heldByUp := map[string]bool{}
	for _, it := range obs.Claimed {
		if !it.LeaseLive(now) {
			d.Claimable++
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
}

// Reconciler runs the wake loop.
type Reconciler struct {
	reg   *Registry
	hq    HQ
	fleet Fleet
	log   *log.Logger
	now   func() time.Time

	mu    sync.Mutex
	state map[string]*officerState
	nudge chan string
}

// Config is NewReconciler's input.
type Config struct {
	Registry *Registry
	HQ       HQ
	Fleet    Fleet
	Logger   *log.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
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
	return &Reconciler{reg: cfg.Registry, hq: cfg.HQ, fleet: cfg.Fleet, log: cfg.Logger, now: cfg.Now,
		state: map[string]*officerState{}, nudge: make(chan string, 64)}, nil
}

func (r *Reconciler) st(id string) *officerState {
	s, ok := r.state[id]
	if !ok {
		s = &officerState{}
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
		r.mu.Unlock()
		r.log.Printf("officers: %s: %v; changing nothing, next check in %s (failure %d)", o.ID, err, next.Sub(now).Round(time.Second), f)
		return PassResult{Err: err}
	}
	d := Decide(o, obs, idleSince, now)
	r.mu.Lock()
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
	return PassResult{Decision: d, ActErrs: r.act(ctx, o, d)}
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
		r.log.Printf("officers: %s: started instance %d (%d claimable, %d held)", o.ID, n, d.Claimable, d.Held)
	}
	for _, n := range d.Stop {
		if err := r.fleet.Stop(ctx, o, n); err != nil {
			errs = append(errs, err)
			r.log.Printf("officers: %s: scale instance %d to zero: %v", o.ID, n, err)
			continue
		}
		r.log.Printf("officers: %s: scaled instance %d to zero (idle %s)", o.ID, n, o.IdleAfter)
	}
	return errs
}

// Nudge asks for a pass for officer id as soon as possible (the webhook
// path). It never blocks; a full queue drops the nudge, and the poll still
// comes.
func (r *Reconciler) Nudge(id string) {
	select {
	case r.nudge <- id:
	default:
	}
}

// RunTick is how often Run looks for a due officer.
var RunTick = time.Second

// passTimeout bounds one pass.
const passTimeout = 2 * time.Minute

// Run drives the loop until ctx ends.
func (r *Reconciler) Run(ctx context.Context) {
	t := time.NewTicker(RunTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-r.nudge:
			r.pass(ctx, id, false)
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
