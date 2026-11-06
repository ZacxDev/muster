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
// queries (officer_obs_query); a routine instance two (no cluster). Each read it makes for an item spends ONE
// unit of that item's single counter, whichever tool made it, so the cap is
// on the investigation and not on a tool: 200 kube reads and 200 raw queries
// is not 400. CountsRead is the one rule for which calls spend it; the server
// keeps ONE ReadBudget for every tool (internal/api officerRuntime.reads).
//
// officer_kube_read and officer_obs_raw always count, on every instance
// offered them: both need a live claim. officer_kube_read is the judgement
// pool's; officer_obs_raw is both pools' (the routine pool's by operator
// decision), so a routine instance's raw read spends its routine
// item's budget exactly as a judgement one does. officer_obs_query is every
// officer's tool and needs no claim: a JUDGEMENT instance's call counts
// against the item it holds, and one holding no item spends nothing (it is
// no item's read); a routine instance's never counts.
//
// Which ANSWERS count (ReadCounted): the two HQ tools reserve the unit before
// they call HQ and give it back when HQ answers 429 (its rate limit), a 5xx
// or a 403 whose code is not_investigating (HQ refused to read for this item
// at all — today every routine item's raw read), or when no answer arrives (a
// timeout, a transport failure): the read was not made. HQ's 2xx and its
// other 4xx refusals (a 403 with any other code, 400 invalid, …) count — the
// instance spent the read on a query HQ judged. A call muster refuses before
// HQ (a bad key, the item) spends nothing. Reserving first is what keeps
// concurrent calls from passing the cap.
//
// THE COUNT IS THE ITEM'S, NOT THE INSTANCE'S: it is keyed on (officer,
// item), and needs_judgement promotes a routine item IN PLACE (same id), so
// the reads a routine instance made of an item it hands on count toward the
// judgement instance's same 200. That is the design (one budget per item),
// stated in ReadBudgetNote.
//
// In memory: a restart forgets the counts, which can only allow more reads,
// never refuse one early. Entries untouched for a day are dropped.
// ---------------------------------------------------------------------------

// ReadsPerItem is the per-item read budget.
const ReadsPerItem = 200

// ReadBudgetNote states the budget, exactly, wherever the instance is told it
// (the kickoff and each counted tool's description).
const ReadBudgetNote = "200 reads per item, ONE count shared by officer_kube_read, officer_obs_raw and officer_obs_query " +
	"(every officer_obs_raw call counts, a routine instance's too; a judgement instance's officer_obs_query counts against the item it holds, " +
	"and is not counted while it holds none; a routine instance's is never counted); " +
	"the 201st read of an item is refused, whichever tool makes it. The count is the item's, not the instance's: " +
	"reads a routine instance made of an item it handed on with needs_judgement count toward the same 200 once it is a judgement item. " +
	"An officer_obs_raw or officer_obs_query call HQ answers with 429, a 5xx or a 403 not_investigating, or that gets no answer from HQ, " +
	"is not counted; HQ's other refusals are"

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

// CodeNotInvestigating is HQ's code on a 403 refusing a read for the item
// outright: the officer's type does not investigate, or HQ takes no raw read
// of an item of that tier.
const CodeNotInvestigating = "not_investigating"

// ReadCounted reports whether an HQ read tool's call that HQ answered with
// err (nil: a 2xx) spends its item's read: THE rule for which answers count
// (see above). Only HQ's 2xx and its 4xx other than 429 and a 403
// not_investigating do.
func ReadCounted(err error) bool {
	if err == nil {
		return true
	}
	var hq *HQError
	if !errors.As(err, &hq) || hq.Status < 400 || hq.Status >= 500 || hq.Status == http.StatusTooManyRequests {
		return false
	}
	return hq.Status != http.StatusForbidden || hq.Code() != CodeNotInvestigating
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
