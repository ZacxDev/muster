package officers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
// 🔴 PERSISTED, AND ADMITTED BY ONE STATEMENT (operator, #436 round 0): the
// ledger is officer_judgement_sessions (migration 0079), one row per
// (officer, UTC day) of admitted item ids, shared by every muster-server
// replica. PGSessionStore.AdmitSession is ONE INSERT … ON CONFLICT DO UPDATE
// whose WHERE re-checks the cap under the row lock, so a restart resets
// nothing and two replicas in a rollout cannot both admit the N+1th. There is
// no in-memory count: one rule, one place.
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

// HasSessionCap reports whether any pool of o is capped.
func (o Officer) HasSessionCap() bool {
	return o.Tiers != nil && o.Tiers.Judgement.MaxSessionsPerDay > 0
}

// SessionStore is the cap's durable ledger. day is a UTC date (2006-01-02);
// item an HQ item id or a pending claim-next token (pendingPrefix).
type SessionStore interface {
	// AdmitSession admits item as one of officer's sessions on day when it is
	// already counted (free) or the day holds fewer than limit — atomically.
	AdmitSession(ctx context.Context, officer, day, item string, limit int) (bool, error)
	// ReleaseSession gives item's session back (its claim did not happen).
	ReleaseSession(ctx context.Context, officer, day, item string) error
	// ResolveSession replaces pending token with item, or drops the token
	// when item is "" or already counted.
	ResolveSession(ctx context.Context, officer, day, token, item string) error
	// Sessions is the day's admitted entries, in admission order.
	Sessions(ctx context.Context, officer, day string) ([]string, error)
}

// pendingPrefix marks a claim-next's entry until HQ names its item. HQ's item
// ids are integers, so none collides.
const pendingPrefix = "pending:"

// ErrSessionCap is a claim the day's cap refuses; the error's text says so.
var ErrSessionCap = errors.New("officers: judgement session cap reached")

// JudgementLedger applies the cap over a SessionStore. scope gives the store
// call the principal of o's owner (the table is owner-scoped). A nil ledger,
// or an uncapped pool, admits everything.
type JudgementLedger struct {
	store SessionStore
	scope func(ctx context.Context, o Officer) context.Context
}

// NewJudgementLedger is the ledger over store; scope may be nil (ctx as is).
func NewJudgementLedger(store SessionStore, scope func(context.Context, Officer) context.Context) *JudgementLedger {
	if scope == nil {
		scope = func(ctx context.Context, _ Officer) context.Context { return ctx }
	}
	return &JudgementLedger{store: store, scope: scope}
}

func utcDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

// Reserve admits a claim of item by pool o at now, counting it at once. undo
// gives the session back when the claim then fails at HQ (a no-op for an
// uncapped pool or an item already counted). The cap's refusal wraps
// ErrSessionCap; a store failure is returned as is (the caller fails closed).
func (l *JudgementLedger) Reserve(ctx context.Context, o Officer, item ItemID, now time.Time) (undo func(), err error) {
	limit := o.SessionCap()
	if l == nil || limit == 0 {
		return func() {}, nil
	}
	sctx, day := l.scope(ctx, o), utcDay(now)
	before, err := l.store.Sessions(sctx, o.ID, day)
	if err != nil {
		return nil, fmt.Errorf("officers: read %s's judgement sessions: %w", o.ID, err)
	}
	ok, err := l.store.AdmitSession(sctx, o.ID, day, string(item), limit)
	if err != nil {
		return nil, fmt.Errorf("officers: admit %s's judgement session: %w", o.ID, err)
	}
	if !ok {
		return nil, capRefusal(o, limit, "item "+string(item))
	}
	for _, e := range before {
		if e == string(item) {
			return func() {}, nil // counted before this claim: undo gives nothing back
		}
	}
	return func() {
		_ = l.store.ReleaseSession(context.WithoutCancel(sctx), o.ID, day, string(item))
	}, nil
}

// ReserveNext admits a claim-next (officer_claim with no item_id: HQ picks
// the item) by pool o at now, holding one session under a pending token until
// done is called with the item HQ claimed — or "" when it claimed none, which
// gives the session back. An item already counted today is not counted twice.
func (l *JudgementLedger) ReserveNext(ctx context.Context, o Officer, now time.Time) (done func(ItemID), err error) {
	limit := o.SessionCap()
	if l == nil || limit == 0 {
		return func(ItemID) {}, nil
	}
	sctx, day := l.scope(ctx, o), utcDay(now)
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	token := pendingPrefix + hex.EncodeToString(b[:])
	ok, err := l.store.AdmitSession(sctx, o.ID, day, token, limit)
	if err != nil {
		return nil, fmt.Errorf("officers: admit %s's judgement session: %w", o.ID, err)
	}
	if !ok {
		return nil, capRefusal(o, limit, "the next item")
	}
	return func(item ItemID) {
		_ = l.store.ResolveSession(context.WithoutCancel(sctx), o.ID, day, token, string(item))
	}, nil
}

