package llmproxy

import (
	"slices"
	"sync"
	"time"
)

// items is the per-item token meter behind max_tokens_per_item: for each
// (officer, instance), the item it holds now and the tokens it has used since
// it began holding it.
//
// It is told about claims by the officer tools (Claimed, Ended) and about what
// HQ says each instance holds by the wake loop (Sync). Sync also ADOPTS: when
// HQ shows an instance holding exactly one item and the meter holds a
// different one or none, the meter holds HQ's. That covers a claim path that
// does not call Claimed and a muster restart (an instance outlives it still
// holding its item), within one wake-loop interval. When the held item
// changes, the count starts again at zero.
//
// 🔴 A CAPPED INSTANCE IS NOT STUCK FOREVER. Once the cap is reached the
// instance's next request is refused, its turn ends, it cannot heartbeat, and
// its lease lapses in HQ (which promotes the item to the judgement tier). The
// wake loop's next read then shows the instance holding nothing, Sync clears
// its item, and its next item starts at zero. Tokens used while holding no
// item are not counted (the proxy applies the cap to a held item only): the
// daily USD budget, not this cap, is the wall for a turn holding no item.
//
// In memory only: a restart forgets the counts (the daily budget is the
// persisted wall); an adopted item starts at zero.
//
// It also keeps the hold's COST (Proxy.ItemCost): the USD every request it
// charged booked, as the provider reported it. 🔴 A COST IS KNOWN ONLY WHEN
// THIS HOLD IS THE ITEM'S WHOLE PROXY SPEND: HQ's first claim of it (attempts
// 1, so no earlier hold spent on it), told to the meter by that claim (not
// adopted by Sync, which follows a restart that forgot the requests before
// it), at least one request charged (no request is no evidence, never a 0),
// and every one at the provider's reported cost (one booked from the proxy's
// pessimistic estimate is not a measured cost), and no request admitted under
// the hold still in flight, i.e. not yet charged to it (its cost would be
// missing from the sum). A request leaves flight the moment book charges it. A later HQ claim of the held item (attempts 2+: its lease lapsed and it
// was claimed again) makes it unknown.
type items struct {
	mu sync.Mutex
	m  map[instanceKey]*held
}

type instanceKey struct {
	officer  string
	instance int
}

type held struct {
	item   string
	since  time.Time
	tokens int64
	// usd and requests are what the hold's charged requests booked
	// (estimated: one of them from the proxy's estimate). partial is why they
	// are NOT the item's whole proxy spend (see the type), "" while they are:
	// CostAdopted, CostNotFirstClaim or CostAttemptsUnknown.
	usd       float64
	requests  int
	estimated bool
	partial   string
	// inflight is the requests admitted under this hold that are not yet
	// booked into it (admit, then done: as soon as book has charged them, or
	// when the handler returns for a request never booked). A hold is
	// replaced by a NEW *held whenever it changes, so a request admitted under
	// an earlier hold finishes on that one and never touches its successor's.
	inflight int
}

// Why an item's cost is not known (Proxy.ItemCostWhy), each a short id for
// the log line officer_complete writes when it sends no cost.
const (
	CostNotHeld         = "not_held"         // the instance does not hold the item (in the meter)
	CostAdopted         = "adopted"          // adopted from HQ's claimed list (a restart): requests before it are not in it
	CostNotFirstClaim   = "not_first_claim"  // HQ's second or later claim: an earlier hold may have spent on it
	CostAttemptsUnknown = "attempts_unknown" // the claim carried no attempts (already-yours, or an HQ without the field)
	CostNoRequests      = "no_requests"      // no request booked: no evidence, never a 0
	CostEstimated       = "estimated"        // a request was booked from the proxy's estimate
	CostInFlight        = "inflight"         // a request admitted under the hold is not booked yet
)

// partialOf is a new hold's partial for a claim HQ answered with attempts.
func partialOf(attempts int) string {
	switch {
	case attempts == 1:
		return ""
	case attempts == 0:
		return CostAttemptsUnknown
	}
	return CostNotFirstClaim
}

// flight is one admitted request's count on its hold: released once (done),
// however many times done is called.
type flight struct {
	h        *held
	released bool
}

