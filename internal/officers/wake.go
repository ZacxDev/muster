package officers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/provision"
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
	// N is the instance number, 1..Slots (Officer.Slots: max_instances, or
	// the chat ceiling past it).
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
	// holds its slot, so the loop logs and counts it rather than starting it
	// — until the bounded auto-restart (autorestart.go) restarts it, when
	// ErrorClass is one a restart fixes.
	Error bool
	// ErrorClass is why it is in error (meaningful only with Error):
	// ErrorUnknown when the fleet cannot say, which is never restarted.
	ErrorClass ErrorClass
	// Down is why the instance is not up, as the fleet sees it ("" when up,
	// or when the fleet cannot say): "no agent yet", "its window @23 on h1
	// is gone", "its last start was refused: …". The start line names it, so
	// every start — and every RESTART — says why it happened.
	Down string
	// PromptPending reports a prompt (a kickoff or a re-prompt) owed to the
	// instance and not yet delivered. Such an instance is not re-prompted.
	PromptPending bool
	// Spent, for an instance of a fresh_session_per_item pool: its current
	// conversation has claimed its one item (Officer.FreshSessionPerItem).
	// Held by no live claim, it is done with that item however the hold
	// ended, and is prompted (into a new conversation) without waiting out
	// RepromptAfter. Always false for every other pool.
	Spent bool
}