// Capped reports whether pool o has spent its cap for now's UTC day (pending
// claim-nexts included), and the items it counted today (still work for it
// at the cap). An uncapped pool is never capped. A store failure is returned.
func (l *JudgementLedger) Capped(ctx context.Context, o Officer, now time.Time) (bool, map[ItemID]bool, error) {
	limit := o.SessionCap()
	if l == nil || limit == 0 {
		return false, nil, nil
	}
	entries, err := l.store.Sessions(l.scope(ctx, o), o.ID, utcDay(now))
	if err != nil {
		return false, nil, fmt.Errorf("officers: read %s's judgement sessions: %w", o.ID, err)
	}
	counted := map[ItemID]bool{}
	for _, e := range entries {
		if !strings.HasPrefix(e, pendingPrefix) {
			counted[ItemID(e)] = true
		}
	}
	return len(entries) >= limit, counted, nil
}

// capRefusal is the cap's refusal: what was not claimed, and that the item
// stays queued in HQ.
func capRefusal(o Officer, limit int, what string) error {
	return fmt.Errorf("%w: officer %s has started all %d of its judgement sessions for today (UTC) "+
		"(tiers.judgement.max_sessions_per_day): %s stays queued in HQ and is not claimed. Do not retry today; "+
		"stop working the queue — muster wakes this pool again tomorrow", ErrSessionCap, o.ID, limit, what)
}

// PGSessionStore is SessionStore on officer_judgement_sessions (migration
// 0079), under row-level security: ctx must act as the officer's owner.
type PGSessionStore struct{ pool *pgxpool.Pool }

// NewPGSessionStore is the store over pool.
func NewPGSessionStore(pool *pgxpool.Pool) *PGSessionStore { return &PGSessionStore{pool: pool} }

// admitSQL is the admission, ONE statement: insert the day's row with item,
// or append item to it — only when item is not already there and the row
// holds fewer than $4. ON CONFLICT DO UPDATE takes the row lock and evaluates
// its WHERE on the row's latest version, so concurrent admits at cap-1 admit
// exactly one. Admitted = this statement wrote item, or item was already
// counted when it started.
const admitSQL = `
WITH up AS (
	INSERT INTO officer_judgement_sessions AS s (officer, day, items)
	VALUES ($1, $2::date, ARRAY[$3::text])
	ON CONFLICT (owner_id, officer, day) DO UPDATE
		SET items = s.items || $3::text, updated_at = now()
		WHERE NOT ($3::text = ANY (s.items)) AND cardinality(s.items) < $4
	RETURNING 1
)
SELECT EXISTS (SELECT 1 FROM up)
    OR EXISTS (SELECT 1 FROM officer_judgement_sessions WHERE officer = $1 AND day = $2::date AND $3::text = ANY (items))`

// AdmitSession implements SessionStore.
func (s *PGSessionStore) AdmitSession(ctx context.Context, officer, day, item string, limit int) (bool, error) {
	items, err := s.Sessions(ctx, officer, day)
	if err != nil {
		return false, err
	}
	for _, e := range items {
		if e == item {
			return true, nil
		}
	}
	if len(items) >= limit {
		return false, nil
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO officer_judgement_sessions AS s (officer, day, items) VALUES ($1, $2::date, ARRAY[$3::text])
		ON CONFLICT (owner_id, officer, day) DO UPDATE SET items = s.items || $3::text WHERE NOT ($3::text = ANY (s.items))`, officer, day, item)
	return err == nil, err
}

// ReleaseSession implements SessionStore.
func (s *PGSessionStore) ReleaseSession(ctx context.Context, officer, day, item string) error {
	_, err := s.pool.Exec(ctx, `UPDATE officer_judgement_sessions SET items = array_remove(items, $3::text), updated_at = now()
		WHERE officer = $1 AND day = $2::date`, officer, day, item)
	return err
}

// ResolveSession implements SessionStore.
func (s *PGSessionStore) ResolveSession(ctx context.Context, officer, day, token, item string) error {
	_, err := s.pool.Exec(ctx, `UPDATE officer_judgement_sessions SET items = CASE
			WHEN $4::text = '' OR $4::text = ANY (items) THEN array_remove(items, $3::text)
			ELSE array_replace(items, $3::text, $4::text) END, updated_at = now()
		WHERE officer = $1 AND day = $2::date`, officer, day, token, item)
	return err
}

// Sessions implements SessionStore.
func (s *PGSessionStore) Sessions(ctx context.Context, officer, day string) ([]string, error) {
	var items []string
	err := s.pool.QueryRow(ctx, `SELECT items FROM officer_judgement_sessions WHERE officer = $1 AND day = $2::date`,
		officer, day).Scan(&items)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return items, err
}
