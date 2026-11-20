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
// Start) an instance that has been in error for a while, BOUNDED three ways
// (and persisted, so a rollout does not reset them: RestartStore):
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
// 🔴 THE COUNTS OUTLIVE THE PROCESS (Config.Restarts: officer_auto_restarts,
// migration 0161). A muster restarted mid-day reads the day's restarts per
// instance — the cap's sum, each backoff's exponent — and which held notices
// were told, so a rollout neither resets the cap nor re-pushes a notice. Only
// the clock of an error ALREADY in progress starts again when a new process
// first sees it, which can only delay a restart. A store that cannot be read
// restarts nothing that pass (fail closed). With no store (tests, a server
// without a database) the book lives in memory.
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
	// ErrorRateLimited: the model account answered rate_limited (its usage
	// limit). Held: a restart sends the next first turn into the same limit,
	// and muster does not know here when it resets.
	ErrorRateLimited ErrorClass = "rate_limited"
	// ErrorSidecar: the instance reads error because a container OTHER than
	// its agent's (a sidecar, a key holder) is failing, while the agent's own
	// is not. Held: a restart would end a live session over a container it
	// does not run in.
	ErrorSidecar ErrorClass = "sidecar"
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
	// Class is why it was in error, and ErrorText the error recorded for it
	// (InstanceState.ErrorText).
	Class     ErrorClass
	ErrorText string
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
	// ErrorText is the error recorded for it (InstanceState.ErrorText).
	ErrorText string
	InError   time.Duration
	// Restarted: the instance was stopped and started (Attempt of Max).
	Restarted    bool
	Attempt, Max int
	// Held: it would be restarted now but is not (HeldWhy): its class is not
	// restartable, or the day's cap is spent. Told once per error episode
	// (per UTC day: the record is the day's).
	Held    bool
	HeldWhy string
	// Recovered: an instance a Held event was told of left error; the
	// notice can be withdrawn.
	Recovered bool
}

// RestartRecord is one instance's auto-restart day in a RestartStore.
type RestartRecord struct {
	Restarts      int
	LastRestartAt time.Time
	HeldTold      bool
	LastError     string
}

// MaxRestartErrorText bounds the error text a RestartStore keeps (migration
// 0161's CHECK is 2,000 characters).
const MaxRestartErrorText = 1000

// RestartStore persists the auto-restart book (officer_auto_restarts,
// PGRestartStore), per officer id, UTC day ("2006-01-02") and instance agent
// name. ctx is Config.RestartContext's: it acts as the officer's owner.
type RestartStore interface {
	// RestartDay is the officer's rows for day, by instance (none: empty).
	RestartDay(ctx context.Context, officer, day string) (map[string]RestartRecord, error)
	// RecordRestart adds one restart of instance at at, with its error text.
	RecordRestart(ctx context.Context, officer, day, instance string, at time.Time, errText string) error
	// SetRestartHeld sets (held) or clears whether the instance's held
	// notice was told, keeping errText when held.
	SetRestartHeld(ctx context.Context, officer, day, instance string, held bool, errText string) error
}

// restartBook is the auto-restart record the loop keeps per officer: the
// store's rows for day (or, with no store, its own).
type restartBook struct {
	// day is the UTC day the book is for; loaded whether the store's rows
	// for it were read (always true with no store).
	day    string
	loaded bool
	// unreadable: this pass's read of the store failed; nothing is
	// restarted or told held until a read succeeds.
	unreadable bool
	// perInstance is each instance's restarts on day, by agent name (the
	// backoff's exponent; their sum is the officer's count).
	perInstance map[string]int
	// held is the instances whose held notice was told on day, by agent name.
	held map[string]bool
}

// count is the officer's restarts on the book's day.
func (b *restartBook) count() int {
	n := 0
	for _, c := range b.perInstance {
		n += c
	}
	return n
}

// reset makes b an empty book for day — but for the held notices told, which
// are carried: an instance held across UTC midnight is not told again by
// this process, and its notice is still withdrawn when it leaves error.
func (b *restartBook) reset(day string) {
	held := b.held
	if held == nil {
		held = map[string]bool{}
	}
	b.day, b.loaded, b.unreadable, b.perInstance, b.held = day, false, false, map[string]int{}, held
}