// Fleet is how the loop observes and moves an officer's instances.
// internal/api implements it over the agents store and the provisioner.
type Fleet interface {
	// Instances returns the state of instances 1..o.Slots() — chat-only
	// instances included, so one started for chats is seen up. An error
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

// HQ is the part of the officer-backend contract the loop uses, for any
// officer. Backends adapts a per-officer Backend factory to it.
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

// SwitchGated is a backend whose on/off switch (Backend.Enabled) the wake
// loop honours: while it answers false, the officer's instances are stopped
// and none is started, whatever its queue holds. The built-in backend is one —
// its switch (officer_defs.enabled) is the only "off" a builtin officer has.
// HQ is NOT: the loop has always read only an HQ officer's items, and an HQ
// officer's switch is HQ's to honour (its queue and its routes), so an HQ
// officer's passes make no extra request.
type SwitchGated interface {
	// SwitchGatesWake marks the backend; it does nothing.
	SwitchGatesWake()
}

// errSwitchedOff is observe's answer for an officer whose SwitchGated
// backend is switched off: not a failure, a pass that stops its instances.
var errSwitchedOff = errors.New("officers: the officer is switched off in its backend")

// SwitchedOff reports whether o's backend is SwitchGated and switched off.
// A backend that is not SwitchGated is never off (no request is made); an
// unreadable switch is an error, never "on".
func (h *Backends) SwitchedOff(ctx context.Context, o Officer) (bool, error) {
	b := h.Backend(o)
	if _, gated := b.(SwitchGated); !gated {
		return false, nil
	}
	on, err := b.Enabled(ctx)
	return !on && err == nil, err
}

// switchReader is the part of the loop's HQ that reads a SwitchGated
// backend's switch (Backends.SwitchedOff), asserted so that an HQ without it
// (every test fake) is never switched off.
type switchReader interface {
	SwitchedOff(ctx context.Context, o Officer) (bool, error)
}

// Backends is every officer's Backend, built once per officer and cached. It
// is THE way muster reaches an officer's backend: the wake loop (as HQ), the
// MCP tools, the ship gates and the transcript, usage and pools pushers all
// go through Backend(o).
//
// 🔴 CACHED BY THE OFFICER'S WHOLE IDENTITY — (owner, id, key) — NEVER BY ITS
// KEY ALONE. A backend holds the officer it was built from, and the built-in
// one runs every statement AS THAT OFFICER'S OWNER. A DB-declared officer's
// key is its instance slug, and a deleted officer's slug is not the cache's to
// know about: Forget on the roster swap cannot win against a pass that read
// the roster BEFORE the swap and calls Backend with the old officer after it
// (round 0's F1, reproduced by that interleaving). Keyed by slug alone, that
// late call re-cached the old owner's backend, and another owner's officer
// given the same slug then read the first owner's queue. Keyed by identity, a
// late call can only re-cache a backend for ITS OWN officer, and every get
// checks the entry it found was built for the officer asked about
// (TestABackendCachedForOneOwnerIsNeverServedToAnother).
type Backends struct {
	mu       sync.Mutex
	backends map[backendIdentity]Backend
	build    func(Officer) Backend
}

// backendIdentity is what a cached backend is for: the officer's owner, its
// id and its key (an env officer's id, a DB officer's slug). Two officers
// that differ in any of them never share a backend.
type backendIdentity struct {
	owner   int64
	id, key string
}

func identityOf(o Officer) backendIdentity {
	return backendIdentity{owner: o.Owner, id: o.ID, key: o.Key()}
}

// NewBackends returns the backends built by build. build nil talks to each
// officer's own HQ with its own token (NewClient(o, nil)).
func NewBackends(build func(Officer) Backend) *Backends {
	if build == nil {
		build = func(o Officer) Backend { return NewClient(o, nil) }
	}
	return &Backends{backends: map[backendIdentity]Backend{}, build: build}
}

// Backend returns the cached backend for o, keyed on o's identity (owner, id,
// key): two owners' DB-declared officers may share an id, and — one deleted,
// the other created — even a slug, never an identity. An entry whose backend
// says it acts for another officer (a builder that ignored its argument) is
// rebuilt, never served.
func (h *Backends) Backend(o Officer) Backend {
	id := identityOf(o)
	h.mu.Lock()
	defer h.mu.Unlock()
	b, ok := h.backends[id]
	if ok && identityOf(b.Officer()) == id {
		return b
	}
	b = h.build(o)
	h.backends[id] = b
	return b
}

// Forget drops every cached backend of an officer keyed key (whoever owns
// it), so the next Backend builds one from the officer as it is now: a
// DB-declared officer changed or deleted through the admin API (its backend
// holds the Officer it was built from).
func (h *Backends) Forget(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id := range h.backends {
		if id.key == key {
			delete(h.backends, id)
		}
	}
}

// Retain drops every cached backend whose officer is not in live, so an entry
// a late call re-cached for an officer that is gone does not outlive the next
// roster load.
func (h *Backends) Retain(live []Officer) {
	keep := make(map[backendIdentity]bool, len(live))
	for _, o := range live {
		keep[identityOf(o)] = true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for id := range h.backends {
		if !keep[id] {
			delete(h.backends, id)
		}
	}
}

// AllItems implements HQ.
func (h *Backends) AllItems(ctx context.Context, o Officer, status string) ([]Item, error) {
	return h.Backend(o).AllItems(ctx, status)
}

// Release implements HQ.
func (h *Backends) Release(ctx context.Context, o Officer, item ItemID, instance string) error {
	_, err := h.Backend(o).Release(ctx, item, instance)
	return err
}

// Complete implements HQ.
func (h *Backends) Complete(ctx context.Context, o Officer, item ItemID, instance string, done Completion) error {
	_, err := h.Backend(o).Complete(ctx, item, instance, done)
	return err
}

// Heartbeat implements HQ.
func (h *Backends) Heartbeat(ctx context.Context, o Officer, item ItemID, instance string, leaseSeconds int) error {
	_, err := h.Backend(o).Heartbeat(ctx, item, instance, leaseSeconds)
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
	// OverBudget: the officer has a daily budget (daily_budget_usd) and it is
	// spent, or its spend could not be read (fail closed); then only items
	// that bypass the budget (Officer.BypassesBudget) are work for it.
	OverBudget bool
	// BudgetRead: the spend was read (Spent is it), as opposed to an
	// unreadable spend that reads as OverBudget. Always false with no budget.
	BudgetRead bool
	// Spent is the officer's spend today in USD, when BudgetRead.
	Spent float64
	// Budget is the daily budget the pass held it to, in USD: the declared
	// daily_budget_usd, or its owner's raise for today
	// (Officer.WithRaisedCap). 0 with no budget.
	Budget float64
	// SpendUnreadable: the officer has a daily budget and its spend could not
	// be read this pass (then OverBudget, fail closed).
	SpendUnreadable bool
	// Chats: the officer's backend serves chats and muster runs them
	// (ChatRunner.On). Only then is a chat item set apart from queue work; a
	// muster that runs no chats sees every item as before.
	Chats bool
	// ChatPlan is what the pool's chats need of its instances
	// (ChatRunner.Want). Zero for a pool that runs no chats.
	ChatPlan ChatPlan
	// ChatHeldBack is the pending chats the pool cannot serve now
	// (ChatRunner.Want): not counted in ChatPending, so a chat that cannot
	// run never keeps the pool up.
	ChatHeldBack map[ItemID]bool
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
	// StopWhy is why each instance in Stop stops when it is not StopReason:
	// a chat-idle stop of a busy pool (chatIdleStops), by instance number.
	StopWhy map[int]string
	// ChatWhy is why each instance in Start was started for chats (a chat
	// home a waiting chat needs, or a chat scale-up), by instance number.
	ChatWhy map[int]string
	// StartWhy is why each instance in Start was down (InstanceState.Down;
	// "" when the fleet did not say), by instance number.
	StartWhy map[int]string
	// Release is live claims held by an instance that is not up.
	Release []Item
	// Restart is the instances in error this pass restarts (stop, release
	// what each held, start: autorestart.go), ascending. Each was counted up
	// by Decide, so none is in Start; none is in Stop or Overrun.
	Restart []Restart
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
	// BudgetWaiting is how many items would be Claimable but for the
	// officer's spent daily budget (obs.OverBudget). An item held back by
	// both caps counts here, not in CapWaiting.
	BudgetWaiting int
	// Held is how many live claims its instances hold.
	Held int
	// ChatPending is how many chat items wait for a chat turn, and ChatHeld
	// how many a chat turn holds now (chat.go). Counted for a pool that runs
	// chats (Officer.ChatsPool) only; never in Claimable or Held — a chat is
	// not queue work.
	ChatPending int
	ChatHeld    int
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
//   - Busy (Claimable > 0 or Held > 0, or chat work): want min(MaxInstances,
//     held-by-up + Claimable) of the queue's instances (1..MaxInstances) up;
//     start the lowest-numbered down ones to reach it. A pool that runs chats
//     also starts the stopped homes its waiting chats wait for
//     (ChatPlan.Homes) and the instances its other waiting chats need
//     (ChatPlan.Need), up to chat_max_instances, and stops an instance
//     quiet for chat_idle_stop (chatIdleStops) — never one doing queue work.
//     Nothing else is stopped while busy.
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
		if p, n, ok := forAgentName([]Officer{o}, holder); ok && p.Tier == o.Tier {
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
	// An item not yet due (not_before ahead: a batched routine item) is not
	// work at all: its claim is refused 409 not_due, so waking an instance
	// for it would claim nothing.
	// 🔴 A CHAT ITEM IS NEVER QUEUE WORK (chat.go): it is not claimable,
	// held, waiting or capped for the queue — no instance is re-prompted for
	// it, and its claim (held as ChatHolder) is never released, capped or
	// counted as an instance's. The chat pool counts it apart (ChatPending,
	// ChatHeld): it keeps the pool busy, and obs.ChatPlan says how many more
	// instances its chats need (the chat scale-up below).
	chatItem := func(it Item) bool { return obs.Chats && it.IsChat() }
	workable := func(it Item) bool { return !chatItem(it) && !it.Blocked && o.worksItem(it) && it.Due(now) }
	// Over its daily budget, the officer may claim only what bypasses it
	// (its owner's own, non-routine items): the claim refuses any other.
	inBudget := func(it Item) bool { return !obs.OverBudget || o.BypassesBudget(it) }
	claimable := func(it Item) bool {
		return workable(it) && (!obs.Capped || obs.Counted[it.ID]) && inBudget(it)
	}
	count := func(it Item) {
		switch {
		case claimable(it):
			d.Claimable++
		case !workable(it):
		case !inBudget(it):
			d.BudgetWaiting++
		default:
			d.CapWaiting++
		}
	}
	for _, it := range obs.Open {
		count(it)
	}
	if obs.Chats && o.ChatsPool() {
		for _, it := range PendingChats(obs, now) {
			if !obs.ChatHeldBack[it.ID] {
				d.ChatPending++
			}
		}
	}
	heldByUp := map[string]bool{}
	// chatHeldBy: the instances whose chat holder (ChatHolder) holds a live
	// chat claim — a turn is running for it somewhere.
	chatHeldBy := map[string]bool{}
	overrun := map[string]bool{}
	// capHolder: the instance numbers holding a capped claim (cappedHolders,
	// the one place that set is computed: the chat runner reads it too).
	// Never started.
	capHolder := cappedHolders(o, obs, now)
	limit := o.ItemCap()
	for _, it := range obs.Claimed {
		if chatItem(it) {
			if o.ChatsPool() && it.LeaseLive(now) {
				d.ChatHeld++
				if inst, ok := strings.CutSuffix(it.ClaimedBy, chatHolderSuffix); ok {
					chatHeldBy[inst] = true
				}
			}
			continue
		}
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
		if cappedClaim(limit, it, now) {
			d.Overrun = append(d.Overrun, Overrun{Item: it, N: in.N, Up: in.Up, Held: now.Sub(*it.ClaimedAt)})
			overrun[in.Name] = true
			continue
		}
		d.Held++
		if in.Up {
			heldByUp[in.Name] = true
		} else {
			d.Release = append(d.Release, it)
		}
	}
	up, upQueue := 0, 0
	upN := map[int]bool{}
	for _, in := range obs.Instances {
		if in.Up {
			up++
			upN[in.N] = true
			if in.N <= o.MaxInstances {
				upQueue++
			}
		}
	}
	chatsOn := obs.Chats && o.ChatsPool()
	chatWork := d.ChatPending > 0 || d.ChatHeld > 0
	if d.Claimable > 0 || d.Held > 0 || chatWork {
		// Queue capacity is the queue's instances (1..max_instances) only: a
		// chat-only instance up takes no queue work.
		want := len(heldByUp) + d.Claimable
		if want > o.MaxInstances {
			want = o.MaxInstances
		}
		downWhy := map[int]string{}
		for _, in := range obs.Instances {
			if !in.Up {
				downWhy[in.N] = in.Down
			}
		}
		start := func(n int) {
			d.Start = append(d.Start, n)
			if d.StartWhy == nil {
				d.StartWhy = map[int]string{}
			}
			d.StartWhy[n] = downWhy[n]
		}
		for _, n := range downInstances(o, obs.Instances) {
			if upQueue+len(d.Start) >= want {
				break
			}
			if !capHolder[n] {
				start(n)
			}
		}
		queueStarts := len(d.Start)
		// 🔴 CHAT SCALE-UP: first every stopped home a waiting chat needs
		// (ChatPlan.Homes: the chat continues its session there, never moved
		// for a stop that comes up within chatHomeUpWithin), then the
		// instances the unplaced chats need beyond them (ChatPlan.Need), the lowest free numbers — never past
		// chat_max_instances, never a capped holder. An instance the queue
		// starts this pass gives the chats lanes too, so it counts — a home
		// among them only once (Want already counted its free lanes).
		if chatsOn && d.ChatPending > 0 && (obs.ChatPlan.Need > 0 || len(obs.ChatPlan.Homes) > 0) {
			homes := obs.ChatPlan.Homes
			need := obs.ChatPlan.Need
			for _, n := range d.Start {
				if n <= o.ChatMaxInstances() && !slices.Contains(homes, n) {
					need--
				}
			}
			chatStart := func(n int, why string) {
				start(n)
				if d.ChatWhy == nil {
					d.ChatWhy = map[int]string{}
				}
				d.ChatWhy[n] = why
			}
			for _, n := range homes {
				if n < 1 || n > o.ChatMaxInstances() || upN[n] || capHolder[n] || slices.Contains(d.Start, n) {
					continue
				}
				chatStart(n, "chat home: a chat waits for the instance its session lives on")
			}
			for n := 1; n <= o.ChatMaxInstances() && need > 0; n++ {
				if upN[n] || capHolder[n] || slices.Contains(d.Start, n) {
					continue
				}
				chatStart(n, fmt.Sprintf("chat scale-up: %d chat(s) wait with every lane busy (chat_sessions %d, chat_max_instances %d)",
					obs.ChatPlan.Waiting, o.ChatSessions(), o.ChatMaxInstances()))
				need--
			}
			sort.Ints(d.Start)
		}
		skip := map[string]bool{}
		for n := range heldByUp {
			skip[n] = true
		}
		for n := range overrun {
			skip[n] = true
		}
		// A chat-only instance (past max_instances) is never prompted for the
		// queue: its primary takes no queue work.
		for _, in := range obs.Instances {
			if in.N > o.MaxInstances {
				skip[in.Name] = true
			}
		}
		d.Reprompt = reprompts(obs.Instances, skip, d.Claimable-queueStarts, promptedAt, now)
		if chatsOn {
			d.Stop = chatIdleStops(o, obs, d, heldByUp, chatHeldBy, overrun, capHolder, promptedAt, now)
			if len(d.Stop) > 0 {
				d.StopWhy = map[int]string{}
				for _, n := range d.Stop {
					d.StopWhy[n] = fmt.Sprintf("chat scale-down: no chat turn, claim or prompt for %s (chat_idle_stop)", o.ChatIdleStop())
				}
			}
		}
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

// cappedClaim reports whether live claim it has been held for limit (the
// pool's ItemCap; 0 caps nothing), by HQ's claimed_at: a claim without one
// has no clock and is not capped.
func cappedClaim(limit time.Duration, it Item, now time.Time) bool {
	return limit > 0 && it.ClaimedAt != nil && now.Sub(*it.ClaimedAt) >= limit
}

// cappedHolders is the instance numbers of pool o that hold a capped queue
// claim in obs at now (Decide's capHolder: never started). The chat runner
// reads it so a chat homed on one is re-homed, not left waiting for a start
// that never comes (rehomeWhy).
func cappedHolders(o Officer, obs Observation, now time.Time) map[int]bool {
	out := map[int]bool{}
	limit := o.ItemCap()
	for _, it := range obs.Claimed {
		if (obs.Chats && it.IsChat()) || !it.LeaseLive(now) || !cappedClaim(limit, it, now) {
			continue
		}
		if p, n, ok := forAgentName([]Officer{o}, it.ClaimedBy); ok && p.Tier == o.Tier {
			out[n] = true
		}
	}
	return out
}

// chatIdleStops is the instances a busy pool that runs chats stops for
// chat idleness (Decide): up, not in error, owed no prompt, and quiet — no
// chat turn, live claim or prompt — for chat_idle_stop by the plan's
// ActiveAt and promptedAt (an instance this process never saw active is not
// stopped: a restarted muster stops nothing on its first pass). 🔴 NEVER ONE
// DOING QUEUE WORK: not one holding a queue claim, a capped claim or a live
// chat claim, nor one a chat turn runs on or a pending chat is homed on, nor
// — while a chat waits for a lane — one within the chat ceiling; and an
// instance within max_instances only when the queue has no work at all
// (nothing claimable or held), so the queue's instance is never stopped
// under it.
func chatIdleStops(o Officer, obs Observation, d Decision, heldByUp, chatHeldBy, overrun map[string]bool, capHolder map[int]bool,
	promptedAt map[int]time.Time, now time.Time) []int {
	plan := obs.ChatPlan
	idle := o.ChatIdleStop()
	var out []int
	for _, in := range obs.Instances {
		n := in.N
		switch {
		case !in.Up || in.Error || in.PromptPending:
			continue
		case heldByUp[in.Name] || overrun[in.Name] || capHolder[n] || chatHeldBy[in.Name]:
			continue
		case plan.Busy[n] > 0 || plan.PendingOn[n] > 0:
			continue
		case plan.Unplaced > 0 && n <= o.ChatMaxInstances():
			continue
		case n <= o.MaxInstances && (d.Claimable > 0 || d.Held > 0):
			continue
		}
		at, seen := plan.ActiveAt[n]
		if !seen || now.Sub(at) < idle {
			continue
		}
		if p, ok := promptedAt[n]; ok && now.Sub(p) < idle {
			continue
		}
		out = append(out, n)
	}
	sort.Ints(out)
	return out
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
// item, so a fresh item gets one prompt, not one per instance. Except a SPENT
// instance (InstanceState.Spent: a fresh_session_per_item pool's whose
// conversation has had its one item): it is due at once, else a fresh pool
// would work at most one item per instance per RepromptAfter. If its last
// turn is still ending, ccd refuses the prompt (409 busy, nothing typed) and
// the next pass tries again. An instance
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
		// A spent instance (a fresh pool's, its one item done however its
		// hold ended — it holds none, above) is not "on its way to the
		// queue": its conversation may claim nothing more, so it is due now.
		if in.PromptPending || (prompted && now.Sub(at) < RepromptAfter && !in.Spent) {
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

// fleetUnread is a pass's failure to read its pools' instances because muster's
// own provisioner could not be reached (Fleet.Instances answering
// provision.ErrBlind) — not the officer's backend, and not a fleet failure of
// any other kind. Its text is the underlying error's. Check retries it at the
// poll interval rather than Backoff's doubling.
//
// ⚠ ONLY ErrBlind. A host officer's host being offline, a missing host-spawn
// store or an unreadable agents table also fail Fleet.Instances, and keep the
// normal backoff: a laptop offline overnight re-read every poll interval would
// cost its HQ ~10x the requests for nothing.
type fleetUnread struct{ err error }

func (e fleetUnread) Error() string { return e.err.Error() }
func (e fleetUnread) Unwrap() error { return e.err }

// Backoff is how long to wait after the failures'th consecutive failed pass:
// PollInterval doubled per failure, capped at MaxBackoff. A pass that failed
// reading its fleet (fleetUnread) waits PollInterval instead.
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
	// fleetFailures is the run of passes that failed reading the fleet
	// (fleetUnread), counted apart from failures so it never grows the
	// backend's backoff.
	fleetFailures int
	nextAt        time.Time
	// promptedAt is when each instance was last started or re-prompted, by
	// instance agent name (the two pools' numbers overlap).
	promptedAt map[string]time.Time
	// errored is the instances last seen in error (logged on change), by
	// instance agent name: since when this process has seen each in error.
	errored map[string]erroredSlot
	// restarts is the day's auto-restarts (autorestart.go).
	restarts restartBook
	// scaledDown is each instance this process scaled to zero itself, and
	// why, by instance agent name: its next start is a WAKE, not a restart
	// (startReason). Cleared when it starts; a restart of muster forgets it,
	// and the fleet's own reason then speaks.
	scaledDown map[string]string
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
	// switchedOff: the last pass found the officer's backend switch off
	// (SwitchGated): logged when it turns off and when it turns on again.
	switchedOff bool
	// budgetAlertDay is the UTC day the owner was last told the daily budget
	// is reached (Config.BudgetAlert): told once per (officer, UTC day) per
	// process — a restarted muster tells them once more that day.
	budgetAlertDay string
	// chatUp and chatWaiting are the last successful pass's chat instances
	// up and chats waiting for a lane, over the officer's chat pools
	// (muster_officer_chat_instances_up, muster_officer_chats_waiting).
	chatUp, chatWaiting int
	// budgetUnreadableDay is the UTC day the owner was last told the spend
	// cannot be read (BudgetAlert.Unreadable): once per (officer, UTC day)
	// per process, like budgetAlertDay.
	budgetUnreadableDay string
	// lateLogged: this officer was more than its poll interval late — a pass
	// started that late (notePassLag), or Run saw it due that long without a
	// free pass slot (noteWaiting) — and that was logged. Cleared by the next
	// pass that starts on time, so each lag episode is logged once.
	lateLogged bool
	// dueSeen is the tick Run first saw this officer due without starting its
	// pass (zero: not waiting). It dates a wait whose nextAt is zero (a first
	// pass, or one Due asked for); the officer's pass starting clears it.
	dueSeen time.Time
	// dueAfterPass: Due asked for a pass while one of this officer's passes was
	// running (Reconciler.passing). That pass's end (passEnded) makes the
	// officer due — Check would otherwise overwrite the request with the next
	// poll time — so the pass asked for runs after it, its wait dated from the
	// first tick after it ended, never from before.
	dueAfterPass bool
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
	// autoRestart is Config.AutoRestart (the zero value: off).
	autoRestart AutoRestart
	// restartNotice is Config.AutoRestartNotice (nil: none).
	restartNotice func(o Officer, e AutoRestartEvent)
	// budgetAlert is Config.BudgetAlert (nil: none).
	budgetAlert func(o Officer, a BudgetAlert)
	// chats is Config.Chats (nil: no chat turns are run).
	chats *ChatRunner
	// kickoff is Config.Kickoff (never nil: KickoffText by default).
	kickoff func(ctx context.Context, o Officer, n int) string
	// refresh, refreshEvery and refreshedAt are Config.Refresh: Run calls it
	// at most every refreshEvery, before it looks for a due officer.
	refresh      func(ctx context.Context)
	refreshEvery time.Duration
	refreshedAt  time.Time

	mu    sync.Mutex
	state map[string]*officerState
	// passing is the key of every officer whose pass Run started and that
	// has not ended (guarded by mu): Run never starts a second pass for one
	// of them, and starts none while maxConcurrentPasses run. It lives on the
	// Reconciler, not in Run, so a Run restarted after a panic still sees the
	// passes its predecessor left running.
	passing map[string]bool
	// passes counts the passes in passing: Run waits for them on shutdown.
	passes sync.WaitGroup
	done   chan struct{}
	// doneOnce closes done once, on Run's ctx-ended return (see Run).
	doneOnce sync.Once
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
	// BudgetAlert, when set, is told once per UTC day (per process) when an
	// officer's spend read on a pass has reached its daily_budget_usd. It is
	// how the owner learns the officer stopped being woken. Called outside
	// the loop's lock, from the pass; it must not block.
	BudgetAlert func(o Officer, a BudgetAlert)
	// Kickoff is the first message instance n of officer o starts with: nil
	// is KickoffText. internal/api reads a builtin officer's type grants into
	// it (KickoffTextWith), so a granted tool is listed.
	Kickoff func(ctx context.Context, o Officer, n int) string
	// Refresh, when set, re-reads the roster into Registry (internal/api: the
	// DB-declared officers, migration 0136). Run calls it on its first tick
	// and then at most every RefreshEvery (DefaultRefreshEvery when 0), so an
	// officer another replica created, changed or deleted reaches this one's
	// loop with no restart. It must not block for long; it is called from
	// Run's goroutine.
	Refresh      func(ctx context.Context)
	RefreshEvery time.Duration
	// Chats runs the officers' chat turns (chat.go): nil runs none, and a
	// chat item then only keeps its pool busy.
	Chats *ChatRunner
	// AutoRestart is the bounded restart of an instance in error
	// (autorestart.go). The zero value is off: an instance in error holds
	// its slot until an operator acts.
	AutoRestart AutoRestart
	// AutoRestartNotice, when set, is told each auto-restart, each restart
	// held (once per error episode: its class is not restartable or the
	// day's cap is spent) and the recovery of a held one. Called outside the
	// loop's lock, from the pass; it must not block.
	AutoRestartNotice func(o Officer, e AutoRestartEvent)
}

// DefaultRefreshEvery is how often Run re-reads the roster (Config.Refresh)
// when nothing else asks: what bounds how late another replica's create,
// pause or delete reaches this one.
const DefaultRefreshEvery = 15 * time.Second

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
	if cfg.RefreshEvery <= 0 {
		cfg.RefreshEvery = DefaultRefreshEvery
	}
	if cfg.Kickoff == nil {
		cfg.Kickoff = func(_ context.Context, o Officer, n int) string { return KickoffText(o, n) }
	}
	return &Reconciler{kickoff: cfg.Kickoff, reg: cfg.Registry, hq: cfg.HQ, fleet: cfg.Fleet, log: cfg.Logger, now: cfg.Now, observed: cfg.Observed,
		judgement: cfg.Judgement, capHold: cfg.CapHold, budgetAlert: cfg.BudgetAlert, autoRestart: cfg.AutoRestart, restartNotice: cfg.AutoRestartNotice, refresh: cfg.Refresh, refreshEvery: cfg.RefreshEvery, chats: cfg.Chats,
		state: map[string]*officerState{}, passing: map[string]bool{}, done: make(chan struct{})}, nil
}

// erroredSlot is an instance in error: how it is named in the log, since
// when this process has seen it in error (the auto-restart's clock), and
// whether its owner was told its restart is held (once per episode).
type erroredSlot struct {
	officer, instance string
	since             time.Time
	class             ErrorClass
	heldTold          bool
	// pool is the pool it is an instance of, for the recovery notice.
	pool Officer
}

// st is the state of the officer keyed key (Officer.Key).
func (r *Reconciler) st(key string) *officerState {
	s, ok := r.state[key]
	if !ok {
		s = &officerState{idleSince: map[string]time.Time{}, promptedAt: map[string]time.Time{}, errored: map[string]erroredSlot{},
			capRefused: map[ItemID]refusedCap{}}
		r.state[key] = s
	}
	return s
}

// prune forgets the state of every officer no longer in the roster (a
// DB-declared officer deleted), so its gauges stop counting it and a later
// officer that reuses its slug starts with none of it.
func (r *Reconciler) prune(list []Officer) {
	keep := make(map[string]bool, len(list))
	for _, o := range list {
		keep[o.Key()] = true
	}
	r.mu.Lock()
	pruned := false
	for k := range r.state {
		if !keep[k] {
			delete(r.state, k)
			pruned = true
		}
	}
	r.mu.Unlock()
	if pruned {
		r.publish()
	}
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

// Due makes the officer keyed key due now: Run passes it on its next tick
// (RunTick) — and never while a pass for the same officer is still running
// (Reconciler.passing), so a pass it asks for never runs beside another pass
// for that officer. The admin API asks it after a pause or a
// resume, so the switch acts within a tick rather than a poll interval, and
// on every "run now" (an already-queued one included).
// Asked while a pass of the officer is running, it takes effect when that pass
// ends (officerState.dueAfterPass): one pass follows it. A Due-made officer
// is due "now", not first: Run orders it by when it was first seen due
// (passOrder), so asking again and again cannot jump officers that have
// waited longer.
func (r *Reconciler) Due(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.state[key]
	if !ok {
		return
	}
	if r.passing[key] {
		s.dueAfterPass = true
		return
	}
	s.nextAt = time.Time{}
}

// Check runs one pass for the officer keyed key (Officer.Key: an env
// officer's id) NOW, whatever its schedule, unless it is backing off and force
// is false.
func (r *Reconciler) Check(ctx context.Context, key string, force bool) PassResult {
	o, err := r.reg.Get(key)
	if err != nil {
		return PassResult{Err: err}
	}
	now := r.now()
	r.mu.Lock()
	s := r.st(key)
	if !force && (s.failures > 0 || s.fleetFailures > 0) && now.Before(s.nextAt) {
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
	if errors.Is(err, errSwitchedOff) {
		return r.switchedOffPass(ctx, o, now)
	}
	if err != nil {
		r.mu.Lock()
		// A pass that saw nothing cannot say the pool is held back by its
		// session cap: muster_officer_judgement_cap_reached is the last
		// SUCCESSFUL pass's state, so a failed pass (and a fault scale-down,
		// which follows one) clears it rather than republish a stale value.
		s.capReached = false
		unread := errors.As(err, new(fleetUnread))
		if unread {
			// Counted apart from failures (fleetFailures): failures is the
			// backend's run, which Backoff doubles, and a long provisioner
			// outage must not leave the next HQ blip at the 10-minute cap.
			// HQ answered this pass (observe reads it first), which ends the
			// backend's run as a success would.
			s.fleetFailures++
			s.failures = 0
		} else {
			s.failures++
		}
		s.nextAt = now.Add(Backoff(o, s.failures))
		if unread {
			// 🔴 AN UNREADABLE FLEET IS RETRIED AT THE POLL INTERVAL, NOT
			// BACKED OFF. The backoff protects the officer's BACKEND (HQ)
			// from a retry storm; this pass read HQ fine and failed on
			// muster's own provisioner, which a retry at the poll interval
			// reads no more often than a healthy officer does. Doubling it
			// meant one bad minute of the provisioner's client
			// (a rate limiter out of budget) parked officers for 8 and 10
			// minutes after it had recovered.
			s.nextAt = now.Add(o.PollInterval)
		}
		f, next := s.failures, s.nextAt
		if unread {
			f = s.fleetFailures
		}
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
				note = fmt.Sprintf(" (%s refusal %s seen on %d of %d consecutive passes before it counts as a configuration fault)", backendWord(o), refusal, strikes, FaultStrikes)
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
		if changed && o.IsBuiltin() {
			r.log.Printf("officers: %s: the built-in backend does not define this officer (answered %s on %d consecutive passes): a CONFIGURATION fault — define it with `muster officer def put` (docs/officers-builtin.md). "+
				"Scaling its instances to zero; retrying with backoff (next in %s)", o.ID, refusal, strikes, next.Sub(now).Round(time.Second))
		} else if changed {
			r.log.Printf("officers: %s: HQ refuses this officer (HQ answered %s on %d consecutive passes): a CONFIGURATION fault, not an outage — check %s%s and that HQ serves the officer API for %s. "+
				"Scaling its instances to zero; retrying with backoff (next in %s)", o.ID, refusal, strikes, TokenEnvPrefix, TokenEnvSuffix(o.ID), o.ID, next.Sub(now).Round(time.Second))
		}
		return r.scaleDownForFault(ctx, o, err, refusal)
	}
	r.noteErrors(o, pools, obs)
	if r.observed != nil {
		// Chat claims are the chat runner's (ChatHolder), never an
		// instance's primary session's: the transcript forwarder and the LLM
		// proxy's claim sync are not told of them.
		claimed := obs[0].Claimed
		if obs[0].Chats {
			claimed = NonChat(claimed)
		}
		r.observed(o, claimed, now)
	}
	r.mu.Lock()
	ds := make([]Decision, len(pools))
	for i, p := range pools {
		prompted := map[int]time.Time{}
		for n := 1; n <= p.Slots(); n++ {
			if at, ok := s.promptedAt[p.InstanceName(n)]; ok {
				prompted[n] = at
			}
		}
		ds[i] = Decide(p, obs[i], idleSince[p.Tier], now, prompted)
	}
	held := make([][]AutoRestartEvent, len(pools))
	for i, p := range pools {
		held[i] = r.planRestarts(s, p, obs[i], &ds[i], now)
	}
	if s.fault != "" {
		r.log.Printf("officers: %s: %s answers again (was a configuration fault: it answered %s)", o.ID, backendWord(o), s.fault)
		s.fault = ""
	}
	if s.switchedOff {
		r.log.Printf("officers: %s: switched on again in its backend", o.ID)
		s.switchedOff = false
	}
	s.strikes = 0
	s.failures, s.fleetFailures = 0, 0
	s.nextAt = now.Add(o.PollInterval)
	s.chatUp, s.chatWaiting = 0, 0
	for i, p := range pools {
		if !obs[i].Chats || !p.ChatsPool() {
			continue
		}
		for _, in := range obs[i].Instances {
			if in.Up && in.N <= p.ChatMaxInstances() {
				s.chatUp++
			}
		}
		s.chatWaiting += obs[i].ChatPlan.Waiting
	}
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
		if day := utcDay(now); reached && s.capLoggedDay != day {
			s.capLoggedDay = day
			reset := CapResetAt(now)
			capLines = append(capLines, fmt.Sprintf("officers: %s: %s: %d claimable judgement item(s) wait in HQ and are not claimed until the cap resets at %s "+
				"(tiers.judgement.max_sessions_per_day; logged once per UTC day)", p.label(), CapReason(obs[i].Used, limit, reset), ds[i].CapWaiting, reset.Format(time.RFC3339)))
		}
	}
	// The daily budget: told once per UTC day, on a pass that READ the spend
	// at or over the cap (an unreadable spend holds work back, fail closed,
	// but is not "the budget is reached").
	var alert *BudgetAlert
	if day := utcDay(now); obs[0].SpendUnreadable && s.budgetUnreadableDay != day {
		// Unreadable: work the cap holds back stays held (fail closed), and
		// the owner is told once that day, since nothing else says why.
		s.budgetUnreadableDay = day
		waiting := 0
		for _, d := range ds {
			waiting += d.BudgetWaiting
		}
		alert = &BudgetAlert{Unreadable: true, Budget: o.Budget.DailyUSD, Waiting: waiting}
	}
	if obs[0].BudgetRead && obs[0].OverBudget {
		if day := utcDay(now); s.budgetAlertDay != day {
			s.budgetAlertDay = day
			waiting := 0
			for _, d := range ds {
				waiting += d.BudgetWaiting
			}
			alert = &BudgetAlert{Spent: obs[0].Spent, Budget: obs[0].Budget, Waiting: waiting, ResetAt: CapResetAt(now)}
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
	if alert != nil && alert.Unreadable {
		r.log.Printf("officers: %s: its spend for daily_budget_usd cannot be read: only its owner's own non-routine items wake it until it can; %d item(s) held back (the owner is told once per UTC day)",
			o.ID, alert.Waiting)
		if r.budgetAlert != nil {
			r.budgetAlert(o, *alert)
		}
	} else if alert != nil {
		r.log.Printf("officers: %s: daily budget reached: $%.2f spent of $%.2f (daily_budget_usd); not woken for new work until %s except for its owner's own non-routine items; %d item(s) held back (logged once per UTC day)",
			o.ID, alert.Spent, alert.Budget, alert.ResetAt.Format(time.RFC3339), alert.Waiting)
		if r.budgetAlert != nil {
			r.budgetAlert(o, *alert)
		}
	}
	for _, c := range ended {
		r.endCapHold(c)
	}
	for i, p := range pools {
		r.tellHeld(p, held[i])
	}
	res := r.actAll(ctx, pools, ds, nil)
	// The chat turns, last: each runs on its own goroutine, on an instance
	// this pass already saw up (one started now gets its chats next pass).
	for i, p := range pools {
		r.chats.Dispatch(ctx, p, obs[i], now)
	}
	return res
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

// switchedOffPass is a pass for an officer whose SwitchGated backend is
// switched off: every up instance of every pool is stopped, nothing is
// started, and the officer is checked again after its poll interval (an
// answer, not a failure: no backoff). Logged when the switch turns off; the
// next ordinary pass logs it on again.
func (r *Reconciler) switchedOffPass(ctx context.Context, o Officer, now time.Time) PassResult {
	r.mu.Lock()
	s := r.st(o.Key())
	turned := !s.switchedOff
	s.switchedOff = true
	s.failures, s.fleetFailures, s.strikes, s.fault, s.capReached = 0, 0, 0, "", false
	s.nextAt = now.Add(o.PollInterval)
	r.mu.Unlock()
	r.publish()
	if turned {
		r.log.Printf("officers: %s: switched OFF in its backend: stopping its instances; none starts until it is switched on", o.ID)
	}
	pools := o.Pools()
	ds := make([]Decision, len(pools))
	for i, p := range pools {
		insts, err := r.fleet.Instances(ctx, p)
		if err != nil {
			return PassResult{Err: err}
		}
		ds[i] = Decision{StopReason: "switched off in its backend"}
		for _, in := range insts {
			if in.Up {
				ds[i].Stop = append(ds[i].Stop, in.N)
			}
		}
		sort.Ints(ds[i].Stop)
	}
	return r.actAll(ctx, pools, ds, nil)
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
	s := r.st(o.Key())
	var entered, left []string
	at := r.now()
	now := map[string]erroredSlot{}
	for i, p := range pools {
		for _, in := range obs[i].Instances {
			if in.Error {
				e, was := s.errored[in.Name]
				if !was {
					e = erroredSlot{since: at}
					entered = append(entered, in.Name)
				}
				e.officer, e.instance, e.class, e.pool = p.label(), p.InstanceLabel(in.N), in.ErrorClass, p
				now[in.Name] = e
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
		switch {
		case !r.autoRestart.On():
			r.log.Printf("officers: %s: instance %s is in error (%s) — it holds its slot and is NOT restarted (auto-restart is off), so the officer runs below target "+
				"until it recovers or an operator acts (agent %s)", e.officer, e.instance, e.class, name)
		case e.class.Restartable():
			r.log.Printf("officers: %s: instance %s is in error (%s) — it holds its slot; muster restarts it once it has been in error %s or more with work waiting, "+
				"at most %d auto-restarts per officer per UTC day (agent %s)", e.officer, e.instance, e.class, r.autoRestart.After, r.autoRestart.MaxPerDay, name)
		default:
			r.log.Printf("officers: %s: instance %s is in error (%s) — it holds its slot and is NOT restarted: a restart does not fix that class, so the officer runs below target "+
				"until it recovers or an operator acts (agent %s)", e.officer, e.instance, e.class, name)
		}
	}
	for _, name := range left {
		e := gone[name]
		r.log.Printf("officers: %s: instance %s left error", e.officer, e.instance)
		if e.heldTold && r.restartNotice != nil {
			r.restartNotice(e.pool, AutoRestartEvent{Instance: e.instance, Name: name, Class: e.class, Recovered: true})
		}
	}
	r.publish()
}

// publish sets the officer gauges from every officer's remembered state.
func (r *Reconciler) publish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	slots, faults, withheld, capReached := 0, 0, 0, 0
	chatUp, chatWaiting := 0, 0
	for _, s := range r.state {
		chatUp += s.chatUp
		chatWaiting += s.chatWaiting
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
	metrics.OfficerChatInstancesUp.Set(float64(chatUp))
	metrics.OfficerChatsWaiting.Set(float64(chatWaiting))
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
	if sw, ok := r.hq.(switchReader); ok {
		off, err := sw.SwitchedOff(ctx, o)
		if err != nil {
			return nil, err
		}
		if off {
			return nil, errSwitchedOff
		}
	}
	open, err := r.hq.AllItems(ctx, o, StatusOpen)
	if err != nil {
		return nil, err
	}
	// The daily budget is the OFFICER's, every pool's spend together: read
	// once. An unreadable spend holds new work back for the pass (fail
	// closed), as an unreadable session ledger does.
	var over, read, unreadable bool
	var spent float64
	budget := o.Budget.DailyUSD
	if o.Budget.DailyUSD > 0 {
		if sr, ok := r.hq.(spendReader); ok {
			v, served, err := sr.SpentToday(ctx, o, r.now())
			switch {
			case err != nil:
				r.log.Printf("officers: %s: reading its spend for daily_budget_usd: %v — not waking it for new work this pass", o.ID, err)
				over, unreadable = true, true
			case served:
				// The owner's raise for today, when there is one
				// (Officer.WithRaisedCap). An unreadable raise is the
				// declared budget: the lower cap, so fail closed.
				ob := o
				if rr, ok := r.hq.(raisedCapReader); ok {
					if c, err := rr.RaisedCapToday(ctx, o, r.now()); err != nil {
						r.log.Printf("officers: %s: reading today's raised cap: %v — holding it to its declared daily_budget_usd this pass", o.ID, err)
					} else {
						ob = o.WithRaisedCap(c)
					}
				}
				budget = ob.Budget.DailyUSD
				spent, read, over = v, true, ob.OverBudget(v)
			}
		}
	}
	claimed, err := r.hq.AllItems(ctx, o, StatusClaimed)
	if err != nil {
		return nil, err
	}
	out := make([]Observation, len(pools))
	for i, p := range pools {
		insts, err := r.fleet.Instances(ctx, p)
		if errors.Is(err, provision.ErrBlind) {
			return nil, fleetUnread{err}
		}
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
		out[i] = Observation{Open: open, Claimed: claimed, Instances: insts, Capped: capped, Counted: counted, Used: used,
			OverBudget: over, BudgetRead: read, Spent: spent, Budget: budget, SpendUnreadable: unreadable}
		out[i].Chats = r.chats.On(p)
		out[i].ChatPlan, out[i].ChatHeldBack = r.chats.Want(ctx, p, out[i], r.now())
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
	for _, rs := range d.Restart {
		errs = append(errs, r.restart(ctx, o, rs)...)
	}
	for _, n := range d.Start {
		if err := r.fleet.Start(ctx, o, n, r.kickoff(ctx, o, n)); err != nil {
			errs = append(errs, err)
			r.log.Printf("officers: %s: start instance %s: %v", o.label(), o.InstanceLabel(n), err)
			continue
		}
		why := r.startReason(o, n, d.StartWhy[n])
		if c, ok := d.ChatWhy[n]; ok {
			why = c + "; " + why
		}
		r.prompted(o, n)
		r.log.Printf("officers: %s: started instance %s — %s (%d claimable, %d held)", o.label(), o.InstanceLabel(n), why, d.Claimable, d.Held)
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
		if why, ok := d.StopWhy[n]; ok {
			reason = why
		}
		if reason == "" {
			reason = "idle " + o.IdleAfter.String()
		}
		r.scaledDownBy(o, n, reason)
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
	s := r.st(o.Key())
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
	s := r.st(o.Key())
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

// backendWord names o's backend in a log line: HQ, or the built-in backend.
func backendWord(o Officer) string {
	if o.IsBuiltin() {
		return "the built-in backend"
	}
	return "HQ"
}

// startReason is why instance n of o is being started: a WAKE after this
// process scaled it to zero (and why it did), or — the fleet's reason it is
// down otherwise — a first start or a RESTART after it ended on its own. The
// scale-down record is consumed: the start it explains is this one.
func (r *Reconciler) startReason(o Officer, n int, down string) string {
	r.mu.Lock()
	st := r.st(o.Key())
	name := o.InstanceName(n)
	stopped, wasStopped := st.scaledDown[name]
	delete(st.scaledDown, name)
	r.mu.Unlock()
	switch {
	case wasStopped:
		return "woken for new work after muster scaled it to zero (" + stopped + ")"
	case down != "":
		return down
	}
	return "it was not running"
}

// scaledDownBy records that this process scaled instance n of o to zero, and
// why (startReason reads it).
func (r *Reconciler) scaledDownBy(o Officer, n int, why string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.st(o.Key())
	if st.scaledDown == nil {
		st.scaledDown = map[string]string{}
	}
	st.scaledDown[o.InstanceName(n)] = why
}

func (r *Reconciler) prompted(o Officer, n int) {
	r.mu.Lock()
	r.st(o.Key()).promptedAt[o.InstanceName(n)] = r.now()
	r.mu.Unlock()
}

// RunTick is how often Run looks for a due officer.
var RunTick = time.Second

// passTimeout bounds one pass.
const passTimeout = 2 * time.Minute

// maxConcurrentPasses is how many officers' passes Run runs at once. Each due
// officer's pass runs on its own goroutine, so one officer's slow pass (a
// slow HQ or fleet read, up to passTimeout) no longer delays every other
// officer's — and with it the claim of their chat items, which happens at
// the end of a pass. The cap bounds the HQ and fleet load a loop can put
// out at once: every pass reads HQ (and may claim, start or stop) and reads
// the fleet, so an uncapped loop would hit both with one request burst per
// officer whenever HQ recovers from an outage or muster restarts. Slots go
// to the due officers that have waited longest (Run), so a cap too small
// for the roster degrades to round-robin — every officer's passes start
// later, none never — and the wait is measured
// (muster_officer_pass_oldest_wait_seconds, muster_officer_pass_start_lag_seconds).
const maxConcurrentPasses = 4

// passCandidate is a due officer Run has not started a pass for, waiting
// since since: its nextAt, or — for a zero nextAt (a first pass, or one Due
// asked for) — the tick Run first saw it due.
type passCandidate struct {
	o     Officer
	since time.Time
}

// passOrder sorts candidates by how long they have waited: the earliest since
// first, ties by key. A zero nextAt is not "first": it waits from the tick it
// was first seen due, like any other wait. What holds: an officer waiting
// since S is started before every officer whose wait began after S. And an
// officer started while it waits cannot be ahead of it again during that wait:
// the pass ends with the next wait beginning at the pass's start plus
// poll_interval (Check; a backoff after a failure is later still), or — a Due
// asked for during the pass (dueAfterPass) or after it — at the first tick
// that sees it due after the pass ended; both are later than S. So each other officer passes a
// waiting one at most once per wait, however often Due is asked for it (a
// "run now" tap or a pause/resume). Two waits that began on the same tick go
// by key. Not covered: a pass that panics before Check sets nextAt leaves its
// officer's old wait in place.
func passOrder(c []passCandidate) {
	sort.Slice(c, func(i, j int) bool {
		if !c[i].since.Equal(c[j].since) {
			return c[i].since.Before(c[j].since)
		}
		return c[i].o.Key() < c[j].o.Key()
	})
}

// Done is closed once Run has returned: the shutdown drain joins the loop on it
// before waiting for re-prompt sends, so no pass can start one meanwhile.
func (r *Reconciler) Done() <-chan struct{} { return r.done }

// Run drives the loop until ctx ends. Call it at most once.
func (r *Reconciler) Run(ctx context.Context) {
	// 🔴 Done closes only when Run stops because its ctx ended. A Run that
	// PANICS is restarted by its supervisor (api.Supervise): closing on that
	// unwind would tell shutdown "stopped" while the restarted Run still works,
	// and the restarted Run's own close would then panic on a closed channel
	// (TestAPanickedReconcilerRunNeitherSaysDoneNorClosesTwice).
	stopped := false
	defer func() {
		if stopped {
			r.doneOnce.Do(func() { close(r.done) })
		}
	}()
	t := time.NewTicker(RunTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// The passes still running end with ctx (each pass's context is
			// derived from it); Run returns — and Done closes — only after
			// they have, as when passes ran on Run's own goroutine.
			r.passes.Wait()
			stopped = true
			return
		case <-t.C:
			now := r.now()
			if r.refresh != nil && (r.refreshedAt.IsZero() || now.Sub(r.refreshedAt) >= r.refreshEvery) {
				r.refreshedAt = now
				r.refresh(ctx)
			}
			list := r.reg.All()
			r.prune(list)
			for _, c := range r.startDue(list, now) {
				r.passes.Add(1)
				go func(c passCandidate) {
					defer r.passes.Done()
					defer r.passEnded(c.o.Key())
					r.pass(ctx, c.o, c.since)
				}(c)
			}
		}
	}
}

// startDue picks, at now, the officers of list whose passes Run starts: every
// due officer with no pass running is a candidate, and the free slots
// (maxConcurrentPasses less the passes running) go to the candidates in
// passOrder — the officer that has waited longest first, so none starves
// however far demand exceeds the slots. The officers picked are marked
// passing; the rest wait for a later tick, measured by
// muster_officer_pass_oldest_wait_seconds and logged by noteWaiting once one
// has waited past its poll interval.
func (r *Reconciler) startDue(list []Officer, now time.Time) []passCandidate {
	r.mu.Lock()
	var due []passCandidate
	for _, o := range list {
		key := o.Key()
		if r.passing[key] {
			continue
		}
		s := r.st(key)
		if now.Before(s.nextAt) {
			// Not due (a pass outside Run — Check — may have moved nextAt
			// on): no wait to date.
			s.dueSeen = time.Time{}
			continue
		}
		if s.dueSeen.IsZero() {
			s.dueSeen = now
		}
		since := s.nextAt
		if since.IsZero() || s.dueSeen.Before(since) {
			since = s.dueSeen
		}
		due = append(due, passCandidate{o: o, since: since})
	}
	passOrder(due)
	n := min(max(maxConcurrentPasses-len(r.passing), 0), len(due))
	start, waiting := due[:n], due[n:]
	for _, c := range start {
		r.passing[c.o.Key()] = true
		r.st(c.o.Key()).dueSeen = time.Time{}
	}
	metrics.OfficerPassesInFlight.Set(float64(len(r.passing)))
	oldest := time.Duration(0)
	var late []passCandidate
	for _, c := range waiting {
		wait := max(now.Sub(c.since), 0)
		oldest = max(oldest, wait)
		if s := r.st(c.o.Key()); c.o.PollInterval > 0 && wait > c.o.PollInterval && !s.lateLogged {
			s.lateLogged = true
			late = append(late, c)
		}
	}
	running := len(r.passing)
	r.mu.Unlock()
	metrics.OfficerPassOldestWait.Set(oldest.Seconds())
	for _, c := range late {
		r.noteWaiting(c.o, now.Sub(c.since), running)
	}
	return start
}

// noteWaiting logs that o has been due for wait — past its poll interval —
// without a free pass slot (running passes hold them all): once per episode,
// which shares lateLogged with notePassLag, so the late start that ends the
// wait is not logged again.
func (r *Reconciler) noteWaiting(o Officer, wait time.Duration, running int) {
	r.log.Printf("officers: %s: due %ds ago and still waiting for a pass slot (%d of %d running, poll_interval %s); logged once until a pass starts on time",
		o.ID, int(wait/time.Second), running, maxConcurrentPasses, o.PollInterval)
}

// passEnded takes the officer keyed key out of passing: its pass is over.
// A Due asked for during it makes the officer due now (dueAfterPass).
func (r *Reconciler) passEnded(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.passing, key)
	if s, ok := r.state[key]; ok && s.dueAfterPass {
		// A Due asked for during the pass: due now. Its wait starts at the
		// first tick that sees it due after this — dueSeen was cleared when
		// the pass started (startDue) and no tick sets it while the pass runs
		// — never at a wait from before the pass.
		s.dueAfterPass = false
		s.nextAt = time.Time{}
	}
	metrics.OfficerPassesInFlight.Set(float64(len(r.passing)))
}

// pass runs one pass of officer o, due since dueAt (passCandidate.since:
// never zero), bounded by passTimeout: its start lag and its duration are
// measured, and a panic in it is logged, not propagated.
func (r *Reconciler) pass(ctx context.Context, o Officer, dueAt time.Time) {
	key := o.Key()
	pctx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Printf("officers: PANIC in a pass for %s: %v", key, rec)
		}
	}()
	began := time.Now()
	r.notePassLag(o, dueAt, r.now())
	r.Check(pctx, key, false)
	metrics.OfficerPassDuration.Observe(time.Since(began).Seconds())
}

// notePassLag records how late o's pass, due at dueAt, started at now
// (muster_officer_pass_start_lag_seconds), and logs a start more than o's
// poll interval late — once per episode: the next pass that starts on time
// ends it. A first pass, or one Due asked for, is dated from the tick Run
// first saw it due (passCandidate.since), so it is measured and, started at
// once, ends an episode like any on-time pass.
func (r *Reconciler) notePassLag(o Officer, dueAt, now time.Time) {
	lag := max(now.Sub(dueAt), 0)
	metrics.OfficerPassStartLag.Observe(lag.Seconds())
	late := o.PollInterval > 0 && lag > o.PollInterval
	r.mu.Lock()
	s := r.st(o.Key())
	first := late && !s.lateLogged
	s.lateLogged = late
	r.mu.Unlock()
	if first {
		r.log.Printf("officers: %s: pass started %ds late (poll_interval %s); logged once until a pass starts on time",
			o.ID, int(lag/time.Second), o.PollInterval)
	}
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
//
// 🔴 A FRESH POOL'S IS THE KICKOFF (Officer.FreshSessionPerItem): its turn
// starts a NEW conversation (internal/api OfficerTurnContext), which has no
// "as before" to refer to — it must call officer_brief and load the charter
// like a first start.
func RepromptText(o Officer, n int) string {
	if o.FreshSessionPerItem() {
		return KickoffText(o, n)
	}
	again := fmt.Sprintf("Call %s again only if you need your charter.", ToolBrief)
	if o.IsBuiltin() {
		// muster refuses a builtin instance's claim and complete while its
		// charter is newer than its last brief (officers.CharterGated)
		again = fmt.Sprintf("If %s or %s answers charter_changed, your charter changed: call %s, follow it, and retry.", ToolClaim, ToolComplete, ToolBrief)
	}
	return fmt.Sprintf("New work for officer %s (you are instance %s, %s): call %s and work the queue as before — %s, %s while you work, %s when done. ",
		o.ID, o.InstanceLabel(n), o.InstanceName(n), ToolItems, ToolClaim, ToolHeartbeat, ToolComplete) + again
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