func newItems() *items { return &items{m: map[instanceKey]*held{}} }

func (t *items) get(k instanceKey) *held {
	h, ok := t.m[k]
	if !ok {
		h = &held{}
		t.m[k] = h
	}
	return h
}

// claimed records that the instance now holds item (a successful
// officer_claim) by the claim HQ answered with attempts (0: not known, e.g.
// muster's already-yours answer, which is no new HQ claim). Re-claiming the
// item it already holds keeps its count, and a claim HQ counts as a second
// or later one (attempts > 1) makes its cost unknown.
func (t *items) claimed(k instanceKey, item string, at time.Time, attempts int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	h := t.get(k)
	if h.item == item {
		if attempts > 1 {
			h.partial = CostNotFirstClaim
		}
		return
	}
	t.m[k] = &held{item: item, since: at, partial: partialOf(attempts)}
}

// ended records that the instance no longer holds item (complete or
// release). An end for an item it does not hold changes nothing.
func (t *items) ended(k instanceKey, item string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	h := t.get(k)
	if h.item != item {
		return
	}
	t.m[k] = &held{since: at}
}

// sync is a wake-loop pass's view of officer's claims, read at at: holding
// maps an instance number to the items HQ shows it claiming. Only an instance
// whose meter last changed before at is touched (a read never undoes a later
// claim or end). If HQ shows it holding the meter's item, its count is kept.
// Otherwise, if HQ shows it holding exactly one item, the meter adopts that
// item from zero; if HQ shows none or several, it holds nothing.
func (t *items) sync(officer string, holding map[int][]string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for n := range holding {
		t.get(instanceKey{officer, n}) // an instance the meter has not seen (a restart) can be adopted too
	}
	for k, h := range t.m {
		if k.officer != officer || h.since.After(at) {
			continue
		}
		its := holding[k.instance]
		switch {
		case h.item != "" && slices.Contains(its, h.item):
		case len(its) == 1:
			t.m[k] = &held{item: its[0], since: at, partial: CostAdopted}
		case h.item != "":
			t.m[k] = &held{since: at}
		}
	}
}

// used is the tokens the instance has used on its held item, and the item.
func (t *items) used(k instanceKey) (int64, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	h := t.get(k)
	return h.tokens, h.item
}

// admit is used, for a request the proxy is admitting: when the instance
// holds an item, the request counts as in flight on that hold until done is
// called with the flight admit returned (nil when it holds none).
func (t *items) admit(k instanceKey) (int64, string, *flight) {
	t.mu.Lock()
	defer t.mu.Unlock()
	h := t.get(k)
	if h.item == "" {
		return h.tokens, "", nil
	}
	h.inflight++
	return h.tokens, h.item, &flight{h: h}
}

// done takes f's request out of flight on its hold. It is idempotent: book
// calls it once the request is charged, and the handler's deferred call (for
// a request never booked) is then a no-op. nil is no flight.
func (t *items) done(f *flight) {
	if f == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if f.released {
		return
	}
	f.released = true
	f.h.inflight--
}

// add charges n tokens and usd (estimated: booked from the proxy's estimate)
// to the instance's held item, if it still holds item (the one the request
// was admitted under). A request admitted holding no item is charged to
// nothing.
func (t *items) add(k instanceKey, item string, n int64, usd float64, estimated bool) {
	if item == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	h := t.get(k)
	if h.item == item {
		h.tokens += n
		h.usd += usd
		h.requests++
		h.estimated = h.estimated || estimated
	}
}

// cost is the USD the instance's hold on item booked, and why that is NOT
// known to be the item's whole proxy spend ("" when it is; see the type):
// the instance does not hold item, the hold is partial, it charged no
// request, one was booked from the proxy's estimate, or a request admitted
// under it is not booked yet.
func (t *items) cost(k instanceKey, item string) (float64, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	h, ok := t.m[k]
	switch {
	case !ok || item == "" || h.item != item:
		return 0, CostNotHeld
	case h.partial != "":
		return 0, h.partial
	case h.requests == 0:
		return 0, CostNoRequests
	case h.estimated:
		return 0, CostEstimated
	case h.inflight > 0:
		return 0, CostInFlight
	}
	return h.usd, ""
}