// syncRestarts brings the officer's book up to date for this pass, outside
// the loop's lock: a new UTC day starts an empty book; with a store, the
// day's rows are read when the book was never read today, or when any of
// the pass's instances is in error (another replica, or a process before
// this one, may have restarted it since). A failed read marks the book
// unreadable (fail closed) and is logged.
func (r *Reconciler) syncRestarts(ctx context.Context, o Officer, obs []Observation, now time.Time) {
	if !r.autoRestart.On() {
		return
	}
	day := utcDay(now)
	r.mu.Lock()
	s := r.st(o.Key())
	if s.restarts.day != day {
		s.restarts.reset(day)
		s.restarts.loaded = r.restartStore == nil
	}
	need := !s.restarts.loaded
	r.mu.Unlock()
	if r.restartStore == nil {
		return
	}
	for _, ob := range obs {
		for _, in := range ob.Instances {
			need = need || in.Error
		}
	}
	if !need {
		return
	}
	rows, err := r.restartStore.RestartDay(r.restartCtx(ctx, o), o.ID, day)
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.restarts.day != day {
		return // a later pass rolled it meanwhile
	}
	if err != nil {
		if !s.restarts.unreadable {
			r.log.Printf("officers: %s: the auto-restart record cannot be read (%v): no instance in error is restarted until it can", o.ID, err)
		}
		s.restarts.unreadable = true
		return
	}
	s.restarts.unreadable, s.restarts.loaded = false, true
	// The day's rows, and every notice this process told (carried over
	// midnight, or written by a store write that failed).
	s.restarts.perInstance = map[string]int{}
	for name, rec := range rows {
		s.restarts.perInstance[name] = rec.Restarts
		if rec.HeldTold {
			s.restarts.held[name] = true
		}
	}
}

// restartCtx is ctx acting as o's owner for the store (Config.RestartContext).
func (r *Reconciler) restartCtx(ctx context.Context, o Officer) context.Context {
	if r.restartContext == nil {
		return ctx
	}
	return r.restartContext(ctx, o)
}

// restartWork reports whether restarting instance in of pool o has something
// to do: queue work its pool could claim (it is a queue instance), a chat
// waiting for a lane it could give (within the chat ceiling), or a live claim
// of its own that a fresh instance could claim again (held).
//
// The daily budget and the judgement session cap are already in
// d.Claimable (Decide counts no item they refuse), and held is filtered by
// them (heldClaims). A waiting chat is not subject to either: chats run only
// on an HQ officer's pods (Officer.RunsChatInstances), which has no daily
// budget, and a chat turn is not a judgement session.
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
// Decide never starts its number), split into those a restarted instance
// could claim again — within the daily budget (or bypassing it) and, under a
// spent judgement session cap, already counted today — and all of them (each
// is released by the restart).
func heldClaims(o Officer, obs Observation, in InstanceState, now time.Time) (claimable, all []Item) {
	limit := o.ItemCap()
	for _, it := range obs.Claimed {
		if (obs.Chats && it.IsChat()) || !it.LeaseLive(now) || it.ClaimedBy != in.Name || cappedClaim(limit, it, now) {
			continue
		}
		all = append(all, it)
		if (obs.OverBudget || obs.SpendUnreadable) && !o.BypassesBudget(it) {
			continue
		}
		if obs.Capped && !obs.Counted[it.ID] {
			continue
		}
		claimable = append(claimable, it)
	}
	return claimable, all
}

// restartPlan is planRestarts' answer: the held restarts to tell and the
// recovered held notices to withdraw, both outside the lock.
type restartPlan struct {
	held, recovered []AutoRestartEvent
}

// planRestarts is the restart half of a pass for pool o, under r.mu: which of
// its instances in error to restart now (they are added to d.Restart), which
// would be but are held (to be told once), and which held ones have left
// error (to be withdrawn). The book is charged here, before the act: a
// restart whose stop or start then fails has still spent its place, so a
// failing restart is bounded too.
func (r *Reconciler) planRestarts(s *officerState, o Officer, obs Observation, d *Decision, now time.Time) restartPlan {
	p := r.autoRestart
	var plan restartPlan
	if !p.On() || s.restarts.day != utcDay(now) {
		return plan
	}
	book := &s.restarts
	for _, in := range obs.Instances {
		if !in.Error && book.held[in.Name] {
			delete(book.held, in.Name)
			plan.recovered = append(plan.recovered, AutoRestartEvent{Instance: o.InstanceLabel(in.N), Name: in.Name, Recovered: true})
		}
	}
	if book.unreadable || !book.loaded {
		return plan
	}
	capHolder := cappedHolders(o, obs, now)
	stopping := map[int]bool{}
	for _, n := range d.Stop {
		stopping[n] = true
	}
	for _, ov := range d.Overrun {
		stopping[ov.N] = true
	}
	insts := append([]InstanceState(nil), obs.Instances...)
	sort.Slice(insts, func(i, j int) bool { return insts[i].N < insts[j].N })
	for _, in := range insts {
		e, ok := s.errored[in.Name]
		if !in.Error || !ok || capHolder[in.N] || stopping[in.N] {
			continue
		}
		claimable, claims := heldClaims(o, obs, in, now)
		if !restartWork(o, *d, in, claimable) {
			continue
		}
		inError := now.Sub(e.since)
		prior := book.perInstance[in.Name]
		if inError < p.Delay(prior) {
			continue
		}
		why := ""
		switch {
		case !in.ErrorClass.Restartable():
			why = fmt.Sprintf("its error (%s) is not one a restart fixes", in.ErrorClass)
		case book.count() >= p.MaxPerDay:
			why = fmt.Sprintf("the officer's %d auto-restarts for today (UTC) are spent", p.MaxPerDay)
		}
		if why != "" {
			if !book.held[in.Name] {
				book.held[in.Name] = true
				plan.held = append(plan.held, AutoRestartEvent{Instance: o.InstanceLabel(in.N), Name: in.Name, Class: in.ErrorClass,
					ErrorText: in.ErrorText, InError: inError, Held: true, HeldWhy: why, Max: p.MaxPerDay})
			}
			continue
		}
		book.perInstance[in.Name] = prior + 1
		// Its next error is timed from this restart: one that is back in error
		// before any pass saw it up is not "in error since before the restart".
		e.since = now
		s.errored[in.Name] = e
		d.Restart = append(d.Restart, Restart{N: in.N, Class: in.ErrorClass, ErrorText: in.ErrorText, InError: inError,
			Attempt: book.count(), Max: p.MaxPerDay, Release: claims})
	}
	return plan
}

