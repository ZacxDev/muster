package officers

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// THE PER-ITEM READ BUDGET (docs/officers.md "The read budget"): 200 reads
// per item, ONE count shared by the three read tools.
//
// A judgement instance investigating an item reads three ways: the cluster
// (officer_kube_read), raw PromQL / LogQL (officer_obs_raw) and HQ's named
// queries (officer_obs_query). Each read it makes for an item spends ONE
// unit of that item's single counter, whichever tool made it, so the cap is
// on the investigation and not on a tool: 200 kube reads and 200 raw queries
// is not 400. CountsRead is the one rule for which calls spend it; the server
// keeps ONE ReadBudget for every tool (internal/api officerRuntime.reads).
//
// officer_kube_read and officer_obs_raw always count: both need a live claim
// and are offered to the judgement pool only. officer_obs_query is every
// pool's tool and needs no claim: a JUDGEMENT instance's call counts against
// the item it holds, and one holding no item spends nothing (it is no item's
// read); a routine instance's never counts (routine items are not
// investigated).
//
// Which ANSWERS count (ReadCounted): the two HQ tools reserve the unit before
// they call HQ and give it back when HQ answers 429 (its rate limit) or a 5xx,
// or when no answer arrives (a timeout, a transport failure): the read was
// not made. HQ's 2xx and its other 4xx refusals (403 not_investigating, 400
// invalid, …) count — the instance spent the read on a query HQ judged. A
// call muster refuses before HQ (a bad key, the item) spends nothing.
// Reserving first is what keeps concurrent calls from passing the cap.
//
// In memory: a restart forgets the counts, which can only allow more reads,
// never refuse one early. Entries untouched for a day are dropped.
// ---------------------------------------------------------------------------

// ReadsPerItem is the per-item read budget.
const ReadsPerItem = 200

// ReadBudgetNote states the budget, exactly, wherever the instance is told it
// (the kickoff and each counted tool's description).
const ReadBudgetNote = "200 reads per item, ONE count shared by officer_kube_read, officer_obs_raw and officer_obs_query " +
	"(a judgement instance's officer_obs_query counts against the item it holds, and is not counted while it holds none); " +
	"the 201st read of an item is refused, whichever tool makes it. An officer_obs_raw or officer_obs_query call HQ answers " +
	"with 429 or a 5xx, or that gets no answer from HQ, is not counted; HQ's other refusals are"

// CountsRead reports whether a call of tool by an instance of pool o spends
// its item's read budget: THE rule (see above).
func CountsRead(o Officer, tool string) bool {
	switch tool {
	case ToolKubeRead, ToolObsRaw:
		return ToolOffered(o, tool)
	case ToolObsQuery:
		return o.Tier == TierJudgement
	}
	return false
}

// ReadCounted reports whether an HQ read tool's call that HQ answered with
// err (nil: a 2xx) spends its item's read: THE rule for which answers count
// (see above). Only HQ's 2xx and its 4xx other than 429 do.
func ReadCounted(err error) bool {
	if err == nil {
		return true
	}
	var hq *HQError
	return errors.As(err, &hq) && hq.Status >= 400 && hq.Status < 500 && hq.Status != http.StatusTooManyRequests
}

// ErrReadBudget is a read refused because its item's budget is spent.
var ErrReadBudget = errors.New("officers: the item's read budget is spent")

// ReadBudget counts reads per (officer, item).
type ReadBudget struct {
	perItem int
	now     func() time.Time

	mu sync.Mutex
	m  map[readKey]*readCount
}

type readKey struct {
	officer string
	item    ItemID
}

type readCount struct {
	n    int
	last time.Time
}

// NewReadBudget is a budget of perItem reads per item; now nil is time.Now.
func NewReadBudget(perItem int, now func() time.Time) *ReadBudget {
	if now == nil {
		now = time.Now
	}
	return &ReadBudget{perItem: perItem, now: now, m: map[readKey]*readCount{}}
}

// Spend counts one read of item by officer and returns how many it has made,
// or refuses (ErrReadBudget, nothing counted) once perItem are spent.
func (b *ReadBudget) Spend(officer string, item ItemID) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for k, c := range b.m {
		if now.Sub(c.last) > 24*time.Hour {
			delete(b.m, k)
		}
	}
	k := readKey{officer, item}
	c, ok := b.m[k]
	if !ok {
		c = &readCount{}
		b.m[k] = c
	}
	if c.n >= b.perItem {
		return c.n, fmt.Errorf("%w: officer %s has made all %d reads of item %s (%s, %s and %s calls together); "+
			"conclude from what you have, and list what you could not check", ErrReadBudget, officer, b.perItem, item, ToolKubeRead, ToolObsRaw, ToolObsQuery)
	}
	c.n++
	c.last = now
	return c.n, nil
}

// Refund gives back one read Spend counted of item by officer (a reserved
// read whose answer does not count, ReadCounted); never below 0.
func (b *ReadBudget) Refund(officer string, item ItemID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c, ok := b.m[readKey{officer, item}]; ok && c.n > 0 {
		c.n--
	}
}

// Spent is how many counted reads of item officer has made.
func (b *ReadBudget) Spent(officer string, item ItemID) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c, ok := b.m[readKey{officer, item}]; ok {
		return c.n
	}
	return 0
}
