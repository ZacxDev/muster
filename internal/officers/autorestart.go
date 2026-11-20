package officers

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/ZacxDev/muster/internal/metrics"
)

// ---------------------------------------------------------------------------
// BOUNDED AUTO-RESTART OF AN INSTANCE IN ERROR
//
// An instance in error holds its slot (instanceUp in internal/api counts it
// up): counting it down would have the loop re-Start it every poll — a
// restart storm that re-sends its first turn each time, and spends a pod cold
// start and a Claude session on every poll for an instance that fails the
// same way. That protection is kept. Without anything past it, though, the
// slot was held until an operator noticed and stopped and started the
// instance by hand: an instance sat in error for about 14 hours with an
// urgent item unclaimed, and at the default max_instances 1 the whole
// officer was down for all of it.
//
// So the loop restarts (Stop, then Start, the same as an operator's Stop and
// Start) an instance that has been in error for a while, BOUNDED three ways:
//
//   - 🔴 ONLY A RESTARTABLE ERROR CLASS (ErrorClass.Restartable): the fleet
//     names why the instance is in error, and a class that a restart cannot
//     fix — a bad configuration, a credential problem, a repository grant
//     nobody approved, or a cause muster does not recognise — is held as
//     before. An unrecognised cause is held: a new error writer defaults to
//     the old behaviour, never to a restart loop.
//   - 🔴 EXPONENTIAL BACKOFF PER INSTANCE: its first restart of the UTC day
//     after After in error, the next after 2×After in its next error, then
//     4×, and so on (AutoRestart.Delay), the clock starting when this process
//     first saw it in error, or at its last restart.
//   - 🔴 A DAILY CAP PER OFFICER (MaxPerDay restarts per UTC day across all
//     of its pools): past it the instance is held as before, and the owner is
//     told once per error episode (AutoRestartHeld).
//
// And only when a restart has something to do: queue work its pool could
// claim, a chat waiting for it, or a live claim it holds itself. An officer
// over its daily budget has none of the first (Decide counts no such item
// claimable), so a budget refusal never restarts anything. A restart is never
// made for an instance number holding a capped claim (Decide never starts
// one) or one stopping this pass.
//
// 🔴 THE SLOT IS THE SAME SLOT. A restart stops instance n and starts
// instance n: the number of instances never moves, so max_instances cannot be
// exceeded by it, and Decide (which counted n up) started no other number in
// its place. A live claim n held is released after the stop and before the
// start, so the item goes back to the queue for one claim — never held by a
// stopped instance's name while another claims it.
//
// ⚠ THE COUNTS ARE THIS PROCESS'S. They live in memory, like the budget
// notice's day: a restarted muster starts the day's count and backoff again.
// That can allow more restarts in a day than MaxPerDay across muster
// restarts, never a restart storm: each is still after After in error.
// ---------------------------------------------------------------------------

// ErrorClass is why an instance is in error, as the fleet classifies it
// (internal/api classifyInstanceError). The zero value is ErrorUnknown.
type ErrorClass string

const (
	// ErrorUnknown: a cause muster does not recognise (or none recorded).
	// Held: a new error writer must not default to a restart loop.
	ErrorUnknown ErrorClass = ""
	// ErrorKickoffStuck: its first turn never landed — the instance dwelt
	// past the kickoff deliverer's bound without a ready session taking it.
	// Nothing was sent, so a restart cannot pay for a turn twice.
	ErrorKickoffStuck ErrorClass = "kickoff_stuck"
	// ErrorCrashLoop: the live instance is crash-looping or its pod failed.
	ErrorCrashLoop ErrorClass = "crash_loop"
	// ErrorApplyFailed: a write to the backend (apply, scale, create, a
	// store write beside them) failed — the transient half of a start.
	ErrorApplyFailed ErrorClass = "apply_failed"
	// ErrorConfig: a configuration a restart re-reads unchanged — the spec
	// could not be built, the image cannot be pulled, an instance of that
	// name already exists differently, the deployment cannot deliver a first
	// turn. Held.
	ErrorConfig ErrorClass = "config"
	// ErrorAuth: a credential problem — the account's credential could not
	// be read or a token minted, or the model account rejected it
	// (auth_failed) or its billing (billing_error). Held: a new credential
	// is a human's step.
	ErrorAuth ErrorClass = "auth"
	// ErrorCheckoutWait: its first turn waited for a repository checkout
	// that never arrived (a grant nobody approved, a clone that never
	// finished). Held: the approval is a human's.
	ErrorCheckoutWait ErrorClass = "checkout_wait"
)

// Restartable reports whether a restart can fix an instance in error of
// class c. THE one predicate: the loop reads nothing else.
func (c ErrorClass) Restartable() bool {
	switch c {
	case ErrorKickoffStuck, ErrorCrashLoop, ErrorApplyFailed:
		return true
	}
	return false
}