// restart acts on one planned restart of pool o: record it, stop the
// instance, release what it held, start it with its first turn. A failed stop
// does neither of the rest (the instance may still be running its claim).
// The record is written first, so the attempt is counted even if muster stops
// mid-restart; a failed write is logged and the restart still made (the
// in-memory book was charged).
func (r *Reconciler) restart(ctx context.Context, o Officer, rs Restart) []error {
	label := o.InstanceLabel(rs.N)
	now := r.now()
	if r.restartStore != nil {
		if err := r.restartStore.RecordRestart(r.restartCtx(ctx, o), o.ID, utcDay(now), o.InstanceName(rs.N), now, truncateRunes(rs.ErrorText, MaxRestartErrorText)); err != nil {
			r.log.Printf("officers: %s: auto-restart of instance %s: record it: %v (restarting anyway; this process counted it)", o.label(), label, err)
		}
	}
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
	r.log.Printf("officers: %s: AUTO-RESTARTED instance %s from error (%s, in error %s): auto-restart %d of %d today (UTC); its error was: %s",
		o.label(), label, rs.Class, rs.InError.Round(time.Second), rs.Attempt, rs.Max, errorTextForLog(rs.ErrorText))
	if r.restartNotice != nil {
		r.restartNotice(o, AutoRestartEvent{Instance: label, Name: o.InstanceName(rs.N), Class: rs.Class, ErrorText: rs.ErrorText,
			InError: rs.InError, Restarted: true, Attempt: rs.Attempt, Max: rs.Max})
	}
	return errs
}

// tellRestarts logs, persists and tells pool o's held restarts and recovered
// held notices (planRestarts' answer), outside the loop's lock.
func (r *Reconciler) tellRestarts(ctx context.Context, o Officer, plan restartPlan) {
	day := utcDay(r.now())
	for _, h := range plan.held {
		metrics.OfficerAutoRestartsHeld.Inc()
		r.log.Printf("officers: %s: instance %s is in error (%s) for %s with work waiting and is NOT auto-restarted: %s — it holds its slot until an operator stops and starts it; its error: %s",
			o.label(), h.Instance, h.Class, h.InError.Round(time.Second), h.HeldWhy, errorTextForLog(h.ErrorText))
		if r.restartStore != nil {
			if err := r.restartStore.SetRestartHeld(r.restartCtx(ctx, o), o.ID, day, h.Name, true, truncateRunes(h.ErrorText, MaxRestartErrorText)); err != nil {
				r.log.Printf("officers: %s: record instance %s's held restart: %v", o.label(), h.Instance, err)
			}
		}
		if r.restartNotice != nil {
			r.restartNotice(o, h)
		}
	}
	for _, rc := range plan.recovered {
		if r.restartStore != nil {
			if err := r.restartStore.SetRestartHeld(r.restartCtx(ctx, o), o.ID, day, rc.Name, false, ""); err != nil {
				r.log.Printf("officers: %s: clear instance %s's held restart: %v", o.label(), rc.Instance, err)
			}
		}
		if r.restartNotice != nil {
			r.restartNotice(o, rc)
		}
	}
}

// maxLogErrorText bounds the error text in one log line.
const maxLogErrorText = 300

// errorTextForLog is t for a log line: "(none recorded)" when empty, else at
// most maxLogErrorText runes.
func errorTextForLog(t string) string {
	if t == "" {
		return "(none recorded)"
	}
	return truncateRunes(t, maxLogErrorText)
}

// truncateRunes is s cut to at most n runes, "…" marking a cut.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
