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
// officer_claim). Re-claiming the item it already holds keeps its count.
func (t *items) claimed(k instanceKey, item string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	h := t.get(k)
	if h.item == item {
		return
	}
	*h = held{item: item, since: at}
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
	*h = held{since: at}
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
			*h = held{item: its[0], since: at}
		case h.item != "":
			*h = held{since: at}
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

// add charges n tokens to the instance's held item, if it still holds item
// (the one the request was admitted under). A request admitted holding no
// item is charged to nothing.
func (t *items) add(k instanceKey, item string, n int64) {
	if item == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	h := t.get(k)
	if h.item == item {
		h.tokens += n
	}
}