// String names the class in a log line ("unknown" for the zero value).
func (c ErrorClass) String() string {
	if c == ErrorUnknown {
		return "unknown"
	}
	return string(c)
}

// AutoRestart is the loop's restart policy (Config.AutoRestart). The zero
// value is off: an instance in error holds its slot until an operator acts.
type AutoRestart struct {
	// After is how long an instance is in error before its first restart of
	// the UTC day; each later restart that day waits twice the one before.
	// 0 is off.
	After time.Duration
	// MaxPerDay is how many restarts one officer (all its pools) may have
	// per UTC day; past it an instance in error is held. 0 is off.
	MaxPerDay int
}

// Defaults for AutoRestart (cmd/muster-server: MUSTER_OFFICER_AUTO_RESTART_*).
const (
	DefaultAutoRestartAfter     = 5 * time.Minute
	DefaultAutoRestartMaxPerDay = 4
	// MaxAutoRestartDelay bounds Delay: one restart's wait is never longer
	// than a day.
	MaxAutoRestartDelay = 24 * time.Hour
)

// On reports whether the policy restarts anything.
func (a AutoRestart) On() bool { return a.After > 0 && a.MaxPerDay > 0 }

// Delay is how long an instance must have been in error before its restart
// when it was already restarted prior times today: After doubled per prior
// restart, at most MaxAutoRestartDelay.
func (a AutoRestart) Delay(prior int) time.Duration {
	d := a.After
	for i := 0; i < prior && d < MaxAutoRestartDelay; i++ {
		d *= 2
	}
	if d > MaxAutoRestartDelay {
		d = MaxAutoRestartDelay
	}
	return d
}

// Restart is one auto-restart a pass decided (Decision.Restart).
type Restart struct {
	// N is the instance number restarted (stopped, then started).
	N int
	// Class is why it was in error.
	Class ErrorClass
	// InError is how long this process has seen it in error.
	InError time.Duration
	// Attempt is this restart's number among the officer's today (1-based),
	// of Max.
	Attempt, Max int
	// Release is the live claims it held: released after its stop, before
	// its start.
	Release []Item
}

// AutoRestartEvent is what Config.AutoRestartNotice is told.
type AutoRestartEvent struct {
	// Instance is the instance's label (Officer.InstanceLabel) and Name its
	// agent name.
	Instance, Name string
	Class          ErrorClass
	InError        time.Duration
	// Restarted: the instance was stopped and started (Attempt of Max).
	Restarted    bool
	Attempt, Max int
	// Held: it would be restarted now but is not (HeldWhy): its class is not
	// restartable, or the day's cap is spent. Told once per error episode.
	Held    bool
	HeldWhy string
	// Recovered: an instance a Held event was told of left error; the
	// notice can be withdrawn.
	Recovered bool
}

// restartBook is the little the loop remembers for restarts, per officer.
type restartBook struct {
	// day is the UTC day count and perInstance are for.
	day string
	// count is the officer's restarts on day.
	count int
	// perInstance is each instance's restarts on day, by agent name (the
	// backoff's exponent).
	perInstance map[string]int
}

// roll starts a new day's book when now is past day.
func (b *restartBook) roll(now time.Time) {
	if d := utcDay(now); b.day != d {
		b.day, b.count, b.perInstance = d, 0, map[string]int{}
	}
}

// restartWork reports whether restarting instance in of pool o has something
// to do: queue work its pool could claim (it is a queue instance), a chat
// waiting for a lane it could give (within the chat ceiling), or a live claim
// it holds itself.
func restartWork(o Officer, d Decision, in InstanceState, held []Item) bool {
	switch {
	case len(held) > 0:
		return true
	case in.N <= o.MaxInstances && d.Claimable > 0:
		return true
	case d.ChatPending > 0 && o.RunsChatInstances() && in.N <= o.ChatMaxInstances():
		return true
	}
	return false
}

// heldClaims is the live queue claims instance in holds in obs at now that
// are not over the pool's wall-time cap (a capped holder is never restarted:
// Decide never starts its number).
func heldClaims(o Officer, obs Observation, in InstanceState, now time.Time) []Item {
	var out []Item
	limit := o.ItemCap()
	for _, it := range obs.Claimed {
		if (obs.Chats && it.IsChat()) || !it.LeaseLive(now) || it.ClaimedBy != in.Name || cappedClaim(limit, it, now) {
			continue
		}
		out = append(out, it)
	}
	return out
}

