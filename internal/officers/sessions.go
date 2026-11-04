package officers

import (
	"fmt"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// THE JUDGEMENT-TIER DAILY SESSION CAP (officers plan §3).
//
// A tiered officer's judgement pool runs on Claude; `tiers.judgement.
// max_sessions_per_day` bounds how many judgement SESSIONS it starts per UTC
// day. A session is one distinct item the judgement pool claims that day:
// re-claiming an item already counted today (a restarted instance, an item
// given back and taken again) is not a new session.
//
// Two places read the ledger, and they must agree:
//   - officer_claim (internal/api): a judgement instance's claim of a NEW item
//     once the day's cap is spent is refused before HQ — the item stays
//     queued there, never dropped;
//   - the wake loop (Decide): a capped judgement pool is not woken or
//     re-prompted for new work, only for its held items and items already
//     counted today. An instance woken for work it may not claim would spend
//     a Claude session on being refused.
//
// ⚠ HELD IN muster-server's MEMORY. A restart starts the day's count again,
// so each restart can admit at most one more cap's worth that day. The routine
// pool's USD budget, which is real money, is persisted (officer_llm_usage);
// this cap bounds subscription usage and is not.
// ---------------------------------------------------------------------------

// MaxSessionsPerDayCap bounds tiers.judgement.max_sessions_per_day.
const MaxSessionsPerDayCap = 100

// SessionCap is a pool's daily judgement-session cap: 0 (no cap) unless o is
// the judgement pool of a tiered officer that declares one.
func (o Officer) SessionCap() int {
	if o.Tiers == nil || o.Tier != TierJudgement {
		return 0
	}
	return o.Tiers.Judgement.MaxSessionsPerDay
}

// JudgementLedger counts each officer's judgement sessions per UTC day. Its
// zero value is not usable; NewJudgementLedger. Safe for concurrent use.
type JudgementLedger struct {
	mu sync.Mutex
	// days is officer id -> the UTC day counted -> the items counted.
	days map[string]ledgerDay
}

type ledgerDay struct {
	day   string
	items map[ItemID]bool
}

// NewJudgementLedger returns an empty ledger.
func NewJudgementLedger() *JudgementLedger {
	return &JudgementLedger{days: map[string]ledgerDay{}}
}

func utcDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

// today is o's counted items for now's UTC day (a fresh day drops the last).
// Called with mu held.
func (l *JudgementLedger) today(o Officer, now time.Time) ledgerDay {
	d, ok := l.days[o.ID]
	if !ok || d.day != utcDay(now) {
		d = ledgerDay{day: utcDay(now), items: map[ItemID]bool{}}
		l.days[o.ID] = d
	}
	return d
}

// Reserve admits a claim of item by pool o at now: always for a pool with no
// cap; for a capped judgement pool, when item is already counted today or the
// day's count is under the cap — counting it at once, so two claims racing for
// the last session cannot both be admitted. The caller undoes a reservation
// whose claim HQ then refused with undo (a no-op when item was already
// counted or o is uncapped). A refusal is an error naming the cap.
func (l *JudgementLedger) Reserve(o Officer, item ItemID, now time.Time) (undo func(), err error) {
	limit := o.SessionCap()
	if l == nil || limit == 0 {
		return func() {}, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	d := l.today(o, now)
	if d.items[item] {
		return func() {}, nil
	}
	if len(d.items) > limit {
		return nil, fmt.Errorf("officer %s has started all %d of its judgement sessions for today (UTC) "+
			"(tiers.judgement.max_sessions_per_day): item %s stays queued in HQ and is not claimed. Do not retry today; "+
			"stop working the queue — muster wakes this pool again tomorrow", o.ID, limit, item)
	}
	d.items[item] = true
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if cur, ok := l.days[o.ID]; ok && cur.day == d.day {
			delete(cur.items, item)
		}
	}, nil
}

// Capped reports whether pool o has spent its cap for now's UTC day, and the
// items it counted today (still work for it at the cap). An uncapped pool is
// never capped.
func (l *JudgementLedger) Capped(o Officer, now time.Time) (bool, map[ItemID]bool) {
	limit := o.SessionCap()
	if l == nil || limit == 0 {
		return false, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	d := l.today(o, now)
	counted := make(map[ItemID]bool, len(d.items))
	for k := range d.items {
		counted[k] = true
	}
	return len(d.items) >= limit, counted
}