// planRestarts is the restart half of a pass for pool o, under r.mu: which of
// its instances in error to restart now (they are added to d.Restart), and
// which would be but are held (returned, to be told once per episode). The
// book is charged here, before the act: a restart whose stop or start then
// fails has still spent its place, so a failing restart is bounded too.
func (r *Reconciler) planRestarts(s *officerState, o Officer, obs Observation, d *Decision, now time.Time) []AutoRestartEvent {
	p := r.autoRestart
	if !p.On() {
		return nil
	}
	s.restarts.roll(now)
	capHolder := cappedHolders(o, obs, now)
	stopping := map[int]bool{}
	for _, n := range d.Stop {
		stopping[n] = true
	}
	for _, ov := range d.Overrun {
		stopping[ov.N] = true
	}
	var held []AutoRestartEvent
	insts := append([]InstanceState(nil), obs.Instances...)
	sort.Slice(insts, func(i, j int) bool { return insts[i].N < insts[j].N })
	for _, in := range insts {
		e, ok := s.errored[in.Name]
		if !in.Error || !ok || capHolder[in.N] || stopping[in.N] {
			continue
		}
		claims := heldClaims(o, obs, in, now)
		if !restartWork(o, *d, in, claims) {
			continue
		}
		inError := now.Sub(e.since)
		prior := s.restarts.perInstance[in.Name]
		if inError < p.Delay(prior) {
			continue
		}
		why := ""
		switch {
		case !in.ErrorClass.Restartable():
			why = fmt.Sprintf("its error (%s) is not one a restart fixes", in.ErrorClass)
		case s.restarts.count >= p.MaxPerDay:
			why = fmt.Sprintf("the officer's %d auto-restarts for today (UTC) are spent", p.MaxPerDay)
		}
		if why != "" {
			if !e.heldTold {
				e.heldTold = true
				s.errored[in.Name] = e
				held = append(held, AutoRestartEvent{Instance: o.InstanceLabel(in.N), Name: in.Name, Class: in.ErrorClass, InError: inError,
					Held: true, HeldWhy: why, Max: p.MaxPerDay})
			}
			continue
		}
		s.restarts.count++
		s.restarts.perInstance[in.Name] = prior + 1
		// Its next error is timed from this restart: one that is back in error
		// before any pass saw it up is not "in error since before the restart".
		e.since = now
		s.errored[in.Name] = e
		d.Restart = append(d.Restart, Restart{N: in.N, Class: in.ErrorClass, InError: inError, Attempt: s.restarts.count, Max: p.MaxPerDay, Release: claims})
	}
	return held
}

// restart acts on one planned restart of pool o: stop the instance, release
// what it held, start it with its first turn. A failed stop does neither of
// the rest (the instance may still be running its claim).
func (r *Reconciler) restart(ctx context.Context, o Officer, rs Restart) []error {
	label := o.InstanceLabel(rs.N)
	if err := r.fleet.Stop(ctx, o, rs.N); err != nil {
		r.log.Printf("officers: %s: auto-restart of instance %s: stop: %v (not started; auto-restart %d of %d today)", o.label(), label, err, rs.Attempt, rs.Max)
		return []error{err}
	}
	var errs []error
	for _, it := range rs.Release {
		if err := r.hq.Release(ctx, o, it.ID, it.ClaimedBy); err != nil {
			errs = append(errs, err)
			r.log.Printf("officers: %s: auto-restart of instance %s: release item %s: %v", o.label(), label, it.ID, err)
			continue
		}
		r.log.Printf("officers: %s: released item %s: its holder %s is being restarted from error", o.label(), it.ID, it.ClaimedBy)
	}
	if err := r.fleet.Start(ctx, o, rs.N, r.kickoff(ctx, o, rs.N)); err != nil {
		r.log.Printf("officers: %s: auto-restart of instance %s: start: %v (it is stopped; auto-restart %d of %d today)", o.label(), label, err, rs.Attempt, rs.Max)
		return append(errs, err)
	}
	r.prompted(o, rs.N)
	metrics.OfficerAutoRestarts.Inc()
	r.log.Printf("officers: %s: AUTO-RESTARTED instance %s from error (%s, in error %s): auto-restart %d of %d today (UTC)",
		o.label(), label, rs.Class, rs.InError.Round(time.Second), rs.Attempt, rs.Max)
	if r.restartNotice != nil {
		r.restartNotice(o, AutoRestartEvent{Instance: label, Name: o.InstanceName(rs.N), Class: rs.Class, InError: rs.InError,
			Restarted: true, Attempt: rs.Attempt, Max: rs.Max})
	}
	return errs
}

// tellHeld logs and tells each held restart of pool o (planRestarts' answer),
// outside the loop's lock.
func (r *Reconciler) tellHeld(o Officer, held []AutoRestartEvent) {
	for _, h := range held {
		metrics.OfficerAutoRestartsHeld.Inc()
		r.log.Printf("officers: %s: instance %s is in error (%s) for %s with work waiting and is NOT auto-restarted: %s — it holds its slot until an operator stops and starts it",
			o.label(), h.Instance, h.Class, h.InError.Round(time.Second), h.HeldWhy)
		if r.restartNotice != nil {
			r.restartNotice(o, h)
		}
	}
}
